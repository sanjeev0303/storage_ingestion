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

	metadata, err := encodeMetadata(asset.Metadata)
	if err != nil {
		return err
	}
	_, err = r.db.Pool.Exec(ctx, `
		INSERT INTO assets (
			id, tenant_id, owner_id, upload_id, object_key, file_name,
			content_type, size_bytes, checksum_sha256, status,
			rejection_reason, created_at, updated_at, dataset, object_id, metadata,
			storage_connection_id, staging_connection_id
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NULLIF($9, ''), $10, NULLIF($11, ''), $12, $13, NULLIF($14, ''),
		        NULLIF($15, ''), $16, COALESCE(NULLIF($17, ''), 'platform'), COALESCE(NULLIF($18, ''), 'platform'))
	`,
		asset.ID, asset.TenantID, asset.OwnerID, asset.UploadID, asset.ObjectKey, asset.FileName,
		asset.ContentType, asset.SizeBytes, asset.ChecksumSHA256, string(asset.Status),
		asset.RejectionReason, asset.CreatedAt, asset.UpdatedAt, asset.Dataset, asset.ObjectID, metadata,
		asset.StorageConnectionID, asset.StagingConnectionID,
	)
	if err != nil {
		return fmt.Errorf("insert asset: %w", err)
	}
	return nil
}

const assetColumns = `
	id, tenant_id, owner_id, upload_id, object_key, file_name,
	content_type, size_bytes, COALESCE(checksum_sha256, ''), status,
	COALESCE(rejection_reason, ''), COALESCE(storage_etag, ''), COALESCE(storage_version_id, ''),
	published_at, created_at, updated_at, COALESCE(dataset, ''),
	COALESCE(object_id, ''), COALESCE(version, 0), metadata, storage_connection_id, staging_connection_id,
	COALESCE(shared_from, ''), COALESCE(blocked_reason, ''), COALESCE(adoption, ''), COALESCE(classification, ''),
	storage_tier, tiered_at, readable_until
`

func scanAsset(row pgx.Row) (*model.Asset, error) {
	return scanAssetWith(row)
}

// scanAssetWith scans assetColumns and then the extra columns after them.
func scanAssetWith(row pgx.Row, extra ...any) (*model.Asset, error) {
	var asset model.Asset
	var status, tier string
	var metadata []byte
	err := row.Scan(append([]any{
		&asset.ID, &asset.TenantID, &asset.OwnerID, &asset.UploadID, &asset.ObjectKey, &asset.FileName,
		&asset.ContentType, &asset.SizeBytes, &asset.ChecksumSHA256, &status,
		&asset.RejectionReason, &asset.StorageETag, &asset.StorageVersionID,
		&asset.PublishedAt, &asset.CreatedAt, &asset.UpdatedAt, &asset.Dataset,
		&asset.ObjectID, &asset.Version, &metadata, &asset.StorageConnectionID, &asset.StagingConnectionID,
		&asset.SharedFrom, &asset.BlockedReason, &asset.Adoption, &asset.Classification,
		&tier, &asset.TieredAt, &asset.ReadableUntil,
	}, extra...)...)
	asset.StorageTier = model.StorageTier(tier)
	if err != nil {
		return nil, err
	}
	asset.Status = model.AssetStatus(status)
	if asset.Metadata, err = decodeMetadata(metadata); err != nil {
		return nil, err
	}
	return &asset, nil
}

func (r *AssetRepository) GetByID(ctx context.Context, tenantID string, assetID string) (*model.Asset, error) {
	asset, err := scanAsset(r.db.Pool.QueryRow(ctx, `
		SELECT `+assetColumns+`
		FROM assets
		WHERE tenant_id = $1 AND id = $2
	`, tenantID, assetID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, model.ErrAssetNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("query asset: %w", err)
	}
	return asset, nil
}

func (r *AssetRepository) GetByUpload(ctx context.Context, tenantID, uploadID string) (*model.Asset, error) {
	asset, err := scanAsset(r.db.Pool.QueryRow(ctx, `
		SELECT `+assetColumns+`
		FROM assets
		WHERE tenant_id = $1 AND upload_id = $2
	`, tenantID, uploadID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, model.ErrAssetNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("query asset by upload: %w", err)
	}
	return asset, nil
}

func (r *AssetRepository) ListAfter(ctx context.Context, afterID, tenantID string, statuses []model.AssetStatus, limit int) ([]model.Asset, error) {
	names := make([]string, 0, len(statuses))
	for _, status := range statuses {
		names = append(names, string(status))
	}

	rows, err := r.db.Pool.Query(ctx, `
		SELECT `+assetColumns+`
		FROM assets
		WHERE id > $1
		  AND ($2 = '' OR tenant_id = $2)
		  AND status = ANY($3)
		ORDER BY id
		LIMIT $4
	`, afterID, tenantID, names, limit)
	if err != nil {
		return nil, fmt.Errorf("list assets: %w", err)
	}
	defer rows.Close()

	var assets []model.Asset
	for rows.Next() {
		asset, err := scanAsset(rows)
		if err != nil {
			return nil, fmt.Errorf("scan asset: %w", err)
		}
		assets = append(assets, *asset)
	}
	return assets, rows.Err()
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

	changed := false
	err := withTx(ctx, r.db, func(tx pgx.Tx) error {
		var uploadID string
		var sizeBytes int64
		err := tx.QueryRow(ctx, `
			UPDATE assets
			SET status = $3,
			    rejection_reason = CASE WHEN $3 = 'REJECTED' THEN NULLIF($4, '') ELSE rejection_reason END,
			    object_key = COALESCE(NULLIF($5, ''), object_key),
			    published_at = CASE WHEN $3 = 'AVAILABLE' THEN $6 ELSE published_at END,
			    updated_at = $6
			WHERE tenant_id = $1
			  AND id = $2
			  AND status = ANY($7)
			RETURNING upload_id, size_bytes
		`, tenantID, assetID, string(to), rejectionReason, objectKey, at, allowed).Scan(&uploadID, &sizeBytes)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("update asset status: %w", err)
		}
		changed = true
		// The tenant's quota follows the asset in the same transaction.
		switch to {
		case model.AssetStatusAvailable:
			// The object first: lock order asset, object, session, usage.
			events, err := publishVersion(ctx, tx, tenantID, assetID, at)
			if err != nil {
				return err
			}
			if err := assetPublished(ctx, tx, tenantID, uploadID, sizeBytes, at); err != nil {
				return err
			}
			return appendAudits(ctx, tx, events)
		case model.AssetStatusRejected:
			return releaseReservation(ctx, tx, tenantID, uploadID, at)
		}
		return nil
	})
	if err != nil || changed {
		return err
	}

	if _, err := r.GetByID(ctx, tenantID, assetID); err != nil {
		return err
	}
	return model.ErrInvalidAssetState
}

func (r *AssetRepository) RecordStorageVersion(ctx context.Context, tenantID, assetID, etag, versionID string, at time.Time) error {
	tag, err := r.db.Pool.Exec(ctx, `
		UPDATE assets
		SET storage_etag = NULLIF($3, ''),
		    storage_version_id = NULLIF($4, ''),
		    updated_at = $5
		WHERE tenant_id = $1 AND id = $2 AND status = 'PROCESSING'
	`, tenantID, assetID, etag, versionID, at)
	if err != nil {
		return fmt.Errorf("record asset storage version: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	if _, err := r.GetByID(ctx, tenantID, assetID); err != nil {
		return err
	}
	return model.ErrInvalidAssetState
}
