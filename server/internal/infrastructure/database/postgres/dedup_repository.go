package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// Content deduplication (migration 000024): identical bytes of one
// organization on one connection are stored once.

// FindTwin returns the oldest available asset of the tenant, on the same
// connection, with the same SHA-256 and size, other than exceptID.
func (r *AssetRepository) FindTwin(ctx context.Context, tenantID, connectionID, sha256 string, size int64, exceptID string) (model.Asset, bool, error) {
	asset, err := scanAsset(r.db.Pool.QueryRow(ctx, `
		SELECT `+assetColumns+` FROM assets
		WHERE tenant_id = $1 AND checksum_sha256 = $3 AND size_bytes = $4 AND id <> $5
		  AND status = 'AVAILABLE' AND storage_connection_id = $2
		ORDER BY created_at, id
		LIMIT 1`, tenantID, connectionID, sha256, size, exceptID))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Asset{}, false, nil
	}
	if err != nil {
		return model.Asset{}, false, fmt.Errorf("find identical asset: %w", err)
	}
	return *asset, true, nil
}

// OlderTwinPending reports whether an older asset of the tenant, on the
// same connection and with the same verified bytes, is still being
// promoted.
func (r *AssetRepository) OlderTwinPending(ctx context.Context, asset model.Asset) (bool, error) {
	var pending bool
	err := r.db.Pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM assets
			WHERE tenant_id = $1 AND checksum_sha256 = $3 AND size_bytes = $4 AND storage_connection_id = $2
			  AND status = 'PROCESSING' AND (created_at, id) < ($5, $6))`,
		asset.TenantID, asset.StorageConnectionID, asset.ChecksumSHA256, asset.SizeBytes, asset.CreatedAt, asset.ID).Scan(&pending)
	if err != nil {
		return false, fmt.Errorf("check identical asset in promotion: %w", err)
	}
	return pending, nil
}

// PublishShared publishes a PROCESSING asset pointing at twin's stored
// object, in one transaction that also checks the twin is still available
// under that exact object, and does what any publication does (the
// catalog's current version, the quota ledger). It answers false, and
// changes nothing, when the twin no longer qualifies; the caller copies
// instead.
func (r *AssetRepository) PublishShared(ctx context.Context, tenantID, assetID string, twin model.Asset, at time.Time) (bool, error) {
	// shared_from names the asset that first stored the bytes, whose ID is
	// in the key, even when the twin found is itself a sharer.
	owner := twin.ID
	if twin.SharedFrom != "" {
		owner = twin.SharedFrom
	}
	shared := false
	err := withTx(ctx, r.db, func(tx pgx.Tx) error {
		var ok bool
		err := tx.QueryRow(ctx, `
			SELECT true FROM assets
			WHERE tenant_id = $1 AND id = $2 AND status = 'AVAILABLE' AND object_key = $3
			  AND COALESCE(storage_etag, '') = $4 AND COALESCE(storage_version_id, '') = $5
			FOR SHARE`, tenantID, twin.ID, twin.ObjectKey, twin.StorageETag, twin.StorageVersionID).Scan(&ok)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("lock identical asset: %w", err)
		}
		var uploadID string
		var sizeBytes int64
		err = tx.QueryRow(ctx, `
			UPDATE assets
			SET status = 'AVAILABLE', object_key = $3, storage_etag = NULLIF($4, ''), storage_version_id = NULLIF($5, ''),
			    shared_from = $6, published_at = $7, updated_at = $7
			WHERE tenant_id = $1 AND id = $2 AND status = 'PROCESSING'
			RETURNING upload_id, size_bytes`,
			tenantID, assetID, twin.ObjectKey, twin.StorageETag, twin.StorageVersionID, owner, at).Scan(&uploadID, &sizeBytes)
		if errors.Is(err, pgx.ErrNoRows) {
			return model.ErrInvalidAssetState
		}
		if err != nil {
			return fmt.Errorf("publish shared asset: %w", err)
		}
		shared = true
		events, err := publishVersion(ctx, tx, tenantID, assetID, at)
		if err != nil {
			return err
		}
		if err := assetPublished(ctx, tx, tenantID, uploadID, sizeBytes, at); err != nil {
			return err
		}
		return appendAudits(ctx, tx, events)
	})
	return shared, err
}

// KeyInUse names another asset, not permanently deleted, that points at
// objectKey: its bytes must stay.
func (r *AssetRepository) KeyInUse(ctx context.Context, tenantID, objectKey, exceptID string) (string, error) {
	var id string
	// An asset whose own deletion has started (no longer restorable) does
	// not keep the bytes: when two sharers are deleted together, each
	// would otherwise leave them to the other and neither remove them.
	err := r.db.Pool.QueryRow(ctx, `
		SELECT a.id FROM assets a WHERE a.tenant_id = $1 AND a.object_key = $2 AND a.id <> $3 AND a.status <> 'DELETED'
		  AND NOT EXISTS (SELECT 1 FROM deletion_operations d WHERE d.tenant_id = a.tenant_id AND d.asset_id = a.id
		                  AND d.state IN ('IN_PROGRESS', 'STORAGE_DELETED'))
		LIMIT 1`,
		tenantID, objectKey, exceptID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("check object key use: %w", err)
	}
	return id, nil
}
