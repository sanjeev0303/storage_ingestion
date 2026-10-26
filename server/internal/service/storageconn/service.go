// Package storageconn manages an organization's storage connections
// (Phase 11): where its bytes may live besides the platform's own
// buckets. Credentials come in once, are sealed with the platform's
// credential key bound to the tenant, connection, provider and version,
// and never come back out: no response, audit event, log line or error
// carries them.
package storageconn

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/config"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/netguard"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/secretbox"
	"github.com/sanjeev0303/object-storage-platform/internal/service/audit"
)

// ErrInvalidRequest: the request is malformed.
var ErrInvalidRequest = errors.New("invalid storage connection request")

// Clients builds provider clients and forgets cached ones: the registry.
type Clients interface {
	// Build builds an uncached client from secrets not stored yet.
	Build(ctx context.Context, c model.StorageConnection, secrets model.StorageSecrets) (interfaces.StorageProvider, func(), error)
	// BuildStored builds an uncached client from the stored credentials.
	BuildStored(ctx context.Context, c model.StorageConnection) (interfaces.StorageProvider, func(), error)
	Invalidate(connectionID string)
}

type Dependencies struct {
	Connections interfaces.StorageConnectionRepository
	// Sealer is nil when BYOS is off: connections can then be listed but
	// not created, tested or rotated.
	Sealer  secretbox.Sealer
	Clients Clients
	Audit   interfaces.AuditRecorder
	Log     *slog.Logger
}

type Service struct {
	deps   Dependencies
	cfg    config.Storage
	policy netguard.Policy
	now    func() time.Time
	newID  func() string
}

func NewService(deps Dependencies, cfg config.Storage, policy netguard.Policy) (*Service, error) {
	if deps.Connections == nil || deps.Clients == nil || deps.Log == nil {
		return nil, errors.New("storage connection dependencies are required")
	}
	return &Service{deps: deps, cfg: cfg, policy: policy, now: func() time.Time { return time.Now().UTC() }, newID: randomID}, nil
}

func randomID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// Enabled tells whether organizations may connect their own storage.
func (s *Service) Enabled() bool { return s.enabled() == nil }

// enabled refuses credential work while BYOS is off.
func (s *Service) enabled() error {
	if !s.cfg.BYOSEnabled || s.deps.Sealer == nil {
		return model.ErrStorageNotConfigured
	}
	return nil
}

// ─── Reads ────────────────────────────────────────────────────────────

func (s *Service) List(ctx context.Context, p model.Principal) ([]model.StorageConnection, string, error) {
	if err := p.Require(model.PermStorageRead); err != nil {
		return nil, "", err
	}
	connections, err := s.deps.Connections.ListConnections(ctx, p.TenantID)
	if err != nil {
		return nil, "", err
	}
	defaultID, err := s.deps.Connections.DefaultConnection(ctx, p.TenantID)
	return connections, defaultID, err
}

func (s *Service) Get(ctx context.Context, p model.Principal, connectionID string) (model.StorageConnection, error) {
	if err := p.Require(model.PermStorageRead); err != nil {
		return model.StorageConnection{}, err
	}
	return s.deps.Connections.GetConnection(ctx, p.TenantID, connectionID)
}

// DefaultConnection is the organization's default connection's ID.
func (s *Service) DefaultConnection(ctx context.Context, p model.Principal) (string, error) {
	return s.deps.Connections.DefaultConnection(ctx, p.TenantID)
}

// ─── Create and change ────────────────────────────────────────────────

// CreateInput is a new connection: its settings and its credentials.
type CreateInput struct {
	Name           string
	Provider       model.StorageProviderType
	AuthMethod     model.StorageAuthMethod
	Bucket         string
	Region         string
	Endpoint       string
	AccountID      string
	ForcePathStyle bool
	RoleARN        string
	StagingPrefix  string
	ReadyPrefix    string
	StagingMode    model.StagingMode
	Secrets        model.StorageSecrets
}

// Create stores a connection as PENDING; a passing test activates it.
// For ASSUME_ROLE the platform generates the external ID the customer's
// role trust policy must require.
func (s *Service) Create(ctx context.Context, p model.Principal, in CreateInput) (model.StorageConnection, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermStorageManage, "storage_connection.create", "storage_connection", ""); err != nil {
		return model.StorageConnection{}, err
	}
	if err := s.enabled(); err != nil {
		return model.StorageConnection{}, err
	}
	now := s.now()
	c := model.StorageConnection{
		ID: "sc_" + s.newID(), TenantID: p.TenantID, Name: strings.TrimSpace(in.Name), Provider: in.Provider,
		AuthMethod: in.AuthMethod, Bucket: in.Bucket, Region: in.Region, Endpoint: strings.TrimRight(in.Endpoint, "/"),
		AccountID: in.AccountID, ForcePathStyle: in.ForcePathStyle, RoleARN: in.RoleARN,
		StagingPrefix: orDefault(in.StagingPrefix, "quarantine"), ReadyPrefix: orDefault(in.ReadyPrefix, "assets"),
		StagingMode: model.StagingMode(orDefault(string(in.StagingMode), string(model.StagingInPlace))),
		Status:      model.ConnectionPending, CreatedBy: p.UserID, UpdatedBy: p.UserID, CreatedAt: now, UpdatedAt: now,
	}
	if c.AuthMethod == model.AuthAssumeRole {
		c.ExternalID = "osp-" + s.newID()
	}
	if err := s.validate(c); err != nil {
		return model.StorageConnection{}, err
	}
	if err := in.Secrets.ValidateFor(c.AuthMethod); err != nil {
		return model.StorageConnection{}, err
	}

	var sealed interfaces.SealedCredential
	if len(c.AuthMethod.SecretFields()) > 0 {
		var err error
		if sealed, err = s.seal(c, 1, in.Secrets); err != nil {
			return model.StorageConnection{}, err
		}
	}
	stored, err := s.deps.Connections.CreateConnection(ctx, c, sealed)
	if err != nil {
		return model.StorageConnection{}, err
	}
	s.record(ctx, p, "storage_connection.create", stored, map[string]any{
		"provider": string(stored.Provider), "auth_method": string(stored.AuthMethod), "staging_mode": string(stored.StagingMode),
	})
	return stored, nil
}

func orDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// validate checks a connection's settings and its endpoint against the
// network policy, as at every later connect.
func (s *Service) validate(c model.StorageConnection) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if _, err := endpointOf(c, s.policy); err != nil {
		return err
	}
	return nil
}

// seal encrypts the secrets for one credential version. The plaintext
// buffer is cleared as soon as it is sealed.
func (s *Service) seal(c model.StorageConnection, version int, secrets model.StorageSecrets) (interfaces.SealedCredential, error) {
	plain, err := json.Marshal(secretFields{
		AccessKeyID: secrets.AccessKeyID, SecretAccessKey: secrets.SecretAccessKey, SessionToken: secrets.SessionToken,
		ServiceAccountJSON: secrets.ServiceAccountJSON, AccountKey: secrets.AccountKey, SASToken: secrets.SASToken,
	})
	if err != nil {
		return interfaces.SealedCredential{}, errors.New("encode credentials")
	}
	sealed, err := s.deps.Sealer.Seal(plain, c.CredentialAAD(version))
	clear(plain)
	if err != nil {
		return interfaces.SealedCredential{}, errors.New("seal credentials")
	}
	return interfaces.SealedCredential{Version: version, KeyVersion: sealed.KeyVersion, Nonce: sealed.Nonce, Ciphertext: sealed.Ciphertext}, nil
}

// secretFields is the sealed form. model.StorageSecrets refuses to
// marshal itself, so it can never leak through a generic encoder; this is
// the one place its fields are written out, and only into the sealer.
type secretFields struct {
	AccessKeyID        string `json:"AccessKeyID,omitempty"`
	SecretAccessKey    string `json:"SecretAccessKey,omitempty"`
	SessionToken       string `json:"SessionToken,omitempty"`
	ServiceAccountJSON string `json:"ServiceAccountJSON,omitempty"`
	AccountKey         string `json:"AccountKey,omitempty"`
	SASToken           string `json:"SASToken,omitempty"`
}

// UpdateInput changes non-secret settings; nil leaves a field alone.
type UpdateInput struct {
	Name           *string
	Region         *string
	Endpoint       *string
	ForcePathStyle *bool
	StagingMode    *model.StagingMode
}

// Update changes a connection's settings at the revision the caller read.
// Uploads and assets stay bound to the connection, not to its settings,
// so they are read through whatever it points at next. On a connection
// that holds work (active or disabled), a change of where it points
// (region, endpoint, path style) is therefore tested first, with the
// stored credentials and against an object it already holds, and refused
// with ErrTestFailed if the test fails; the old settings stay. The status
// never changes here: a pending connection still needs a passing test.
func (s *Service) Update(ctx context.Context, p model.Principal, connectionID string, revision int64, in UpdateInput) (model.StorageConnection, TestResult, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermStorageManage, "storage_connection.update", "storage_connection", connectionID); err != nil {
		return model.StorageConnection{}, TestResult{}, err
	}
	if revision <= 0 {
		return model.StorageConnection{}, TestResult{}, fmt.Errorf("%w: the connection's revision is required (If-Match)", ErrInvalidRequest)
	}
	current, err := s.ownConnection(ctx, p, connectionID)
	if err != nil {
		return model.StorageConnection{}, TestResult{}, err
	}
	changed := current
	if in.Name != nil {
		changed.Name = strings.TrimSpace(*in.Name)
	}
	if in.Region != nil {
		changed.Region = *in.Region
	}
	if in.Endpoint != nil {
		trimmed := strings.TrimRight(*in.Endpoint, "/")
		in.Endpoint = &trimmed
		changed.Endpoint = trimmed
	}
	if in.ForcePathStyle != nil {
		changed.ForcePathStyle = *in.ForcePathStyle
	}
	if in.StagingMode != nil {
		changed.StagingMode = *in.StagingMode
	}
	if err := s.validate(changed); err != nil {
		return model.StorageConnection{}, TestResult{}, err
	}
	var result TestResult
	moved := changed.Region != current.Region || changed.Endpoint != current.Endpoint || changed.ForcePathStyle != current.ForcePathStyle
	if moved && current.HoldsWork() {
		if err := s.enabled(); err != nil {
			return model.StorageConnection{}, TestResult{}, err
		}
		if result, err = s.check(ctx, changed, s.stored(changed), false); err != nil {
			return model.StorageConnection{}, TestResult{}, err
		}
		if !result.OK {
			s.record(ctx, p, "storage_connection.update", current, map[string]any{"outcome": "refused", "category": result.Category})
			return current, result, ErrTestFailed
		}
	}
	// The revision check makes the tested settings the ones stored: a
	// change made meanwhile fails this with a conflict.
	stored, err := s.deps.Connections.UpdateConnection(ctx, p.TenantID, connectionID, revision, interfaces.ConnectionChange{
		Name: in.Name, Region: in.Region, Endpoint: in.Endpoint, ForcePathStyle: in.ForcePathStyle, StagingMode: in.StagingMode,
		Actor: p.UserID, At: s.now(),
	})
	if err != nil {
		return model.StorageConnection{}, TestResult{}, err
	}
	s.deps.Clients.Invalidate(connectionID)
	s.record(ctx, p, "storage_connection.update", stored, map[string]any{"status": string(stored.Status), "tested": result.OK})
	return stored, result, nil
}

// ownConnection reads one of the tenant's own connections; the platform
// connection is not the tenant's to change.
func (s *Service) ownConnection(ctx context.Context, p model.Principal, connectionID string) (model.StorageConnection, error) {
	if model.IsPlatformConnection(connectionID) {
		return model.StorageConnection{}, fmt.Errorf("%w: the platform connection cannot be changed", ErrInvalidRequest)
	}
	return s.deps.Connections.GetConnection(ctx, p.TenantID, connectionID)
}

// ─── Credentials ──────────────────────────────────────────────────────

// RotateCredentials replaces a connection's credentials. The new ones are
// tested first and stored only if they work; the old ones are never
// returned. Clients built from the old ones are dropped here, and other
// processes drop theirs when their connection cache expires.
func (s *Service) RotateCredentials(ctx context.Context, p model.Principal, connectionID string, revision int64, secrets model.StorageSecrets) (model.StorageConnection, TestResult, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermStorageManage, "storage_connection.rotate", "storage_connection", connectionID); err != nil {
		return model.StorageConnection{}, TestResult{}, err
	}
	if err := s.enabled(); err != nil {
		return model.StorageConnection{}, TestResult{}, err
	}
	if revision <= 0 {
		return model.StorageConnection{}, TestResult{}, fmt.Errorf("%w: the connection's revision is required (If-Match)", ErrInvalidRequest)
	}
	c, err := s.ownConnection(ctx, p, connectionID)
	if err != nil {
		return model.StorageConnection{}, TestResult{}, err
	}
	if len(c.AuthMethod.SecretFields()) == 0 {
		return model.StorageConnection{}, TestResult{}, fmt.Errorf("%w: %s connections have no stored credentials", ErrInvalidRequest, c.AuthMethod)
	}
	if err := secrets.ValidateFor(c.AuthMethod); err != nil {
		return model.StorageConnection{}, TestResult{}, err
	}

	result, err := s.runTest(ctx, c, secrets, false)
	if err != nil {
		return model.StorageConnection{}, TestResult{}, err
	}
	if !result.OK {
		s.record(ctx, p, "storage_connection.rotate", c, map[string]any{"outcome": "refused", "category": result.Category})
		return c, result, ErrTestFailed
	}
	sealed, err := s.seal(c, c.CredentialVersion+1, secrets)
	if err != nil {
		return model.StorageConnection{}, TestResult{}, err
	}
	stored, err := s.deps.Connections.RotateCredential(ctx, p.TenantID, connectionID, revision, sealed, p.UserID, s.now())
	if err != nil {
		return model.StorageConnection{}, TestResult{}, err
	}
	s.deps.Clients.Invalidate(connectionID)
	s.record(ctx, p, "storage_connection.rotate", stored, map[string]any{"rotation": stored.CredentialVersion})
	return stored, result, nil
}

// ─── Default, disable, enable, delete ─────────────────────────────────

// SetDefault makes an active connection (or the platform's) the one new
// uploads go to unless they ask for another.
func (s *Service) SetDefault(ctx context.Context, p model.Principal, connectionID string) (model.StorageConnection, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermStorageManage, "storage_connection.set_default", "storage_connection", connectionID); err != nil {
		return model.StorageConnection{}, err
	}
	c, err := s.deps.Connections.GetConnection(ctx, p.TenantID, connectionID)
	if err != nil {
		return model.StorageConnection{}, err
	}
	if !c.AcceptsNewWork() {
		return model.StorageConnection{}, fmt.Errorf("%w: test it before making it the default", model.ErrConnectionNotActive)
	}
	if err := s.deps.Connections.SetDefault(ctx, p.TenantID, c.ID); err != nil {
		return model.StorageConnection{}, err
	}
	s.record(ctx, p, "storage_connection.set_default", c, nil)
	return c, nil
}

// Disable stops new uploads to a connection. What it already holds is
// still read, processed and deleted through it. The default cannot be
// disabled: choose another default first.
func (s *Service) Disable(ctx context.Context, p model.Principal, connectionID string) (model.StorageConnection, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermStorageManage, "storage_connection.disable", "storage_connection", connectionID); err != nil {
		return model.StorageConnection{}, err
	}
	if _, err := s.ownConnection(ctx, p, connectionID); err != nil {
		return model.StorageConnection{}, err
	}
	stored, err := s.deps.Connections.Disable(ctx, p.TenantID, connectionID, p.UserID, s.now())
	if err != nil {
		return model.StorageConnection{}, err
	}
	s.deps.Clients.Invalidate(connectionID)
	s.record(ctx, p, "storage_connection.disable", stored, nil)
	return stored, nil
}

// Enable tests a disabled connection and makes it active again if the
// test passes. It stays disabled while tested and when the test fails, so
// what it holds is still served; a pending connection would serve nothing.
func (s *Service) Enable(ctx context.Context, p model.Principal, connectionID string) (model.StorageConnection, TestResult, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermStorageManage, "storage_connection.enable", "storage_connection", connectionID); err != nil {
		return model.StorageConnection{}, TestResult{}, err
	}
	if err := s.enabled(); err != nil {
		return model.StorageConnection{}, TestResult{}, err
	}
	c, err := s.ownConnection(ctx, p, connectionID)
	if err != nil {
		return model.StorageConnection{}, TestResult{}, err
	}
	if c.Status != model.ConnectionDisabled {
		return model.StorageConnection{}, TestResult{}, fmt.Errorf("%w: it is %s, not disabled", model.ErrConnectionNotActive, c.Status)
	}
	result, err := s.check(ctx, c, s.stored(c), false)
	if err != nil {
		return model.StorageConnection{}, TestResult{}, err
	}
	if result.OK {
		active, err := s.deps.Connections.SetStatus(ctx, p.TenantID, connectionID,
			[]model.ConnectionStatus{model.ConnectionDisabled}, model.ConnectionActive, p.UserID, s.now())
		if err != nil {
			return model.StorageConnection{}, TestResult{}, err
		}
		s.record(ctx, p, "storage_connection.enable", active, nil)
	}
	return s.recordTest(ctx, p, c, result, false)
}

// Delete removes a disabled or never-activated connection that nothing
// depends on, and erases its credentials. Its bucket and objects are the
// customer's and are left alone.
func (s *Service) Delete(ctx context.Context, p model.Principal, connectionID string) (model.StorageConnection, interfaces.ConnectionDependents, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermStorageManage, "storage_connection.delete", "storage_connection", connectionID); err != nil {
		return model.StorageConnection{}, interfaces.ConnectionDependents{}, err
	}
	if _, err := s.ownConnection(ctx, p, connectionID); err != nil {
		return model.StorageConnection{}, interfaces.ConnectionDependents{}, err
	}
	stored, err := s.deps.Connections.Delete(ctx, p.TenantID, connectionID, p.UserID, s.now())
	if errors.Is(err, model.ErrConnectionInUse) {
		dependents, depErr := s.deps.Connections.Dependents(ctx, p.TenantID, connectionID)
		if depErr != nil {
			return model.StorageConnection{}, interfaces.ConnectionDependents{}, err
		}
		return model.StorageConnection{}, dependents, err
	}
	if err != nil {
		return model.StorageConnection{}, interfaces.ConnectionDependents{}, err
	}
	s.deps.Clients.Invalidate(connectionID)
	s.record(ctx, p, "storage_connection.delete", stored, nil)
	return stored, interfaces.ConnectionDependents{}, nil
}

// record audits a change. Details never hold credentials, endpoints'
// secrets or provider messages.
func (s *Service) record(ctx context.Context, p model.Principal, action string, c model.StorageConnection, details map[string]any) {
	if s.deps.Audit == nil {
		return
	}
	e := audit.From(ctx, p, action, "storage_connection", c.ID)
	if details == nil {
		details = map[string]any{}
	}
	details["name"] = c.Name
	details["status"] = string(c.Status)
	e.Details = details
	s.deps.Audit.Record(ctx, e)
}
