// Package asset manages the asset lifecycle after publication: retention
// policies, legal holds, and the recoverable deletion workflow.
package asset

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	events "github.com/sanjeev0303/object-storage-platform/internal/dto/event"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/correlation"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/metrics"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/retry"
	"github.com/sanjeev0303/object-storage-platform/internal/service/audit"
)

// ErrNotFound covers missing assets and assets the caller may not see.
var ErrNotFound = errors.New("asset not found")

type Dependencies struct {
	Assets     interfaces.AssetRepository
	Uploads    interfaces.UploadRepository
	Jobs       interfaces.ProcessingJobRepository
	Quarantine interfaces.QuarantineRepository
	Policies   interfaces.RetentionPolicyRepository
	Holds      interfaces.LegalHoldRepository
	Deletions  interfaces.DeletionRepository
	Events     interfaces.LifecycleEventRepository
	Cursors    interfaces.CursorRepository
	Storage    interfaces.LifecycleStorage
	// Metrics is optional.
	Metrics *metrics.Registry
	// Router reaches assets on tenants' storage connections; without it
	// only the platform's buckets are reachable.
	Router interfaces.StorageRouter
	// Packs takes deleted assets out of their packs; nil when nothing is
	// packed.
	Packs interfaces.PackReleaser
	// Locks records Object Lock settings applied on buckets that have it;
	// nil leaves Object Lock alone.
	Locks interfaces.ObjectLockRepository
	// Shared says which other asset still points at a stored object
	// (deduplicated content), whose bytes must then stay; nil when no
	// content is shared.
	Shared KeyUse
	// LockSync queues the scope of each policy or hold change, so its
	// assets are locked within a cycle instead of on the sweep's next
	// pass; nil leaves that to the sweep.
	LockSync LockSync
}

// LockSync is the queue of scopes to lock ahead of the sweep
// (postgres.LockSyncRepository).
type LockSync interface {
	Request(ctx context.Context, r model.LockSyncRequest) error
	Next(ctx context.Context, tenantID string) (model.LockSyncRequest, bool, error)
	Assets(ctx context.Context, r model.LockSyncRequest, limit int) ([]model.Asset, error)
	Advance(ctx context.Context, id, position string, done bool, at time.Time) error
}

// KeyUse names another asset, not permanently deleted, that points at
// objectKey (postgres.AssetRepository).
type KeyUse interface {
	KeyInUse(ctx context.Context, tenantID, objectKey, exceptID string) (string, error)
}

type Config struct {
	// Owner identifies this worker on the leases it takes.
	Owner string
	// Where an asset's objects live: its quarantine source, and its
	// delivery copy at DeliveryPrefix/tenant/asset.
	QuarantineBucket string
	DeliveryBucket   string
	DeliveryPrefix   string

	Platform PlatformRetention

	BatchSize   int
	Concurrency int
	// Lease bounds each deletion step as well as its claim.
	Lease time.Duration
	Retry retry.Policy
	// RecheckInterval is how often a PROTECTED deletion is checked again.
	RecheckInterval time.Duration
	// ExpireAssets lets retention evaluation schedule expired assets for
	// deletion. Off, expirations are only reported.
	ExpireAssets bool
	// MaxDeletionsPerRun bounds the deletions one cycle works on.
	MaxDeletionsPerRun int
	// TenantID limits the worker cycle to one tenant.
	TenantID string
	// DryRun reports what a cycle would do and writes nothing.
	DryRun bool
}

// Service runs the asset lifecycle. Several instances may run at once:
// deletions are claimed with leases and every change checks the lease.
type Service struct {
	deps  Dependencies
	cfg   Config
	log   *slog.Logger
	now   func() time.Time
	newID func() string
	// security, when set, records principal-facing changes in the security
	// audit log, next to the lifecycle's own event trail.
	security interfaces.AuditRecorder
	// locks caches which buckets have Object Lock on.
	locks lockBuckets
}

// Guard records principal-facing deletions, restores and retention
// changes in the security audit log.
// Route lets lifecycle work reach assets on tenants' storage connections.
func (s *Service) Route(router interfaces.StorageRouter) *Service {
	s.deps.Router = router
	return s
}

func (s *Service) Guard(security interfaces.AuditRecorder) *Service {
	s.security = security
	return s
}

func (s *Service) audit(ctx context.Context, p model.Principal, action, resourceType, resourceID string, details map[string]any) {
	if s.security == nil {
		return
	}
	e := audit.From(ctx, p, action, resourceType, resourceID)
	e.Details = details
	s.security.Record(ctx, e)
}

func NewService(deps Dependencies, cfg Config, log *slog.Logger) (*Service, error) {
	if deps.Assets == nil || deps.Uploads == nil || deps.Jobs == nil || deps.Quarantine == nil || deps.Policies == nil ||
		deps.Holds == nil || deps.Deletions == nil || deps.Events == nil || deps.Cursors == nil || deps.Storage == nil || log == nil {
		return nil, errors.New("lifecycle dependencies are required")
	}
	cfg.DeliveryPrefix = strings.Trim(cfg.DeliveryPrefix, "/")
	if cfg.Owner == "" || cfg.QuarantineBucket == "" || cfg.DeliveryBucket == "" || cfg.DeliveryPrefix == "" || cfg.DeliveryPrefix == "quarantine" {
		return nil, errors.New("lifecycle owner, buckets and a non-quarantine delivery prefix are required")
	}
	if cfg.BatchSize <= 0 || cfg.Concurrency <= 0 || cfg.Lease <= 0 || cfg.RecheckInterval <= 0 || cfg.MaxDeletionsPerRun <= 0 {
		return nil, errors.New("lifecycle batch size, concurrency, lease, recheck interval and deletion limit must be positive")
	}
	if cfg.Retry.MaxAttempts <= 0 || cfg.Retry.BaseDelay <= 0 || cfg.Retry.MaxDelay < cfg.Retry.BaseDelay {
		return nil, errors.New("lifecycle retry policy is invalid")
	}
	p := cfg.Platform
	if p.DefaultMinRetention < p.MinRetentionFloor || p.DefaultDeletionGrace < p.DeletionGraceFloor ||
		p.DefaultQuarantineRetention < p.QuarantineRetentionFloor || p.DefaultQuarantineRetention <= 0 || p.DefaultMaxAge < 0 {
		return nil, errors.New("platform retention defaults must not be below their floors")
	}

	return &Service{
		deps: deps, cfg: cfg, log: log,
		now:   func() time.Time { return time.Now().UTC() },
		newID: newID,
	}, nil
}

// DeletionResult is the deletion an asset now has. Created is false when
// an existing deletion was returned.
type DeletionResult struct {
	Operation model.DeletionOperation
	Created   bool
}

// RequestDeletion soft-deletes an asset of the caller's organization: it
// is no longer delivered from this moment and stays restorable during its
// grace period.
func (s *Service) RequestDeletion(ctx context.Context, principal model.Principal, assetID string) (DeletionResult, error) {
	asset, err := s.owned(ctx, principal, assetID, model.PermAssetsDelete)
	if err != nil {
		return DeletionResult{}, err
	}
	result, err := s.requestDeletion(ctx, asset, model.DeletionReasonUser, "user:"+principal.UserID, "")
	if err == nil && result.Created {
		s.audit(ctx, principal, "asset.delete", "asset", asset.ID, map[string]any{"operation_id": result.Operation.ID})
	}
	return result, err
}

// OperatorDelete soft-deletes any asset of the tenant, for an operator.
func (s *Service) OperatorDelete(ctx context.Context, tenantID, assetID, actor, note string) (DeletionResult, error) {
	asset, err := s.tenantAsset(ctx, tenantID, assetID)
	if err != nil {
		return DeletionResult{}, err
	}
	return s.requestDeletion(ctx, asset, model.DeletionReasonOperator, "operator:"+actor, note)
}

// Restore undoes a deletion while nothing has been removed yet.
func (s *Service) Restore(ctx context.Context, principal model.Principal, assetID string) (model.DeletionOperation, error) {
	asset, err := s.owned(ctx, principal, assetID, model.PermAssetsDelete)
	if err != nil {
		return model.DeletionOperation{}, err
	}
	op, err := s.restore(ctx, asset, "user:"+principal.UserID)
	if err == nil {
		s.audit(ctx, principal, "asset.restore", "asset", asset.ID, map[string]any{"operation_id": op.ID})
	}
	return op, err
}

// SetPolicyFor sets a retention policy of the caller's organization.
func (s *Service) SetPolicyFor(ctx context.Context, principal model.Principal, policy model.RetentionPolicy) (model.RetentionPolicy, error) {
	if err := audit.Require(ctx, s.security, principal, model.PermRetentionManage, "retention.policy_set", "retention_policy", policy.ScopeKey); err != nil {
		return model.RetentionPolicy{}, err
	}
	policy.TenantID, policy.CreatedBy = principal.TenantID, "user:"+principal.UserID
	stored, err := s.SetPolicy(ctx, policy)
	if err == nil {
		s.audit(ctx, principal, "retention.policy_set", "retention_policy", stored.ID,
			map[string]any{"scope": stored.Scope, "scope_key": stored.ScopeKey, "version": stored.Version})
	}
	return stored, err
}

// PlaceHoldFor places a legal hold in the caller's organization.
func (s *Service) PlaceHoldFor(ctx context.Context, principal model.Principal, hold model.LegalHold) (model.LegalHold, error) {
	if err := audit.Require(ctx, s.security, principal, model.PermRetentionManage, "retention.hold_place", "legal_hold", ""); err != nil {
		return model.LegalHold{}, err
	}
	hold.TenantID, hold.PlacedBy = principal.TenantID, "user:"+principal.UserID
	stored, err := s.PlaceHold(ctx, hold)
	if err == nil {
		s.audit(ctx, principal, "retention.hold_place", "legal_hold", stored.ID,
			map[string]any{"scope": stored.Scope, "scope_key": stored.ScopeKey})
	}
	return stored, err
}

// PoliciesFor returns the newest version of each retention policy of the
// caller's organization. Reading them needs the same permission as
// setting them.
func (s *Service) PoliciesFor(ctx context.Context, principal model.Principal) ([]model.RetentionPolicy, error) {
	if err := audit.Require(ctx, s.security, principal, model.PermRetentionManage, "retention.policy_read", "retention_policy", ""); err != nil {
		return nil, err
	}
	return s.deps.Policies.Latest(ctx, principal.TenantID)
}

// PolicyHistoryFor returns every version of one scope's retention policy,
// newest first.
func (s *Service) PolicyHistoryFor(ctx context.Context, principal model.Principal, scope, scopeKey string) ([]model.RetentionPolicy, error) {
	if err := audit.Require(ctx, s.security, principal, model.PermRetentionManage, "retention.policy_read", "retention_policy", scopeKey); err != nil {
		return nil, err
	}
	return s.deps.Policies.History(ctx, principal.TenantID, scope, scopeKey)
}

// HoldsFor lists the legal holds of the caller's organization, active ones
// only unless includeReleased. Hold reasons can be confidential, so this
// needs the permission that places and releases them.
func (s *Service) HoldsFor(ctx context.Context, principal model.Principal, includeReleased bool) ([]model.LegalHold, error) {
	if err := audit.Require(ctx, s.security, principal, model.PermRetentionManage, "retention.hold_read", "legal_hold", ""); err != nil {
		return nil, err
	}
	return s.deps.Holds.List(ctx, principal.TenantID, includeReleased)
}

// ReleaseHoldFor releases a legal hold of the caller's organization.
func (s *Service) ReleaseHoldFor(ctx context.Context, principal model.Principal, holdID string) error {
	if err := audit.Require(ctx, s.security, principal, model.PermRetentionManage, "retention.hold_release", "legal_hold", holdID); err != nil {
		return err
	}
	err := s.ReleaseHold(ctx, principal.TenantID, holdID, "user:"+principal.UserID)
	if err == nil {
		s.audit(ctx, principal, "retention.hold_release", "legal_hold", holdID, nil)
	}
	return err
}

// OperatorRestore undoes a deletion for an operator.
func (s *Service) OperatorRestore(ctx context.Context, tenantID, assetID, actor string) (model.DeletionOperation, error) {
	asset, err := s.tenantAsset(ctx, tenantID, assetID)
	if err != nil {
		return model.DeletionOperation{}, err
	}
	return s.restore(ctx, asset, "operator:"+actor)
}

func (s *Service) requestDeletion(ctx context.Context, asset model.Asset, reason, actor, note string) (DeletionResult, error) {
	switch asset.Status {
	case model.AssetStatusDeleted:
		return DeletionResult{}, ErrNotFound
	case model.AssetStatusSoftDeleted:
		return s.existingDeletion(ctx, asset)
	case model.AssetStatusQuarantined, model.AssetStatusProcessing:
		// An active processing job still uses the asset's objects. A job
		// that ended (exhausted or failed) no longer does.
		job, err := s.deps.Jobs.GetByAsset(ctx, asset.TenantID, asset.ID)
		if err != nil || job.State == model.ProcessingPending {
			return DeletionResult{}, model.ErrDeletionBlocked
		}
	}

	now := s.now()
	policy, err := s.policyFor(ctx, asset, now)
	if err != nil {
		return DeletionResult{}, err
	}
	purgeAfter := now.Add(policy.DeletionGrace)
	detail := note
	if reason == model.DeletionReasonExpired {
		detail = fmt.Sprintf("max age %s (%s)", policy.MaxAge, policy.MaxAgeFrom)
	}

	op := model.DeletionOperation{
		ID: s.newID(), TenantID: asset.TenantID, AssetID: asset.ID, State: model.DeletionScheduled,
		Reason: reason, RequestedBy: actor, PreviousStatus: asset.Status, PolicyDetail: detail,
		PurgeAfter: purgeAfter, MaxAttempts: s.cfg.Retry.MaxAttempts, NextAttemptAt: purgeAfter,
		CreatedAt: now, UpdatedAt: now,
	}
	event := s.newEvent(asset.TenantID, asset.ID, op.ID, "deletion_requested", "SCHEDULED",
		fmt.Sprintf("%s; restorable until %s", strings.ToLower(reason), purgeAfter.Format(time.RFC3339)), actor)
	scheduled := events.NewDeletionScheduled(asset.TenantID, asset.ID, op.ID, now, correlation.From(ctx))

	stored, created, err := s.deps.Deletions.SoftDelete(ctx, op, []model.AssetStatus{asset.Status}, event, scheduled)
	if err != nil {
		return DeletionResult{}, err
	}
	if created {
		s.deps.Metrics.Add("deletions_requested_"+strings.ToLower(reason), 1)
		s.log.Info("asset soft-deleted", "asset_id", asset.ID, "tenant_id", asset.TenantID, "reason", reason,
			"operation_id", stored.ID, "purge_after", stored.PurgeAfter)
	}
	return DeletionResult{Operation: stored, Created: created}, nil
}

// existingDeletion answers a repeated request: the live deletion, or the
// latest one when it failed and waits for an operator.
func (s *Service) existingDeletion(ctx context.Context, asset model.Asset) (DeletionResult, error) {
	op, err := s.deps.Deletions.GetLive(ctx, asset.TenantID, asset.ID)
	if err == nil {
		return DeletionResult{Operation: op}, nil
	}
	if !errors.Is(err, model.ErrDeletionNotFound) {
		return DeletionResult{}, err
	}
	ops, err := s.deps.Deletions.ListForAsset(ctx, asset.TenantID, asset.ID)
	if err != nil {
		return DeletionResult{}, err
	}
	if len(ops) == 0 {
		return DeletionResult{}, model.ErrDeletionNotFound
	}
	return DeletionResult{Operation: ops[0]}, nil
}

func (s *Service) restore(ctx context.Context, asset model.Asset, actor string) (model.DeletionOperation, error) {
	if asset.Status != model.AssetStatusSoftDeleted {
		return model.DeletionOperation{}, model.ErrNotRestorable
	}
	event := s.newEvent(asset.TenantID, asset.ID, "", "restored", "CANCELLED", "deletion cancelled before purge", actor)
	op, err := s.deps.Deletions.Restore(ctx, asset.TenantID, asset.ID, s.now(), event)
	if err != nil {
		return model.DeletionOperation{}, err
	}
	s.deps.Metrics.Add("deletions_restored", 1)
	s.log.Info("asset restored", "asset_id", asset.ID, "tenant_id", asset.TenantID, "operation_id", op.ID,
		"status", string(op.PreviousStatus))
	return op, nil
}

// SetPolicy stores a new version of a tenant, dataset or asset policy.
// Settings below a mandatory platform floor are refused.
func (s *Service) SetPolicy(ctx context.Context, policy model.RetentionPolicy) (model.RetentionPolicy, error) {
	now := s.now()
	if policy.EffectiveFrom.IsZero() {
		policy.EffectiveFrom = now
	}
	policy.EffectiveFrom = policy.EffectiveFrom.UTC()
	policy.ID = s.newID()
	policy.CreatedAt = now
	if err := validatePolicy(s.cfg.Platform, policy); err != nil {
		return model.RetentionPolicy{}, err
	}
	if policy.Scope == model.ScopeAsset {
		if _, err := s.tenantAsset(ctx, policy.TenantID, policy.ScopeKey); err != nil {
			return model.RetentionPolicy{}, err
		}
	}

	stored, err := s.deps.Policies.AddVersion(ctx, policy)
	if err != nil {
		return model.RetentionPolicy{}, err
	}
	s.requestLockSync(ctx, policy.TenantID, policy.Scope, policy.ScopeKey, "policy "+stored.ID)
	s.record(ctx, s.newEvent(policy.TenantID, assetIDOf(policy.Scope, policy.ScopeKey), "", "policy_changed", "",
		fmt.Sprintf("%s %s v%d effective %s: %s", policy.Scope, policy.ScopeKey, stored.Version,
			stored.EffectiveFrom.Format(time.RFC3339), policy.Reason), policy.CreatedBy))
	return stored, nil
}

// PlaceHold blocks every permanent deletion in the scope until released.
func (s *Service) PlaceHold(ctx context.Context, hold model.LegalHold) (model.LegalHold, error) {
	switch hold.Scope {
	case model.ScopeTenant:
		if hold.ScopeKey != "" {
			return model.LegalHold{}, errors.New("a tenant hold has no scope key")
		}
	case model.ScopeDataset:
		if !model.ValidDataset(hold.ScopeKey) {
			return model.LegalHold{}, errors.New("invalid dataset name")
		}
	case model.ScopeAsset:
		if _, err := s.tenantAsset(ctx, hold.TenantID, hold.ScopeKey); err != nil {
			return model.LegalHold{}, err
		}
	default:
		return model.LegalHold{}, errors.New("hold scope must be TENANT, DATASET or ASSET")
	}
	if hold.TenantID == "" || hold.Reason == "" || hold.PlacedBy == "" {
		return model.LegalHold{}, errors.New("tenant, reason and actor are required")
	}
	hold.ID = s.newID()
	hold.PlacedAt = s.now()

	stored, err := s.deps.Holds.Place(ctx, hold)
	if err != nil {
		return model.LegalHold{}, err
	}
	s.requestLockSync(ctx, hold.TenantID, hold.Scope, hold.ScopeKey, "hold "+stored.ID)
	s.deps.Metrics.Add("legal_holds_placed", 1)
	s.record(ctx, s.newEvent(hold.TenantID, assetIDOf(hold.Scope, hold.ScopeKey), "", "hold_placed", "",
		fmt.Sprintf("hold %s on %s %s: %s", stored.ID, hold.Scope, hold.ScopeKey, hold.Reason), hold.PlacedBy))
	return stored, nil
}

// ReleaseHold ends a legal hold. Its legal_hold.released event, written in
// the same transaction, makes the deletions it blocked due again at once.
func (s *Service) ReleaseHold(ctx context.Context, tenantID, holdID, actor string) error {
	// The hold's scope, for taking it off locked versions promptly.
	var scope model.LegalHold
	if s.deps.LockSync != nil {
		active, err := s.deps.Holds.List(ctx, tenantID, false)
		if err != nil {
			return err
		}
		for _, h := range active {
			if h.ID == holdID {
				scope = h
			}
		}
	}
	now := s.now()
	released := events.NewHoldReleased(tenantID, holdID, now, correlation.From(ctx))
	if err := s.deps.Holds.Release(ctx, tenantID, holdID, actor, now, released); err != nil {
		return err
	}
	if scope.ID != "" {
		s.requestLockSync(ctx, tenantID, scope.Scope, scope.ScopeKey, "released hold "+holdID)
	}
	s.record(ctx, s.newEvent(tenantID, "", "", "hold_released", "", "hold "+holdID, actor))
	return nil
}

// Requeue gives a FAILED deletion a fresh attempt budget.
func (s *Service) Requeue(ctx context.Context, tenantID, operationID, actor string) (model.DeletionOperation, error) {
	op, err := s.deps.Deletions.Requeue(ctx, tenantID, operationID, s.now())
	if err != nil {
		return op, err
	}
	s.record(ctx, s.newEvent(tenantID, op.AssetID, op.ID, "deletion_requeued", string(op.State), "", actor))
	return op, nil
}

// Inspection is everything the lifecycle knows about one asset.
type Inspection struct {
	Asset       model.Asset
	Policy      EffectivePolicy
	RetainUntil time.Time
	ExpiresAt   *time.Time
	Holds       []model.LegalHold
	Operations  []model.DeletionOperation
	Events      []model.LifecycleEvent
	// LockedUntil is the S3 Object Lock date set on the stored version,
	// when the platform set one.
	LockedUntil *time.Time
}

// Inspect returns an asset's lifecycle record, for operators. Unlike the
// owner's view it includes tombstoned assets, whose deletion record is
// exactly what an operator may need to see.
func (s *Service) Inspect(ctx context.Context, tenantID, assetID string) (Inspection, error) {
	asset, err := s.loadAsset(ctx, tenantID, assetID)
	if err != nil {
		return Inspection{}, err
	}
	return s.inspect(ctx, asset)
}

// OwnerInspect is Inspect for the asset's owner. Legal holds are left out:
// whether a hold exists is not the owner's to see.
func (s *Service) OwnerInspect(ctx context.Context, principal model.Principal, assetID string) (Inspection, error) {
	asset, err := s.owned(ctx, principal, assetID, model.PermAssetsRead)
	if err != nil {
		return Inspection{}, err
	}
	inspection, err := s.inspect(ctx, asset)
	inspection.Holds = nil
	inspection.Events = nil
	return inspection, err
}

func (s *Service) inspect(ctx context.Context, asset model.Asset) (Inspection, error) {
	now := s.now()
	policy, err := s.policyFor(ctx, asset, now)
	if err != nil {
		return Inspection{}, err
	}
	out := Inspection{Asset: asset, Policy: policy, RetainUntil: policy.RetainUntil(asset.CreatedAt)}
	if at, ok := policy.ExpiresAt(asset.CreatedAt); ok {
		out.ExpiresAt = &at
	}
	if out.Holds, err = s.deps.Holds.Active(ctx, asset.TenantID, asset.Dataset, asset.ID); err != nil {
		return out, err
	}
	if out.Operations, err = s.deps.Deletions.ListForAsset(ctx, asset.TenantID, asset.ID); err != nil {
		return out, err
	}
	if s.deps.Locks != nil {
		lock, found, err := s.deps.Locks.GetLock(ctx, asset.TenantID, asset.ID)
		if err != nil {
			return out, err
		}
		if found {
			out.LockedUntil = lock.RetainUntil
		}
	}
	out.Events, err = s.deps.Events.List(ctx, asset.TenantID, asset.ID, 200)
	return out, err
}

// owned loads an asset of the caller's organization for a caller with
// perm. Another tenant's asset, and tombstones, are ErrNotFound.
func (s *Service) owned(ctx context.Context, principal model.Principal, assetID string, perm model.Permission) (model.Asset, error) {
	if !principal.Valid() {
		return model.Asset{}, model.ErrUnauthenticated
	}
	if err := audit.Require(ctx, s.security, principal, perm, "asset.access", "asset", assetID); err != nil {
		return model.Asset{}, err
	}
	return s.tenantAsset(ctx, principal.TenantID, assetID)
}

// tenantAsset loads the tenant's asset; tombstones are ErrNotFound.
func (s *Service) tenantAsset(ctx context.Context, tenantID, assetID string) (model.Asset, error) {
	asset, err := s.loadAsset(ctx, tenantID, assetID)
	if err != nil {
		return model.Asset{}, err
	}
	if asset.Status == model.AssetStatusDeleted {
		return model.Asset{}, ErrNotFound
	}
	return asset, nil
}

func (s *Service) loadAsset(ctx context.Context, tenantID, assetID string) (model.Asset, error) {
	if strings.TrimSpace(tenantID) == "" || strings.TrimSpace(assetID) == "" || len(assetID) > 200 {
		return model.Asset{}, ErrNotFound
	}
	asset, err := s.deps.Assets.GetByID(ctx, tenantID, assetID)
	if errors.Is(err, model.ErrAssetNotFound) {
		return model.Asset{}, ErrNotFound
	}
	if err != nil {
		return model.Asset{}, fmt.Errorf("load asset: %w", err)
	}
	return *asset, nil
}

// policyFor resolves the policy in effect for the asset at asOf.
func (s *Service) policyFor(ctx context.Context, asset model.Asset, asOf time.Time) (EffectivePolicy, error) {
	return NewPolicyResolver(s.deps.Policies, s.cfg.Platform).For(ctx, asset, asOf)
}

func (s *Service) newEvent(tenantID, assetID, operationID, name, outcome, reason, actor string) model.LifecycleEvent {
	return model.LifecycleEvent{
		ID: s.newID(), TenantID: tenantID, AssetID: assetID, OperationID: operationID,
		Event: name, Outcome: outcome, Reason: truncate(reason, 500), Actor: actor, CreatedAt: s.now(),
	}
}

// record appends an audit event. A failed write is logged and counted;
// the operation's own state stays the source of truth.
func (s *Service) record(ctx context.Context, event model.LifecycleEvent) {
	if err := s.deps.Events.Record(ctx, event); err != nil {
		s.deps.Metrics.Add("lifecycle_event_write_failures", 1)
		s.log.Warn("record lifecycle event", "event", event.Event, "asset_id", event.AssetID, "error", err)
	}
}

func assetIDOf(scope, key string) string {
	if scope == model.ScopeAsset {
		return key
	}
	return ""
}

func truncate(text string, limit int) string {
	if len(text) > limit {
		return text[:limit]
	}
	return text
}

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
