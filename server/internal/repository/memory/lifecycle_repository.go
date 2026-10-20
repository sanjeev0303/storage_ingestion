package memory

import (
	"context"
	"errors"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// RetentionPolicyRepository keeps policy versions in memory. For tests.
type RetentionPolicyRepository struct {
	mu       sync.Mutex
	policies []model.RetentionPolicy
}

var _ interfaces.RetentionPolicyRepository = (*RetentionPolicyRepository)(nil)

func NewRetentionPolicyRepository() *RetentionPolicyRepository {
	return &RetentionPolicyRepository{}
}

func (r *RetentionPolicyRepository) AddVersion(ctx context.Context, policy model.RetentionPolicy) (model.RetentionPolicy, error) {
	if err := ctx.Err(); err != nil {
		return model.RetentionPolicy{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	policy.Version = 1
	for _, p := range r.policies {
		if p.TenantID == policy.TenantID && p.Scope == policy.Scope && p.ScopeKey == policy.ScopeKey && p.Version >= policy.Version {
			policy.Version = p.Version + 1
		}
	}
	r.policies = append(r.policies, policy)
	return policy, nil
}

func (r *RetentionPolicyRepository) Effective(ctx context.Context, tenantID, scope, scopeKey string, asOf time.Time) (model.RetentionPolicy, bool, error) {
	if err := ctx.Err(); err != nil {
		return model.RetentionPolicy{}, false, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	var best model.RetentionPolicy
	found := false
	for _, p := range r.policies {
		if p.TenantID != tenantID || p.Scope != scope || p.ScopeKey != scopeKey || p.EffectiveFrom.After(asOf) {
			continue
		}
		if !found || p.EffectiveFrom.After(best.EffectiveFrom) || (p.EffectiveFrom.Equal(best.EffectiveFrom) && p.Version > best.Version) {
			best, found = p, true
		}
	}
	return best, found, nil
}

func (r *RetentionPolicyRepository) History(ctx context.Context, tenantID, scope, scopeKey string) ([]model.RetentionPolicy, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	var out []model.RetentionPolicy
	for _, p := range r.policies {
		if p.TenantID == tenantID && p.Scope == scope && p.ScopeKey == scopeKey {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version > out[j].Version })
	return out, nil
}

// LegalHoldRepository keeps legal holds in memory. For tests.
type LegalHoldRepository struct {
	mu    sync.Mutex
	holds []model.LegalHold
	// outbox keeps the events written with releases, for tests.
	outbox []model.OutboxEvent
}

var _ interfaces.LegalHoldRepository = (*LegalHoldRepository)(nil)

func NewLegalHoldRepository() *LegalHoldRepository {
	return &LegalHoldRepository{}
}

func (r *LegalHoldRepository) Place(ctx context.Context, hold model.LegalHold) (model.LegalHold, error) {
	if err := ctx.Err(); err != nil {
		return model.LegalHold{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.holds = append(r.holds, hold)
	return hold, nil
}

func (r *LegalHoldRepository) Release(ctx context.Context, tenantID, holdID, releasedBy string, at time.Time, outbox ...model.OutboxEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, h := range r.holds {
		if h.TenantID == tenantID && h.ID == holdID && h.ReleasedAt == nil {
			released := at
			r.holds[i].ReleasedAt, r.holds[i].ReleasedBy = &released, releasedBy
			r.outbox = append(r.outbox, outbox...)
			return nil
		}
	}
	return model.ErrHoldNotFound
}

// Outbox returns the events written with releases so far.
func (r *LegalHoldRepository) Outbox() []model.OutboxEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.outbox)
}

// released returns the tenant's hold if it has been released.
func (r *LegalHoldRepository) released(tenantID, holdID string) (model.LegalHold, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, h := range r.holds {
		if h.TenantID == tenantID && h.ID == holdID && h.ReleasedAt != nil {
			return h, true
		}
	}
	return model.LegalHold{}, false
}

func (r *LegalHoldRepository) Active(ctx context.Context, tenantID, dataset, assetID string) ([]model.LegalHold, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.active(tenantID, dataset, assetID), nil
}

func (r *LegalHoldRepository) active(tenantID, dataset, assetID string) []model.LegalHold {
	var out []model.LegalHold
	for _, h := range r.holds {
		if h.TenantID != tenantID || h.ReleasedAt != nil {
			continue
		}
		if h.Scope == model.ScopeTenant ||
			(h.Scope == model.ScopeDataset && dataset != "" && h.ScopeKey == dataset) ||
			(h.Scope == model.ScopeAsset && assetID != "" && h.ScopeKey == assetID) {
			out = append(out, h)
		}
	}
	return out
}

func (r *LegalHoldRepository) List(ctx context.Context, tenantID string, includeReleased bool) ([]model.LegalHold, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []model.LegalHold
	for _, h := range r.holds {
		if h.TenantID == tenantID && (includeReleased || h.ReleasedAt == nil) {
			out = append(out, h)
		}
	}
	return out, nil
}

// LifecycleEventRepository keeps lifecycle events in memory. For tests.
type LifecycleEventRepository struct {
	mu     sync.Mutex
	events []model.LifecycleEvent
}

var _ interfaces.LifecycleEventRepository = (*LifecycleEventRepository)(nil)

func NewLifecycleEventRepository() *LifecycleEventRepository {
	return &LifecycleEventRepository{}
}

func (r *LifecycleEventRepository) Record(ctx context.Context, event model.LifecycleEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
	return nil
}

func (r *LifecycleEventRepository) List(ctx context.Context, tenantID, assetID string, limit int) ([]model.LifecycleEvent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []model.LifecycleEvent
	for _, e := range r.events {
		if e.TenantID == tenantID && (assetID == "" || e.AssetID == assetID) && len(out) < limit {
			out = append(out, e)
		}
	}
	return out, nil
}

func (r *LifecycleEventRepository) RecordedSince(ctx context.Context, tenantID, assetID, event string, since time.Time) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.events {
		if e.TenantID == tenantID && e.AssetID == assetID && e.Event == event && !e.CreatedAt.Before(since) {
			return true, nil
		}
	}
	return false, nil
}

// Names lists every recorded event name in order, for assertions.
func (r *LifecycleEventRepository) Names() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var names []string
	for _, e := range r.events {
		names = append(names, e.Event)
	}
	return names
}

// CursorRepository keeps scan cursors in memory. For tests.
type CursorRepository struct {
	mu        sync.Mutex
	positions map[string]string
}

var _ interfaces.CursorRepository = (*CursorRepository)(nil)

func NewCursorRepository() *CursorRepository {
	return &CursorRepository{positions: make(map[string]string)}
}

func (r *CursorRepository) Get(ctx context.Context, name string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.positions[name], nil
}

func (r *CursorRepository) Set(ctx context.Context, name, position string, _ time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.positions[name] = position
	return nil
}

// DeletionRepository mirrors the PostgreSQL deletion workflow, including
// its leases and its multi-record steps, over the other memory
// repositories. For tests.
type DeletionRepository struct {
	mu         sync.Mutex
	ops        map[string]model.DeletionOperation
	outbox     []model.OutboxEvent
	assets     *AssetRepository
	tokens     *DownloadTokenRepository
	uploads    *UploadRepository
	jobs       *ProcessingJobRepository
	quarantine *QuarantineRepository
	holds      *LegalHoldRepository
	events     *LifecycleEventRepository
}

var _ interfaces.DeletionRepository = (*DeletionRepository)(nil)

func NewDeletionRepository(assets *AssetRepository, tokens *DownloadTokenRepository, uploads *UploadRepository,
	jobs *ProcessingJobRepository, quarantine *QuarantineRepository, holds *LegalHoldRepository, events *LifecycleEventRepository) *DeletionRepository {
	return &DeletionRepository{
		ops: make(map[string]model.DeletionOperation), assets: assets, tokens: tokens, uploads: uploads,
		jobs: jobs, quarantine: quarantine, holds: holds, events: events,
	}
}

func (r *DeletionRepository) liveFor(tenantID, assetID string) (model.DeletionOperation, bool) {
	for _, op := range r.ops {
		if op.TenantID == tenantID && op.AssetID == assetID && op.State.IsLive() {
			return op, true
		}
	}
	return model.DeletionOperation{}, false
}

func (r *DeletionRepository) setAssetStatus(tenantID, assetID string, from []model.AssetStatus, to model.AssetStatus, at time.Time) bool {
	r.assets.mu.Lock()
	defer r.assets.mu.Unlock()
	asset, ok := r.assets.assets[assetID]
	if !ok || asset.TenantID != tenantID || !slices.Contains(from, asset.Status) {
		return false
	}
	asset.Status, asset.UpdatedAt = to, at
	r.assets.assets[assetID] = asset
	return true
}

func (r *DeletionRepository) SoftDelete(ctx context.Context, op model.DeletionOperation, from []model.AssetStatus, event model.LifecycleEvent, outbox ...model.OutboxEvent) (model.DeletionOperation, bool, error) {
	if err := ctx.Err(); err != nil {
		return model.DeletionOperation{}, false, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, err := r.assets.GetByID(ctx, op.TenantID, op.AssetID); err != nil {
		return model.DeletionOperation{}, false, err
	}
	if live, ok := r.liveFor(op.TenantID, op.AssetID); ok {
		return live, false, nil
	}
	if !r.setAssetStatus(op.TenantID, op.AssetID, from, model.AssetStatusSoftDeleted, op.CreatedAt) {
		return model.DeletionOperation{}, false, model.ErrInvalidAssetState
	}
	r.ops[op.ID] = op
	event.OperationID = op.ID
	_ = r.events.Record(ctx, event)
	r.outbox = append(r.outbox, outbox...)
	return op, true, nil
}

// Outbox returns the events written with soft deletes so far.
func (r *DeletionRepository) Outbox() []model.OutboxEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.outbox)
}

func (r *DeletionRepository) Restore(ctx context.Context, tenantID, assetID string, at time.Time, event model.LifecycleEvent) (model.DeletionOperation, error) {
	if err := ctx.Err(); err != nil {
		return model.DeletionOperation{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	op, ok := r.liveFor(tenantID, assetID)
	if !ok || !op.State.Restorable() {
		return model.DeletionOperation{}, model.ErrNotRestorable
	}
	if !r.setAssetStatus(tenantID, assetID, []model.AssetStatus{model.AssetStatusSoftDeleted}, op.PreviousStatus, at) {
		return model.DeletionOperation{}, model.ErrNotRestorable
	}
	completed := at
	op.State, op.CompletedAt, op.LeaseOwner, op.LeaseExpiresAt, op.UpdatedAt = model.DeletionCancelled, &completed, "", nil, at
	r.ops[op.ID] = op
	event.OperationID = op.ID
	_ = r.events.Record(ctx, event)
	return op, nil
}

func (r *DeletionRepository) Get(ctx context.Context, tenantID, operationID string) (model.DeletionOperation, error) {
	if err := ctx.Err(); err != nil {
		return model.DeletionOperation{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	op, ok := r.ops[operationID]
	if !ok || op.TenantID != tenantID {
		return model.DeletionOperation{}, model.ErrDeletionNotFound
	}
	return op, nil
}

func (r *DeletionRepository) GetLive(ctx context.Context, tenantID, assetID string) (model.DeletionOperation, error) {
	if err := ctx.Err(); err != nil {
		return model.DeletionOperation{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if op, ok := r.liveFor(tenantID, assetID); ok {
		return op, nil
	}
	return model.DeletionOperation{}, model.ErrDeletionNotFound
}

func (r *DeletionRepository) ListForAsset(ctx context.Context, tenantID, assetID string) ([]model.DeletionOperation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []model.DeletionOperation
	for _, op := range r.ops {
		if op.TenantID == tenantID && op.AssetID == assetID {
			out = append(out, op)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func (r *DeletionRepository) due(now time.Time, limit int, tenantID string) []model.DeletionOperation {
	var due []model.DeletionOperation
	for _, op := range r.ops {
		leaseFree := op.LeaseExpiresAt == nil || !op.LeaseExpiresAt.After(now)
		if op.State.IsLive() && !op.NextAttemptAt.After(now) && leaseFree && (tenantID == "" || op.TenantID == tenantID) {
			due = append(due, op)
		}
	}
	sort.Slice(due, func(i, j int) bool { return due[i].NextAttemptAt.Before(due[j].NextAttemptAt) })
	if len(due) > limit {
		due = due[:limit]
	}
	return due
}

func (r *DeletionRepository) Claim(ctx context.Context, owner string, now time.Time, lease time.Duration, limit int, tenantID string) ([]model.DeletionOperation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	claimed := r.due(now, limit, tenantID)
	expires := now.Add(lease)
	for i := range claimed {
		claimed[i].LeaseOwner, claimed[i].LeaseExpiresAt, claimed[i].UpdatedAt = owner, &expires, now
		r.ops[claimed[i].ID] = claimed[i]
	}
	return claimed, nil
}

func (r *DeletionRepository) Due(ctx context.Context, now time.Time, limit int, tenantID string) ([]model.DeletionOperation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.due(now, limit, tenantID), nil
}

func (r *DeletionRepository) ClaimOne(ctx context.Context, tenantID, operationID, owner string, now time.Time, lease time.Duration) (model.DeletionOperation, bool, error) {
	if err := ctx.Err(); err != nil {
		return model.DeletionOperation{}, false, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	op, ok := r.ops[operationID]
	leaseFree := op.LeaseExpiresAt == nil || !op.LeaseExpiresAt.After(now)
	if !ok || op.TenantID != tenantID || !op.State.IsLive() || op.NextAttemptAt.After(now) || !leaseFree {
		return model.DeletionOperation{}, false, nil
	}
	expires := now.Add(lease)
	op.LeaseOwner, op.LeaseExpiresAt, op.UpdatedAt = owner, &expires, now
	r.ops[op.ID] = op
	return op, true, nil
}

func (r *DeletionRepository) ExpediteProtected(ctx context.Context, tenantID, holdID string, now time.Time) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	hold, ok := r.holds.released(tenantID, holdID)
	if !ok {
		return 0, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	changed := 0
	for id, op := range r.ops {
		if op.TenantID != tenantID || op.State != model.DeletionProtected || !op.NextAttemptAt.After(now) {
			continue
		}
		asset, err := r.assets.GetByID(ctx, tenantID, op.AssetID)
		if err != nil {
			continue
		}
		covered := hold.Scope == model.ScopeTenant ||
			(hold.Scope == model.ScopeDataset && hold.ScopeKey == asset.Dataset) ||
			(hold.Scope == model.ScopeAsset && hold.ScopeKey == asset.ID)
		if !covered {
			continue
		}
		op.NextAttemptAt, op.UpdatedAt = now, now
		r.ops[id] = op
		changed++
	}
	return changed, nil
}

// leased applies change to an operation owner holds, in one of states.
func (r *DeletionRepository) leased(ctx context.Context, id, owner string, now time.Time, states []model.DeletionState, change func(*model.DeletionOperation)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	op, ok := r.ops[id]
	if !ok || op.LeaseOwner != owner || op.LeaseExpiresAt == nil || !op.LeaseExpiresAt.After(now) || !slices.Contains(states, op.State) {
		return model.ErrLeaseLost
	}
	change(&op)
	op.UpdatedAt = now
	r.ops[id] = op
	return nil
}

var (
	startableStates = []model.DeletionState{model.DeletionScheduled, model.DeletionProtected, model.DeletionRetryWait}
	liveStates      = []model.DeletionState{model.DeletionScheduled, model.DeletionProtected, model.DeletionInProgress, model.DeletionRetryWait, model.DeletionStorageDeleted}
)

func release(op *model.DeletionOperation) {
	op.LeaseOwner, op.LeaseExpiresAt = "", nil
}

func (r *DeletionRepository) Protect(ctx context.Context, id, owner string, now, next time.Time, reason string) error {
	return r.leased(ctx, id, owner, now, startableStates, func(op *model.DeletionOperation) {
		op.State, op.ProtectedReason, op.NextAttemptAt = model.DeletionProtected, reason, next
		release(op)
	})
}

func (r *DeletionRepository) Start(ctx context.Context, id, owner string, now time.Time) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	r.mu.Lock()
	op, ok := r.ops[id]
	r.mu.Unlock()
	if !ok {
		return false, model.ErrLeaseLost
	}

	asset, err := r.assets.GetByID(ctx, op.TenantID, op.AssetID)
	if err != nil {
		return false, err
	}
	r.holds.mu.Lock()
	held := len(r.holds.active(asset.TenantID, asset.Dataset, asset.ID)) > 0
	r.holds.mu.Unlock()

	started := false
	err = r.leased(ctx, id, owner, now, startableStates, func(op *model.DeletionOperation) {
		if held || asset.Status != model.AssetStatusSoftDeleted {
			return
		}
		op.State, op.AttemptCount, op.ProtectedReason, op.NextAttemptAt = model.DeletionInProgress, op.AttemptCount+1, "", now
		started = true
	})
	return started, err
}

func (r *DeletionRepository) MarkStorageDeleted(ctx context.Context, id, owner string, now time.Time) error {
	return r.leased(ctx, id, owner, now, []model.DeletionState{model.DeletionInProgress}, func(op *model.DeletionOperation) {
		at := now
		op.State, op.StorageDeletedAt, op.LastError = model.DeletionStorageDeleted, &at, ""
	})
}

func (r *DeletionRepository) Complete(ctx context.Context, id, owner string, now time.Time, event model.LifecycleEvent) error {
	var op model.DeletionOperation
	err := r.leased(ctx, id, owner, now, []model.DeletionState{model.DeletionStorageDeleted}, func(o *model.DeletionOperation) {
		at := now
		o.State, o.CompletedAt = model.DeletionCompleted, &at
		release(o)
		op = *o
	})
	if err != nil {
		return err
	}
	r.setAssetStatus(op.TenantID, op.AssetID, []model.AssetStatus{model.AssetStatusSoftDeleted}, model.AssetStatusDeleted, now)
	_, _ = r.tokens.RevokeForAsset(ctx, op.TenantID, op.AssetID, now)
	return r.events.Record(ctx, event)
}

func (r *DeletionRepository) Retry(ctx context.Context, id, owner string, now, next time.Time, lastError string) error {
	return r.leased(ctx, id, owner, now, liveStates, func(op *model.DeletionOperation) {
		if op.State == model.DeletionInProgress {
			op.State = model.DeletionRetryWait
		}
		op.NextAttemptAt, op.LastError = next, lastError
		release(op)
	})
}

func (r *DeletionRepository) Fail(ctx context.Context, id, owner string, now time.Time, lastError string, event model.LifecycleEvent) error {
	err := r.leased(ctx, id, owner, now, liveStates, func(op *model.DeletionOperation) {
		op.State, op.LastError = model.DeletionFailed, lastError
		release(op)
	})
	if err != nil {
		return err
	}
	return r.events.Record(ctx, event)
}

func (r *DeletionRepository) Cancel(ctx context.Context, id, owner string, now time.Time, reason string) error {
	return r.leased(ctx, id, owner, now, startableStates, func(op *model.DeletionOperation) {
		at := now
		op.State, op.LastError, op.CompletedAt = model.DeletionCancelled, reason, &at
		release(op)
	})
}

func (r *DeletionRepository) Requeue(ctx context.Context, tenantID, id string, now time.Time) (model.DeletionOperation, error) {
	if err := ctx.Err(); err != nil {
		return model.DeletionOperation{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	op, ok := r.ops[id]
	if !ok || op.TenantID != tenantID {
		return model.DeletionOperation{}, model.ErrDeletionNotFound
	}
	if op.State != model.DeletionFailed {
		return model.DeletionOperation{}, model.ErrInvalidAssetState
	}
	op.State, op.AttemptCount, op.NextAttemptAt, op.LastError, op.UpdatedAt = model.DeletionRetryWait, 0, now, "", now
	r.ops[id] = op
	return op, nil
}

func (r *DeletionRepository) References(ctx context.Context, asset model.Asset, keys []string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var refs []string

	r.uploads.mu.RLock()
	for _, s := range r.uploads.uploads {
		active := s.Status == model.UploadStatusPending || s.Status == model.UploadStatusUploading ||
			s.Status == model.UploadStatusCompleting || s.Status == model.UploadStatusAborting
		own := s.TenantID == asset.TenantID && s.ID == asset.UploadID
		if active && !own && slices.Contains(keys, s.ObjectKey) {
			refs = append(refs, "upload "+s.ID+" is "+string(s.Status))
		}
	}
	r.uploads.mu.RUnlock()

	r.jobs.mu.Lock()
	for _, job := range r.jobs.jobs {
		if job.State == model.ProcessingPending && job.AssetID != asset.ID && slices.Contains(keys, job.PromotionKey) {
			refs = append(refs, "processing job "+job.ID+" is active")
		}
	}
	r.jobs.mu.Unlock()

	r.assets.mu.Lock()
	for _, other := range r.assets.assets {
		if other.ID != asset.ID && other.Status != model.AssetStatusDeleted && slices.Contains(keys, other.ObjectKey) {
			refs = append(refs, "asset "+other.ID+" is "+string(other.Status))
		}
	}
	r.assets.mu.Unlock()

	r.quarantine.mu.Lock()
	for _, record := range r.quarantine.records {
		if record.Status == model.QuarantineHeld && record.AssetID != asset.ID && slices.Contains(keys, record.ObjectKey) {
			refs = append(refs, "quarantine hold "+record.ID)
		}
	}
	r.quarantine.mu.Unlock()

	sort.Strings(refs)
	return refs, nil
}

// Ops returns a snapshot of every operation, for assertions.
func (r *DeletionRepository) Ops() []model.DeletionOperation {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]model.DeletionOperation, 0, len(r.ops))
	for _, op := range r.ops {
		out = append(out, op)
	}
	return out
}

// InventoryRepository keeps scans and orphan candidates in memory. For tests.
type InventoryRepository struct {
	mu         sync.Mutex
	scans      map[string]model.InventoryScan
	leases     map[string]scanLease
	candidates map[string]model.OrphanCandidate
}

type scanLease struct {
	owner   string
	expires time.Time
}

var _ interfaces.InventoryRepository = (*InventoryRepository)(nil)

func NewInventoryRepository() *InventoryRepository {
	return &InventoryRepository{
		scans: make(map[string]model.InventoryScan), leases: make(map[string]scanLease),
		candidates: make(map[string]model.OrphanCandidate),
	}
}

// connectionOrPlatform reads "" as the platform connection.
func connectionOrPlatform(id string) string {
	if id == "" {
		return model.PlatformConnectionID
	}
	return id
}

func (r *InventoryRepository) OpenScan(ctx context.Context, connectionID, bucket, prefix, owner, newID string, now time.Time, lease time.Duration) (model.InventoryScan, bool, error) {
	connectionID = connectionOrPlatform(connectionID)
	if err := ctx.Err(); err != nil {
		return model.InventoryScan{}, false, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	for id, scan := range r.scans {
		if scan.ConnectionID != connectionID || scan.Bucket != bucket || scan.Prefix != prefix || scan.Status != model.ScanRunning {
			continue
		}
		held := r.leases[id]
		if held.owner != "" && held.owner != owner && held.expires.After(now) {
			return model.InventoryScan{}, false, nil
		}
		r.leases[id] = scanLease{owner: owner, expires: now.Add(lease)}
		return scan, true, nil
	}

	scan := model.InventoryScan{ID: newID, ConnectionID: connectionID, Bucket: bucket, Prefix: prefix, Status: model.ScanRunning, StartedAt: now, UpdatedAt: now}
	r.scans[newID] = scan
	r.leases[newID] = scanLease{owner: owner, expires: now.Add(lease)}
	return scan, true, nil
}

func (r *InventoryRepository) SaveProgress(ctx context.Context, scan model.InventoryScan, owner string, now time.Time, lease time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	stored, ok := r.scans[scan.ID]
	if !ok || stored.Status != model.ScanRunning || r.leases[scan.ID].owner != owner {
		return model.ErrLeaseLost
	}
	scan.Status, scan.UpdatedAt = model.ScanRunning, now
	r.scans[scan.ID] = scan
	r.leases[scan.ID] = scanLease{owner: owner, expires: now.Add(lease)}
	return nil
}

func (r *InventoryRepository) FinishScan(ctx context.Context, scan model.InventoryScan, owner string, now time.Time, status string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	stored, ok := r.scans[scan.ID]
	if !ok || stored.Status != model.ScanRunning || r.leases[scan.ID].owner != owner {
		return model.ErrLeaseLost
	}
	scan.Status, scan.Cursor, scan.UpdatedAt = status, "", now
	if status == model.ScanCompleted {
		at := now
		scan.CompletedAt = &at
	}
	r.scans[scan.ID] = scan
	delete(r.leases, scan.ID)
	return nil
}

func (r *InventoryRepository) LastCompleted(ctx context.Context, connectionID, bucket, prefix string) (model.InventoryScan, bool, error) {
	connectionID = connectionOrPlatform(connectionID)
	if err := ctx.Err(); err != nil {
		return model.InventoryScan{}, false, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var last model.InventoryScan
	found := false
	for _, scan := range r.scans {
		if scan.ConnectionID == connectionID && scan.Bucket == bucket && scan.Prefix == prefix && scan.Status == model.ScanCompleted &&
			(!found || scan.CompletedAt.After(*last.CompletedAt)) {
			last, found = scan, true
		}
	}
	return last, found, nil
}

func (r *InventoryRepository) RecentScans(ctx context.Context, limit int) ([]model.InventoryScan, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []model.InventoryScan
	for _, scan := range r.scans {
		out = append(out, scan)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.After(out[j].StartedAt) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func open(status string) bool {
	return status == model.CandidateMarked || status == model.CandidateFailed
}

func (r *InventoryRepository) Mark(ctx context.Context, c model.OrphanCandidate, scanID string, now time.Time) (model.OrphanCandidate, error) {
	if err := ctx.Err(); err != nil {
		return model.OrphanCandidate{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	c.ConnectionID = connectionOrPlatform(c.ConnectionID)
	for id, existing := range r.candidates {
		if existing.ConnectionID != c.ConnectionID || existing.Bucket != c.Bucket || existing.ObjectKey != c.ObjectKey || !open(existing.Status) {
			continue
		}
		switch {
		case existing.ETag != c.ETag:
			existing.Observations, existing.FirstSeenAt, existing.AttemptCount = 1, now, 0
		case existing.LastScanID != scanID:
			existing.Observations++
		}
		existing.ETag, existing.SizeBytes, existing.Category, existing.TenantID, existing.Reason = c.ETag, c.SizeBytes, c.Category, c.TenantID, c.Reason
		existing.LastSeenAt, existing.LastScanID, existing.UpdatedAt = now, scanID, now
		r.candidates[id] = existing
		return existing, nil
	}

	c.Status, c.Observations, c.FirstSeenAt, c.LastSeenAt, c.LastScanID = model.CandidateMarked, 1, now, now, scanID
	c.CreatedAt, c.UpdatedAt = now, now
	r.candidates[c.ID] = c
	return c, nil
}

func (r *InventoryRepository) DueCandidates(ctx context.Context, connectionID, bucket, keyPrefix string, seenBefore time.Time, minObservations, maxAttempts, limit int) ([]model.OrphanCandidate, error) {
	connectionID = connectionOrPlatform(connectionID)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	var due []model.OrphanCandidate
	for _, c := range r.candidates {
		scan, ok := r.scans[c.LastScanID]
		if c.ConnectionID == connectionID && c.Bucket == bucket && strings.HasPrefix(c.ObjectKey, keyPrefix) && open(c.Status) &&
			c.Observations >= minObservations && !c.FirstSeenAt.After(seenBefore) && c.AttemptCount < maxAttempts &&
			ok && scan.Status == model.ScanCompleted {
			due = append(due, c)
		}
	}
	sort.Slice(due, func(i, j int) bool { return due[i].FirstSeenAt.Before(due[j].FirstSeenAt) })
	if len(due) > limit {
		due = due[:limit]
	}
	return due, nil
}

func (r *InventoryRepository) Resolve(ctx context.Context, id, status, reason string, now time.Time) error {
	if status != model.CandidateDeleted && status != model.CandidateCleared {
		return errors.New("a candidate resolves as DELETED or CLEARED")
	}
	return r.update(ctx, id, func(c *model.OrphanCandidate) {
		at := now
		c.Status, c.Reason, c.ResolvedAt, c.UpdatedAt = status, reason, &at, now
	})
}

func (r *InventoryRepository) FailCandidate(ctx context.Context, id, lastError string, now time.Time) error {
	return r.update(ctx, id, func(c *model.OrphanCandidate) {
		c.Status, c.AttemptCount, c.LastError, c.UpdatedAt = model.CandidateFailed, c.AttemptCount+1, lastError, now
	})
}

func (r *InventoryRepository) update(ctx context.Context, id string, change func(*model.OrphanCandidate)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.candidates[id]
	if ok && open(c.Status) {
		change(&c)
		r.candidates[id] = c
	}
	return nil
}

func (r *InventoryRepository) ListCandidates(ctx context.Context, status string, limit int) ([]model.OrphanCandidate, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []model.OrphanCandidate
	for _, c := range r.candidates {
		if status == "" || c.Status == status {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ObjectKey < out[j].ObjectKey })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
