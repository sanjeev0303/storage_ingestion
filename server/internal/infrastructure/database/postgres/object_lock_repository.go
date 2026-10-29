package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// ObjectLockRepository records what Object Lock settings were applied to
// assets' stored versions.
type ObjectLockRepository struct {
	db *DB
}

func NewObjectLockRepository(db *DB) *ObjectLockRepository {
	return &ObjectLockRepository{db: db}
}

func (r *ObjectLockRepository) GetLock(ctx context.Context, tenantID, assetID string) (model.ObjectLock, bool, error) {
	l := model.ObjectLock{AssetID: assetID, TenantID: tenantID}
	err := r.db.Pool.QueryRow(ctx, `
		SELECT retain_until, legal_hold, applied_at FROM object_locks WHERE tenant_id = $1 AND asset_id = $2`,
		tenantID, assetID).Scan(&l.RetainUntil, &l.LegalHold, &l.AppliedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return l, false, nil
	}
	if err != nil {
		return l, false, fmt.Errorf("read object lock: %w", err)
	}
	return l, true, nil
}

func (r *ObjectLockRepository) PutLock(ctx context.Context, l model.ObjectLock) error {
	_, err := r.db.Pool.Exec(ctx, `
		INSERT INTO object_locks (asset_id, tenant_id, retain_until, legal_hold, applied_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (asset_id) DO UPDATE SET retain_until = $3, legal_hold = $4, applied_at = $5`,
		l.AssetID, l.TenantID, l.RetainUntil, l.LegalHold, l.AppliedAt)
	if err != nil {
		return fmt.Errorf("record object lock: %w", err)
	}
	return nil
}
