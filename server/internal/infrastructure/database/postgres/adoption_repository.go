package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sanjeev0303/object-storage-platform/internal/dto/event"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// AdoptionRepository keeps adoption jobs and their entries (migration
// 000031), and publishes adopted objects.
type AdoptionRepository struct {
	db *DB
}

func NewAdoptionRepository(db *DB) *AdoptionRepository {
	return &AdoptionRepository{db: db}
}

const adoptionJobColumns = `id, tenant_id, connection_id, dataset, source, source_ref, COALESCE(inline_source, ''), mode,
	require_digest, state, entries, adopted, rejected, failed, bytes, error, created_by, created_at, updated_at, finished_at`

func scanAdoptionJob(row pgx.Row) (model.AdoptionJob, error) {
	var j model.AdoptionJob
	err := row.Scan(&j.ID, &j.TenantID, &j.ConnectionID, &j.Dataset, &j.Source, &j.SourceRef, &j.InlineSource, &j.Mode,
		&j.RequireDigest, &j.State, &j.Entries, &j.Adopted, &j.Rejected, &j.Failed, &j.Bytes, &j.Error, &j.CreatedBy,
		&j.CreatedAt, &j.UpdatedAt, &j.FinishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return j, model.ErrAdoptionNotFound
	}
	return j, err
}

func (r *AdoptionRepository) CreateJob(ctx context.Context, j model.AdoptionJob) error {
	var inline *string
	if j.InlineSource != "" {
		inline = &j.InlineSource
	}
	_, err := r.db.Pool.Exec(ctx, `
		INSERT INTO adoption_jobs (id, tenant_id, connection_id, dataset, source, source_ref, inline_source, mode,
			require_digest, state, created_by, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'QUEUED', $10, $11, $11)`,
		j.ID, j.TenantID, j.ConnectionID, j.Dataset, j.Source, j.SourceRef, inline, j.Mode, j.RequireDigest, j.CreatedBy, j.CreatedAt)
	if err != nil {
		return fmt.Errorf("create adoption job: %w", err)
	}
	return nil
}

func (r *AdoptionRepository) GetJob(ctx context.Context, tenantID, id string) (model.AdoptionJob, error) {
	return scanAdoptionJob(r.db.Pool.QueryRow(ctx, `SELECT `+adoptionJobColumns+` FROM adoption_jobs WHERE tenant_id = $1 AND id = $2`, tenantID, id))
}

func (r *AdoptionRepository) ListJobs(ctx context.Context, tenantID string, limit int) ([]model.AdoptionJob, error) {
	rows, err := r.db.Pool.Query(ctx, `SELECT `+adoptionJobColumns+` FROM adoption_jobs WHERE tenant_id = $1
		ORDER BY created_at DESC LIMIT $2`, tenantID, limit)
	if err != nil {
		return nil, fmt.Errorf("list adoption jobs: %w", err)
	}
	return collect(rows, scanAdoptionJob)
}

// ClaimJob leases the job unattended longest that still has work.
func (r *AdoptionRepository) ClaimJob(ctx context.Context, owner string, now time.Time, lease time.Duration) (model.AdoptionJob, bool, error) {
	j, err := scanAdoptionJob(r.db.Pool.QueryRow(ctx, `
		UPDATE adoption_jobs SET lease_owner = $1, lease_expires_at = $2::timestamptz + $3::interval, updated_at = $2
		WHERE id = (
			SELECT id FROM adoption_jobs
			WHERE state IN ('QUEUED', 'READING', 'RUNNING') AND (lease_expires_at IS NULL OR lease_expires_at < $2)
			ORDER BY updated_at LIMIT 1 FOR UPDATE SKIP LOCKED)
		RETURNING `+adoptionJobColumns, owner, now, lease.String()))
	if errors.Is(err, model.ErrAdoptionNotFound) {
		return j, false, nil
	}
	return j, err == nil, err
}

// SetState moves a job on while owner holds it; terminal states release it.
func (r *AdoptionRepository) SetState(ctx context.Context, id, owner, state, message string, now time.Time) error {
	tag, err := r.db.Pool.Exec(ctx, `
		UPDATE adoption_jobs SET state = $3, error = $4, updated_at = $5,
			finished_at = CASE WHEN $3 IN ('COMPLETED', 'FAILED', 'CANCELLED') THEN $5::timestamptz END,
			lease_owner = CASE WHEN $3 IN ('COMPLETED', 'FAILED', 'CANCELLED') THEN NULL ELSE lease_owner END,
			lease_expires_at = CASE WHEN $3 IN ('COMPLETED', 'FAILED', 'CANCELLED') THEN NULL ELSE lease_expires_at END
		WHERE id = $1 AND lease_owner = $2 AND state NOT IN ('COMPLETED', 'FAILED', 'CANCELLED')`, id, owner, state, message, now)
	if err != nil {
		return fmt.Errorf("move adoption job: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return model.ErrLeaseLost
	}
	return nil
}

// Release gives a running job back for the next cycle.
func (r *AdoptionRepository) Release(ctx context.Context, id, owner string) error {
	_, err := r.db.Pool.Exec(ctx, `UPDATE adoption_jobs SET lease_owner = NULL, lease_expires_at = NULL
		WHERE id = $1 AND lease_owner = $2`, id, owner)
	return err
}

// Cancel stops a job that has not finished.
func (r *AdoptionRepository) Cancel(ctx context.Context, tenantID, id string, now time.Time) error {
	tag, err := r.db.Pool.Exec(ctx, `
		UPDATE adoption_jobs SET state = 'CANCELLED', finished_at = $3, updated_at = $3, lease_owner = NULL, lease_expires_at = NULL
		WHERE tenant_id = $1 AND id = $2 AND state IN ('QUEUED', 'READING', 'RUNNING')`, tenantID, id, now)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return model.ErrAdoptionNotFound
	}
	return nil
}

// AddEntries stores a batch of a job's entries. Reading a source again
// after a crash adds nothing twice: entries are keyed by line.
func (r *AdoptionRepository) AddEntries(ctx context.Context, entries []model.AdoptionEntry, now time.Time) error {
	batch := &pgx.Batch{}
	for _, e := range entries {
		expected, _ := json.Marshal(e.Expected)
		var metadata []byte
		if len(e.Metadata) > 0 {
			metadata, _ = json.Marshal(e.Metadata)
		}
		batch.Queue(`
			INSERT INTO adoption_entries (job_id, line, tenant_id, object_key, url, path, size_bytes, content_type, expected,
				metadata, state, code, detail, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
			ON CONFLICT (job_id, line) DO NOTHING`,
			e.JobID, e.Line, e.TenantID, e.ObjectKey, e.URL, e.Path, e.SizeBytes, e.ContentType, expected, metadata,
			e.State, e.Code, e.Detail, now)
	}
	if err := r.db.Pool.SendBatch(ctx, batch).Close(); err != nil {
		return fmt.Errorf("store adoption entries: %w", err)
	}
	return nil
}

const adoptionEntryColumns = `job_id, line, tenant_id, object_key, url, path, size_bytes, content_type, expected, metadata,
	state, code, detail, asset_id, upload_id, sha256, attempts, updated_at`

func scanAdoptionEntry(row pgx.Row) (model.AdoptionEntry, error) {
	var e model.AdoptionEntry
	var expected, metadata []byte
	err := row.Scan(&e.JobID, &e.Line, &e.TenantID, &e.ObjectKey, &e.URL, &e.Path, &e.SizeBytes, &e.ContentType, &expected,
		&metadata, &e.State, &e.Code, &e.Detail, &e.AssetID, &e.UploadID, &e.SHA256, &e.Attempts, &e.UpdatedAt)
	if err != nil {
		return e, err
	}
	_ = json.Unmarshal(expected, &e.Expected)
	if len(metadata) > 0 {
		_ = json.Unmarshal(metadata, &e.Metadata)
	}
	return e, nil
}

func (r *AdoptionRepository) PendingEntries(ctx context.Context, jobID string, limit int) ([]model.AdoptionEntry, error) {
	rows, err := r.db.Pool.Query(ctx, `SELECT `+adoptionEntryColumns+` FROM adoption_entries
		WHERE job_id = $1 AND state = 'PENDING' ORDER BY line LIMIT $2`, jobID, limit)
	if err != nil {
		return nil, fmt.Errorf("list pending entries: %w", err)
	}
	return collect(rows, scanAdoptionEntry)
}

// Entries pages through a job's entries for its result file. An imported
// entry's state follows its upload: ADOPTED once published, REJECTED when
// processing refused it.
func (r *AdoptionRepository) Entries(ctx context.Context, tenantID, jobID string, after int64, limit int) ([]model.AdoptionEntry, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT e.job_id, e.line, e.tenant_id, e.object_key, e.url, e.path, e.size_bytes, e.content_type, e.expected, e.metadata,
		       CASE WHEN e.state = 'IMPORTED' AND a.status = 'AVAILABLE' THEN 'ADOPTED'
		            WHEN e.state = 'IMPORTED' AND a.status = 'REJECTED' THEN 'REJECTED' ELSE e.state END,
		       CASE WHEN e.state = 'IMPORTED' AND a.status = 'REJECTED' THEN COALESCE(a.rejection_reason, '') ELSE e.code END,
		       e.detail, COALESCE(NULLIF(e.asset_id, ''), a.id, ''), e.upload_id, e.sha256, e.attempts, e.updated_at
		FROM adoption_entries AS e
		LEFT JOIN assets AS a ON e.upload_id <> '' AND a.upload_id = e.upload_id AND a.tenant_id = e.tenant_id
		WHERE e.tenant_id = $1 AND e.job_id = $2 AND e.line > $3
		ORDER BY e.line LIMIT $4`, tenantID, jobID, after, limit)
	if err != nil {
		return nil, fmt.Errorf("list adoption entries: %w", err)
	}
	return collect(rows, scanAdoptionEntry)
}

func (r *AdoptionRepository) FinishEntry(ctx context.Context, e model.AdoptionEntry, now time.Time) error {
	_, err := r.db.Pool.Exec(ctx, `
		UPDATE adoption_entries SET state = $3, code = $4, detail = $5, asset_id = $6, upload_id = $7, sha256 = $8,
			attempts = $9, updated_at = $10, size_bytes = GREATEST(size_bytes, $11)
		WHERE job_id = $1 AND line = $2`,
		e.JobID, e.Line, e.State, e.Code, truncateText(e.Detail, 500), e.AssetID, e.UploadID, e.SHA256, e.Attempts, now, e.SizeBytes)
	if err != nil {
		return fmt.Errorf("record entry outcome: %w", err)
	}
	return nil
}

// Count refreshes a job's counters from its entries.
func (r *AdoptionRepository) Count(ctx context.Context, jobID string, now time.Time) (model.AdoptionJob, error) {
	return scanAdoptionJob(r.db.Pool.QueryRow(ctx, `
		UPDATE adoption_jobs AS j SET entries = c.total, adopted = c.adopted, rejected = c.rejected, failed = c.failed,
			bytes = c.bytes, updated_at = $2
		FROM (SELECT count(*) AS total,
			count(*) FILTER (WHERE state IN ('ADOPTED', 'IMPORTED')) AS adopted,
			count(*) FILTER (WHERE state IN ('REJECTED', 'SKIPPED')) AS rejected,
			count(*) FILTER (WHERE state = 'FAILED') AS failed,
			COALESCE(sum(size_bytes) FILTER (WHERE state IN ('ADOPTED', 'IMPORTED')), 0) AS bytes
			FROM adoption_entries WHERE job_id = $1) AS c
		WHERE j.id = $1
		RETURNING j.id, j.tenant_id, j.connection_id, j.dataset, j.source, j.source_ref, COALESCE(j.inline_source, ''), j.mode,
			j.require_digest, j.state, j.entries, j.adopted, j.rejected, j.failed, j.bytes, j.error, j.created_by,
			j.created_at, j.updated_at, j.finished_at`, jobID, now))
}

func truncateText(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// AdoptedObject is an object that passed every check, to publish where it is.
type AdoptedObject struct {
	TenantID, OwnerID, ConnectionID, Dataset string
	Path, ObjectKey, FileName, ContentType   string
	SizeBytes                                int64
	SHA256, ETag, VersionID                  string
	Metadata                                 map[string]string
	Mode                                     string
	Digests                                  model.Digests
	// The checks' evidence, for the processing record.
	DetectedType, Scanner, ScannerVersion, Coverage string
	At                                              time.Time
}

// AdoptObject publishes an object in place, in one transaction: an upload
// session that records where it came from, its catalog object and the
// asset, a finished processing record carrying the checks' evidence, its
// digests, the catalog version and the organization's usage.
func (r *AdoptionRepository) AdoptObject(ctx context.Context, o AdoptedObject) (string, error) {
	assetID := event.NewID()
	uploadID := event.NewID()
	metadata, err := encodeMetadata(o.Metadata)
	if err != nil {
		return "", err
	}
	err = withTx(ctx, r.db, func(tx pgx.Tx) error {
		session := &model.UploadSession{TenantID: o.TenantID, OwnerID: o.OwnerID, Dataset: o.Dataset, Path: o.Path, CreatedAt: o.At}
		if err := claimObject(ctx, tx, session); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO upload_sessions (id, tenant_id, owner_id, object_key, file_name, content_type, expected_size, checksum_sha256,
				status, expires_at, created_at, updated_at, completed_at, dataset, quota_state, reserved_bytes, path, object_id,
				metadata, storage_connection_id, staging_connection_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'VERIFIED', $9::timestamptz + interval '1 second', $9, $9, $9, NULLIF($10, ''),
				'CONSUMED', 0, $11, $12, $13, $14, $14)`,
			uploadID, o.TenantID, o.OwnerID, o.ObjectKey, o.FileName, o.ContentType, o.SizeBytes, o.SHA256, o.At, o.Dataset,
			o.Path, session.ObjectID, metadata, o.ConnectionID); err != nil {
			return fmt.Errorf("record adopted upload: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO assets (id, tenant_id, owner_id, upload_id, object_key, file_name, content_type, size_bytes, checksum_sha256,
				status, created_at, updated_at, published_at, storage_etag, storage_version_id, dataset, object_id, metadata,
				storage_connection_id, staging_connection_id, adoption)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'AVAILABLE', $10, $10, $10, NULLIF($11, ''), NULLIF($12, ''), NULLIF($13, ''),
				$14, $15, $16, $16, $17)`,
			assetID, o.TenantID, o.OwnerID, uploadID, o.ObjectKey, o.FileName, o.ContentType, o.SizeBytes, o.SHA256, o.At,
			o.ETag, o.VersionID, o.Dataset, session.ObjectID, metadata, o.ConnectionID, o.Mode); err != nil {
			return fmt.Errorf("record adopted asset: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO processing_jobs (id, tenant_id, asset_id, upload_id, stage, state, attempt_count, max_attempts, next_attempt_at,
				checksum_algorithm, checksum_verified_at, detected_content_type, scanner, scan_result, scanned_at, promotion_key,
				promoted_at, started_at, completed_at, created_at, updated_at, verification_status, checksum_source,
				validation_status, promotion_status, storage_connection_id, scanner_version, content_checks)
			VALUES ($1, $2, $3, $4, 'CLEANUP', 'SUCCEEDED', 1, 1, $5, 'SHA-256', $5, $6, NULLIF($7, ''),
				CASE WHEN $7 = '' THEN 'NOT_SCANNED' ELSE 'CLEAN' END, $5, $8, $5, $5, $5, $5, $5, 'PASSED', 'COMPUTED',
				'PASSED', 'PUBLISHED', $9, NULLIF($10, ''), NULLIF($11, ''))`,
			event.NewID(), o.TenantID, assetID, uploadID, o.At, o.DetectedType, o.Scanner, o.ObjectKey, o.ConnectionID,
			o.ScannerVersion, o.Coverage); err != nil {
			return fmt.Errorf("record adoption checks: %w", err)
		}
		digests, _ := json.Marshal(o.Digests)
		if _, err := tx.Exec(ctx, `INSERT INTO asset_digests (asset_id, tenant_id, digests, created_at) VALUES ($1, $2, $3, $4)`,
			assetID, o.TenantID, digests, o.At); err != nil {
			return fmt.Errorf("record digests: %w", err)
		}
		events, err := publishVersion(ctx, tx, o.TenantID, assetID, o.At)
		if err != nil {
			return err
		}
		if err := addUsage(ctx, tx, o.TenantID, usageDelta{stored: o.SizeBytes, assets: 1}, o.At); err != nil {
			return err
		}
		return appendAudits(ctx, tx, events)
	})
	if err != nil {
		return "", err
	}
	return assetID, nil
}

// AdoptedKey reports whether a live asset already adopts the object.
func (r *AdoptionRepository) AdoptedKey(ctx context.Context, tenantID, connectionID, key string) (string, error) {
	var id string
	err := r.db.Pool.QueryRow(ctx, `SELECT id FROM assets WHERE tenant_id = $1 AND storage_connection_id = $2 AND object_key = $3
		AND adoption IS NOT NULL AND status NOT IN ('DELETED', 'REJECTED') LIMIT 1`, tenantID, connectionID, key).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// Digests returns the digests stored for assets, by asset.
func (r *AdoptionRepository) Digests(ctx context.Context, tenantID string, assetIDs []string) (map[string]model.Digests, error) {
	rows, err := r.db.Pool.Query(ctx, `SELECT asset_id, digests FROM asset_digests WHERE tenant_id = $1 AND asset_id = ANY($2)`,
		tenantID, assetIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]model.Digests{}
	for rows.Next() {
		var id string
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, err
		}
		var d model.Digests
		_ = json.Unmarshal(raw, &d)
		out[id] = d
	}
	return out, rows.Err()
}

// PutDigests stores digests computed for an export.
func (r *AdoptionRepository) PutDigests(ctx context.Context, tenantID, assetID string, d model.Digests, at time.Time) error {
	raw, _ := json.Marshal(d)
	_, err := r.db.Pool.Exec(ctx, `INSERT INTO asset_digests (asset_id, tenant_id, digests, created_at) VALUES ($1, $2, $3, $4)
		ON CONFLICT (asset_id) DO UPDATE SET digests = asset_digests.digests || EXCLUDED.digests`, assetID, tenantID, raw, at)
	return err
}
