package postgres

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// DeletionRepository runs the deletion workflow in PostgreSQL.
type DeletionRepository struct {
	db *DB
}

var _ interfaces.DeletionRepository = (*DeletionRepository)(nil)

func NewDeletionRepository(db *DB) *DeletionRepository {
	return &DeletionRepository{db: db}
}

const deletionColumns = `
	id, tenant_id, asset_id, state, reason, requested_by, previous_status,
	COALESCE(policy_detail, ''), purge_after, COALESCE(protected_reason, ''),
	attempt_count, max_attempts, next_attempt_at, COALESCE(last_error, ''),
	COALESCE(lease_owner, ''), lease_expires_at, storage_deleted_at, completed_at,
	created_at, updated_at
`

// liveStates are the states still working towards deletion.
const liveStates = `('SCHEDULED', 'PROTECTED', 'IN_PROGRESS', 'RETRY_WAIT', 'STORAGE_DELETED')`

func scanDeletion(row pgx.Row) (model.DeletionOperation, error) {
	var op model.DeletionOperation
	var state, previous string
	err := row.Scan(&op.ID, &op.TenantID, &op.AssetID, &state, &op.Reason, &op.RequestedBy, &previous,
		&op.PolicyDetail, &op.PurgeAfter, &op.ProtectedReason,
		&op.AttemptCount, &op.MaxAttempts, &op.NextAttemptAt, &op.LastError,
		&op.LeaseOwner, &op.LeaseExpiresAt, &op.StorageDeletedAt, &op.CompletedAt,
		&op.CreatedAt, &op.UpdatedAt)
	op.State, op.PreviousStatus = model.DeletionState(state), model.AssetStatus(previous)
	return op, err
}

func (r *DeletionRepository) SoftDelete(ctx context.Context, op model.DeletionOperation, from []model.AssetStatus, event model.LifecycleEvent, outbox ...model.OutboxEvent) (model.DeletionOperation, bool, error) {
	tx, err := r.db.Pool.Begin(ctx)
	if err != nil {
		return model.DeletionOperation{}, false, fmt.Errorf("begin soft delete: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	// Locking the asset row serializes concurrent requests and restores.
	var status string
	err = tx.QueryRow(ctx, `SELECT status FROM assets WHERE tenant_id = $1 AND id = $2 FOR UPDATE`,
		op.TenantID, op.AssetID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.DeletionOperation{}, false, model.ErrAssetNotFound
	}
	if err != nil {
		return model.DeletionOperation{}, false, fmt.Errorf("lock asset: %w", err)
	}

	live, err := scanDeletion(tx.QueryRow(ctx, `
		SELECT `+deletionColumns+` FROM deletion_operations
		WHERE tenant_id = $1 AND asset_id = $2 AND state IN `+liveStates,
		op.TenantID, op.AssetID))
	if err == nil {
		return live, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return model.DeletionOperation{}, false, fmt.Errorf("find live deletion: %w", err)
	}
	if !slices.Contains(from, model.AssetStatus(status)) {
		return model.DeletionOperation{}, false, model.ErrInvalidAssetState
	}

	var uploadID string
	var sizeBytes int64
	if err := tx.QueryRow(ctx, `
		UPDATE assets SET status = 'SOFT_DELETED', updated_at = $3
		WHERE tenant_id = $1 AND id = $2
		RETURNING upload_id, size_bytes
	`, op.TenantID, op.AssetID, op.CreatedAt).Scan(&uploadID, &sizeBytes); err != nil {
		return model.DeletionOperation{}, false, fmt.Errorf("soft-delete asset: %w", err)
	}
	// A deleted version stops being current at once (lock order: asset,
	// object, session, usage).
	catalogEvents, err := retireVersion(ctx, tx, op.TenantID, op.AssetID, op.CreatedAt)
	if err != nil {
		return model.DeletionOperation{}, false, err
	}
	// Its bytes stay counted, as pending deletion, until the purge.
	usage := usageDelta{pending: sizeBytes}
	if model.AssetStatus(status) == model.AssetStatusAvailable {
		usage.stored, usage.assets = -sizeBytes, -1
	} else if err := releaseReservation(ctx, tx, op.TenantID, uploadID, op.CreatedAt); err != nil {
		return model.DeletionOperation{}, false, err
	}
	if err := addUsage(ctx, tx, op.TenantID, usage, op.CreatedAt); err != nil {
		return model.DeletionOperation{}, false, err
	}
	stored, err := scanDeletion(tx.QueryRow(ctx, `
		INSERT INTO deletion_operations (
			id, tenant_id, asset_id, state, reason, requested_by, previous_status, policy_detail,
			purge_after, max_attempts, next_attempt_at, created_at, updated_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NULLIF($8, ''), $9, $10, $11, $12, $12)
		RETURNING `+deletionColumns,
		op.ID, op.TenantID, op.AssetID, string(op.State), op.Reason, op.RequestedBy, string(op.PreviousStatus),
		op.PolicyDetail, op.PurgeAfter, op.MaxAttempts, op.NextAttemptAt, op.CreatedAt))
	if err != nil {
		return model.DeletionOperation{}, false, fmt.Errorf("record deletion: %w", err)
	}
	event.OperationID = stored.ID
	if err := insertEvent(ctx, tx, event); err != nil {
		return model.DeletionOperation{}, false, err
	}
	if err := insertOutbox(ctx, tx, outbox...); err != nil {
		return model.DeletionOperation{}, false, err
	}
	if err := appendAudits(ctx, tx, catalogEvents); err != nil {
		return model.DeletionOperation{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return model.DeletionOperation{}, false, fmt.Errorf("commit soft delete: %w", err)
	}
	return stored, true, nil
}

func (r *DeletionRepository) Restore(ctx context.Context, tenantID, assetID string, at time.Time, event model.LifecycleEvent) (model.DeletionOperation, error) {
	tx, err := r.db.Pool.Begin(ctx)
	if err != nil {
		return model.DeletionOperation{}, fmt.Errorf("begin restore: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	// Only an operation that has not started removing objects can be
	// cancelled; the state check and the change are one statement.
	op, err := scanDeletion(tx.QueryRow(ctx, `
		UPDATE deletion_operations
		SET state = 'CANCELLED', completed_at = $3, lease_owner = NULL, lease_expires_at = NULL, updated_at = $3
		WHERE tenant_id = $1 AND asset_id = $2 AND state IN ('SCHEDULED', 'PROTECTED')
		RETURNING `+deletionColumns,
		tenantID, assetID, at))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.DeletionOperation{}, model.ErrNotRestorable
	}
	if err != nil {
		return model.DeletionOperation{}, fmt.Errorf("cancel deletion: %w", err)
	}

	var sizeBytes int64
	err = tx.QueryRow(ctx, `
		UPDATE assets SET status = $3, updated_at = $4
		WHERE tenant_id = $1 AND id = $2 AND status = 'SOFT_DELETED'
		RETURNING size_bytes
	`, tenantID, assetID, string(op.PreviousStatus), at).Scan(&sizeBytes)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.DeletionOperation{}, model.ErrNotRestorable
	}
	if err != nil {
		return model.DeletionOperation{}, fmt.Errorf("restore asset: %w", err)
	}
	catalogEvents, err := restoreVersion(ctx, tx, tenantID, assetID, at)
	if err != nil {
		return model.DeletionOperation{}, err
	}
	usage := usageDelta{pending: -sizeBytes}
	if op.PreviousStatus == model.AssetStatusAvailable {
		usage.stored, usage.assets = sizeBytes, 1
	}
	if err := addUsage(ctx, tx, tenantID, usage, at); err != nil {
		return model.DeletionOperation{}, err
	}
	event.OperationID = op.ID
	if err := insertEvent(ctx, tx, event); err != nil {
		return model.DeletionOperation{}, err
	}
	if err := appendAudits(ctx, tx, catalogEvents); err != nil {
		return model.DeletionOperation{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return model.DeletionOperation{}, fmt.Errorf("commit restore: %w", err)
	}
	return op, nil
}

func (r *DeletionRepository) Get(ctx context.Context, tenantID, operationID string) (model.DeletionOperation, error) {
	op, err := scanDeletion(r.db.Pool.QueryRow(ctx, `
		SELECT `+deletionColumns+` FROM deletion_operations WHERE tenant_id = $1 AND id = $2
	`, tenantID, operationID))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.DeletionOperation{}, model.ErrDeletionNotFound
	}
	if err != nil {
		return model.DeletionOperation{}, fmt.Errorf("get deletion: %w", err)
	}
	return op, nil
}

func (r *DeletionRepository) GetLive(ctx context.Context, tenantID, assetID string) (model.DeletionOperation, error) {
	op, err := scanDeletion(r.db.Pool.QueryRow(ctx, `
		SELECT `+deletionColumns+` FROM deletion_operations
		WHERE tenant_id = $1 AND asset_id = $2 AND state IN `+liveStates,
		tenantID, assetID))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.DeletionOperation{}, model.ErrDeletionNotFound
	}
	if err != nil {
		return model.DeletionOperation{}, fmt.Errorf("get live deletion: %w", err)
	}
	return op, nil
}

func (r *DeletionRepository) ListForAsset(ctx context.Context, tenantID, assetID string) ([]model.DeletionOperation, error) {
	return r.query(ctx, `
		SELECT `+deletionColumns+` FROM deletion_operations
		WHERE tenant_id = $1 AND asset_id = $2
		ORDER BY created_at DESC, id
	`, tenantID, assetID)
}

func (r *DeletionRepository) Claim(ctx context.Context, owner string, now time.Time, lease time.Duration, limit int, tenantID string) ([]model.DeletionOperation, error) {
	if owner == "" || lease <= 0 || limit <= 0 {
		return nil, errors.New("claim needs an owner, a positive lease and a positive limit")
	}
	// SKIP LOCKED: concurrent workers each get different rows. An expired
	// lease is claimable again, so a crashed worker never blocks recovery.
	return r.query(ctx, `
		WITH due AS (
			SELECT id AS due_id
			FROM deletion_operations
			WHERE state IN `+liveStates+`
			  AND next_attempt_at <= $2
			  AND (lease_expires_at IS NULL OR lease_expires_at <= $2)
			  AND ($5 = '' OR tenant_id = $5)
			ORDER BY next_attempt_at
			LIMIT $4
			FOR UPDATE SKIP LOCKED
		)
		UPDATE deletion_operations AS d
		SET lease_owner = $1,
		    lease_expires_at = $2::timestamptz + make_interval(secs => $3),
		    updated_at = $2
		FROM due
		WHERE d.id = due.due_id
		RETURNING `+deletionColumns,
		owner, now, lease.Seconds(), limit, tenantID)
}

func (r *DeletionRepository) Due(ctx context.Context, now time.Time, limit int, tenantID string) ([]model.DeletionOperation, error) {
	return r.query(ctx, `
		SELECT `+deletionColumns+` FROM deletion_operations
		WHERE state IN `+liveStates+`
		  AND next_attempt_at <= $1
		  AND (lease_expires_at IS NULL OR lease_expires_at <= $1)
		  AND ($3 = '' OR tenant_id = $3)
		ORDER BY next_attempt_at
		LIMIT $2
	`, now, limit, tenantID)
}

func (r *DeletionRepository) ClaimOne(ctx context.Context, tenantID, operationID, owner string, now time.Time, lease time.Duration) (model.DeletionOperation, bool, error) {
	if owner == "" || lease <= 0 {
		return model.DeletionOperation{}, false, errors.New("claim needs an owner and a positive lease")
	}
	op, err := scanDeletion(r.db.Pool.QueryRow(ctx, `
		UPDATE deletion_operations
		SET lease_owner = $3,
		    lease_expires_at = $4::timestamptz + make_interval(secs => $5),
		    updated_at = $4
		WHERE tenant_id = $1 AND id = $2
		  AND state IN `+liveStates+`
		  AND next_attempt_at <= $4
		  AND (lease_expires_at IS NULL OR lease_expires_at <= $4)
		RETURNING `+deletionColumns,
		tenantID, operationID, owner, now, lease.Seconds()))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.DeletionOperation{}, false, nil
	}
	if err != nil {
		return model.DeletionOperation{}, false, fmt.Errorf("claim deletion: %w", err)
	}
	return op, true, nil
}

func (r *DeletionRepository) ExpediteProtected(ctx context.Context, tenantID, holdID string, now time.Time) (int, error) {
	// The hold must be released; the scope test is the one Start uses.
	tag, err := r.db.Pool.Exec(ctx, `
		UPDATE deletion_operations AS d
		SET next_attempt_at = $3, updated_at = $3
		FROM assets AS a, legal_holds AS h
		WHERE h.tenant_id = $1 AND h.id = $2 AND h.released_at IS NOT NULL
		  AND d.tenant_id = h.tenant_id AND d.state = 'PROTECTED' AND d.next_attempt_at > $3
		  AND a.tenant_id = d.tenant_id AND a.id = d.asset_id
		  AND (h.scope = 'TENANT'
		       OR (h.scope = 'DATASET' AND h.scope_key = COALESCE(a.dataset, ''))
		       OR (h.scope = 'ASSET' AND h.scope_key = a.id))
	`, tenantID, holdID, now)
	if err != nil {
		return 0, fmt.Errorf("expedite protected deletions: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

func (r *DeletionRepository) Protect(ctx context.Context, operationID, owner string, now, next time.Time, reason string) error {
	return r.leased(ctx, `
		UPDATE deletion_operations
		SET state = 'PROTECTED', protected_reason = $5, next_attempt_at = $4,
		    lease_owner = NULL, lease_expires_at = NULL, updated_at = $3
		WHERE id = $1 AND lease_owner = $2 AND lease_expires_at > $3
		  AND state IN ('SCHEDULED', 'PROTECTED', 'RETRY_WAIT')
	`, operationID, owner, now, next, reason)
}

func (r *DeletionRepository) Wait(ctx context.Context, operationID, owner string, now, next time.Time, reason string) error {
	return r.leased(ctx, `
		UPDATE deletion_operations
		SET state = 'RETRY_WAIT', attempt_count = GREATEST(attempt_count - 1, 0),
		    next_attempt_at = $4, last_error = $5,
		    lease_owner = NULL, lease_expires_at = NULL, updated_at = $3
		WHERE id = $1 AND lease_owner = $2 AND lease_expires_at > $3 AND state = 'IN_PROGRESS'
	`, operationID, owner, now, next, reason)
}

func (r *DeletionRepository) Start(ctx context.Context, operationID, owner string, now time.Time) (bool, error) {
	// A hold committed before this statement, or a restore, stops it.
	tag, err := r.db.Pool.Exec(ctx, `
		UPDATE deletion_operations AS d
		SET state = 'IN_PROGRESS', attempt_count = d.attempt_count + 1, protected_reason = NULL,
		    next_attempt_at = $3, updated_at = $3
		WHERE d.id = $1 AND d.lease_owner = $2 AND d.lease_expires_at > $3
		  AND d.state IN ('SCHEDULED', 'PROTECTED', 'RETRY_WAIT')
		  AND EXISTS (
			SELECT 1 FROM assets AS a
			WHERE a.id = d.asset_id AND a.tenant_id = d.tenant_id AND a.status = 'SOFT_DELETED'
			  AND NOT EXISTS (
				SELECT 1 FROM legal_holds AS h
				WHERE h.tenant_id = a.tenant_id AND h.released_at IS NULL
				  AND (h.scope = 'TENANT'
				       OR (h.scope = 'DATASET' AND h.scope_key = COALESCE(a.dataset, ''))
				       OR (h.scope = 'ASSET' AND h.scope_key = a.id))
			  )
		  )
	`, operationID, owner, now)
	if err != nil {
		return false, fmt.Errorf("start deletion: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return true, nil
	}

	// Not started: is the lease still ours (a hold or restore stopped it),
	// or is it gone?
	var state, leaseOwner string
	var leaseExpires *time.Time
	err = r.db.Pool.QueryRow(ctx, `
		SELECT state, COALESCE(lease_owner, ''), lease_expires_at FROM deletion_operations WHERE id = $1
	`, operationID).Scan(&state, &leaseOwner, &leaseExpires)
	if err != nil {
		return false, fmt.Errorf("read deletion: %w", err)
	}
	if leaseOwner == owner && leaseExpires != nil && leaseExpires.After(now) {
		return false, nil
	}
	return false, model.ErrLeaseLost
}

func (r *DeletionRepository) MarkStorageDeleted(ctx context.Context, operationID, owner string, now time.Time) error {
	return r.leased(ctx, `
		UPDATE deletion_operations
		SET state = 'STORAGE_DELETED', storage_deleted_at = $3, last_error = NULL, updated_at = $3
		WHERE id = $1 AND lease_owner = $2 AND lease_expires_at > $3 AND state = 'IN_PROGRESS'
	`, operationID, owner, now)
}

func (r *DeletionRepository) Complete(ctx context.Context, operationID, owner string, now time.Time, event model.LifecycleEvent) error {
	tx, err := r.db.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin deletion completion: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	var tenantID, assetID string
	err = tx.QueryRow(ctx, `
		UPDATE deletion_operations
		SET state = 'COMPLETED', completed_at = $3, lease_owner = NULL, lease_expires_at = NULL, updated_at = $3
		WHERE id = $1 AND lease_owner = $2 AND lease_expires_at > $3 AND state = 'STORAGE_DELETED'
		RETURNING tenant_id, asset_id
	`, operationID, owner, now).Scan(&tenantID, &assetID)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.ErrLeaseLost
	}
	if err != nil {
		return fmt.Errorf("complete deletion: %w", err)
	}

	// The asset row stays as a tombstone: proof of the deletion and of
	// whom the asset belonged to.
	var sizeBytes int64
	err = tx.QueryRow(ctx, `
		UPDATE assets SET status = 'DELETED', updated_at = $3
		WHERE tenant_id = $1 AND id = $2 AND status = 'SOFT_DELETED'
		RETURNING size_bytes
	`, tenantID, assetID, now).Scan(&sizeBytes)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("tombstone asset: %w", err)
	}
	if err := addUsage(ctx, tx, tenantID, usageDelta{pending: -sizeBytes}, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE download_tokens SET revoked_at = $3
		WHERE tenant_id = $1 AND asset_id = $2 AND revoked_at IS NULL
	`, tenantID, assetID, now); err != nil {
		return fmt.Errorf("revoke download links: %w", err)
	}
	if err := insertEvent(ctx, tx, event); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit deletion completion: %w", err)
	}
	return nil
}

// Retry releases the operation until next. Only a started deletion becomes
// RETRY_WAIT; one that failed before starting keeps its state, so it stays
// restorable.
func (r *DeletionRepository) Retry(ctx context.Context, operationID, owner string, now, next time.Time, lastError string) error {
	return r.leased(ctx, `
		UPDATE deletion_operations
		SET state = CASE WHEN state = 'IN_PROGRESS' THEN 'RETRY_WAIT' ELSE state END,
		    next_attempt_at = $4, last_error = NULLIF($5, ''),
		    lease_owner = NULL, lease_expires_at = NULL, updated_at = $3
		WHERE id = $1 AND lease_owner = $2 AND lease_expires_at > $3 AND state IN `+liveStates,
		operationID, owner, now, next, lastError)
}

func (r *DeletionRepository) Fail(ctx context.Context, operationID, owner string, now time.Time, lastError string, event model.LifecycleEvent) error {
	tx, err := r.db.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin deletion failure: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	tag, err := tx.Exec(ctx, `
		UPDATE deletion_operations
		SET state = 'FAILED', last_error = NULLIF($4, ''), lease_owner = NULL, lease_expires_at = NULL, updated_at = $3
		WHERE id = $1 AND lease_owner = $2 AND lease_expires_at > $3 AND state IN `+liveStates,
		operationID, owner, now, lastError)
	if err != nil {
		return fmt.Errorf("fail deletion: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return model.ErrLeaseLost
	}
	if err := insertEvent(ctx, tx, event); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *DeletionRepository) Cancel(ctx context.Context, operationID, owner string, now time.Time, reason string) error {
	return r.leased(ctx, `
		UPDATE deletion_operations
		SET state = 'CANCELLED', last_error = NULLIF($4, ''), completed_at = $3,
		    lease_owner = NULL, lease_expires_at = NULL, updated_at = $3
		WHERE id = $1 AND lease_owner = $2 AND lease_expires_at > $3
		  AND state IN ('SCHEDULED', 'PROTECTED', 'RETRY_WAIT')
	`, operationID, owner, now, reason)
}

func (r *DeletionRepository) Requeue(ctx context.Context, tenantID, operationID string, now time.Time) (model.DeletionOperation, error) {
	op, err := scanDeletion(r.db.Pool.QueryRow(ctx, `
		UPDATE deletion_operations
		SET state = 'RETRY_WAIT', attempt_count = 0, next_attempt_at = $3, last_error = NULL, updated_at = $3
		WHERE tenant_id = $1 AND id = $2 AND state = 'FAILED'
		RETURNING `+deletionColumns,
		tenantID, operationID, now))
	if errors.Is(err, pgx.ErrNoRows) {
		if _, getErr := r.Get(ctx, tenantID, operationID); getErr != nil {
			return model.DeletionOperation{}, getErr
		}
		return model.DeletionOperation{}, model.ErrInvalidAssetState
	}
	if err != nil {
		return model.DeletionOperation{}, fmt.Errorf("requeue deletion: %w", err)
	}
	return op, nil
}

func (r *DeletionRepository) References(ctx context.Context, asset model.Asset, keys []string) ([]string, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT 'upload ' || id || ' is ' || status
		FROM upload_sessions
		WHERE object_key = ANY($4)
		  AND status IN ('PENDING', 'UPLOADING', 'COMPLETING', 'ABORTING')
		  AND NOT (tenant_id = $1 AND id = $3)
		UNION ALL
		SELECT 'processing job ' || id || ' is active'
		FROM processing_jobs
		WHERE promotion_key = ANY($4) AND state = 'PENDING' AND asset_id <> $2
		UNION ALL
		SELECT 'asset ' || id || ' is ' || status
		FROM assets
		WHERE object_key = ANY($4) AND id <> $2 AND status <> 'DELETED'
		UNION ALL
		SELECT 'quarantine hold ' || id
		FROM quarantine_records
		WHERE object_key = ANY($4) AND status = 'HELD' AND asset_id <> $2
		UNION ALL
		SELECT 'catalog object ' || id || ' has it as its current version'
		FROM catalog_objects
		WHERE tenant_id = $1 AND current_asset_id = $2
	`, asset.TenantID, asset.ID, asset.UploadID, keys)
	if err != nil {
		return nil, fmt.Errorf("find references: %w", err)
	}
	defer rows.Close()

	var refs []string
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			return nil, fmt.Errorf("scan reference: %w", err)
		}
		refs = append(refs, ref)
	}
	return refs, rows.Err()
}

// leased runs an update that must hit exactly the leased operation.
func (r *DeletionRepository) leased(ctx context.Context, sql string, args ...any) error {
	tag, err := r.db.Pool.Exec(ctx, sql, args...)
	if err != nil {
		return fmt.Errorf("update deletion: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return model.ErrLeaseLost
	}
	return nil
}

func (r *DeletionRepository) query(ctx context.Context, sql string, args ...any) ([]model.DeletionOperation, error) {
	rows, err := r.db.Pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("query deletions: %w", err)
	}
	defer rows.Close()

	var ops []model.DeletionOperation
	for rows.Next() {
		op, err := scanDeletion(rows)
		if err != nil {
			return nil, fmt.Errorf("scan deletion: %w", err)
		}
		ops = append(ops, op)
	}
	return ops, rows.Err()
}
