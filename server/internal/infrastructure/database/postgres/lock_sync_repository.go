package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// LockSyncRepository queues the scopes whose retention policy or holds
// changed (migration 000028), for the cleanup worker to lock first.
type LockSyncRepository struct {
	db *DB
}

func NewLockSyncRepository(db *DB) *LockSyncRepository {
	return &LockSyncRepository{db: db}
}

func (r *LockSyncRepository) Request(ctx context.Context, req model.LockSyncRequest) error {
	_, err := r.db.Pool.Exec(ctx, `
		INSERT INTO lock_sync_requests (id, tenant_id, scope, scope_key, reason, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $6)`,
		req.ID, req.TenantID, req.Scope, req.ScopeKey, req.Reason, req.CreatedAt)
	if err != nil {
		return fmt.Errorf("queue lock sync: %w", err)
	}
	return nil
}

// Next is the oldest unfinished request, of one tenant when tenantID is
// set.
func (r *LockSyncRepository) Next(ctx context.Context, tenantID string) (model.LockSyncRequest, bool, error) {
	var req model.LockSyncRequest
	err := r.db.Pool.QueryRow(ctx, `
		SELECT id, tenant_id, scope, scope_key, position, reason, created_at FROM lock_sync_requests
		WHERE done_at IS NULL AND ($1 = '' OR tenant_id = $1)
		ORDER BY created_at, id LIMIT 1`, tenantID).
		Scan(&req.ID, &req.TenantID, &req.Scope, &req.ScopeKey, &req.Position, &req.Reason, &req.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return req, false, nil
	}
	if err != nil {
		return req, false, fmt.Errorf("next lock sync: %w", err)
	}
	return req, true, nil
}

// Assets is the next batch of available assets in the request's scope,
// after its position.
func (r *LockSyncRepository) Assets(ctx context.Context, req model.LockSyncRequest, limit int) ([]model.Asset, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT `+assetColumns+` FROM assets
		WHERE tenant_id = $1 AND status = 'AVAILABLE' AND id > $2
		  AND CASE $3 WHEN 'TENANT' THEN true WHEN 'DATASET' THEN dataset = $4 ELSE id = $4 END
		ORDER BY id LIMIT $5`, req.TenantID, req.Position, req.Scope, req.ScopeKey, limit)
	if err != nil {
		return nil, fmt.Errorf("list assets to lock: %w", err)
	}
	found, err := collect(rows, scanAsset)
	if err != nil {
		return nil, err
	}
	out := make([]model.Asset, len(found))
	for i, a := range found {
		out[i] = *a
	}
	return out, nil
}

func (r *LockSyncRepository) Advance(ctx context.Context, id, position string, done bool, at time.Time) error {
	_, err := r.db.Pool.Exec(ctx, `
		UPDATE lock_sync_requests SET position = $2, updated_at = $3::timestamptz, done_at = CASE WHEN $4::boolean THEN $3::timestamptz END
		WHERE id = $1`, id, position, at, done)
	if err != nil {
		return fmt.Errorf("advance lock sync: %w", err)
	}
	return nil
}
