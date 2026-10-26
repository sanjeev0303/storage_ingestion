package interfaces

import (
	"context"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// RetentionPolicyRepository stores append-only policy versions.
type RetentionPolicyRepository interface {
	// AddVersion stores the next version of the scope's policy and returns
	// it with its version number. Earlier versions are kept.
	AddVersion(ctx context.Context, policy model.RetentionPolicy) (model.RetentionPolicy, error)
	// Effective returns the version in effect at asOf: the latest one whose
	// EffectiveFrom has passed. ok is false when the scope has none.
	Effective(ctx context.Context, tenantID, scope, scopeKey string, asOf time.Time) (model.RetentionPolicy, bool, error)
	// History returns every version of the scope's policy, newest first.
	History(ctx context.Context, tenantID, scope, scopeKey string) ([]model.RetentionPolicy, error)
}

// LegalHoldRepository stores legal holds. Every lookup is tenant-scoped.
type LegalHoldRepository interface {
	Place(ctx context.Context, hold model.LegalHold) (model.LegalHold, error)
	// Release ends a hold and writes outbox in the same transaction.
	// model.ErrHoldNotFound when the tenant has no active hold with this ID.
	Release(ctx context.Context, tenantID, holdID, releasedBy string, at time.Time, outbox ...model.OutboxEvent) error
	// Active returns the unreleased holds covering the tenant, the dataset
	// (when not empty) and the asset (when not empty).
	Active(ctx context.Context, tenantID, dataset, assetID string) ([]model.LegalHold, error)
	List(ctx context.Context, tenantID string, includeReleased bool) ([]model.LegalHold, error)
}

// DeletionRepository runs the durable deletion workflow. Every method that
// takes an owner changes the operation only while owner holds an unexpired
// lease, and returns model.ErrLeaseLost otherwise.
type DeletionRepository interface {
	// SoftDelete moves the asset from one of from to SOFT_DELETED and
	// records op, event and the outbox events, in one transaction. When
	// the asset already has a live operation, that one is returned with
	// created false and nothing changes. model.ErrInvalidAssetState when
	// the asset is in any other status.
	SoftDelete(ctx context.Context, op model.DeletionOperation, from []model.AssetStatus, event model.LifecycleEvent, outbox ...model.OutboxEvent) (model.DeletionOperation, bool, error)
	// Restore cancels the asset's restorable operation and returns the
	// asset to the operation's PreviousStatus, in one transaction.
	// model.ErrNotRestorable when there is no such operation.
	Restore(ctx context.Context, tenantID, assetID string, at time.Time, event model.LifecycleEvent) (model.DeletionOperation, error)

	Get(ctx context.Context, tenantID, operationID string) (model.DeletionOperation, error)
	// GetLive returns the asset's live operation, or model.ErrDeletionNotFound.
	GetLive(ctx context.Context, tenantID, assetID string) (model.DeletionOperation, error)
	ListForAsset(ctx context.Context, tenantID, assetID string) ([]model.DeletionOperation, error)

	// Claim leases up to limit due live operations, of one tenant when
	// tenantID is not empty. An operation whose lease expired (a crashed
	// worker) is claimable again.
	Claim(ctx context.Context, owner string, now time.Time, lease time.Duration, limit int, tenantID string) ([]model.DeletionOperation, error)
	// Due lists the operations Claim would take, without claiming them.
	Due(ctx context.Context, now time.Time, limit int, tenantID string) ([]model.DeletionOperation, error)
	// ClaimOne leases one operation the same way, by ID. ok is false when
	// it is not live, not due yet or leased by another worker.
	ClaimOne(ctx context.Context, tenantID, operationID, owner string, now time.Time, lease time.Duration) (op model.DeletionOperation, ok bool, err error)
	// ExpediteProtected makes the PROTECTED operations covered by a
	// released hold due at now, and returns how many it changed. Every
	// check still runs again before anything is deleted.
	ExpediteProtected(ctx context.Context, tenantID, holdID string, now time.Time) (int, error)

	// Protect parks the operation as PROTECTED until next and releases it.
	Protect(ctx context.Context, operationID, owner string, now, next time.Time, reason string) error
	// Wait parks a started operation until next and gives back the attempt
	// Start counted: waiting for a pack to let go of the asset's bytes is
	// not a failure.
	Wait(ctx context.Context, operationID, owner string, now, next time.Time, reason string) error
	// Start moves the operation to IN_PROGRESS and counts an attempt, but
	// only while the asset is still SOFT_DELETED and no active legal hold
	// covers it; the check and the change are one statement. started is
	// false (and nothing changes) when either condition fails.
	Start(ctx context.Context, operationID, owner string, now time.Time) (started bool, err error)
	// MarkStorageDeleted records that every object is confirmed gone.
	MarkStorageDeleted(ctx context.Context, operationID, owner string, now time.Time) error
	// Complete tombstones the asset (SOFT_DELETED -> DELETED), revokes its
	// download links, finishes the operation and records event, in one
	// transaction.
	Complete(ctx context.Context, operationID, owner string, now time.Time, event model.LifecycleEvent) error
	// Retry releases the operation as RETRY_WAIT until next.
	Retry(ctx context.Context, operationID, owner string, now, next time.Time, lastError string) error
	// Fail ends the operation as FAILED and records event.
	Fail(ctx context.Context, operationID, owner string, now time.Time, lastError string, event model.LifecycleEvent) error
	// Cancel ends an operation whose asset is no longer soft-deleted.
	Cancel(ctx context.Context, operationID, owner string, now time.Time, reason string) error
	// Requeue gives a FAILED operation a fresh attempt budget as
	// RETRY_WAIT. model.ErrInvalidAssetState for any other state.
	Requeue(ctx context.Context, tenantID, operationID string, now time.Time) (model.DeletionOperation, error)

	// References describes live records, other than the asset's own
	// finished ones, that still point at any of keys: an upload in
	// progress, an active processing job, an open quarantine hold or
	// another asset. Physical objects are removed only when it is empty.
	References(ctx context.Context, asset model.Asset, keys []string) ([]string, error)
}

// LifecycleEventRepository appends lifecycle audit events.
type LifecycleEventRepository interface {
	Record(ctx context.Context, event model.LifecycleEvent) error
	// List returns a tenant's events, of one asset when assetID is not
	// empty, oldest first, at most limit.
	List(ctx context.Context, tenantID, assetID string, limit int) ([]model.LifecycleEvent, error)
	// RecordedSince reports whether the asset has an event of this name
	// at or after since, so repeated findings are not recorded again.
	RecordedSince(ctx context.Context, tenantID, assetID, event string, since time.Time) (bool, error)
}

// CursorRepository keeps where resumable database scans continue.
type CursorRepository interface {
	// Get returns the saved position, or "" when there is none.
	Get(ctx context.Context, name string) (string, error)
	Set(ctx context.Context, name, position string, at time.Time) error
}
