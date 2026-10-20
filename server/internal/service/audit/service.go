// Package audit records and serves the security audit log.
//
// Events that describe a security state change (accounts, sessions,
// memberships, roles, quotas) are written by their repositories in the
// same transaction as the change. Everything else (uploads, downloads,
// denied attempts) is recorded here after the fact; a failure to record is
// logged and counted, never turned into a failure of the operation.
package audit

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/correlation"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/metrics"
)

type Service struct {
	repo      interfaces.AuditRepository
	log       *slog.Logger
	metrics   *metrics.Registry
	retention time.Duration
	now       func() time.Time
}

func NewService(repo interfaces.AuditRepository, log *slog.Logger, registry *metrics.Registry, retention time.Duration) *Service {
	return &Service{repo: repo, log: log, metrics: registry, retention: retention, now: func() time.Time { return time.Now().UTC() }}
}

// Record appends e. It never fails the caller.
func (s *Service) Record(ctx context.Context, e model.AuditEvent) {
	e.Details = Scrub(e.Details)
	if _, err := s.repo.Append(context.WithoutCancel(ctx), e); err != nil {
		s.log.Error("audit event not recorded", "action", e.Action, "tenant_id", e.TenantID, "error", err)
		if s.metrics != nil {
			s.metrics.Add("audit_record_failures", 1)
		}
	}
}

// Filter selects audit events for a listing.
type Filter struct {
	BeforeSeq int64
	Limit     int
	Action    string
	Outcome   string
	ActorID   string
}

// List pages the caller's organization's audit log, newest first. Reading
// the audit log is itself audited, and so is a refused attempt.
func (s *Service) List(ctx context.Context, p model.Principal, f Filter) ([]model.AuditEvent, error) {
	if err := Require(ctx, s, p, model.PermAuditRead, "audit.read", "audit_log", p.TenantID); err != nil {
		return nil, err
	}
	events, err := s.repo.List(ctx, interfaces.AuditFilter{
		Chain: p.TenantID, BeforeSeq: f.BeforeSeq, Limit: f.Limit, Action: f.Action, Outcome: f.Outcome, ActorID: f.ActorID,
	})
	if err != nil {
		return nil, err
	}
	s.Record(ctx, From(ctx, p, "audit.read", "audit_log", p.TenantID))
	return events, nil
}

// PlatformList pages any chain (an organization's, or the platform's own
// with model.PlatformChain) for a platform administrator or auditor.
func (s *Service) PlatformList(ctx context.Context, p model.Principal, chain string, f Filter) ([]model.AuditEvent, error) {
	if err := s.requirePlatform(ctx, p, model.PlatformAuditRead, "audit.read", chain); err != nil {
		return nil, err
	}
	events, err := s.repo.List(ctx, interfaces.AuditFilter{
		Chain: chain, BeforeSeq: f.BeforeSeq, Limit: f.Limit, Action: f.Action, Outcome: f.Outcome, ActorID: f.ActorID,
	})
	if err != nil {
		return nil, err
	}
	e := From(ctx, p, "audit.read", "audit_log", chain)
	e.TenantID = ""
	s.Record(ctx, e)
	return events, nil
}

// Verify checks a chain's hashes. The organization's own auditors may
// verify its chain; platform auditors any chain.
func (s *Service) Verify(ctx context.Context, p model.Principal, chain string) (interfaces.AuditVerification, error) {
	if chain != p.TenantID || !p.Can(model.PermAuditRead) {
		if err := s.requirePlatform(ctx, p, model.PlatformAuditRead, "audit.verify", chain); err != nil {
			return interfaces.AuditVerification{}, err
		}
	}
	return s.repo.Verify(ctx, chain)
}

// VerifyAll checks every chain, for the operator CLI.
func (s *Service) VerifyAll(ctx context.Context) ([]interfaces.AuditVerification, error) {
	chains, err := s.repo.Chains(ctx)
	if err != nil {
		return nil, err
	}
	var out []interfaces.AuditVerification
	for _, chain := range chains {
		v, err := s.repo.Verify(ctx, chain)
		if err != nil {
			return out, err
		}
		out = append(out, v)
	}
	return out, nil
}

// Purge removes events older than the retention period, up to limit, and
// records that it did.
func (s *Service) Purge(ctx context.Context, limit int) (int, error) {
	now := s.now()
	n, err := s.repo.Purge(ctx, now.Add(-s.retention), limit, now)
	if n > 0 {
		e := System(ctx, "audit.purge", "audit_log", "")
		e.Details = map[string]any{"purged": n, "older_than": now.Add(-s.retention).Format(time.RFC3339)}
		s.Record(ctx, e)
	}
	return n, err
}

func (s *Service) requirePlatform(ctx context.Context, p model.Principal, perm model.PlatformPermission, action, resource string) error {
	if err := p.RequirePlatform(perm); err != nil {
		e := Denied(ctx, p, action, "audit_log", resource, err.Error())
		e.TenantID = ""
		s.Record(ctx, e)
		return err
	}
	return nil
}

// ─── Building events ──────────────────────────────────────────────────

// From is a successful action by p, with the request's correlation ID
// and client address.
func From(ctx context.Context, p model.Principal, action, resourceType, resourceID string) model.AuditEvent {
	actorType := model.ActorUser
	if p.Service {
		actorType = model.ActorService
	}
	return model.AuditEvent{
		TenantID: p.TenantID, ActorType: actorType, ActorID: p.UserID, Action: action,
		ResourceType: resourceType, ResourceID: resourceID, Outcome: model.AuditSuccess,
		CorrelationID: correlation.From(ctx), SourceIP: correlation.Source(ctx), CreatedAt: time.Now().UTC(),
	}
}

// Denied is a refused attempt by p.
func Denied(ctx context.Context, p model.Principal, action, resourceType, resourceID, reason string) model.AuditEvent {
	e := From(ctx, p, action, resourceType, resourceID)
	e.Outcome, e.Reason = model.AuditDenied, reason
	return e
}

// Anonymous is an action by a caller not (yet) identified, such as a
// failed sign-in.
func Anonymous(ctx context.Context, action, resourceType, resourceID string) model.AuditEvent {
	return model.AuditEvent{
		ActorType: model.ActorAnonymous, ActorID: "anonymous", Action: action, ResourceType: resourceType,
		ResourceID: resourceID, Outcome: model.AuditSuccess, CorrelationID: correlation.From(ctx),
		SourceIP: correlation.Source(ctx), CreatedAt: time.Now().UTC(),
	}
}

// System is an action by the system itself: a worker or a sweep.
func System(ctx context.Context, action, resourceType, resourceID string) model.AuditEvent {
	return model.AuditEvent{
		ActorType: model.ActorService, ActorID: "system", Action: action, ResourceType: resourceType,
		ResourceID: resourceID, Outcome: model.AuditSuccess, CorrelationID: correlation.From(ctx), CreatedAt: time.Now().UTC(),
	}
}

// Require checks perm for p; a refusal is recorded as a denied attempt
// at action on the resource.
func Require(ctx context.Context, rec interfaces.AuditRecorder, p model.Principal, perm model.Permission, action, resourceType, resourceID string) error {
	err := p.Require(perm)
	if err != nil && rec != nil {
		rec.Record(ctx, Denied(ctx, p, action, resourceType, resourceID, err.Error()))
	}
	return err
}

// sensitive are fragments of detail keys that must never be recorded:
// credentials, tokens and signed URLs.
var sensitive = []string{"password", "secret", "token", "authorization", "cookie", "url", "signature", "credential"}

// Scrub drops details whose keys look sensitive, at any depth. Callers
// should not pass them in the first place; this is the last line.
func Scrub(details map[string]any) map[string]any {
	if details == nil {
		return nil
	}
	out := make(map[string]any, len(details))
	for k, v := range details {
		lower := strings.ToLower(k)
		drop := false
		for _, s := range sensitive {
			if strings.Contains(lower, s) {
				drop = true
				break
			}
		}
		if drop {
			continue
		}
		out[k] = scrubValue(v)
	}
	return out
}

func scrubValue(v any) any {
	switch v := v.(type) {
	case map[string]any:
		return Scrub(v)
	case []any:
		list := make([]any, len(v))
		for i, item := range v {
			list[i] = scrubValue(item)
		}
		return list
	}
	return v
}
