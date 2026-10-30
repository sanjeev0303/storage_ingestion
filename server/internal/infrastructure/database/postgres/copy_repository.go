package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// CopyRepository keeps the copy engine's jobs, replicas, scrubs, clean
// points and export schedules (docs/copy-engine.md).
type CopyRepository struct {
	db *DB
}

func NewCopyRepository(db *DB) *CopyRepository { return &CopyRepository{db: db} }

// ─── Jobs ─────────────────────────────────────────────────────────────

const copyJobColumns = `id, tenant_id, kind, dataset_id, version_id, source_connection_id, target_connection_id, target_prefix,
	options, state, planned, total_items, done_items, failed_items, total_bytes, done_bytes, report, error,
	coalesce(idempotency_key, ''), created_by, created_at, updated_at, started_at, finished_at`

func scanCopyJob(row pgx.Row) (model.CopyJob, error) {
	var j model.CopyJob
	err := row.Scan(&j.ID, &j.TenantID, &j.Kind, &j.DatasetID, &j.VersionID, &j.SourceConnectionID, &j.TargetConnectionID,
		&j.TargetPrefix, &j.Options, &j.State, &j.Planned, &j.TotalItems, &j.DoneItems, &j.FailedItems, &j.TotalBytes,
		&j.DoneBytes, &j.Report, &j.Error, &j.IdempotencyKey, &j.CreatedBy, &j.CreatedAt, &j.UpdatedAt, &j.StartedAt, &j.FinishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return j, model.ErrCopyJobNotFound
	}
	return j, err
}

// CreateJob records a job, or returns the one already made with the same
// idempotency key (created false).
func (r *CopyRepository) CreateJob(ctx context.Context, j model.CopyJob) (model.CopyJob, bool, error) {
	var key any
	if j.IdempotencyKey != "" {
		key = j.IdempotencyKey
	}
	if len(j.Options) == 0 {
		j.Options = json.RawMessage(`{}`)
	}
	stored, err := scanCopyJob(r.db.Pool.QueryRow(ctx, `
		INSERT INTO copy_jobs (id, tenant_id, kind, dataset_id, version_id, source_connection_id, target_connection_id,
		    target_prefix, options, state, idempotency_key, created_by, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'QUEUED', $10, $11, $12, $12)
		ON CONFLICT (tenant_id, idempotency_key) WHERE idempotency_key IS NOT NULL DO NOTHING
		RETURNING `+copyJobColumns,
		j.ID, j.TenantID, j.Kind, j.DatasetID, j.VersionID, j.SourceConnectionID, j.TargetConnectionID, j.TargetPrefix,
		j.Options, key, j.CreatedBy, j.CreatedAt))
	if errors.Is(err, model.ErrCopyJobNotFound) && j.IdempotencyKey != "" {
		existing, err := scanCopyJob(r.db.Pool.QueryRow(ctx, `SELECT `+copyJobColumns+` FROM copy_jobs
			WHERE tenant_id = $1 AND idempotency_key = $2`, j.TenantID, j.IdempotencyKey))
		return existing, false, err
	}
	if err != nil {
		return stored, false, fmt.Errorf("create copy job: %w", err)
	}
	return stored, true, nil
}

func (r *CopyRepository) GetJob(ctx context.Context, tenantID, id string) (model.CopyJob, error) {
	return scanCopyJob(r.db.Pool.QueryRow(ctx, `SELECT `+copyJobColumns+` FROM copy_jobs WHERE tenant_id = $1 AND id = $2`, tenantID, id))
}

func (r *CopyRepository) ListJobs(ctx context.Context, tenantID, datasetID string, limit int) ([]model.CopyJob, error) {
	rows, err := r.db.Pool.Query(ctx, `SELECT `+copyJobColumns+` FROM copy_jobs
		WHERE tenant_id = $1 AND ($2 = '' OR dataset_id = $2) ORDER BY created_at DESC LIMIT $3`, tenantID, datasetID, limit)
	if err != nil {
		return nil, fmt.Errorf("list copy jobs: %w", err)
	}
	return collect(rows, scanCopyJob)
}

// ClaimJob leases the queued or running job unattended longest.
func (r *CopyRepository) ClaimJob(ctx context.Context, owner string, now time.Time, lease time.Duration) (model.CopyJob, bool, error) {
	j, err := scanCopyJob(r.db.Pool.QueryRow(ctx, `
		UPDATE copy_jobs SET lease_owner = $1, lease_expires_at = $2::timestamptz + $3::interval, updated_at = $2,
		    state = 'RUNNING', started_at = coalesce(started_at, $2)
		WHERE id = (SELECT id FROM copy_jobs WHERE state IN ('QUEUED', 'RUNNING')
		            AND (lease_expires_at IS NULL OR lease_expires_at < $2)
		            ORDER BY updated_at LIMIT 1 FOR UPDATE SKIP LOCKED)
		RETURNING `+copyJobColumns, owner, now, lease.String()))
	if errors.Is(err, model.ErrCopyJobNotFound) {
		return j, false, nil
	}
	return j, err == nil, err
}

// Release gives a lease back so the next pass can carry on at once.
func (r *CopyRepository) Release(ctx context.Context, id, owner string) error {
	_, err := r.db.Pool.Exec(ctx, `UPDATE copy_jobs SET lease_owner = NULL, lease_expires_at = NULL
		WHERE id = $1 AND lease_owner = $2`, id, owner)
	return err
}

// Finish ends a job with its report.
func (r *CopyRepository) Finish(ctx context.Context, id, owner, state, message string, report any, now time.Time) error {
	body, err := json.Marshal(report)
	if err != nil {
		return err
	}
	_, err = r.db.Pool.Exec(ctx, `
		UPDATE copy_jobs SET state = $3, error = $4, report = $5, updated_at = $6, finished_at = $6,
		    lease_owner = NULL, lease_expires_at = NULL
		WHERE id = $1 AND lease_owner = $2 AND state IN ('QUEUED', 'RUNNING')`, id, owner, state, message, body, now)
	if err != nil {
		return fmt.Errorf("finish copy job: %w", err)
	}
	return nil
}

func (r *CopyRepository) Cancel(ctx context.Context, tenantID, id string, now time.Time) error {
	tag, err := r.db.Pool.Exec(ctx, `UPDATE copy_jobs SET state = 'CANCELLED', updated_at = $3, finished_at = $3,
		lease_owner = NULL, lease_expires_at = NULL WHERE tenant_id = $1 AND id = $2 AND state IN ('QUEUED', 'RUNNING')`, tenantID, id, now)
	if err != nil {
		return fmt.Errorf("cancel copy job: %w", err)
	}
	if tag.RowsAffected() == 0 {
		if _, err := r.GetJob(ctx, tenantID, id); err != nil {
			return err
		}
		return fmt.Errorf("%w: the job has already ended", model.ErrInvalidCopyJob)
	}
	return nil
}

// AddItems records a job's items and marks it planned, in one go.
func (r *CopyRepository) AddItems(ctx context.Context, jobID string, items []model.CopyItem) error {
	tx, err := r.db.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var bytes int64
	for _, it := range items {
		bytes += it.SizeBytes
		if _, err := tx.Exec(ctx, `
			INSERT INTO copy_items (job_id, ordinal, tenant_id, asset_id, path, target_key, size_bytes, checksum_sha256, state)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'PENDING') ON CONFLICT DO NOTHING`,
			jobID, it.Ordinal, it.TenantID, it.AssetID, it.Path, it.TargetKey, it.SizeBytes, it.ChecksumSHA256); err != nil {
			return fmt.Errorf("add copy item: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE copy_jobs SET planned = true, total_items = $2, total_bytes = $3 WHERE id = $1`,
		jobID, len(items), bytes); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

const copyItemColumns = `job_id, ordinal, tenant_id, asset_id, path, target_key, size_bytes, checksum_sha256, state,
	target_version_id, target_etag, error, duration_ms, done_at`

func scanCopyItem(row pgx.Row) (model.CopyItem, error) {
	var it model.CopyItem
	err := row.Scan(&it.JobID, &it.Ordinal, &it.TenantID, &it.AssetID, &it.Path, &it.TargetKey, &it.SizeBytes, &it.ChecksumSHA256,
		&it.State, &it.TargetVersionID, &it.TargetETag, &it.Error, &it.DurationMS, &it.DoneAt)
	return it, err
}

func (r *CopyRepository) PendingItems(ctx context.Context, jobID string, limit int) ([]model.CopyItem, error) {
	rows, err := r.db.Pool.Query(ctx, `SELECT `+copyItemColumns+` FROM copy_items
		WHERE job_id = $1 AND state = 'PENDING' ORDER BY ordinal LIMIT $2`, jobID, limit)
	if err != nil {
		return nil, fmt.Errorf("pending copy items: %w", err)
	}
	return collect(rows, scanCopyItem)
}

// Items pages a job's items; state "" is every state.
func (r *CopyRepository) Items(ctx context.Context, tenantID, jobID, state string, after, limit int) ([]model.CopyItem, error) {
	rows, err := r.db.Pool.Query(ctx, `SELECT `+copyItemColumns+` FROM copy_items
		WHERE tenant_id = $1 AND job_id = $2 AND ($3 = '' OR state = $3) AND ordinal > $4 ORDER BY ordinal LIMIT $5`,
		tenantID, jobID, state, after, limit)
	if err != nil {
		return nil, fmt.Errorf("list copy items: %w", err)
	}
	return collect(rows, scanCopyItem)
}

// FinishItem records one item's outcome and the job's running totals.
func (r *CopyRepository) FinishItem(ctx context.Context, it model.CopyItem, now time.Time) error {
	tx, err := r.db.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	tag, err := tx.Exec(ctx, `UPDATE copy_items SET state = $3, target_version_id = $4, target_etag = $5, error = $6,
		duration_ms = $7, done_at = $8 WHERE job_id = $1 AND ordinal = $2 AND state = 'PENDING'`,
		it.JobID, it.Ordinal, it.State, it.TargetVersionID, it.TargetETag, it.Error, it.DurationMS, now)
	if err != nil {
		return fmt.Errorf("finish copy item: %w", err)
	}
	if tag.RowsAffected() == 1 {
		done, failed, bytes := int64(0), int64(0), int64(0)
		if it.State == model.ItemDone {
			done, bytes = 1, it.SizeBytes
		} else {
			failed = 1
		}
		if _, err := tx.Exec(ctx, `UPDATE copy_jobs SET done_items = done_items + $2, failed_items = failed_items + $3,
			done_bytes = done_bytes + $4, updated_at = $5 WHERE id = $1`, it.JobID, done, failed, bytes, now); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// ─── What to copy ─────────────────────────────────────────────────────

// StoredAsset is an asset's bytes as stored, with where they are.
type StoredAsset struct {
	AssetID      string
	TenantID     string
	ConnectionID string
	ObjectKey    string
	VersionID    string
	ETag         string
	SizeBytes    int64
	SHA256       string
	ContentType  string
	SharedFrom   string
	Adoption     string
	Path         string
}

const storedAssetColumns = `a.id, a.tenant_id, coalesce(a.storage_connection_id, 'platform'), a.object_key,
	coalesce(a.storage_version_id, ''), coalesce(a.storage_etag, ''), a.size_bytes, coalesce(a.checksum_sha256, ''),
	a.content_type, coalesce(a.shared_from, ''), coalesce(a.adoption, '')`

func scanStoredAsset(row pgx.Row, path bool) (StoredAsset, error) {
	var a StoredAsset
	dest := []any{&a.AssetID, &a.TenantID, &a.ConnectionID, &a.ObjectKey, &a.VersionID, &a.ETag, &a.SizeBytes, &a.SHA256,
		&a.ContentType, &a.SharedFrom, &a.Adoption}
	if path {
		dest = append(dest, &a.Path)
	}
	return a, row.Scan(dest...)
}

// datasetAssets is every available asset a dataset refers to: the
// members of each of its versions, and the current versions of its draft
// members. $1 tenant, $2 dataset.
const datasetAssets = `
	SELECT vm.asset_id FROM dataset_version_members AS vm JOIN dataset_versions AS dv ON dv.id = vm.version_id
	WHERE vm.tenant_id = $1 AND dv.dataset_id = $2
	UNION
	SELECT coalesce(m.asset_id, o.current_asset_id) FROM dataset_members AS m
	JOIN catalog_objects AS o ON o.id = m.object_id AND o.tenant_id = m.tenant_id
	WHERE m.tenant_id = $1 AND m.dataset_id = $2 AND coalesce(m.asset_id, o.current_asset_id) IS NOT NULL`

// DatasetAssets lists the available assets a dataset refers to.
func (r *CopyRepository) DatasetAssets(ctx context.Context, tenantID, datasetID string) ([]StoredAsset, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT `+storedAssetColumns+` FROM assets AS a
		WHERE a.tenant_id = $1 AND a.status = 'AVAILABLE' AND a.id IN (`+datasetAssets+`)
		ORDER BY a.object_key, a.id`, tenantID, datasetID)
	if err != nil {
		return nil, fmt.Errorf("list dataset assets: %w", err)
	}
	return collect(rows, func(row pgx.Row) (StoredAsset, error) { return scanStoredAsset(row, false) })
}

// VersionAssets lists a snapshot's members with their paths.
func (r *CopyRepository) VersionAssets(ctx context.Context, tenantID, versionID string) ([]StoredAsset, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT `+storedAssetColumns+`, vm.path FROM dataset_version_members AS vm
		JOIN assets AS a ON a.id = vm.asset_id AND a.tenant_id = vm.tenant_id
		WHERE vm.tenant_id = $1 AND vm.version_id = $2 AND a.status = 'AVAILABLE'
		ORDER BY vm.ordinal`, tenantID, versionID)
	if err != nil {
		return nil, fmt.Errorf("list version assets: %w", err)
	}
	return collect(rows, func(row pgx.Row) (StoredAsset, error) { return scanStoredAsset(row, true) })
}

// GetStoredAsset reads one asset as stored.
func (r *CopyRepository) GetStoredAsset(ctx context.Context, tenantID, assetID string) (StoredAsset, error) {
	a, err := scanStoredAsset(r.db.Pool.QueryRow(ctx, `SELECT `+storedAssetColumns+` FROM assets AS a
		WHERE a.tenant_id = $1 AND a.id = $2`, tenantID, assetID), false)
	if errors.Is(err, pgx.ErrNoRows) {
		return a, model.ErrAssetNotFound
	}
	return a, err
}

// SwitchedAsset is where one asset's bytes now are after a migration.
type SwitchedAsset struct {
	SourceConnectionID string
	SourceKey          string
	TargetKey          string
	VersionID          string
	ETag               string
}

// SwitchConnection moves a dataset's assets to the target connection in
// one transaction: every asset stored at a migrated object (its sharers
// too), packed or not, now reads from the copy. Adopted objects become the
// platform's own copies. The dataset's new uploads go to the target.
func (r *CopyRepository) SwitchConnection(ctx context.Context, tenantID, datasetID, targetID string, moved []SwitchedAsset, now time.Time) (int64, error) {
	tx, err := r.db.Pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	// Assets' storage bindings are fixed (000015), but for this switch.
	if _, err := tx.Exec(ctx, `SELECT set_config('osp.migrating_assets', 'on', true)`); err != nil {
		return 0, err
	}
	var switched int64
	for _, m := range moved {
		var ids []string
		rows, err := tx.Query(ctx, `
			UPDATE assets SET storage_connection_id = $3, object_key = $4, storage_version_id = NULLIF($5, ''),
			    storage_etag = NULLIF($6, ''), adoption = NULL, updated_at = $8
			WHERE tenant_id = $1 AND coalesce(storage_connection_id, 'platform') = $2 AND object_key = $7 AND status = 'AVAILABLE'
			RETURNING id`, tenantID, m.SourceConnectionID, targetID, m.TargetKey, m.VersionID, m.ETag, m.SourceKey, now)
		if err != nil {
			return 0, fmt.Errorf("switch assets: %w", err)
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return 0, err
			}
			ids = append(ids, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return 0, fmt.Errorf("switch assets: %w", err)
		}
		switched += int64(len(ids))
		if len(ids) == 0 {
			continue
		}
		// Packed assets now read their own copy, not the pack.
		if _, err := tx.Exec(ctx, `UPDATE pack_entries SET state = 'DELETED', deleted_at = $3, updated_at = $3
			WHERE tenant_id = $1 AND asset_id = ANY($2) AND state = 'PUBLISHED'`, tenantID, ids, now); err != nil {
			return 0, fmt.Errorf("release migrated pack entries: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE datasets SET storage_connection_id = $3, updated_at = $4 WHERE tenant_id = $1 AND id = $2`,
		tenantID, datasetID, targetID, now); err != nil {
		return 0, fmt.Errorf("point the dataset at the target: %w", err)
	}
	return switched, tx.Commit(ctx)
}

// ─── Replicas ─────────────────────────────────────────────────────────

// PendingReplica is an asset a dataset wants copied to its replica
// connection, with the dataset's vault retention.
type PendingReplica struct {
	StoredAsset
	ReplicaConnectionID string
	VaultRetentionDays  int
}

// PendingReplicas finds assets of protected datasets with no verified
// copy at the dataset's replica connection, skipping ones that failed
// recently.
func (r *CopyRepository) PendingReplicas(ctx context.Context, now time.Time, limit int) ([]PendingReplica, error) {
	rows, err := r.db.Pool.Query(ctx, `
		WITH wanted AS (
		    SELECT DISTINCT ON (d.tenant_id, x.asset_id, d.replica_connection_id)
		           d.tenant_id, x.asset_id, d.replica_connection_id, d.vault_retention_days
		    FROM datasets AS d
		    CROSS JOIN LATERAL (
		        SELECT vm.asset_id FROM dataset_version_members AS vm JOIN dataset_versions AS dv ON dv.id = vm.version_id WHERE vm.tenant_id = d.tenant_id AND dv.dataset_id = d.id
		        UNION
		        SELECT coalesce(m.asset_id, o.current_asset_id) FROM dataset_members AS m
		        JOIN catalog_objects AS o ON o.id = m.object_id AND o.tenant_id = m.tenant_id
		        WHERE m.tenant_id = d.tenant_id AND m.dataset_id = d.id) AS x(asset_id)
		    WHERE d.replica_connection_id IS NOT NULL AND x.asset_id IS NOT NULL
		    ORDER BY d.tenant_id, x.asset_id, d.replica_connection_id, d.vault_retention_days DESC)
		SELECT `+storedAssetColumns+`, w.replica_connection_id, w.vault_retention_days
		FROM wanted AS w
		JOIN assets AS a ON a.id = w.asset_id AND a.tenant_id = w.tenant_id AND a.status = 'AVAILABLE'
		JOIN storage_connections AS c ON c.id = w.replica_connection_id AND c.status = 'ACTIVE'
		WHERE coalesce(a.storage_connection_id, 'platform') <> w.replica_connection_id
		  -- An archived object is copied once it is restored (docs/tiering.md).
		  AND (a.storage_tier IN ('STANDARD', 'INFREQUENT') OR a.readable_until > $1)
		  AND NOT EXISTS (SELECT 1 FROM asset_replicas AS r WHERE r.tenant_id = w.tenant_id AND r.asset_id = w.asset_id
		                  AND r.connection_id = w.replica_connection_id)
		  AND NOT EXISTS (SELECT 1 FROM replica_failures AS f WHERE f.tenant_id = w.tenant_id AND f.asset_id = w.asset_id
		                  AND f.connection_id = w.replica_connection_id AND f.retry_after > $1)
		LIMIT $2`, now, limit)
	if err != nil {
		return nil, fmt.Errorf("find pending replicas: %w", err)
	}
	return collect(rows, func(row pgx.Row) (PendingReplica, error) {
		var p PendingReplica
		err := row.Scan(&p.AssetID, &p.TenantID, &p.ConnectionID, &p.ObjectKey, &p.VersionID, &p.ETag, &p.SizeBytes, &p.SHA256,
			&p.ContentType, &p.SharedFrom, &p.Adoption, &p.ReplicaConnectionID, &p.VaultRetentionDays)
		return p, err
	})
}

func (r *CopyRepository) PutReplica(ctx context.Context, rep model.AssetReplica) error {
	tx, err := r.db.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `
		INSERT INTO asset_replicas (tenant_id, asset_id, connection_id, object_key, version_id, etag, size_bytes, checksum_sha256,
		    lock_mode, locked_until, replicated_at, verified_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		ON CONFLICT (tenant_id, asset_id, connection_id) DO UPDATE SET object_key = EXCLUDED.object_key,
		    version_id = EXCLUDED.version_id, etag = EXCLUDED.etag, size_bytes = EXCLUDED.size_bytes,
		    checksum_sha256 = EXCLUDED.checksum_sha256, lock_mode = EXCLUDED.lock_mode, locked_until = EXCLUDED.locked_until,
		    replicated_at = EXCLUDED.replicated_at, verified_at = EXCLUDED.verified_at`,
		rep.TenantID, rep.AssetID, rep.ConnectionID, rep.ObjectKey, rep.VersionID, rep.ETag, rep.SizeBytes, rep.ChecksumSHA256,
		rep.LockMode, rep.LockedUntil, rep.ReplicatedAt, rep.VerifiedAt); err != nil {
		return fmt.Errorf("record replica: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM replica_failures WHERE tenant_id = $1 AND asset_id = $2 AND connection_id = $3`,
		rep.TenantID, rep.AssetID, rep.ConnectionID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ReplicaFailed backs a failed replica off, longer each time.
func (r *CopyRepository) ReplicaFailed(ctx context.Context, tenantID, assetID, connectionID, message string, now time.Time) error {
	_, err := r.db.Pool.Exec(ctx, `
		INSERT INTO replica_failures (tenant_id, asset_id, connection_id, error, attempts, retry_after)
		VALUES ($1, $2, $3, $4, 1, $5::timestamptz + interval '5 minutes')
		ON CONFLICT (tenant_id, asset_id, connection_id) DO UPDATE SET error = EXCLUDED.error,
		    attempts = replica_failures.attempts + 1,
		    retry_after = $5::timestamptz + least(interval '5 minutes' * power(2, replica_failures.attempts), interval '1 day')`,
		tenantID, assetID, connectionID, message, now)
	if err != nil {
		return fmt.Errorf("record replica failure: %w", err)
	}
	return nil
}

const replicaColumns = `tenant_id, asset_id, connection_id, object_key, version_id, etag, size_bytes, checksum_sha256,
	lock_mode, locked_until, replicated_at, verified_at`

func scanReplica(row pgx.Row) (model.AssetReplica, error) {
	var rep model.AssetReplica
	err := row.Scan(&rep.TenantID, &rep.AssetID, &rep.ConnectionID, &rep.ObjectKey, &rep.VersionID, &rep.ETag, &rep.SizeBytes,
		&rep.ChecksumSHA256, &rep.LockMode, &rep.LockedUntil, &rep.ReplicatedAt, &rep.VerifiedAt)
	return rep, err
}

// VerifiedReplica finds an asset's most recent verified copy elsewhere.
func (r *CopyRepository) VerifiedReplica(ctx context.Context, tenantID, assetID string) (model.AssetReplica, bool, error) {
	rep, err := scanReplica(r.db.Pool.QueryRow(ctx, `SELECT `+replicaColumns+` FROM asset_replicas
		WHERE tenant_id = $1 AND asset_id = $2 ORDER BY verified_at DESC LIMIT 1`, tenantID, assetID))
	if errors.Is(err, pgx.ErrNoRows) {
		return rep, false, nil
	}
	if err != nil {
		return rep, false, fmt.Errorf("find replica: %w", err)
	}
	return rep, true, nil
}

// ReplicationStatus is how far a dataset's replica has got.
type ReplicationStatus struct {
	Wanted, Replicated, Failing int64
	ReplicatedBytes             int64
	NewestReplicaAt             *time.Time
	// OldestPendingSince is when the oldest asset still without a copy
	// was published: the recovery point is at least that old.
	OldestPendingSince *time.Time
	LastError          string
}

func (r *CopyRepository) ReplicationStatus(ctx context.Context, tenantID, datasetID, connectionID string) (ReplicationStatus, error) {
	var s ReplicationStatus
	err := r.db.Pool.QueryRow(ctx, `
		WITH wanted AS (`+datasetAssets+`)
		SELECT count(*),
		       count(rep.asset_id),
		       count(f.asset_id),
		       coalesce(sum(rep.size_bytes), 0),
		       max(rep.verified_at),
		       min(coalesce(a.published_at, a.created_at)) FILTER (WHERE rep.asset_id IS NULL),
		       coalesce(max(f.error), '')
		FROM wanted AS w
		JOIN assets AS a ON a.id = w.asset_id AND a.tenant_id = $1 AND a.status = 'AVAILABLE'
		LEFT JOIN asset_replicas AS rep ON rep.tenant_id = $1 AND rep.asset_id = w.asset_id AND rep.connection_id = $3
		LEFT JOIN replica_failures AS f ON f.tenant_id = $1 AND f.asset_id = w.asset_id AND f.connection_id = $3`,
		tenantID, datasetID, connectionID).Scan(&s.Wanted, &s.Replicated, &s.Failing, &s.ReplicatedBytes, &s.NewestReplicaAt,
		&s.OldestPendingSince, &s.LastError)
	if err != nil {
		return s, fmt.Errorf("replication status: %w", err)
	}
	return s, nil
}

// EscrowMissing reports a dataset that keeps escrow copies and lacks a
// verified one of the asset at its replica connection.
func (r *CopyRepository) EscrowMissing(ctx context.Context, tenantID, assetID string) (string, bool, error) {
	var name string
	err := r.db.Pool.QueryRow(ctx, `
		SELECT d.name FROM datasets AS d
		WHERE d.tenant_id = $1 AND d.escrow
		  AND (EXISTS (SELECT 1 FROM dataset_version_members AS vm JOIN dataset_versions AS dv ON dv.id = vm.version_id WHERE vm.tenant_id = d.tenant_id AND dv.dataset_id = d.id AND vm.asset_id = $2)
		       OR EXISTS (SELECT 1 FROM dataset_members AS m LEFT JOIN catalog_objects AS o ON o.id = m.object_id
		                  WHERE m.tenant_id = d.tenant_id AND m.dataset_id = d.id AND coalesce(m.asset_id, o.current_asset_id) = $2))
		  AND (d.replica_connection_id IS NULL OR NOT EXISTS (SELECT 1 FROM asset_replicas AS rep
		       WHERE rep.tenant_id = d.tenant_id AND rep.asset_id = $2 AND rep.connection_id = d.replica_connection_id))
		LIMIT 1`, tenantID, assetID).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("check escrow: %w", err)
	}
	return name, true, nil
}

// ─── Scrubs ───────────────────────────────────────────────────────────

// ScrubDue finds assets of datasets that scrub every N days whose last
// scrub is older than that, or that were never scrubbed.
func (r *CopyRepository) ScrubDue(ctx context.Context, now time.Time, limit int) ([]StoredAsset, error) {
	rows, err := r.db.Pool.Query(ctx, `
		WITH wanted AS (
		    SELECT d.tenant_id, x.asset_id, min(d.scrub_days) AS days
		    FROM datasets AS d
		    CROSS JOIN LATERAL (
		        SELECT vm.asset_id FROM dataset_version_members AS vm JOIN dataset_versions AS dv ON dv.id = vm.version_id WHERE vm.tenant_id = d.tenant_id AND dv.dataset_id = d.id
		        UNION
		        SELECT coalesce(m.asset_id, o.current_asset_id) FROM dataset_members AS m
		        JOIN catalog_objects AS o ON o.id = m.object_id AND o.tenant_id = m.tenant_id
		        WHERE m.tenant_id = d.tenant_id AND m.dataset_id = d.id) AS x(asset_id)
		    WHERE d.scrub_days > 0 AND x.asset_id IS NOT NULL
		    GROUP BY 1, 2)
		SELECT `+storedAssetColumns+`
		FROM wanted AS w
		JOIN assets AS a ON a.id = w.asset_id AND a.tenant_id = w.tenant_id AND a.status = 'AVAILABLE'
		LEFT JOIN scrub_results AS s ON s.tenant_id = w.tenant_id AND s.asset_id = w.asset_id
		WHERE (s.checked_at IS NULL OR s.checked_at < $1::timestamptz - make_interval(days => w.days))
		  -- Archived objects are not re-read: it would need a restore, and bill retrieval.
		  AND (a.storage_tier IN ('STANDARD', 'INFREQUENT') OR a.readable_until > $1)
		ORDER BY s.checked_at NULLS FIRST
		LIMIT $2`, now, limit)
	if err != nil {
		return nil, fmt.Errorf("find scrubs due: %w", err)
	}
	return collect(rows, func(row pgx.Row) (StoredAsset, error) { return scanStoredAsset(row, false) })
}

func (r *CopyRepository) PutScrub(ctx context.Context, tenantID string, s model.ScrubResult) error {
	_, err := r.db.Pool.Exec(ctx, `
		INSERT INTO scrub_results (tenant_id, asset_id, checked_at, ok, detail, repaired) VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (tenant_id, asset_id) DO UPDATE SET checked_at = EXCLUDED.checked_at, ok = EXCLUDED.ok,
		    detail = EXCLUDED.detail, repaired = EXCLUDED.repaired`,
		tenantID, s.AssetID, s.CheckedAt, s.OK, s.Detail, s.Repaired)
	if err != nil {
		return fmt.Errorf("record scrub: %w", err)
	}
	return nil
}

// ScrubFailures lists a dataset's members whose last scrub failed.
func (r *CopyRepository) ScrubFailures(ctx context.Context, tenantID, datasetID string, limit int) ([]model.ScrubResult, error) {
	rows, err := r.db.Pool.Query(ctx, `
		WITH wanted AS (`+datasetAssets+`)
		SELECT s.asset_id, coalesce(o.path, ''), s.checked_at, s.ok, s.detail, s.repaired
		FROM scrub_results AS s
		JOIN wanted AS w ON w.asset_id = s.asset_id
		LEFT JOIN assets AS a ON a.id = s.asset_id AND a.tenant_id = s.tenant_id
		LEFT JOIN catalog_objects AS o ON o.id = a.object_id AND o.tenant_id = a.tenant_id
		WHERE s.tenant_id = $1 AND (NOT s.ok OR s.repaired)
		ORDER BY s.checked_at DESC LIMIT $3`, tenantID, datasetID, limit)
	if err != nil {
		return nil, fmt.Errorf("list scrub failures: %w", err)
	}
	return collect(rows, func(row pgx.Row) (model.ScrubResult, error) {
		var s model.ScrubResult
		err := row.Scan(&s.AssetID, &s.Path, &s.CheckedAt, &s.OK, &s.Detail, &s.Repaired)
		return s, err
	})
}

// Repaired points an asset at the copy written back over its damaged
// object.
func (r *CopyRepository) Repaired(ctx context.Context, tenantID, assetID, versionID, etag string, now time.Time) error {
	_, err := r.db.Pool.Exec(ctx, `UPDATE assets SET storage_version_id = NULLIF($3, ''), storage_etag = NULLIF($4, ''), updated_at = $5
		WHERE tenant_id = $1 AND id = $2`, tenantID, assetID, versionID, etag, now)
	if err != nil {
		return fmt.Errorf("record repair: %w", err)
	}
	return nil
}

// ─── Dataset protection and clean points ──────────────────────────────

func (r *CopyRepository) Protection(ctx context.Context, tenantID, datasetID string) (model.Protection, string, error) {
	var p model.Protection
	var connection string
	err := r.db.Pool.QueryRow(ctx, `
		SELECT coalesce(replica_connection_id, ''), escrow, vault_retention_days, scrub_days, coalesce(clean_version_id, ''),
		       coalesce(storage_connection_id, '')
		FROM datasets WHERE tenant_id = $1 AND id = $2`, tenantID, datasetID).
		Scan(&p.ReplicaConnectionID, &p.Escrow, &p.VaultRetentionDays, &p.ScrubDays, &p.CleanVersionID, &connection)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, "", model.ErrDatasetNotFound
	}
	return p, connection, err
}

func (r *CopyRepository) SetCleanVersion(ctx context.Context, tenantID, datasetID, versionID string) error {
	tag, err := r.db.Pool.Exec(ctx, `UPDATE datasets SET clean_version_id = NULLIF($3, '') WHERE tenant_id = $1 AND id = $2
		AND ($3 = '' OR EXISTS (SELECT 1 FROM dataset_versions WHERE id = $3 AND dataset_id = $2 AND tenant_id = $1))`,
		tenantID, datasetID, versionID)
	if err != nil {
		return fmt.Errorf("set clean point: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return model.ErrDatasetVersionNotFound
	}
	return nil
}

// CleanDiff is one path that differs between the clean point and now.
type CleanDiff struct {
	Path       string    `json:"path"`
	Change     string    `json:"change"`
	AssetID    string    `json:"asset_id,omitempty"`
	CleanAsset string    `json:"clean_asset_id,omitempty"`
	UploadedBy string    `json:"uploaded_by,omitempty"`
	UploadedAt time.Time `json:"uploaded_at,omitempty"`
}

// DiffSinceClean compares the dataset's current members with its clean
// point, by path, naming who uploaded each version that changed.
func (r *CopyRepository) DiffSinceClean(ctx context.Context, tenantID, datasetID, cleanVersionID string, limit int) ([]CleanDiff, error) {
	rows, err := r.db.Pool.Query(ctx, `
		WITH clean AS (
		    SELECT path, asset_id FROM dataset_version_members WHERE tenant_id = $1 AND version_id = $3),
		now AS (
		    SELECT o.path, coalesce(m.asset_id, o.current_asset_id) AS asset_id
		    FROM dataset_members AS m JOIN catalog_objects AS o ON o.id = m.object_id AND o.tenant_id = m.tenant_id
		    WHERE m.tenant_id = $1 AND m.dataset_id = $2)
		SELECT coalesce(n.path, c.path),
		       CASE WHEN c.path IS NULL THEN 'ADDED'
		            WHEN n.path IS NULL OR a.id IS NULL OR a.status <> 'AVAILABLE' THEN 'REMOVED' ELSE 'CHANGED' END,
		       coalesce(n.asset_id, ''), coalesce(c.asset_id, ''), coalesce(u.owner_id, ''), coalesce(a.created_at, 'epoch')
		FROM now AS n FULL JOIN clean AS c ON c.path = n.path
		LEFT JOIN assets AS a ON a.id = n.asset_id AND a.tenant_id = $1
		LEFT JOIN upload_sessions AS u ON u.id = a.upload_id
		WHERE c.path IS NULL OR n.path IS NULL OR c.asset_id IS DISTINCT FROM n.asset_id OR a.id IS NULL OR a.status <> 'AVAILABLE'
		ORDER BY 1 LIMIT $4`, tenantID, datasetID, cleanVersionID, limit)
	if err != nil {
		return nil, fmt.Errorf("diff against the clean point: %w", err)
	}
	return collect(rows, func(row pgx.Row) (CleanDiff, error) {
		var d CleanDiff
		err := row.Scan(&d.Path, &d.Change, &d.AssetID, &d.CleanAsset, &d.UploadedBy, &d.UploadedAt)
		return d, err
	})
}

// ─── Export bundle ────────────────────────────────────────────────────

// Rows streams a query's rows as JSON objects to fn, for the export
// bundle. Each query names its own columns.
func (r *CopyRepository) Rows(ctx context.Context, query string, args []any, fn func(map[string]any) error) error {
	rows, err := r.db.Pool.Query(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("export query: %w", err)
	}
	defer rows.Close()
	fields := rows.FieldDescriptions()
	for rows.Next() {
		values, err := rows.Values()
		if err != nil {
			return err
		}
		out := make(map[string]any, len(values))
		for i, v := range values {
			out[fields[i].Name] = v
		}
		if err := fn(out); err != nil {
			return err
		}
	}
	return rows.Err()
}

// ExportSchedule is an organization's scheduled export bundle.
type ExportSchedule struct {
	TenantID      string     `json:"-"`
	ConnectionID  string     `json:"connection_id"`
	IntervalHours int        `json:"interval_hours"`
	LastJobID     string     `json:"last_job_id,omitempty"`
	LastRunAt     *time.Time `json:"last_run_at,omitempty"`
	UpdatedBy     string     `json:"updated_by"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

func (r *CopyRepository) Schedule(ctx context.Context, tenantID string) (ExportSchedule, bool, error) {
	var s ExportSchedule
	err := r.db.Pool.QueryRow(ctx, `SELECT tenant_id, connection_id, interval_hours, coalesce(last_job_id, ''), last_run_at,
		updated_by, updated_at FROM tenant_export_schedule WHERE tenant_id = $1`, tenantID).
		Scan(&s.TenantID, &s.ConnectionID, &s.IntervalHours, &s.LastJobID, &s.LastRunAt, &s.UpdatedBy, &s.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return s, false, nil
	}
	return s, err == nil, err
}

func (r *CopyRepository) PutSchedule(ctx context.Context, s ExportSchedule) error {
	_, err := r.db.Pool.Exec(ctx, `
		INSERT INTO tenant_export_schedule (tenant_id, connection_id, interval_hours, updated_by, updated_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (tenant_id) DO UPDATE SET connection_id = EXCLUDED.connection_id, interval_hours = EXCLUDED.interval_hours,
		    updated_by = EXCLUDED.updated_by, updated_at = EXCLUDED.updated_at`,
		s.TenantID, s.ConnectionID, s.IntervalHours, s.UpdatedBy, s.UpdatedAt)
	if err != nil {
		return fmt.Errorf("save export schedule: %w", err)
	}
	return nil
}

func (r *CopyRepository) DeleteSchedule(ctx context.Context, tenantID string) error {
	_, err := r.db.Pool.Exec(ctx, `DELETE FROM tenant_export_schedule WHERE tenant_id = $1`, tenantID)
	return err
}

// DueSchedules lists schedules whose next bundle is due.
func (r *CopyRepository) DueSchedules(ctx context.Context, now time.Time) ([]ExportSchedule, error) {
	rows, err := r.db.Pool.Query(ctx, `SELECT tenant_id, connection_id, interval_hours, coalesce(last_job_id, ''), last_run_at,
		updated_by, updated_at FROM tenant_export_schedule
		WHERE last_run_at IS NULL OR last_run_at < $1::timestamptz - make_interval(hours => interval_hours)`, now)
	if err != nil {
		return nil, fmt.Errorf("due export schedules: %w", err)
	}
	return collect(rows, func(row pgx.Row) (ExportSchedule, error) {
		var s ExportSchedule
		err := row.Scan(&s.TenantID, &s.ConnectionID, &s.IntervalHours, &s.LastJobID, &s.LastRunAt, &s.UpdatedBy, &s.UpdatedAt)
		return s, err
	})
}

func (r *CopyRepository) MarkScheduleRun(ctx context.Context, tenantID, jobID string, now time.Time) error {
	_, err := r.db.Pool.Exec(ctx, `UPDATE tenant_export_schedule SET last_job_id = $2, last_run_at = $3 WHERE tenant_id = $1`,
		tenantID, jobID, now)
	return err
}

// ─── Mass deletion ────────────────────────────────────────────────────

// DeletionCount is how many deletions one actor asked for recently.
type DeletionCount struct {
	TenantID string
	Actor    string
	Count    int64
}

// RecentDeletions counts deletions requested since a time, by actor.
func (r *CopyRepository) RecentDeletions(ctx context.Context, since time.Time, atLeast int) ([]DeletionCount, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT tenant_id, requested_by, count(*) FROM deletion_operations
		WHERE created_at >= $1 AND reason <> 'POLICY_EXPIRED' AND requested_by <> ''
		GROUP BY 1, 2 HAVING count(*) >= $2`, since, atLeast)
	if err != nil {
		return nil, fmt.Errorf("count recent deletions: %w", err)
	}
	return collect(rows, func(row pgx.Row) (DeletionCount, error) {
		var d DeletionCount
		err := row.Scan(&d.TenantID, &d.Actor, &d.Count)
		return d, err
	})
}
