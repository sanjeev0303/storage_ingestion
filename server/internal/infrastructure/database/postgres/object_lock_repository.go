package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

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
		SELECT retain_until, legal_hold, applied_at, COALESCE(mode, ''), verified_at, COALESCE(verify_result, '')
		FROM object_locks WHERE tenant_id = $1 AND asset_id = $2`,
		tenantID, assetID).Scan(&l.RetainUntil, &l.LegalHold, &l.AppliedAt, &l.Mode, &l.VerifiedAt, &l.VerifyResult)
	if errors.Is(err, pgx.ErrNoRows) {
		return l, false, nil
	}
	if err != nil {
		return l, false, fmt.Errorf("read object lock: %w", err)
	}
	return l, true, nil
}

// DueVerification lists locks still in force, those verified longest ago
// (or never) first.
func (r *ObjectLockRepository) DueVerification(ctx context.Context, now time.Time, limit int) ([]model.ObjectLock, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT asset_id, tenant_id, retain_until, legal_hold, applied_at, COALESCE(mode, ''), verified_at, COALESCE(verify_result, '')
		FROM object_locks
		WHERE legal_hold OR retain_until > $1
		ORDER BY verified_at NULLS FIRST, asset_id
		LIMIT $2`, now, limit)
	if err != nil {
		return nil, fmt.Errorf("list locks to verify: %w", err)
	}
	return collect(rows, func(row pgx.Row) (model.ObjectLock, error) {
		var l model.ObjectLock
		err := row.Scan(&l.AssetID, &l.TenantID, &l.RetainUntil, &l.LegalHold, &l.AppliedAt, &l.Mode, &l.VerifiedAt, &l.VerifyResult)
		return l, err
	})
}

// RecordVerification keeps a read-back's outcome.
func (r *ObjectLockRepository) RecordVerification(ctx context.Context, assetID, result string, at time.Time) error {
	if _, err := r.db.Pool.Exec(ctx, `UPDATE object_locks SET verified_at = $2, verify_result = $3 WHERE asset_id = $1`,
		assetID, at, result); err != nil {
		return fmt.Errorf("record lock verification: %w", err)
	}
	return nil
}

func (r *ObjectLockRepository) PutLock(ctx context.Context, l model.ObjectLock) error {
	_, err := r.db.Pool.Exec(ctx, `
		INSERT INTO object_locks (asset_id, tenant_id, retain_until, legal_hold, applied_at, mode)
		VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''))
		ON CONFLICT (asset_id) DO UPDATE SET retain_until = $3, legal_hold = $4, applied_at = $5, mode = NULLIF($6, '')`,
		l.AssetID, l.TenantID, l.RetainUntil, l.LegalHold, l.AppliedAt, l.Mode)
	if err != nil {
		return fmt.Errorf("record object lock: %w", err)
	}
	return nil
}
