// Package evidence produces what auditors, inspectors and regulators ask
// for: audit chain anchors outside the database, audit exports and
// scheduled exception reviews with sign-off, lawful-basis evidence for
// training, chain-of-custody and retention evidence packs, model training
// summaries, and control mappings (docs/evidence.md).
package evidence

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/service/access"
	"github.com/sanjeev0303/object-storage-platform/internal/service/audit"
	"github.com/sanjeev0303/object-storage-platform/internal/service/dataset"
	"github.com/sanjeev0303/object-storage-platform/internal/service/delivery"
	"github.com/sanjeev0303/object-storage-platform/internal/service/streaming"
)

// Store is the evidence repository.
type Store interface {
	Settings(ctx context.Context, tenantID string) (model.EvidenceSettings, error)
	PutSettings(ctx context.Context, s model.EvidenceSettings) error
	Tenants(ctx context.Context) ([]model.EvidenceSettings, error)
	Head(ctx context.Context, chain string) (int64, []byte, error)
	HashAt(ctx context.Context, chain string, seq int64) ([]byte, bool, error)
	Export(ctx context.Context, chain string, afterSeq int64, start, end time.Time, limit int) ([]postgres.ExportedEvent, error)
	PutAnchor(ctx context.Context, a model.AuditAnchor) error
	Anchors(ctx context.Context, chain string, limit int) ([]model.AuditAnchor, error)
	PutReview(ctx context.Context, v model.AuditReview) error
	Reviews(ctx context.Context, tenantID string, limit int) ([]model.AuditReview, error)
	Review(ctx context.Context, tenantID, id string) (model.AuditReview, error)
	LastReviewEnd(ctx context.Context, tenantID string) (*time.Time, error)
	SignReview(ctx context.Context, tenantID, id, by, note string, at time.Time) error
	Actions(ctx context.Context, chain string, actions []string, from, to time.Time, limit int) ([]model.AuditEvent, error)
	FailedChecks(ctx context.Context, tenantID string, from, to time.Time, limit int) ([]model.ReviewFinding, error)
	BulkReads(ctx context.Context, tenantID, tz string, start, end, threshold int, from, to time.Time) ([]model.ReviewFinding, error)
	Provenance(ctx context.Context, tenantID, datasetID string) (model.Provenance, error)
	PutProvenance(ctx context.Context, tenantID, datasetID string, p model.Provenance) error
	VersionOrders(ctx context.Context, tenantID string, versionIDs []string) ([]postgres.VersionOrder, error)
	RunVersions(ctx context.Context, tenantID, runID string) ([]string, time.Time, error)
	AuditSeqOf(ctx context.Context, chain, action, resourceID string) (int64, error)

	Custody(ctx context.Context, tenantID, versionID string) ([]postgres.CustodyRow, error)
	Reads(ctx context.Context, tenantID string, assetIDs []string, limit int) (map[string][]postgres.ReadRow, error)
	Digests(ctx context.Context, assetIDs []string) (map[string]map[string]string, error)
	PutDigests(ctx context.Context, tenantID, assetID string, digests map[string]string, at time.Time) error
	Model(ctx context.Context, tenantID, modelID string) (postgres.ModelRow, error)
	VersionProfile(ctx context.Context, tenantID, versionID string) (postgres.ContentProfile, error)
	PublishSummary(ctx context.Context, tenantID, modelID string, at time.Time) error
	SummariesDue(ctx context.Context, now time.Time) (int, error)
	Members(ctx context.Context, tenantID string) ([]postgres.MemberRow, error)
	Tokens(ctx context.Context, tenantID string, now time.Time) ([]postgres.TokenRow, error)
	ControlFacts(ctx context.Context, tenantID string, now time.Time) (postgres.ControlFacts, error)
	AsOf(ctx context.Context, tenantID, namespace string, at time.Time, limit int) ([]postgres.AsOfRow, error)
}

// Holds places and releases legal holds through the lifecycle, so its
// rules (two-person release) apply.
type Holds interface {
	PlaceHoldFor(ctx context.Context, p model.Principal, hold model.LegalHold) (model.LegalHold, error)
	HoldsFor(ctx context.Context, p model.Principal, includeReleased bool) ([]model.LegalHold, error)
	ReleaseHoldGoverned(ctx context.Context, p model.Principal, holdID, reason string) error
}

type Alerter interface {
	AddAlert(ctx context.Context, a model.SecurityAlert) (bool, error)
}

type Signer struct {
	KeyID string
	Key   ed25519.PrivateKey
}

type Deps struct {
	Store     Store
	Chains    interfaces.AuditRepository
	Audit     interfaces.AuditRecorder
	Router    interfaces.StorageRouter
	Datasets  *dataset.Service
	Delivery  *delivery.Service
	Streaming *streaming.Service
	Access    *access.Service
	Holds     Holds
	Alerts    Alerter
	Signer    Signer
	// TSA, when set, is an RFC 3161 time-stamping service each anchor is
	// stamped by.
	TSA    string
	Client *http.Client
	Log    *slog.Logger
}

type Service struct {
	deps Deps
	now  func() time.Time
}

func New(deps Deps) *Service {
	if deps.Client == nil {
		deps.Client = &http.Client{Timeout: 30 * time.Second}
	}
	return &Service{deps: deps, now: func() time.Time { return time.Now().UTC() }}
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", model.ErrInvalidEvidence, fmt.Sprintf(format, args...))
}

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func (s *Service) record(ctx context.Context, p model.Principal, action, kind, id string, details map[string]any) {
	if s.deps.Audit == nil {
		return
	}
	e := audit.From(ctx, p, action, kind, id)
	e.Details = details
	s.deps.Audit.Record(ctx, e)
}

// ─── Settings ─────────────────────────────────────────────────────────

func (s *Service) Settings(ctx context.Context, p model.Principal) (model.EvidenceSettings, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermAuditRead, "evidence.settings_read", "organization", p.TenantID); err != nil {
		return model.EvidenceSettings{}, err
	}
	return s.deps.Store.Settings(ctx, p.TenantID)
}

func (s *Service) PutSettings(ctx context.Context, p model.Principal, in model.EvidenceSettings) (model.EvidenceSettings, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermOrgManage, "evidence.settings", "organization", p.TenantID); err != nil {
		return model.EvidenceSettings{}, err
	}
	if _, err := time.LoadLocation(in.Timezone); err != nil || strings.TrimSpace(in.Timezone) == "" {
		return model.EvidenceSettings{}, invalid("timezone is an IANA name such as Asia/Kolkata")
	}
	switch {
	case in.AnchorEveryMinutes < 5 || in.AnchorEveryMinutes > 1440:
		return model.EvidenceSettings{}, invalid("anchor_every_minutes is 5 to 1440")
	case in.AnchorLockDays < 0 || in.AnchorLockDays > 36500:
		return model.EvidenceSettings{}, invalid("anchor_lock_days is 0 to 36500")
	case in.ReviewEveryDays < 0 || in.ReviewEveryDays > 365:
		return model.EvidenceSettings{}, invalid("review_every_days is 0 (none) to 365")
	case in.WorkStartHour < 0 || in.WorkStartHour > 23 || in.WorkEndHour < 1 || in.WorkEndHour > 24 || in.WorkEndHour <= in.WorkStartHour:
		return model.EvidenceSettings{}, invalid("working hours run from work_start_hour to a later work_end_hour, 0 to 24")
	case in.BulkReadThreshold < 1:
		return model.EvidenceSettings{}, invalid("bulk_read_threshold is at least 1")
	}
	if in.AnchorConnectionID != "" {
		if _, err := s.deps.Router.Locate(ctx, p.TenantID, in.AnchorConnectionID); err != nil {
			return model.EvidenceSettings{}, model.ErrConnectionNotFound
		}
	}
	in.TenantID, in.UpdatedBy, in.UpdatedAt = p.TenantID, p.UserID, s.now()
	if err := s.deps.Store.PutSettings(ctx, in); err != nil {
		return model.EvidenceSettings{}, err
	}
	s.record(ctx, p, "evidence.settings", "organization", p.TenantID, map[string]any{
		"anchor_connection_id": in.AnchorConnectionID, "review_every_days": in.ReviewEveryDays, "require_lawful_basis": in.RequireLawfulBasis})
	return in, nil
}

var _ Store = (*postgres.EvidenceRepository)(nil)
