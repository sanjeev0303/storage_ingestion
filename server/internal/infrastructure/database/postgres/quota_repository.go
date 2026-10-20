package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// QuotaRepository stores tenant quota policies and storage usage.
//
// Usage counters change in the same transaction as the session or asset
// change they reflect:
//   - creating an upload session reserves its declared bytes;
//   - a session that fails, is aborted or expires releases them;
//   - its asset becoming AVAILABLE turns them into stored bytes (the
//     verified size), and a rejected asset releases them;
//   - soft deletion moves stored bytes to pending deletion; the purge
//     removes them.
//
// The reservation's state is on the session row, so each of these is a
// compare-and-set that cannot apply twice. Locks are taken in the order
// asset, session, usage; a reservation takes the usage row first but only
// inserts its session, so it waits on nothing.
type QuotaRepository struct {
	db *DB
}

func NewQuotaRepository(db *DB) *QuotaRepository {
	return &QuotaRepository{db: db}
}

type queryer interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// readQuota returns a tenant's overrides; zero fields are unset.
func readQuota(ctx context.Context, q queryer, tenantID string) (model.TenantQuota, error) {
	var out model.TenantQuota
	var storage, pending, assets, object, entries, manifest *int64
	var jobs, uploads, multipart, processing, requests *int
	err := q.QueryRow(ctx, `
		SELECT max_storage_bytes, max_active_jobs, max_active_uploads, max_pending_entries, max_assets, max_object_bytes,
		       max_active_multipart, max_pending_processing, max_manifest_entries, max_manifest_bytes, max_requests_per_minute
		FROM tenant_quotas WHERE tenant_id = $1
	`, tenantID).Scan(&storage, &jobs, &uploads, &pending, &assets, &object, &multipart, &processing, &entries, &manifest, &requests)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return out, fmt.Errorf("read tenant quota: %w", err)
	}
	set64 := func(dst *int64, v *int64) {
		if v != nil {
			*dst = *v
		}
	}
	set := func(dst *int, v *int) {
		if v != nil {
			*dst = *v
		}
	}
	set64(&out.MaxStorageBytes, storage)
	set(&out.MaxActiveJobs, jobs)
	set(&out.MaxActiveUploads, uploads)
	set64(&out.MaxPendingEntries, pending)
	set64(&out.MaxAssets, assets)
	set64(&out.MaxObjectBytes, object)
	set(&out.MaxActiveMultipart, multipart)
	set(&out.MaxPendingProcessing, processing)
	set64(&out.MaxManifestEntries, entries)
	set64(&out.MaxManifestBytes, manifest)
	set(&out.MaxRequestsPerMinute, requests)
	return out, nil
}

// writeQuota replaces a tenant's overrides; zero fields become unset.
func writeQuota(ctx context.Context, tx pgx.Tx, tenantID string, q model.TenantQuota, now time.Time) error {
	orNil := func(v int64) any {
		if v <= 0 {
			return nil
		}
		return v
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO tenant_quotas (tenant_id, max_storage_bytes, max_active_jobs, max_active_uploads, max_pending_entries,
		                           max_assets, max_object_bytes, max_active_multipart, max_pending_processing,
		                           max_manifest_entries, max_manifest_bytes, max_requests_per_minute, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		ON CONFLICT (tenant_id) DO UPDATE SET
		    max_storage_bytes = EXCLUDED.max_storage_bytes, max_active_jobs = EXCLUDED.max_active_jobs,
		    max_active_uploads = EXCLUDED.max_active_uploads, max_pending_entries = EXCLUDED.max_pending_entries,
		    max_assets = EXCLUDED.max_assets, max_object_bytes = EXCLUDED.max_object_bytes,
		    max_active_multipart = EXCLUDED.max_active_multipart, max_pending_processing = EXCLUDED.max_pending_processing,
		    max_manifest_entries = EXCLUDED.max_manifest_entries, max_manifest_bytes = EXCLUDED.max_manifest_bytes,
		    max_requests_per_minute = EXCLUDED.max_requests_per_minute, updated_at = EXCLUDED.updated_at
	`, tenantID, orNil(q.MaxStorageBytes), orNil(int64(q.MaxActiveJobs)), orNil(int64(q.MaxActiveUploads)),
		orNil(q.MaxPendingEntries), orNil(q.MaxAssets), orNil(q.MaxObjectBytes), orNil(int64(q.MaxActiveMultipart)),
		orNil(int64(q.MaxPendingProcessing)), orNil(q.MaxManifestEntries), orNil(q.MaxManifestBytes),
		orNil(int64(q.MaxRequestsPerMinute)), now)
	if err != nil {
		return fmt.Errorf("set tenant quota: %w", err)
	}
	return nil
}

func (r *QuotaRepository) Policy(ctx context.Context, tenantID string) (model.TenantQuota, error) {
	return readQuota(ctx, r.db.Pool, tenantID)
}

func (r *QuotaRepository) SetPolicy(ctx context.Context, tenantID string, q model.TenantQuota, now time.Time, audit ...model.AuditEvent) error {
	return withTx(ctx, r.db, func(tx pgx.Tx) error {
		if err := writeQuota(ctx, tx, tenantID, q, now); err != nil {
			return err
		}
		return appendAudits(ctx, tx, audit)
	})
}

// usageDelta changes the counters of tenant_usage.
type usageDelta struct {
	reserved, uploads, stored, assets, pending int64
}

func addUsage(ctx context.Context, tx pgx.Tx, tenantID string, d usageDelta, now time.Time) error {
	if d == (usageDelta{}) {
		return nil
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO tenant_usage (tenant_id, reserved_bytes, reserved_uploads, stored_bytes, asset_count, pending_deletion_bytes, updated_at)
		VALUES ($1, GREATEST($2, 0), GREATEST($3, 0), GREATEST($4, 0), GREATEST($5, 0), GREATEST($6, 0), $7)
		ON CONFLICT (tenant_id) DO UPDATE SET
		    reserved_bytes = GREATEST(tenant_usage.reserved_bytes + $2, 0),
		    reserved_uploads = GREATEST(tenant_usage.reserved_uploads + $3, 0),
		    stored_bytes = GREATEST(tenant_usage.stored_bytes + $4, 0),
		    asset_count = GREATEST(tenant_usage.asset_count + $5, 0),
		    pending_deletion_bytes = GREATEST(tenant_usage.pending_deletion_bytes + $6, 0),
		    updated_at = $7
	`, tenantID, d.reserved, d.uploads, d.stored, d.assets, d.pending, now)
	if err != nil {
		return fmt.Errorf("update storage usage: %w", err)
	}
	return nil
}

// settleReservation moves an upload's reservation out of RESERVED: to
// CONSUMED when its asset is published, otherwise to RELEASED. It returns
// the reserved bytes, or 0 when nothing was reserved any more.
func settleReservation(ctx context.Context, tx pgx.Tx, tenantID, uploadID, to string) (int64, error) {
	var bytes int64
	err := tx.QueryRow(ctx, `
		UPDATE upload_sessions SET quota_state = $3
		WHERE tenant_id = $1 AND id = $2 AND quota_state = 'RESERVED'
		RETURNING reserved_bytes
	`, tenantID, uploadID, to).Scan(&bytes)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("settle reservation: %w", err)
	}
	return bytes, nil
}

// releaseReservation gives back an upload's reserved bytes.
func releaseReservation(ctx context.Context, tx pgx.Tx, tenantID, uploadID string, now time.Time) error {
	bytes, err := settleReservation(ctx, tx, tenantID, uploadID, "RELEASED")
	if err != nil || bytes == 0 {
		return err
	}
	return addUsage(ctx, tx, tenantID, usageDelta{reserved: -bytes, uploads: -1}, now)
}

// assetPublished turns an upload's reservation into stored bytes of the
// asset's verified size.
func assetPublished(ctx context.Context, tx pgx.Tx, tenantID, uploadID string, sizeBytes int64, now time.Time) error {
	reserved, err := settleReservation(ctx, tx, tenantID, uploadID, "CONSUMED")
	if err != nil {
		return err
	}
	d := usageDelta{stored: sizeBytes, assets: 1}
	if reserved > 0 {
		d.reserved, d.uploads = -reserved, -1
	}
	return addUsage(ctx, tx, tenantID, d, now)
}

func lockStorageUsage(ctx context.Context, tx pgx.Tx, tenantID string, now time.Time) (model.StorageUsage, error) {
	u := model.StorageUsage{TenantID: tenantID}
	err := tx.QueryRow(ctx, `
		INSERT INTO tenant_usage (tenant_id, updated_at) VALUES ($1, $2)
		ON CONFLICT (tenant_id) DO UPDATE SET updated_at = tenant_usage.updated_at
		RETURNING reserved_bytes, reserved_uploads, stored_bytes, asset_count, pending_deletion_bytes
	`, tenantID, now).Scan(&u.ReservedBytes, &u.ReservedUploads, &u.StoredBytes, &u.AssetCount, &u.PendingDeletionBytes)
	if err != nil {
		return u, fmt.Errorf("lock storage usage: %w", err)
	}
	return u, nil
}

func (r *QuotaRepository) CreateReservedUpload(ctx context.Context, upload *model.UploadSession, idempotencyKey string, limits model.TenantQuota, multipart bool) error {
	if upload == nil {
		return errors.New("upload session must not be nil")
	}
	return withTx(ctx, r.db, func(tx pgx.Tx) error {
		// A catalogued upload claims its object first (lock order:
		// object, then usage).
		if upload.Path != "" {
			if err := claimObject(ctx, tx, upload); err != nil {
				return err
			}
		}
		// The usage row is the tenant's reservation lock: reservations run
		// one at a time per tenant, so each sees the ones before it.
		usage, err := lockStorageUsage(ctx, tx, upload.TenantID, upload.CreatedAt)
		if err != nil {
			return err
		}
		size := upload.ExpectedSize
		used := usage.ReservedBytes + usage.StoredBytes + usage.PendingDeletionBytes
		switch {
		case limits.MaxObjectBytes > 0 && size > limits.MaxObjectBytes:
			return &model.QuotaError{Limit: "max_object_bytes", Max: limits.MaxObjectBytes, Requested: size}
		case limits.MaxStorageBytes > 0 && used+size > limits.MaxStorageBytes:
			return &model.QuotaError{Limit: "storage_bytes", Max: limits.MaxStorageBytes, Used: used, Requested: size}
		case limits.MaxAssets > 0 && usage.AssetCount+usage.ReservedUploads+1 > limits.MaxAssets:
			return &model.QuotaError{Limit: "assets", Max: limits.MaxAssets, Used: usage.AssetCount + usage.ReservedUploads, Requested: 1}
		}

		var active, activeMultipart, pending int64
		if err := tx.QueryRow(ctx, `
			SELECT count(*), count(*) FILTER (WHERE multipart OR storage_upload_id IS NOT NULL)
			FROM upload_sessions
			WHERE tenant_id = $1 AND status IN ('PENDING', 'UPLOADING', 'COMPLETING', 'ABORTING')
		`, upload.TenantID).Scan(&active, &activeMultipart); err != nil {
			return fmt.Errorf("count active uploads: %w", err)
		}
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM processing_jobs WHERE tenant_id = $1 AND state = 'PENDING'
		`, upload.TenantID).Scan(&pending); err != nil {
			return fmt.Errorf("count pending processing: %w", err)
		}
		switch {
		case limits.MaxActiveUploads > 0 && active >= int64(limits.MaxActiveUploads):
			return &model.QuotaError{Limit: "active_uploads", Max: int64(limits.MaxActiveUploads), Used: active, Requested: 1}
		case multipart && limits.MaxActiveMultipart > 0 && activeMultipart >= int64(limits.MaxActiveMultipart):
			return &model.QuotaError{Limit: "active_multipart", Max: int64(limits.MaxActiveMultipart), Used: activeMultipart, Requested: 1}
		case limits.MaxPendingProcessing > 0 && pending >= int64(limits.MaxPendingProcessing):
			return &model.QuotaError{Limit: "pending_processing", Max: int64(limits.MaxPendingProcessing), Used: pending, Requested: 1}
		}

		if err := insertUploadSession(ctx, tx, upload, idempotencyKey, true, multipart); err != nil {
			return err
		}
		return addUsage(ctx, tx, upload.TenantID, usageDelta{reserved: size, uploads: 1}, upload.CreatedAt)
	})
}

func (r *QuotaRepository) Usage(ctx context.Context, tenantID string) (model.StorageUsage, error) {
	u := model.StorageUsage{TenantID: tenantID}
	err := r.db.Pool.QueryRow(ctx, `
		SELECT reserved_bytes, reserved_uploads, stored_bytes, asset_count, pending_deletion_bytes
		FROM tenant_usage WHERE tenant_id = $1
	`, tenantID).Scan(&u.ReservedBytes, &u.ReservedUploads, &u.StoredBytes, &u.AssetCount, &u.PendingDeletionBytes)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return u, fmt.Errorf("read storage usage: %w", err)
	}
	err = r.db.Pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM upload_sessions WHERE tenant_id = $1 AND status IN ('PENDING', 'UPLOADING', 'COMPLETING', 'ABORTING')),
		  (SELECT count(*) FROM upload_sessions WHERE tenant_id = $1 AND status IN ('PENDING', 'UPLOADING', 'COMPLETING', 'ABORTING')
		                                         AND (multipart OR storage_upload_id IS NOT NULL)),
		  (SELECT count(*) FROM processing_jobs WHERE tenant_id = $1 AND state = 'PENDING'),
		  (SELECT COALESCE(sum(size_bytes), 0) FROM assets WHERE tenant_id = $1 AND status IN ('QUARANTINED', 'PROCESSING')),
		  (SELECT COALESCE(sum(p.size_bytes), 0) FROM upload_parts AS p JOIN upload_sessions AS s ON s.id = p.upload_id
		    WHERE s.tenant_id = $1 AND s.status IN ('PENDING', 'UPLOADING', 'COMPLETING'))
	`, tenantID).Scan(&u.ActiveUploads, &u.ActiveMultipart, &u.PendingProcessing, &u.QuarantinedBytes, &u.IncompleteMultipartBytes)
	if err != nil {
		return u, fmt.Errorf("measure storage usage: %w", err)
	}
	return u, nil
}

func (r *QuotaRepository) Reconcile(ctx context.Context, tenantID string, now time.Time) (model.StorageUsage, model.StorageUsage, int, error) {
	var before, after model.StorageUsage
	settled := 0
	// First settle reservations that outlived their upload. Each statement
	// is a compare-and-set on the session, so a concurrent change wins or
	// loses cleanly; the usage row is not locked yet (lock order).
	steps := []struct {
		sql  string
		args []any
	}{
		// The session ended without an asset.
		{`UPDATE upload_sessions SET quota_state = 'RELEASED'
		  WHERE tenant_id = $1 AND quota_state = 'RESERVED' AND status IN ('FAILED', 'ABORTED', 'EXPIRED')`,
			[]any{tenantID}},
		// Its asset was published, or rejected or deleted.
		{`UPDATE upload_sessions AS s SET quota_state = 'CONSUMED'
		  FROM assets AS a WHERE a.upload_id = s.id AND a.tenant_id = s.tenant_id
		    AND s.tenant_id = $1 AND s.quota_state = 'RESERVED' AND a.status = 'AVAILABLE'`,
			[]any{tenantID}},
		{`UPDATE upload_sessions AS s SET quota_state = 'RELEASED'
		  FROM assets AS a WHERE a.upload_id = s.id AND a.tenant_id = s.tenant_id
		    AND s.tenant_id = $1 AND s.quota_state = 'RESERVED' AND a.status IN ('REJECTED', 'SOFT_DELETED', 'DELETED')`,
			[]any{tenantID}},
		// A single-request upload past its expiry can no longer be sent:
		// expire it. (Multipart sessions are expired by their own
		// reconciliation, which aborts the provider upload first.)
		{`UPDATE upload_sessions
		  SET status = 'EXPIRED', quota_state = 'RELEASED', failure_reason = COALESCE(failure_reason, 'expired'),
		      aborted_at = $2, updated_at = $2
		  WHERE tenant_id = $1 AND quota_state = 'RESERVED' AND status = 'PENDING'
		    AND NOT multipart AND storage_upload_id IS NULL AND expires_at < $2`,
			[]any{tenantID, now}},
	}
	for _, step := range steps {
		tag, err := r.db.Pool.Exec(ctx, step.sql, step.args...)
		if err != nil {
			return before, after, settled, fmt.Errorf("settle reservations: %w", err)
		}
		settled += int(tag.RowsAffected())
	}

	err := withTx(ctx, r.db, func(tx pgx.Tx) error {
		var err error
		if before, err = lockStorageUsage(ctx, tx, tenantID, now); err != nil {
			return err
		}
		after.TenantID = tenantID
		err = tx.QueryRow(ctx, `
			SELECT
			  (SELECT COALESCE(sum(reserved_bytes), 0) FROM upload_sessions WHERE tenant_id = $1 AND quota_state = 'RESERVED'),
			  (SELECT count(*) FROM upload_sessions WHERE tenant_id = $1 AND quota_state = 'RESERVED'),
			  (SELECT COALESCE(sum(size_bytes), 0) FROM assets WHERE tenant_id = $1 AND status = 'AVAILABLE'),
			  (SELECT count(*) FROM assets WHERE tenant_id = $1 AND status = 'AVAILABLE'),
			  (SELECT COALESCE(sum(size_bytes), 0) FROM assets WHERE tenant_id = $1 AND status = 'SOFT_DELETED')
		`, tenantID).Scan(&after.ReservedBytes, &after.ReservedUploads, &after.StoredBytes, &after.AssetCount, &after.PendingDeletionBytes)
		if err != nil {
			return fmt.Errorf("recount storage usage: %w", err)
		}
		_, err = tx.Exec(ctx, `
			UPDATE tenant_usage
			SET reserved_bytes = $2, reserved_uploads = $3, stored_bytes = $4, asset_count = $5, pending_deletion_bytes = $6, updated_at = $7
			WHERE tenant_id = $1
		`, tenantID, after.ReservedBytes, after.ReservedUploads, after.StoredBytes, after.AssetCount, after.PendingDeletionBytes, now)
		if err != nil {
			return fmt.Errorf("repair storage usage: %w", err)
		}
		return nil
	})
	return before, after, settled, err
}

func (r *QuotaRepository) TenantsWithUsage(ctx context.Context, after string, limit int) ([]string, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT tenant_id FROM (
			SELECT tenant_id FROM tenant_usage
			UNION
			SELECT id FROM organizations WHERE status <> 'DELETED'
		) AS t
		WHERE tenant_id > $1
		ORDER BY tenant_id LIMIT $2
	`, after, limit)
	if err != nil {
		return nil, fmt.Errorf("list tenants: %w", err)
	}
	return collect(rows, func(row pgx.Row) (string, error) {
		var id string
		return id, row.Scan(&id)
	})
}
