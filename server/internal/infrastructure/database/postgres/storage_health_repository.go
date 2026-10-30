package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// Storage health (docs/storage-posture.md): a staged second credential,
// posture reports, where regulated datasets may be stored, and objects
// changed outside the platform.

// StageCredential stores a tested credential next to the current one.
func (r *StorageConnectionRepository) StageCredential(ctx context.Context, tenantID, connectionID string, sealed interfaces.SealedCredential, actor string, at time.Time) (model.StorageConnection, error) {
	c, err := scanConnection(r.db.Pool.QueryRow(ctx, `
		UPDATE storage_connections SET
		    staged_credential_version = $3, staged_key_version = $4, staged_nonce = $5, staged_ciphertext = $6,
		    staged_at = $8, staged_by = $7, revision = revision + 1, updated_by = $7, updated_at = $8
		WHERE tenant_id = $1 AND id = $2 AND status <> 'DELETED' AND credential_version < $3
		RETURNING `+connectionColumns,
		tenantID, connectionID, sealed.Version, sealed.KeyVersion, sealed.Nonce, sealed.Ciphertext, actor, at))
	if errors.Is(err, model.ErrConnectionNotFound) {
		return c, r.missed(ctx, tenantID, connectionID)
	}
	if err != nil {
		return c, fmt.Errorf("stage storage credential: %w", err)
	}
	return c, nil
}

// StagedCredential reads the staged credential's ciphertext.
func (r *StorageConnectionRepository) StagedCredential(ctx context.Context, tenantID, connectionID string) (interfaces.SealedCredential, error) {
	var sealed interfaces.SealedCredential
	err := r.db.Pool.QueryRow(ctx, `
		SELECT staged_credential_version, staged_key_version, staged_nonce, staged_ciphertext FROM storage_connections
		WHERE tenant_id = $1 AND id = $2 AND staged_ciphertext IS NOT NULL AND status <> 'DELETED'`,
		tenantID, connectionID).Scan(&sealed.Version, &sealed.KeyVersion, &sealed.Nonce, &sealed.Ciphertext)
	if errors.Is(err, pgx.ErrNoRows) {
		return sealed, model.ErrNoStagedCredential
	}
	if err != nil {
		return sealed, fmt.Errorf("read staged credential: %w", err)
	}
	return sealed, nil
}

// PromoteCredential makes the staged credential current and keeps the one
// it replaces until keepUntil, for a rollback. Every SET reads the row as
// it was, so the two swap in one statement.
func (r *StorageConnectionRepository) PromoteCredential(ctx context.Context, tenantID, connectionID, actor string, keepUntil, at time.Time) (model.StorageConnection, error) {
	c, err := scanConnection(r.db.Pool.QueryRow(ctx, `
		UPDATE storage_connections SET
		    previous_credential_version = credential_version, previous_key_version = credential_key_version,
		    previous_nonce = credential_nonce, previous_ciphertext = credential_ciphertext, previous_until = $4,
		    credential_version = staged_credential_version, credential_key_version = staged_key_version,
		    credential_nonce = staged_nonce, credential_ciphertext = staged_ciphertext, credential_rotated_at = $5,
		    staged_credential_version = NULL, staged_key_version = NULL, staged_nonce = NULL, staged_ciphertext = NULL,
		    staged_at = NULL, staged_by = NULL,
		    revision = revision + 1, updated_by = $3, updated_at = $5
		WHERE tenant_id = $1 AND id = $2 AND staged_ciphertext IS NOT NULL AND status <> 'DELETED'
		RETURNING `+connectionColumns,
		tenantID, connectionID, actor, keepUntil, at))
	if errors.Is(err, model.ErrConnectionNotFound) {
		if missing := r.missed(ctx, tenantID, connectionID); !errors.Is(missing, model.ErrConnectionNotFound) {
			return c, model.ErrNoStagedCredential
		}
	}
	if err != nil {
		return c, fmt.Errorf("promote storage credential: %w", err)
	}
	return c, nil
}

// RollbackCredential puts back the credential the last promotion
// replaced, while it is kept.
func (r *StorageConnectionRepository) RollbackCredential(ctx context.Context, tenantID, connectionID, actor string, at time.Time) (model.StorageConnection, error) {
	c, err := scanConnection(r.db.Pool.QueryRow(ctx, `
		UPDATE storage_connections SET
		    credential_version = previous_credential_version, credential_key_version = previous_key_version,
		    credential_nonce = previous_nonce, credential_ciphertext = previous_ciphertext, credential_rotated_at = $4,
		    previous_credential_version = NULL, previous_key_version = NULL, previous_nonce = NULL,
		    previous_ciphertext = NULL, previous_until = NULL,
		    revision = revision + 1, updated_by = $3, updated_at = $4
		WHERE tenant_id = $1 AND id = $2 AND previous_ciphertext IS NOT NULL AND previous_until > $4 AND status <> 'DELETED'
		RETURNING `+connectionColumns,
		tenantID, connectionID, actor, at))
	if errors.Is(err, model.ErrConnectionNotFound) {
		if missing := r.missed(ctx, tenantID, connectionID); !errors.Is(missing, model.ErrConnectionNotFound) {
			return c, model.ErrNoPreviousCredential
		}
	}
	if err != nil {
		return c, fmt.Errorf("roll back storage credential: %w", err)
	}
	return c, nil
}

// DiscardStaged drops a staged credential without using it.
func (r *StorageConnectionRepository) DiscardStaged(ctx context.Context, tenantID, connectionID, actor string, at time.Time) (model.StorageConnection, error) {
	c, err := scanConnection(r.db.Pool.QueryRow(ctx, `
		UPDATE storage_connections SET
		    staged_credential_version = NULL, staged_key_version = NULL, staged_nonce = NULL, staged_ciphertext = NULL,
		    staged_at = NULL, staged_by = NULL, revision = revision + 1, updated_by = $3, updated_at = $4
		WHERE tenant_id = $1 AND id = $2 AND staged_ciphertext IS NOT NULL AND status <> 'DELETED'
		RETURNING `+connectionColumns,
		tenantID, connectionID, actor, at))
	if errors.Is(err, model.ErrConnectionNotFound) {
		if missing := r.missed(ctx, tenantID, connectionID); !errors.Is(missing, model.ErrConnectionNotFound) {
			return c, model.ErrNoStagedCredential
		}
	}
	if err != nil {
		return c, fmt.Errorf("discard staged credential: %w", err)
	}
	return c, nil
}

// ExpirePrevious erases replaced credentials past their rollback window.
func (r *StorageConnectionRepository) ExpirePrevious(ctx context.Context, now time.Time) (int64, error) {
	tag, err := r.db.Pool.Exec(ctx, `
		UPDATE storage_connections SET previous_credential_version = NULL, previous_key_version = NULL,
		    previous_nonce = NULL, previous_ciphertext = NULL, previous_until = NULL
		WHERE previous_ciphertext IS NOT NULL AND previous_until <= $1`, now)
	if err != nil {
		return 0, fmt.Errorf("expire previous credentials: %w", err)
	}
	return tag.RowsAffected(), nil
}

// Posture reads a connection's latest posture report.
func (r *StorageConnectionRepository) Posture(ctx context.Context, tenantID, connectionID string) (model.PostureReport, bool, error) {
	report := model.PostureReport{ConnectionID: connectionID}
	var checks []byte
	err := r.db.Pool.QueryRow(ctx, `
		SELECT checked_at, worst, phi_safe, checks, lifecycle_hash, owner_account, location, location_verified, encryption_key
		FROM connection_posture WHERE tenant_id = $1 AND connection_id = $2`, tenantID, connectionID).
		Scan(&report.CheckedAt, &report.Worst, &report.PHISafe, &checks, &report.LifecycleHash, &report.OwnerAccount,
			&report.Location, &report.LocationVerified, &report.EncryptionKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return report, false, nil
	}
	if err != nil {
		return report, false, fmt.Errorf("read connection posture: %w", err)
	}
	if err := json.Unmarshal(checks, &report.Checks); err != nil {
		return report, false, fmt.Errorf("decode connection posture: %w", err)
	}
	return report, true, nil
}

// PutPosture stores a connection's latest posture report.
func (r *StorageConnectionRepository) PutPosture(ctx context.Context, tenantID string, report model.PostureReport) error {
	checks, err := json.Marshal(report.Checks)
	if err != nil {
		return err
	}
	_, err = r.db.Pool.Exec(ctx, `
		INSERT INTO connection_posture (connection_id, tenant_id, checked_at, worst, phi_safe, checks, lifecycle_hash, owner_account,
		    location, location_verified, encryption_key)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (connection_id) DO UPDATE SET checked_at = EXCLUDED.checked_at, worst = EXCLUDED.worst,
		    phi_safe = EXCLUDED.phi_safe, checks = EXCLUDED.checks, lifecycle_hash = EXCLUDED.lifecycle_hash,
		    owner_account = EXCLUDED.owner_account, location = EXCLUDED.location, location_verified = EXCLUDED.location_verified,
		    encryption_key = EXCLUDED.encryption_key`,
		report.ConnectionID, tenantID, report.CheckedAt, report.Worst, report.PHISafe, checks, report.LifecycleHash, report.OwnerAccount,
		report.Location, report.LocationVerified, report.EncryptionKey)
	if err != nil {
		return fmt.Errorf("store connection posture: %w", err)
	}
	return nil
}

// PostureDue lists active and disabled connections whose posture was last
// checked before the given time, or never.
func (r *StorageConnectionRepository) PostureDue(ctx context.Context, before time.Time, limit int) ([]model.StorageConnection, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT `+connectionColumns+` FROM storage_connections AS c
		WHERE c.tenant_id IS NOT NULL AND c.status IN ('ACTIVE', 'DISABLED')
		  AND NOT EXISTS (SELECT 1 FROM connection_posture AS p WHERE p.connection_id = c.id AND p.checked_at >= $1)
		ORDER BY c.id LIMIT $2`, before, limit)
	if err != nil {
		return nil, fmt.Errorf("list connections due a posture check: %w", err)
	}
	return collect(rows, func(row pgx.Row) (model.StorageConnection, error) { return scanConnection(row) })
}

// DatasetRules reads the placement rules a dataset sets (none for a
// dataset that does not exist).
func (r *StorageConnectionRepository) DatasetRules(ctx context.Context, tenantID, dataset string) (model.PlacementRules, error) {
	var rules model.PlacementRules
	err := r.db.Pool.QueryRow(ctx, `
		SELECT coalesce(data_class, ''), residency, require_customer_key FROM datasets WHERE tenant_id = $1 AND name = $2`,
		tenantID, dataset).Scan(&rules.DataClass, &rules.Residency, &rules.RequireCustomerKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.PlacementRules{}, nil
	}
	if err != nil {
		return rules, fmt.Errorf("read dataset placement rules: %w", err)
	}
	return rules, nil
}

// LocatedMembers is where one snapshot's members are stored.
type LocatedMembers struct {
	ConnectionID     string
	Location         string
	LocationVerified bool
	EncryptionKey    string
	Members          int64
	Bytes            int64
}

// VersionLocations groups a snapshot's members by the connection storing
// them, with each connection's location and key. The platform's own
// connection is located by its configured region.
func (r *StorageConnectionRepository) VersionLocations(ctx context.Context, tenantID, versionID string) ([]LocatedMembers, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT coalesce(a.storage_connection_id, 'platform') AS connection_id,
		       coalesce(nullif(cp.location, ''), lower(coalesce(sc.region, ''))) AS location,
		       coalesce(cp.location_verified, false), coalesce(sc.encryption_key, ''),
		       count(*), coalesce(sum(vm.size_bytes), 0)
		FROM dataset_version_members AS vm
		JOIN assets AS a ON a.id = vm.asset_id AND a.tenant_id = vm.tenant_id
		LEFT JOIN storage_connections AS sc ON sc.id = coalesce(a.storage_connection_id, 'platform')
		LEFT JOIN connection_posture AS cp ON cp.connection_id = sc.id
		WHERE vm.tenant_id = $1 AND vm.version_id = $2
		GROUP BY 1, 2, 3, 4 ORDER BY 5 DESC`, tenantID, versionID)
	if err != nil {
		return nil, fmt.Errorf("locate version members: %w", err)
	}
	return collect(rows, func(row pgx.Row) (LocatedMembers, error) {
		var l LocatedMembers
		err := row.Scan(&l.ConnectionID, &l.Location, &l.LocationVerified, &l.EncryptionKey, &l.Members, &l.Bytes)
		return l, err
	})
}

// DatasetRulesByID reads a dataset's placement rules by its ID.
func (r *StorageConnectionRepository) DatasetRulesByID(ctx context.Context, tenantID, datasetID string) (model.PlacementRules, error) {
	var rules model.PlacementRules
	err := r.db.Pool.QueryRow(ctx, `
		SELECT coalesce(data_class, ''), residency, require_customer_key FROM datasets WHERE tenant_id = $1 AND id = $2`,
		tenantID, datasetID).Scan(&rules.DataClass, &rules.Residency, &rules.RequireCustomerKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return rules, model.ErrDatasetNotFound
	}
	if err != nil {
		return rules, fmt.Errorf("read dataset placement rules: %w", err)
	}
	return rules, nil
}

// SetPlatformRegion records where the platform's own bucket is, as the
// operator configured it, so placement and residency reports can name it.
func (r *StorageConnectionRepository) SetPlatformRegion(ctx context.Context, region string) error {
	_, err := r.db.Pool.Exec(ctx, `UPDATE storage_connections SET region = $1 WHERE id = 'platform' AND coalesce(region, '') <> $1`, region)
	if err != nil {
		return fmt.Errorf("record the platform region: %w", err)
	}
	return nil
}

// ─── Drift ────────────────────────────────────────────────────────────

// StoredObject is one object the platform keeps on a connection: an
// asset's own object, or a pack holding many.
type StoredObject struct {
	AssetID   string
	PackID    string
	Key       string
	ETag      string
	VersionID string
	SizeBytes int64
}

// StoredObjects pages the published objects on a connection: assets not
// packed, then packs. after is the last key seen.
func (r *StorageConnectionRepository) StoredObjects(ctx context.Context, tenantID, connectionID, after string, limit int) ([]StoredObject, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT asset_id, pack_id, object_key, etag, version_id, size_bytes FROM (
		    SELECT a.id AS asset_id, '' AS pack_id, a.object_key, coalesce(a.storage_etag, '') AS etag,
		           coalesce(a.storage_version_id, '') AS version_id, a.size_bytes
		    FROM assets AS a
		    WHERE a.tenant_id = $1 AND a.storage_connection_id = $2 AND a.status = 'AVAILABLE'
		      AND NOT EXISTS (SELECT 1 FROM pack_entries AS e JOIN packs AS p ON p.id = e.pack_id
		                      WHERE e.tenant_id = a.tenant_id AND e.asset_id = a.id AND e.state = 'PUBLISHED' AND p.state = 'PUBLISHED')
		    UNION ALL
		    SELECT '', p.id, p.object_key, coalesce(p.storage_etag, ''), coalesce(p.storage_version_id, ''), p.total_bytes
		    FROM packs AS p WHERE p.tenant_id = $1 AND p.storage_connection_id = $2 AND p.state = 'PUBLISHED'
		) AS o
		WHERE object_key > $3
		ORDER BY object_key LIMIT $4`, tenantID, connectionID, after, limit)
	if err != nil {
		return nil, fmt.Errorf("list stored objects: %w", err)
	}
	return collect(rows, func(row pgx.Row) (StoredObject, error) {
		var o StoredObject
		err := row.Scan(&o.AssetID, &o.PackID, &o.Key, &o.ETag, &o.VersionID, &o.SizeBytes)
		return o, err
	})
}

// KnownKeys reports which of keys the platform wrote on the connection:
// any asset (in any state) or pack, current or not.
func (r *StorageConnectionRepository) KnownKeys(ctx context.Context, tenantID, connectionID string, keys []string) (map[string]bool, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT object_key FROM assets WHERE tenant_id = $1 AND storage_connection_id = $2 AND object_key = ANY($3)
		UNION SELECT object_key FROM packs WHERE tenant_id = $1 AND storage_connection_id = $2 AND object_key = ANY($3)
		UNION SELECT manifest_key FROM packs WHERE tenant_id = $1 AND storage_connection_id = $2 AND manifest_key = ANY($3)`,
		tenantID, connectionID, keys)
	if err != nil {
		return nil, fmt.Errorf("match stored keys: %w", err)
	}
	known, err := collect(rows, func(row pgx.Row) (string, error) {
		var k string
		return k, row.Scan(&k)
	})
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(known))
	for _, k := range known {
		out[k] = true
	}
	return out, nil
}

// DriftDue lists active and disabled connections whose drift scan last
// ran before the given time, or never.
func (r *StorageConnectionRepository) DriftDue(ctx context.Context, before time.Time, limit int) ([]model.StorageConnection, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT `+connectionColumns+` FROM storage_connections AS c
		WHERE c.tenant_id IS NOT NULL AND c.status IN ('ACTIVE', 'DISABLED')
		  AND NOT EXISTS (SELECT 1 FROM connection_drift_scans AS d WHERE d.connection_id = c.id AND d.last_run_at >= $1)
		ORDER BY c.id LIMIT $2`, before, limit)
	if err != nil {
		return nil, fmt.Errorf("list connections due a drift scan: %w", err)
	}
	return collect(rows, func(row pgx.Row) (model.StorageConnection, error) { return scanConnection(row) })
}

// DriftCursor reads where a connection's drift scan is.
func (r *StorageConnectionRepository) DriftCursor(ctx context.Context, connectionID string) (string, string, error) {
	var objects, listing string
	err := r.db.Pool.QueryRow(ctx, `SELECT object_cursor, listing_cursor FROM connection_drift_scans WHERE connection_id = $1`,
		connectionID).Scan(&objects, &listing)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("read drift cursor: %w", err)
	}
	return objects, listing, nil
}

// SaveDriftCursor records where a connection's drift scan stopped.
func (r *StorageConnectionRepository) SaveDriftCursor(ctx context.Context, tenantID, connectionID, objects, listing string, checked int, at time.Time) error {
	_, err := r.db.Pool.Exec(ctx, `
		INSERT INTO connection_drift_scans (connection_id, tenant_id, object_cursor, listing_cursor, objects_checked, last_run_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (connection_id) DO UPDATE SET object_cursor = EXCLUDED.object_cursor, listing_cursor = EXCLUDED.listing_cursor,
		    objects_checked = connection_drift_scans.objects_checked + EXCLUDED.objects_checked, last_run_at = EXCLUDED.last_run_at`,
		connectionID, tenantID, objects, listing, checked, at)
	if err != nil {
		return fmt.Errorf("save drift cursor: %w", err)
	}
	return nil
}

// AddDrift records a finding unless the same one is already open.
func (r *StorageConnectionRepository) AddDrift(ctx context.Context, f model.DriftFinding) (bool, error) {
	tag, err := r.db.Pool.Exec(ctx, `
		INSERT INTO storage_drift_findings (id, tenant_id, connection_id, asset_id, object_key, kind, detail, detected_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (tenant_id, connection_id, object_key, kind) WHERE resolved_at IS NULL DO NOTHING`,
		f.ID, f.TenantID, f.ConnectionID, f.AssetID, f.ObjectKey, f.Kind, f.Detail, f.DetectedAt)
	if err != nil {
		return false, fmt.Errorf("record drift: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

const driftColumns = `id, tenant_id, connection_id, asset_id, object_key, kind, detail, detected_at, resolved_at, coalesce(resolved_by, '')`

func scanDrift(row pgx.Row) (model.DriftFinding, error) {
	var f model.DriftFinding
	err := row.Scan(&f.ID, &f.TenantID, &f.ConnectionID, &f.AssetID, &f.ObjectKey, &f.Kind, &f.Detail, &f.DetectedAt, &f.ResolvedAt, &f.ResolvedBy)
	return f, err
}

// Drift lists a tenant's findings, newest first; connectionID "" is all.
func (r *StorageConnectionRepository) Drift(ctx context.Context, tenantID, connectionID string, open bool, limit int) ([]model.DriftFinding, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT `+driftColumns+` FROM storage_drift_findings
		WHERE tenant_id = $1 AND ($2 = '' OR connection_id = $2) AND (NOT $3 OR resolved_at IS NULL)
		ORDER BY detected_at DESC, id LIMIT $4`, tenantID, connectionID, open, limit)
	if err != nil {
		return nil, fmt.Errorf("list drift: %w", err)
	}
	return collect(rows, scanDrift)
}

// ResolveDrift closes a finding someone has looked into.
func (r *StorageConnectionRepository) ResolveDrift(ctx context.Context, tenantID, id, actor string, at time.Time) (model.DriftFinding, error) {
	f, err := scanDrift(r.db.Pool.QueryRow(ctx, `
		UPDATE storage_drift_findings SET resolved_at = $4, resolved_by = $3
		WHERE tenant_id = $1 AND id = $2 AND resolved_at IS NULL
		RETURNING `+driftColumns, tenantID, id, actor, at))
	if errors.Is(err, pgx.ErrNoRows) {
		return f, model.ErrDriftNotFound
	}
	if err != nil {
		return f, fmt.Errorf("resolve drift: %w", err)
	}
	return f, nil
}

// VersionDrift lists the open findings that touch a snapshot's members:
// their own objects, or the packs holding them.
func (r *StorageConnectionRepository) VersionDrift(ctx context.Context, tenantID, versionID string, limit int) ([]model.DriftFinding, []string, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT `+driftColumnsF+`, vm.path FROM dataset_version_members AS vm
		JOIN storage_drift_findings AS f ON f.tenant_id = vm.tenant_id AND f.resolved_at IS NULL AND (
		    f.asset_id = vm.asset_id OR f.object_key IN (
		        SELECT p.object_key FROM pack_entries AS e JOIN packs AS p ON p.id = e.pack_id
		        WHERE e.tenant_id = vm.tenant_id AND e.asset_id = vm.asset_id))
		WHERE vm.tenant_id = $1 AND vm.version_id = $2
		ORDER BY vm.path LIMIT $3`, tenantID, versionID, limit)
	if err != nil {
		return nil, nil, fmt.Errorf("list version drift: %w", err)
	}
	defer rows.Close()
	var findings []model.DriftFinding
	var paths []string
	for rows.Next() {
		var f model.DriftFinding
		var path string
		if err := rows.Scan(&f.ID, &f.TenantID, &f.ConnectionID, &f.AssetID, &f.ObjectKey, &f.Kind, &f.Detail,
			&f.DetectedAt, &f.ResolvedAt, &f.ResolvedBy, &path); err != nil {
			return nil, nil, err
		}
		findings, paths = append(findings, f), append(paths, path)
	}
	return findings, paths, rows.Err()
}

const driftColumnsF = `f.id, f.tenant_id, f.connection_id, f.asset_id, f.object_key, f.kind, f.detail, f.detected_at,
	f.resolved_at, coalesce(f.resolved_by, '')`
