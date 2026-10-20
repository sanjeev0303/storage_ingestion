package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sanjeev0303/object-storage-platform/internal/dto/event"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// Catalog reconciliation and maintenance queries. Every scan is a keyset
// page of one tenant; repairs change catalog rows only, under the same
// lock order as the asset transactions (assets, then the object).

var _ interfaces.CatalogMaintenance = (*CatalogRepository)(nil)

// CatalogTenantsAfter pages the tenants that have assets.
func (r *CatalogRepository) CatalogTenantsAfter(ctx context.Context, after string, limit int) ([]string, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT DISTINCT tenant_id FROM assets WHERE tenant_id > $1 ORDER BY tenant_id LIMIT $2
	`, after, limit)
	if err != nil {
		return nil, fmt.Errorf("list catalog tenants: %w", err)
	}
	return collect(rows, func(row pgx.Row) (string, error) {
		var id string
		return id, row.Scan(&id)
	})
}

// CheckObjects reads a page of a tenant's objects, in path order after
// (dataset, pathKey), with what their versions say about them.
func (r *CatalogRepository) CheckObjects(ctx context.Context, tenantID string, after model.CatalogPosition, limit int) ([]model.ObjectCheck, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT `+catalogColumns+`, COALESCE(a.status, ''),
		       o.current_asset_id IS NOT NULL AND (a.version IS DISTINCT FROM o.current_version
		           OR a.size_bytes IS DISTINCT FROM o.size_bytes OR a.content_type IS DISTINCT FROM o.content_type
		           OR a.checksum_sha256::text IS DISTINCT FROM o.checksum_sha256
		           OR a.storage_connection_id IS DISTINCT FROM o.storage_connection_id),
		       o.current_asset_id IS NULL AND EXISTS (
		           SELECT 1 FROM assets AS v WHERE v.tenant_id = o.tenant_id AND v.object_id = o.id
		             AND v.status = 'AVAILABLE' AND v.version IS NOT NULL),
		       EXISTS (
		           SELECT 1 FROM assets AS v WHERE v.tenant_id = o.tenant_id AND v.object_id = o.id
		             AND v.status = 'AVAILABLE' AND v.version IS NULL),
		       CASE WHEN o.versioning = 'DISABLED' THEN ARRAY(
		           SELECT v.id FROM assets AS v WHERE v.tenant_id = o.tenant_id AND v.object_id = o.id
		             AND v.status = 'AVAILABLE' AND v.version IS NOT NULL AND v.id <> COALESCE(o.current_asset_id, ''))
		       ELSE ARRAY[]::text[] END
		FROM catalog_objects AS o
		LEFT JOIN assets AS a ON a.id = o.current_asset_id AND a.tenant_id = o.tenant_id
		WHERE o.tenant_id = $1 AND (o.dataset, o.path_key) > ($2, $3)
		ORDER BY o.dataset, o.path_key
		LIMIT $4
	`, tenantID, after.Dataset, after.PathKey, limit)
	if err != nil {
		return nil, fmt.Errorf("check catalog objects: %w", err)
	}
	return collect(rows, func(row pgx.Row) (model.ObjectCheck, error) {
		var c model.ObjectCheck
		var versioning string
		var metadata []byte
		o := &c.Object
		err := row.Scan(&o.ID, &o.TenantID, &o.Dataset, &o.Path, &o.PathKey, &versioning, &o.Revision, &metadata,
			&o.CurrentAssetID, &o.CurrentVersion, &o.SizeBytes, &o.ContentType, &o.ChecksumSHA256, &o.PublishedAt,
			&o.Versions, &o.DeletedAt, &o.CreatedBy, &o.CreatedAt, &o.UpdatedAt, &o.StorageConnectionID,
			&c.CurrentStatus, &c.CurrentStale, &c.MissingCurrent, &c.Unnumbered, &c.ReplacedLive)
		o.Versioning = model.Versioning(versioning)
		return c, err
	})
}

// RepairCurrent numbers the object's unnumbered AVAILABLE versions in the
// order they were created, then keeps a valid current version (refreshing
// its copied metadata) or chooses the newest AVAILABLE one, or none.
func (r *CatalogRepository) RepairCurrent(ctx context.Context, tenantID, objectID string, at time.Time) error {
	return withTx(ctx, r.db, func(tx pgx.Tx) error {
		// The object's assets first, then the object: the lock order.
		rows, err := tx.Query(ctx, `
			SELECT id, status, version IS NULL FROM assets WHERE tenant_id = $1 AND object_id = $2
			ORDER BY created_at, id FOR UPDATE
		`, tenantID, objectID)
		if err != nil {
			return fmt.Errorf("lock versions: %w", err)
		}
		type version struct {
			id, status string
			unnumbered bool
		}
		versions, err := collect(rows, func(row pgx.Row) (version, error) {
			var v version
			return v, row.Scan(&v.id, &v.status, &v.unnumbered)
		})
		if err != nil {
			return err
		}
		var current string
		var next int
		if err := tx.QueryRow(ctx, `
			SELECT COALESCE(current_asset_id, ''), next_version FROM catalog_objects
			WHERE tenant_id = $1 AND id = $2 FOR UPDATE
		`, tenantID, objectID).Scan(&current, &next); err != nil {
			return fmt.Errorf("lock catalog object: %w", err)
		}

		numbered := 0
		for _, v := range versions {
			if v.unnumbered && v.status == string(model.AssetStatusAvailable) {
				if _, err := tx.Exec(ctx, `UPDATE assets SET version = $3 WHERE tenant_id = $1 AND id = $2`, tenantID, v.id, next); err != nil {
					return fmt.Errorf("number version: %w", err)
				}
				next++
				numbered++
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE catalog_objects SET next_version = $3 WHERE tenant_id = $1 AND id = $2`, tenantID, objectID, next); err != nil {
			return err
		}

		chosen := ""
		for _, v := range versions {
			if v.id == current && v.status == string(model.AssetStatusAvailable) {
				chosen = current
			}
		}
		if chosen == "" {
			err := tx.QueryRow(ctx, `
				SELECT id FROM assets WHERE tenant_id = $1 AND object_id = $2 AND status = 'AVAILABLE' AND version IS NOT NULL
				ORDER BY version DESC LIMIT 1
			`, tenantID, objectID).Scan(&chosen)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("find newest version: %w", err)
			}
		}
		if err := makeCurrent(ctx, tx, tenantID, objectID, chosen, at); err != nil {
			return err
		}
		_, err = appendAudit(ctx, tx, catalogEvent(tenantID, "catalog.reconciled", "catalog_object", objectID, at,
			map[string]any{"previous_asset_id": current, "current_asset_id": chosen, "numbered_versions": numbered}))
		return err
	})
}

// UncataloguedAssets pages a tenant's AVAILABLE assets that have no
// object: assets published before the catalog existed.
func (r *CatalogRepository) UncataloguedAssets(ctx context.Context, tenantID, afterID string, limit int) ([]model.Asset, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT `+assetColumns+` FROM assets
		WHERE tenant_id = $1 AND id > $2 AND object_id IS NULL AND status = 'AVAILABLE'
		ORDER BY id LIMIT $3
	`, tenantID, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("list uncatalogued assets: %w", err)
	}
	return collect(rows, func(row pgx.Row) (model.Asset, error) {
		a, err := scanAsset(row)
		if err != nil {
			return model.Asset{}, err
		}
		return *a, nil
	})
}

// AttachLegacy makes an uncatalogued AVAILABLE asset a version of the
// object at path in its dataset (created if needed). It becomes current
// when the object has no current version or a current one published
// earlier. false when the asset changed meanwhile.
func (r *CatalogRepository) AttachLegacy(ctx context.Context, asset model.Asset, path string, at time.Time) (bool, error) {
	attached := false
	err := withTx(ctx, r.db, func(tx pgx.Tx) error {
		var status string
		var objectID *string
		err := tx.QueryRow(ctx, `SELECT status, object_id FROM assets WHERE tenant_id = $1 AND id = $2 FOR UPDATE`,
			asset.TenantID, asset.ID).Scan(&status, &objectID)
		if err != nil {
			return fmt.Errorf("lock asset: %w", err)
		}
		if status != string(model.AssetStatusAvailable) || objectID != nil {
			return nil
		}
		key := model.PathKey(path)
		if _, err := tx.Exec(ctx, `
			INSERT INTO catalog_objects (id, tenant_id, dataset, path, path_key, created_by, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, 'catalog-backfill', $6, $6)
			ON CONFLICT (tenant_id, dataset, path_key) DO NOTHING
		`, event.NewID(), asset.TenantID, asset.Dataset, path, key, asset.CreatedAt); err != nil {
			return fmt.Errorf("create catalog object: %w", err)
		}
		var object string
		var next int
		var currentPublished *time.Time
		if err := tx.QueryRow(ctx, `
			SELECT o.id, o.next_version, a.published_at
			FROM catalog_objects AS o LEFT JOIN assets AS a ON a.id = o.current_asset_id AND a.tenant_id = o.tenant_id
			WHERE o.tenant_id = $1 AND o.dataset = $2 AND o.path_key = $3
			FOR UPDATE OF o
		`, asset.TenantID, asset.Dataset, key).Scan(&object, &next, &currentPublished); err != nil {
			return fmt.Errorf("lock catalog object: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE assets SET object_id = $3, version = $4 WHERE tenant_id = $1 AND id = $2`,
			asset.TenantID, asset.ID, object, next); err != nil {
			return fmt.Errorf("attach asset: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE catalog_objects SET next_version = $3 WHERE tenant_id = $1 AND id = $2`,
			asset.TenantID, object, next+1); err != nil {
			return err
		}
		newer := currentPublished == nil || asset.PublishedAt == nil || !asset.PublishedAt.Before(*currentPublished)
		var hasCurrent bool
		if err := tx.QueryRow(ctx, `SELECT current_asset_id IS NOT NULL FROM catalog_objects WHERE tenant_id = $1 AND id = $2`,
			asset.TenantID, object).Scan(&hasCurrent); err != nil {
			return err
		}
		if !hasCurrent || newer {
			if err := makeCurrent(ctx, tx, asset.TenantID, object, asset.ID, at); err != nil {
				return err
			}
		}
		attached = true
		_, err = appendAudit(ctx, tx, catalogEvent(asset.TenantID, "catalog.backfilled", "asset", asset.ID, at,
			map[string]any{"object_id": object, "version": next}))
		return err
	})
	return attached, err
}

// PublishedVersions pages a tenant's AVAILABLE catalogued versions, for
// checks against storage.
func (r *CatalogRepository) PublishedVersions(ctx context.Context, tenantID, afterID string, limit int) ([]model.Asset, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT `+assetColumns+` FROM assets
		WHERE tenant_id = $1 AND id > $2 AND status = 'AVAILABLE' AND object_id IS NOT NULL
		ORDER BY id LIMIT $3
	`, tenantID, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("list published versions: %w", err)
	}
	return collect(rows, func(row pgx.Row) (model.Asset, error) {
		a, err := scanAsset(row)
		if err != nil {
			return model.Asset{}, err
		}
		return *a, nil
	})
}

// ExistingAssets returns which of ids are assets of the tenant, in any
// status (tombstones included: a tombstone's bytes are the lifecycle's).
func (r *CatalogRepository) ExistingAssets(ctx context.Context, tenantID string, ids []string) (map[string]bool, error) {
	rows, err := r.db.Pool.Query(ctx, `SELECT id FROM assets WHERE tenant_id = $1 AND id = ANY($2)`, tenantID, ids)
	if err != nil {
		return nil, fmt.Errorf("find assets: %w", err)
	}
	found, err := collect(rows, func(row pgx.Row) (string, error) {
		var id string
		return id, row.Scan(&id)
	})
	out := make(map[string]bool, len(found))
	for _, id := range found {
		out[id] = true
	}
	return out, err
}

// StaleUploads counts a tenant's completed uploads older than before
// that never became assets.
func (r *CatalogRepository) StaleUploads(ctx context.Context, tenantID string, before time.Time) (int, error) {
	var n int
	err := r.db.Pool.QueryRow(ctx, `
		SELECT count(*) FROM upload_sessions AS s
		WHERE s.tenant_id = $1 AND s.status IN ('COMPLETED', 'VERIFIED') AND s.updated_at < $2
		  AND NOT EXISTS (SELECT 1 FROM assets AS a WHERE a.upload_id = s.id)
	`, tenantID, before).Scan(&n)
	return n, err
}

// PathIndexValid reports whether the unique path index is valid: an
// interrupted concurrent rebuild leaves it invalid, and then duplicates
// are possible.
func (r *CatalogRepository) PathIndexValid(ctx context.Context) (bool, error) {
	var valid bool
	err := r.db.Pool.QueryRow(ctx, `
		SELECT indisvalid FROM pg_index WHERE indexrelid = 'ux_catalog_objects_path'::regclass
	`).Scan(&valid)
	return valid, err
}

// Stats sizes the catalog: counts, and the bytes of its table and the
// assets table with their indexes.
func (r *CatalogRepository) Stats(ctx context.Context) (model.CatalogStats, error) {
	s := model.CatalogStats{TableBytes: map[string]int64{}, IndexBytes: map[string]int64{}}
	err := r.db.Pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM catalog_objects),
		       (SELECT count(*) FROM catalog_objects WHERE current_asset_id IS NOT NULL),
		       (SELECT count(*) FROM assets WHERE version IS NOT NULL)
	`).Scan(&s.Objects, &s.PublishedObjects, &s.Versions)
	if err != nil {
		return s, fmt.Errorf("count catalog: %w", err)
	}
	rows, err := r.db.Pool.Query(ctx, `
		SELECT c.relname, c.relkind::text, pg_relation_size(c.oid)
		FROM pg_class AS c
		WHERE c.relname IN ('catalog_objects', 'assets')
		   OR c.oid IN (SELECT indexrelid FROM pg_index WHERE indrelid IN ('catalog_objects'::regclass, 'assets'::regclass))
	`)
	if err != nil {
		return s, fmt.Errorf("size catalog: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name, kind string
		var size int64
		if err := rows.Scan(&name, &kind, &size); err != nil {
			return s, err
		}
		if kind == "i" {
			s.IndexBytes[name] = size
		} else {
			s.TableBytes[name] = size
		}
	}
	return s, rows.Err()
}

// isQueryTimeout reports a statement cancelled by statement_timeout.
func isQueryTimeout(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "57014"
}
