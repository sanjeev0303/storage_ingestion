// Package webhook manages an organization's webhook endpoints and sends
// them signed events (docs/webhooks.md).
package webhook

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/netguard"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/secretbox"
	"github.com/sanjeev0303/object-storage-platform/internal/service/audit"
)

var (
	ErrInvalid = errors.New("invalid webhook endpoint")
	// ErrNotConfigured: secrets cannot be sealed without the credential
	// keyring (STORAGE_CREDENTIAL_KEYS).
	ErrNotConfigured = errors.New("webhooks need the credential keyring (STORAGE_CREDENTIAL_KEYS)")
)

// MaxEndpoints bounds one organization's endpoints.
const MaxEndpoints = 20

type Store interface {
	Create(ctx context.Context, e model.WebhookEndpoint) error
	Get(ctx context.Context, tenantID, id string) (model.WebhookEndpoint, error)
	List(ctx context.Context, tenantID string) ([]model.WebhookEndpoint, error)
	Update(ctx context.Context, e model.WebhookEndpoint) error
	Delete(ctx context.Context, tenantID, id string) error
	Enqueue(ctx context.Context, d model.WebhookDelivery) error
	Deliveries(ctx context.Context, tenantID, endpointID, status, before string, limit int) ([]model.WebhookDelivery, string, error)
	Redeliver(ctx context.Context, tenantID, endpointID, deliveryID string, now time.Time) (model.WebhookDelivery, error)
}

type Service struct {
	store  Store
	sealer secretbox.Sealer
	policy netguard.Policy
	audit  interfaces.AuditRecorder
	now    func() time.Time
}

// NewService: sealer may be nil, and every change then answers
// ErrNotConfigured.
func NewService(store Store, sealer secretbox.Sealer, policy netguard.Policy, rec interfaces.AuditRecorder) *Service {
	return &Service{store: store, sealer: sealer, policy: policy, audit: rec, now: func() time.Time { return time.Now().UTC() }}
}

// Input is what a caller sets on an endpoint. Nil fields are left as they
// are on an update.
type Input struct {
	URL         *string
	Description *string
	EventTypes  []string
	Enabled     *bool
}

func (s *Service) require(ctx context.Context, p model.Principal, action, id string) error {
	return audit.Require(ctx, s.audit, p, model.PermOrgManage, action, "webhook", id)
}

func (s *Service) List(ctx context.Context, p model.Principal) ([]model.WebhookEndpoint, error) {
	if err := s.require(ctx, p, "webhook.read", ""); err != nil {
		return nil, err
	}
	return s.store.List(ctx, p.TenantID)
}

func (s *Service) Get(ctx context.Context, p model.Principal, id string) (model.WebhookEndpoint, error) {
	if err := s.require(ctx, p, "webhook.read", id); err != nil {
		return model.WebhookEndpoint{}, err
	}
	return s.store.Get(ctx, p.TenantID, id)
}

// Create registers an endpoint and returns it with its signing secret,
// which is shown this once.
func (s *Service) Create(ctx context.Context, p model.Principal, in Input) (model.WebhookEndpoint, string, error) {
	if err := s.require(ctx, p, "webhook.create", ""); err != nil {
		return model.WebhookEndpoint{}, "", err
	}
	if s.sealer == nil {
		return model.WebhookEndpoint{}, "", ErrNotConfigured
	}
	if in.URL == nil {
		return model.WebhookEndpoint{}, "", fmt.Errorf("%w: url is required", ErrInvalid)
	}
	existing, err := s.store.List(ctx, p.TenantID)
	if err != nil {
		return model.WebhookEndpoint{}, "", err
	}
	if len(existing) >= MaxEndpoints {
		return model.WebhookEndpoint{}, "", fmt.Errorf("%w: at most %d endpoints per organization", ErrInvalid, MaxEndpoints)
	}
	now := s.now()
	e := model.WebhookEndpoint{ID: newID(), TenantID: p.TenantID, Enabled: true, CreatedBy: p.UserID, CreatedAt: now, UpdatedAt: now}
	if err := s.apply(&e, in); err != nil {
		return model.WebhookEndpoint{}, "", err
	}
	secret, err := s.newSecret(&e)
	if err != nil {
		return model.WebhookEndpoint{}, "", err
	}
	if err := s.store.Create(ctx, e); err != nil {
		return model.WebhookEndpoint{}, "", err
	}
	s.record(ctx, p, "webhook.create", e)
	return e, secret, nil
}

func (s *Service) Update(ctx context.Context, p model.Principal, id string, in Input) (model.WebhookEndpoint, error) {
	if err := s.require(ctx, p, "webhook.update", id); err != nil {
		return model.WebhookEndpoint{}, err
	}
	e, err := s.store.Get(ctx, p.TenantID, id)
	if err != nil {
		return e, err
	}
	if err := s.apply(&e, in); err != nil {
		return e, err
	}
	e.UpdatedAt = s.now()
	if err := s.store.Update(ctx, e); err != nil {
		return e, err
	}
	s.record(ctx, p, "webhook.update", e)
	return e, nil
}

// RotateSecret replaces the signing secret at once and returns the new
// one. Receivers must switch to it; the old one stops signing immediately.
func (s *Service) RotateSecret(ctx context.Context, p model.Principal, id string) (string, error) {
	if err := s.require(ctx, p, "webhook.rotate_secret", id); err != nil {
		return "", err
	}
	if s.sealer == nil {
		return "", ErrNotConfigured
	}
	e, err := s.store.Get(ctx, p.TenantID, id)
	if err != nil {
		return "", err
	}
	secret, err := s.newSecret(&e)
	if err != nil {
		return "", err
	}
	e.UpdatedAt = s.now()
	if err := s.store.Update(ctx, e); err != nil {
		return "", err
	}
	s.record(ctx, p, "webhook.rotate_secret", e)
	return secret, nil
}

func (s *Service) Delete(ctx context.Context, p model.Principal, id string) error {
	if err := s.require(ctx, p, "webhook.delete", id); err != nil {
		return err
	}
	if err := s.store.Delete(ctx, p.TenantID, id); err != nil {
		return err
	}
	s.audit.Record(ctx, audit.From(ctx, p, "webhook.delete", "webhook", id))
	return nil
}

// Test queues a webhook.test event to the endpoint, whatever it
// subscribed to.
func (s *Service) Test(ctx context.Context, p model.Principal, id string) (model.WebhookDelivery, error) {
	if err := s.require(ctx, p, "webhook.test", id); err != nil {
		return model.WebhookDelivery{}, err
	}
	e, err := s.store.Get(ctx, p.TenantID, id)
	if err != nil {
		return model.WebhookDelivery{}, err
	}
	payload, _ := json.Marshal(map[string]string{"endpoint_id": e.ID, "requested_by": p.UserID})
	d := model.WebhookDelivery{
		ID: newID(), EventID: newID(), EndpointID: e.ID, TenantID: e.TenantID, EventType: "webhook.test",
		Payload: payload, Status: model.HookPending, NextAttemptAt: s.now(), CreatedAt: s.now(),
	}
	return d, s.store.Enqueue(ctx, d)
}

func (s *Service) Deliveries(ctx context.Context, p model.Principal, id, status, before string, limit int) ([]model.WebhookDelivery, string, error) {
	if err := s.require(ctx, p, "webhook.read", id); err != nil {
		return nil, "", err
	}
	switch model.DeliveryStatus(status) {
	case "", model.HookPending, model.HookDelivered, model.HookFailed:
	default:
		return nil, "", fmt.Errorf("%w: status must be PENDING, DELIVERED or FAILED", ErrInvalid)
	}
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	if _, err := s.store.Get(ctx, p.TenantID, id); err != nil {
		return nil, "", err
	}
	return s.store.Deliveries(ctx, p.TenantID, id, status, before, limit)
}

func (s *Service) Redeliver(ctx context.Context, p model.Principal, id, deliveryID string) (model.WebhookDelivery, error) {
	if err := s.require(ctx, p, "webhook.redeliver", id); err != nil {
		return model.WebhookDelivery{}, err
	}
	return s.store.Redeliver(ctx, p.TenantID, id, deliveryID, s.now())
}

// apply validates in and sets it on e.
func (s *Service) apply(e *model.WebhookEndpoint, in Input) error {
	if in.URL != nil {
		u, err := s.policy.ValidateURLWithPath(strings.TrimSpace(*in.URL))
		if err != nil {
			return fmt.Errorf("%w: %v", ErrInvalid, err)
		}
		e.URL = u.String()
	}
	if in.Description != nil {
		if len(*in.Description) > 500 {
			return fmt.Errorf("%w: description is longer than 500 characters", ErrInvalid)
		}
		e.Description = strings.TrimSpace(*in.Description)
	}
	if in.EventTypes != nil {
		types := []string{}
		for _, t := range in.EventTypes {
			if !slices.Contains(model.WebhookEventTypes, t) {
				return fmt.Errorf("%w: unknown event type %q", ErrInvalid, t)
			}
			if !slices.Contains(types, t) {
				types = append(types, t)
			}
		}
		if len(types) == 0 {
			return fmt.Errorf("%w: subscribe to at least one event type", ErrInvalid)
		}
		e.EventTypes = types
	}
	if in.Enabled != nil {
		e.Enabled = *in.Enabled
	}
	if len(e.EventTypes) == 0 {
		return fmt.Errorf("%w: event_types is required", ErrInvalid)
	}
	return nil
}

// newSecret makes a signing secret, seals it onto e and returns it in the
// form receivers use: whsec_ and the base64 of 32 random bytes.
func (s *Service) newSecret(e *model.WebhookEndpoint) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	sealed, err := s.sealer.Seal(raw, associatedData(e))
	if err != nil {
		return "", err
	}
	e.SecretKeyVersion, e.SecretNonce, e.SecretCipher = sealed.KeyVersion, sealed.Nonce, sealed.Ciphertext
	return "whsec_" + base64.StdEncoding.EncodeToString(raw), nil
}

// associatedData binds a sealed secret to its endpoint: copied to another
// row or tenant, it does not open.
func associatedData(e *model.WebhookEndpoint) []byte {
	return []byte("webhook:" + e.TenantID + ":" + e.ID)
}

func (s *Service) record(ctx context.Context, p model.Principal, action string, e model.WebhookEndpoint) {
	ev := audit.From(ctx, p, action, "webhook", e.ID)
	// The host only: a path or query can carry a receiver's token.
	host := e.URL
	if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+3:]
	}
	if i := strings.IndexAny(host, "/?"); i >= 0 {
		host = host[:i]
	}
	ev.Details = map[string]any{"host": host, "event_types": e.EventTypes, "enabled": e.Enabled}
	s.audit.Record(ctx, ev)
}

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
