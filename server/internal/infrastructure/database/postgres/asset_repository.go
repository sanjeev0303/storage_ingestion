package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// AssetRepository stores assets in Neon PostgreSQL.
type AssetRepository struct {
	db *DB
}

var _ interfaces.AssetRepository = (*AssetRepository)(nil)

func NewAssetRepository(db *DB) *AssetRepository {
	return &AssetRepository{db: db}
}

func (r *AssetRepository) Create(ctx context.Context, asset *model.Asset) error {
	if asset == nil {
		return errors.New("asset must not be nil")
	}
	if err := asset.Validate(); err != nil {
		return err
	}

	_, err := r.db.Pool.Exec(ctx, `
		INSERT INTO assets (
			id, tenant_id, owner_id, upload_id, object_key, file_name,
			content_type, size_bytes, checksum_sha256, status,
			rejection_reason, created_at, updated_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NULLIF($9, ''), $10, NULLIF($11, ''), $12, $13)
	`,
		asset.ID, asset.TenantID, asset.OwnerID, asset.UploadID, asset.ObjectKey, asset.FileName,
		asset.ContentType, asset.SizeBytes, asset.ChecksumSHA256, string(asset.Status),
		asset.RejectionReason, asset.CreatedAt, asset.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert asset: %w", err)
	}
	return nil
}

func (r *AssetRepository) GetByID(ctx context.Context, tenantID string, assetID string) (*model.Asset, error) {
	var asset model.Asset
	var status string

	err := r.db.Pool.QueryRow(ctx, `
		SELECT id, tenant_id, owner_id, upload_id, object_key, file_name,
		       content_type, size_bytes, COALESCE(checksum_sha256, ''), status,
		       COALESCE(rejection_reason, ''), created_at, updated_at
		FROM assets
		WHERE tenant_id = $1 AND id = $2
	`, tenantID, assetID).Scan(
		&asset.ID, &asset.TenantID, &asset.OwnerID, &asset.UploadID, &asset.ObjectKey, &asset.FileName,
		&asset.ContentType, &asset.SizeBytes, &asset.ChecksumSHA256, &status,
		&asset.RejectionReason, &asset.CreatedAt, &asset.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, model.ErrAssetNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("query asset: %w", err)
	}

	asset.Status = model.AssetStatus(status)
	return &asset, nil
}

func (r *AssetRepository) TransitionStatus(
	ctx context.Context,
	tenantID string,
	assetID string,
	from []model.AssetStatus,
	to model.AssetStatus,
	rejectionReason string,
	objectKey string,
	at time.Time,
) error {
	allowed := make([]string, 0, len(from))
	for _, status := range from {
		if model.CanTransitionAsset(status, to) {
			allowed = append(allowed, string(status))
		}
	}
	if len(allowed) == 0 {
		return fmt.Errorf("%w: no allowed transition to %s", model.ErrInvalidAssetState, to)
	}

	tag, err := r.db.Pool.Exec(ctx, `
		UPDATE assets
		SET status = $3,
		    rejection_reason = CASE WHEN $3 = 'REJECTED' THEN NULLIF($4, '') ELSE rejection_reason END,
		    object_key = COALESCE(NULLIF($5, ''), object_key),
		    updated_at = $6
		WHERE tenant_id = $1
		  AND id = $2
		  AND status = ANY($7)
	`, tenantID, assetID, string(to), rejectionReason, objectKey, at, allowed)
	if err != nil {
		return fmt.Errorf("update asset status: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}

	if _, err := r.GetByID(ctx, tenantID, assetID); err != nil {
		return err
	}
	return model.ErrInvalidAssetState
}
