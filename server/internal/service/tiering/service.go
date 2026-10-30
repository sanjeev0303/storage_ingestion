// Package tiering moves objects between storage tiers by rule, restores
// archived ones on request, estimates costs and keeps budgets, and expires
// old snapshots (docs/tiering.md).
package tiering

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/retry"
	"github.com/sanjeev0303/object-storage-platform/internal/service/audit"
	"github.com/sanjeev0303/object-storage-platform/internal/service/dataset"
)

// Store is what the service keeps and reads.
type Store interface {
	PutRule(ctx context.Context, r model.TierRule) error
	Rule(ctx context.Context, tenantID, id string) (model.TierRule, error)
	Rules(ctx context.Context, tenantID string) ([]model.TierRule, error)
	DeleteRule(ctx context.Context, tenantID, id string) error
	EnabledRules(ctx context.Context) ([]model.TierRule, error)
	Candidates(ctx context.Context, rule model.TierRule, warmer []string, publishedBefore, unreadBefore, now time.Time, limit int) ([]model.TierCandidate, error)
	PackCandidates(ctx context.Context, rule model.TierRule, warmer []string, publishedBefore, unreadBefore, now time.Time, limit int) ([]model.TierPack, error)
	SetPackTier(ctx context.Context, tenantID, packID, fromETag string, tier model.StorageTier, etag, versionID string, now time.Time) (bool, error)
	PackOf(ctx context.Context, tenantID, assetID string) (model.TierPack, bool, error)
	SetPackReadable(ctx context.Context, tenantID, packID string, tier model.StorageTier, until *time.Time, now time.Time) error
	RecordMove(ctx context.Context, m model.TierMove) error
	Moves(ctx context.Context, tenantID string, limit int) ([]model.TierMove, error)
	SetTier(ctx context.Context, tenantID, assetID, fromETag string, tier model.StorageTier, etag, versionID string, now time.Time) (bool, error)
	SetReadable(ctx context.Context, tenantID, assetID string, tier model.StorageTier, until *time.Time, now time.Time) error

	CreateRestore(ctx context.Context, x model.ArchiveRestore) (model.ArchiveRestore, bool, error)
	Restores(ctx context.Context, tenantID, assetID, versionID string, limit int) ([]model.ArchiveRestore, error)
	OpenRestores(ctx context.Context, limit int) ([]model.ArchiveRestore, error)
	UpdateRestore(ctx context.Context, x model.ArchiveRestore) error
	ExpireRestores(ctx context.Context, now time.Time) (int, error)
	ArchivedMembers(ctx context.Context, tenantID, versionID string) ([]model.TierCandidate, error)
	ProviderOf(ctx context.Context, tenantID, connectionID string) (model.StorageProviderType, error)

	Budget(ctx context.Context, tenantID string) (model.CostBudget, bool, error)
	SetBudget(ctx context.Context, b model.CostBudget) error
	Stored(ctx context.Context, tenantID, datasetID string) ([]postgres.StoredBytes, error)
	Usage(ctx context.Context, tenantID string, from time.Time) (postgres.MonthUsage, error)
	Savings(ctx context.Context, tenantID string) (postgres.Savings, error)

	VersionUses(ctx context.Context, tenantID, datasetID string) ([]postgres.VersionUse, error)
	Freed(ctx context.Context, tenantID string, versionIDs []string) ([]model.Asset, error)
	Expire(ctx context.Context, tenantID string, versionIDs []string, by string, now time.Time) error
	VersionProfile(ctx context.Context, tenantID, versionID string, now time.Time) ([]postgres.VersionProfile, error)
	VersionObjects(ctx context.Context, tenantID, versionID, afterPath string, limit int) ([]postgres.VersionObject, error)
}

// Assets reads an asset and its connection's provider.
type Assets interface {
	GetByID(ctx context.Context, tenantID, assetID string) (*model.Asset, error)
}

type Alerter interface {
	AddAlert(ctx context.Context, a model.SecurityAlert) (bool, error)
}

type Deps struct {
	Store    Store
	Assets   Assets
	Router   interfaces.StorageRouter
	Datasets *dataset.Service
	Audit    interfaces.AuditRecorder
	Alerts   Alerter
	// Delete asks for an asset's deletion as the caller, through the
	// lifecycle (holds, retention, escrow and its grace period apply).
	Delete func(ctx context.Context, p model.Principal, assetID string) error
	Log    *slog.Logger
}

type Config struct {
	// PlatformTiering lets rules move objects on the platform's own
	// storage, when that storage has storage classes (Amazon S3).
	PlatformTiering bool
	// PerRule bounds the assets one rule moves per pass.
	PerRule int
}

type Service struct {
	deps Deps
	cfg  Config
	now  func() time.Time

	mu    sync.Mutex
	spend map[string]spendCache
}

func New(deps Deps, cfg Config) *Service {
	if cfg.PerRule <= 0 {
		cfg.PerRule = 200
	}
	return &Service{deps: deps, cfg: cfg, now: func() time.Time { return time.Now().UTC() }, spend: map[string]spendCache{}}
}

var ErrInvalid = model.ErrInvalidTiering

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
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

// canTier reports whether objects on provider can be moved to tier.
func (s *Service) canTier(provider model.StorageProviderType, tier model.StorageTier) bool {
	switch provider {
	case model.ProviderAWSS3, model.ProviderGCS, model.ProviderAzureBlob:
		return true
	case model.ProviderR2:
		return tier == model.TierInfrequent || tier == model.TierStandard
	case model.ProviderPlatform:
		return s.cfg.PlatformTiering
	}
	return false
}

// ─── Rules ────────────────────────────────────────────────────────────

type RuleInput struct {
	DatasetID  string            `json:"dataset_id"`
	Name       string            `json:"name"`
	Tier       model.StorageTier `json:"tier"`
	MinAgeDays int               `json:"min_age_days"`
	UnreadDays int               `json:"unread_days"`
	Enabled    *bool             `json:"enabled"`
}

func (s *Service) Rules(ctx context.Context, p model.Principal) ([]model.TierRule, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermStorageRead, "tiering.read", "organization", p.TenantID); err != nil {
		return nil, err
	}
	return s.deps.Store.Rules(ctx, p.TenantID)
}

// PutRule creates a rule, or changes one when id is set.
func (s *Service) PutRule(ctx context.Context, p model.Principal, id string, in RuleInput) (model.TierRule, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermStorageManage, "tiering.rule", "organization", p.TenantID); err != nil {
		return model.TierRule{}, err
	}
	in.Name = strings.TrimSpace(in.Name)
	switch {
	case in.Name == "" || len(in.Name) > 100:
		return model.TierRule{}, invalid("a rule has a name of 1 to 100 characters")
	case in.Tier != model.TierInfrequent && in.Tier != model.TierArchive && in.Tier != model.TierDeepArchive:
		return model.TierRule{}, invalid("tier is INFREQUENT, ARCHIVE or DEEP_ARCHIVE")
	case in.MinAgeDays < 0 || in.MinAgeDays > 36500 || in.UnreadDays < 0 || in.UnreadDays > 36500:
		return model.TierRule{}, invalid("min_age_days and unread_days are 0 to 36500")
	case in.MinAgeDays == 0 && in.UnreadDays == 0:
		return model.TierRule{}, invalid("a rule waits for an age or an unread time, or it would move new uploads at once")
	}
	if in.DatasetID != "" {
		if _, err := s.deps.Datasets.Get(ctx, p, in.DatasetID); err != nil {
			return model.TierRule{}, err
		}
	}
	now := s.now()
	rule := model.TierRule{ID: newID(), TenantID: p.TenantID, DatasetID: in.DatasetID, Name: in.Name, Tier: in.Tier,
		MinAgeDays: in.MinAgeDays, UnreadDays: in.UnreadDays, Enabled: true, CreatedBy: p.UserID, CreatedAt: now, UpdatedAt: now}
	if id != "" {
		old, err := s.deps.Store.Rule(ctx, p.TenantID, id)
		if err != nil {
			return model.TierRule{}, err
		}
		rule.ID, rule.DatasetID, rule.CreatedBy, rule.CreatedAt, rule.Enabled = old.ID, old.DatasetID, old.CreatedBy, old.CreatedAt, old.Enabled
	}
	if in.Enabled != nil {
		rule.Enabled = *in.Enabled
	}
	if err := s.deps.Store.PutRule(ctx, rule); err != nil {
		return model.TierRule{}, err
	}
	s.record(ctx, p, "tiering.rule", "tier_rule", rule.ID, map[string]any{"tier": rule.Tier, "min_age_days": rule.MinAgeDays,
		"unread_days": rule.UnreadDays, "dataset_id": rule.DatasetID, "enabled": rule.Enabled})
	return rule, nil
}

func (s *Service) DeleteRule(ctx context.Context, p model.Principal, id string) error {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermStorageManage, "tiering.rule_delete", "tier_rule", id); err != nil {
		return err
	}
	if err := s.deps.Store.DeleteRule(ctx, p.TenantID, id); err != nil {
		return err
	}
	s.record(ctx, p, "tiering.rule_delete", "tier_rule", id, nil)
	return nil
}

func (s *Service) Moves(ctx context.Context, p model.Principal) ([]model.TierMove, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermStorageRead, "tiering.read", "organization", p.TenantID); err != nil {
		return nil, err
	}
	return s.deps.Store.Moves(ctx, p.TenantID, 100)
}

// ─── The worker's pass ────────────────────────────────────────────────

// Stats is what one pass did.
type Stats struct {
	Moved, Failed, RestoresStarted, RestoresReady, RestoresFailed, RestoresExpired int
}

// RunOnce applies every enabled rule, then moves restores along.
func (s *Service) RunOnce(ctx context.Context) (Stats, error) {
	var st Stats
	var errs []error
	rules, err := s.deps.Store.EnabledRules(ctx)
	if err != nil {
		return st, err
	}
	for _, rule := range rules {
		if ctx.Err() != nil {
			break
		}
		errs = append(errs, s.apply(ctx, rule, &st))
	}
	errs = append(errs, s.restores(ctx, &st))
	return st, errors.Join(errs...)
}

func warmerThan(tier model.StorageTier) []string {
	var out []string
	for _, t := range []model.StorageTier{model.TierStandard, model.TierInfrequent, model.TierArchive, model.TierDeepArchive} {
		if tier.Colder(t) {
			out = append(out, string(t))
		}
	}
	return out
}

func (s *Service) apply(ctx context.Context, rule model.TierRule, st *Stats) error {
	now := s.now()
	candidates, err := s.deps.Store.Candidates(ctx, rule, warmerThan(rule.Tier), now.AddDate(0, 0, -rule.MinAgeDays),
		now.AddDate(0, 0, -rule.UnreadDays), now, s.cfg.PerRule)
	if err != nil {
		return err
	}
	for _, c := range candidates {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		reason := s.move(ctx, rule, c)
		state := "MOVED"
		if reason != "" {
			state = "FAILED"
			st.Failed++
		} else {
			st.Moved++
		}
		_ = s.deps.Store.RecordMove(ctx, model.TierMove{ID: newID(), TenantID: rule.TenantID, AssetID: c.Asset.ID, RuleID: rule.ID,
			From: c.Asset.StorageTier, To: rule.Tier, SizeBytes: c.Asset.SizeBytes, State: state, Reason: reason, CreatedAt: s.now()})
	}

	// Packs move whole, once every file in them may.
	packs, err := s.deps.Store.PackCandidates(ctx, rule, warmerThan(rule.Tier), now.AddDate(0, 0, -rule.MinAgeDays),
		now.AddDate(0, 0, -rule.UnreadDays), now, s.cfg.PerRule)
	if err != nil {
		return err
	}
	for _, p := range packs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		reason := s.movePack(ctx, rule, p)
		state := "MOVED"
		if reason != "" {
			state = "FAILED"
			st.Failed++
		} else {
			st.Moved += int(p.Files)
			reason = fmt.Sprintf("a pack of %d files", p.Files)
		}
		_ = s.deps.Store.RecordMove(ctx, model.TierMove{ID: newID(), TenantID: rule.TenantID, AssetID: p.ID, RuleID: rule.ID,
			From: p.Tier, To: rule.Tier, SizeBytes: p.Bytes, State: state, Reason: reason, CreatedAt: s.now()})
	}
	return nil
}

// movePack puts a whole pack, and so every file in it, in the rule's tier.
func (s *Service) movePack(ctx context.Context, rule model.TierRule, p model.TierPack) string {
	if !s.canTier(p.Provider, rule.Tier) {
		return "the connection cannot move objects to " + string(rule.Tier)
	}
	loc, err := s.deps.Router.Locate(ctx, p.TenantID, p.ConnectionID)
	if err != nil {
		return "the connection is not available: " + retry.Sanitize(err)
	}
	tierer, ok := loc.Storage.(interfaces.TierStorage)
	if !ok {
		return "the connection cannot move objects between tiers"
	}
	meta, err := tierer.SetTier(ctx, p.Bucket, p.ObjectKey, p.ETag, p.Bytes, rule.Tier)
	if err != nil {
		return "the provider refused: " + retry.Sanitize(err)
	}
	recorded, err := s.deps.Store.SetPackTier(ctx, p.TenantID, p.ID, p.ETag, rule.Tier, meta.ETag, meta.VersionID, s.now())
	if err != nil || !recorded {
		return "the pack changed while it moved; it is checked again next pass"
	}
	if p.VersionID != "" && meta.VersionID != "" && meta.VersionID != p.VersionID {
		if err := loc.Storage.DeleteObjectVersion(ctx, p.Bucket, p.ObjectKey, p.VersionID); err != nil {
			s.deps.Log.Warn("remove the pack version a tier move replaced", "pack_id", p.ID, "error", retry.Sanitize(err))
		}
	}
	s.deps.Log.Info("pack moved", "tenant_id", p.TenantID, "pack_id", p.ID, "files", p.Files, "to", rule.Tier, "rule_id", rule.ID)
	return ""
}

// move puts one asset in the rule's tier and records it. It returns why
// it could not, or "".
func (s *Service) move(ctx context.Context, rule model.TierRule, c model.TierCandidate) string {
	a := c.Asset
	if !s.canTier(c.Provider, rule.Tier) {
		return "the connection cannot move objects to " + string(rule.Tier)
	}
	loc, err := s.deps.Router.Locate(ctx, a.TenantID, connectionOf(a))
	if err != nil {
		return "the connection is not available: " + retry.Sanitize(err)
	}
	tierer, ok := loc.Storage.(interfaces.TierStorage)
	if !ok {
		return "the connection cannot move objects between tiers"
	}
	meta, err := tierer.SetTier(ctx, loc.ReadyBucket, a.ObjectKey, a.StorageETag, a.SizeBytes, rule.Tier)
	if err != nil {
		return "the provider refused: " + retry.Sanitize(err)
	}
	if meta.SizeBytes != 0 && meta.SizeBytes != a.SizeBytes {
		return fmt.Sprintf("the moved object is %d bytes, not %d", meta.SizeBytes, a.SizeBytes)
	}
	recorded, err := s.deps.Store.SetTier(ctx, a.TenantID, a.ID, a.StorageETag, rule.Tier, meta.ETag, meta.VersionID, s.now())
	if err != nil || !recorded {
		return "the asset changed while it moved; it is checked again next pass"
	}
	// A provider that copies to change class leaves the old version
	// behind on a versioned bucket, still billed at the old tier.
	if a.StorageVersionID != "" && meta.VersionID != "" && meta.VersionID != a.StorageVersionID {
		if err := loc.Storage.DeleteObjectVersion(ctx, loc.ReadyBucket, a.ObjectKey, a.StorageVersionID); err != nil {
			s.deps.Log.Warn("remove the version a tier move replaced", "asset_id", a.ID, "error", retry.Sanitize(err))
		}
	}
	s.deps.Log.Info("asset moved", "tenant_id", a.TenantID, "asset_id", a.ID, "from", a.StorageTier, "to", rule.Tier, "rule_id", rule.ID)
	return ""
}

func connectionOf(a model.Asset) string {
	if a.StorageConnectionID == "" {
		return model.PlatformConnectionID
	}
	return a.StorageConnectionID
}
