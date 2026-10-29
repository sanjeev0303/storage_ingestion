package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// IngestionRepository stores bulk ingestion in PostgreSQL. Counter and
// quota accounting lives in batch_accounting.go.
type IngestionRepository struct {
	db *DB
}

func NewIngestionRepository(db *DB) *IngestionRepository {
	return &IngestionRepository{db: db}
}

var _ interfaces.IngestionRepository = (*IngestionRepository)(nil)

const datasetColumns = `id, tenant_id, name, completion_policy, status, max_bytes, reserved_bytes,
	stored_bytes, created_by, created_at, updated_at, ready_at,
	description, tags, metadata, lifecycle_status, revision, members_revision, member_count, next_version,
	COALESCE(current_version_id, ''), COALESCE(profile_id, ''), profile_version, COALESCE(latest_report_id, ''),
	derivation, COALESCE(updated_by, ''), archived_at, COALESCE(storage_connection_id, ''),
	COALESCE(parent_dataset_id, ''), COALESCE(branch_name, ''), COALESCE(fork_version_id, ''), COALESCE(merge_base_version_id, '')`

func scanDataset(row pgx.Row) (model.Dataset, error) {
	var d model.Dataset
	var policy, status, lifecycle string
	var metadata, derivation []byte
	err := row.Scan(&d.ID, &d.TenantID, &d.Name, &policy, &status, &d.MaxBytes, &d.ReservedBytes,
		&d.StoredBytes, &d.CreatedBy, &d.CreatedAt, &d.UpdatedAt, &d.ReadyAt,
		&d.Description, &d.Tags, &metadata, &lifecycle, &d.Revision, &d.MembersRevision, &d.MemberCount, &d.NextVersion,
		&d.CurrentVersionID, &d.ProfileID, &d.ProfileVersion, &d.LatestReportID,
		&derivation, &d.UpdatedBy, &d.ArchivedAt, &d.StorageConnectionID,
		&d.ParentID, &d.BranchName, &d.ForkVersionID, &d.MergeBaseVersionID)
	if err != nil {
		return d, err
	}
	d.CompletionPolicy, d.Status, d.Lifecycle = model.CompletionPolicy(policy), model.DatasetStatus(status), model.DatasetLifecycle(lifecycle)
	if d.Metadata, err = decodeMetadata(metadata); err != nil {
		return d, err
	}
	if len(derivation) > 0 {
		d.Derivation = &model.Derivation{}
		if err := json.Unmarshal(derivation, d.Derivation); err != nil {
			return d, fmt.Errorf("decode derivation: %w", err)
		}
	}
	return d, nil
}

const ingestionJobColumns = `id, tenant_id, dataset_id, created_by, idempotency_key, state, max_concurrency,
	COALESCE(manifest_source, ''), COALESCE(manifest_object_key, ''), COALESCE(manifest_etag, ''),
	manifest_bytes, COALESCE(manifest_sha256, ''), manifest_complete, parsed_lines, parsed_bytes,
	total_entries, invalid_entries, pending_entries, scheduled_entries, uploading_entries,
	uploaded_entries, processing_entries, published_entries, retryable_entries, failed_entries,
	cancelled_entries, required_failed, expected_bytes, stored_bytes, COALESCE(error, ''),
	created_at, updated_at, finalized_at, completed_at, cancelled_at`

func scanIngestionJob(row pgx.Row) (model.IngestionJob, error) {
	var j model.IngestionJob
	var state string
	c := &j.Counters
	err := row.Scan(&j.ID, &j.TenantID, &j.DatasetID, &j.CreatedBy, &j.IdempotencyKey, &state, &j.MaxConcurrency,
		&j.ManifestSource, &j.ManifestObjectKey, &j.ManifestETag,
		&j.ManifestBytes, &j.ManifestSHA256, &j.ManifestComplete, &j.ParsedLines, &j.ParsedBytes,
		&c.Total, &c.Invalid, &c.Pending, &c.Scheduled, &c.Uploading,
		&c.Uploaded, &c.Processing, &c.Published, &c.Retryable, &c.Failed,
		&c.Cancelled, &c.RequiredFailed, &c.ExpectedBytes, &c.StoredBytes, &j.Error,
		&j.CreatedAt, &j.UpdatedAt, &j.FinalizedAt, &j.CompletedAt, &j.CancelledAt)
	j.State = model.JobState(state)
	return j, err
}

const entryColumns = `job_id, position, tenant_id, dataset_id, COALESCE(path, ''), COALESCE(path_key, ''),
	size_bytes, COALESCE(checksum_sha256, ''), COALESCE(content_type, ''), metadata, required,
	COALESCE(strategy, ''), state, COALESCE(upload_id, ''), upload_generation, COALESCE(asset_id, ''),
	attempts, manual_retries, next_attempt_at, lease_expires_at, reserved_bytes, stored_bytes,
	COALESCE(error_code, ''), COALESCE(error_detail, ''), created_at, updated_at`

func scanEntry(row pgx.Row) (model.IngestionEntry, error) {
	var e model.IngestionEntry
	var state string
	var metadata []byte
	err := row.Scan(&e.JobID, &e.Position, &e.TenantID, &e.DatasetID, &e.Path, &e.PathKey,
		&e.SizeBytes, &e.ChecksumSHA256, &e.ContentType, &metadata, &e.Required,
		&e.Strategy, &state, &e.UploadID, &e.UploadGeneration, &e.AssetID,
		&e.Attempts, &e.ManualRetries, &e.NextAttemptAt, &e.LeaseExpiresAt, &e.ReservedBytes, &e.StoredBytes,
		&e.ErrorCode, &e.ErrorDetail, &e.CreatedAt, &e.UpdatedAt)
	e.State = model.EntryState(state)
	if len(metadata) > 0 {
		e.Metadata = metadata
	}
	return e, err
}

func collect[T any](rows pgx.Rows, scan func(pgx.Row) (T, error)) ([]T, error) {
	defer rows.Close()
	var out []T
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// inTx runs fn in a transaction, committed only if fn succeeds.
func (r *IngestionRepository) inTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := r.db.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin ingestion transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit ingestion transaction: %w", err)
	}
	return nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// ─── Datasets ─────────────────────────────────────────────────────────

func (r *IngestionRepository) CreateDataset(ctx context.Context, d model.Dataset) (model.Dataset, bool, error) {
	stored, err := scanDataset(r.db.Pool.QueryRow(ctx, `
		INSERT INTO datasets (id, tenant_id, name, completion_policy, status, max_bytes, created_by, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 'OPEN', $5, $6, $7, $7)
		ON CONFLICT (tenant_id, name) DO NOTHING
		RETURNING `+datasetColumns,
		d.ID, d.TenantID, d.Name, string(d.CompletionPolicy), d.MaxBytes, d.CreatedBy, d.CreatedAt))
	if err == nil {
		return stored, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return model.Dataset{}, false, fmt.Errorf("create dataset: %w", err)
	}
	existing, err := scanDataset(r.db.Pool.QueryRow(ctx,
		`SELECT `+datasetColumns+` FROM datasets WHERE tenant_id = $1 AND name = $2`, d.TenantID, d.Name))
	if err != nil {
		return model.Dataset{}, false, fmt.Errorf("read existing dataset: %w", err)
	}
	if existing.CompletionPolicy != d.CompletionPolicy || existing.MaxBytes != d.MaxBytes {
		return existing, false, model.ErrDatasetConflict
	}
	return existing, false, nil
}

func (r *IngestionRepository) GetDataset(ctx context.Context, tenantID, datasetID string) (model.Dataset, error) {
	d, err := scanDataset(r.db.Pool.QueryRow(ctx,
		`SELECT `+datasetColumns+` FROM datasets WHERE tenant_id = $1 AND id = $2`, tenantID, datasetID))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Dataset{}, model.ErrDatasetNotFound
	}
	if err != nil {
		return model.Dataset{}, fmt.Errorf("get dataset: %w", err)
	}
	return d, nil
}

// ─── Jobs ─────────────────────────────────────────────────────────────

func (r *IngestionRepository) CreateJob(ctx context.Context, job model.IngestionJob, maxActiveJobs int) (model.IngestionJob, bool, error) {
	var stored model.IngestionJob
	created := false
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		existing, err := scanIngestionJob(tx.QueryRow(ctx,
			`SELECT `+ingestionJobColumns+` FROM ingestion_jobs WHERE tenant_id = $1 AND idempotency_key = $2`,
			job.TenantID, job.IdempotencyKey))
		if err == nil {
			if existing.DatasetID != job.DatasetID || existing.MaxConcurrency != job.MaxConcurrency {
				return model.ErrIngestionConflict
			}
			stored = existing
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("find job by idempotency key: %w", err)
		}

		// Lock order: tenant usage, then dataset, then job.
		if err := lockUsage(ctx, tx, job.TenantID, job.CreatedAt); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `
			UPDATE ingestion_tenant_usage SET active_jobs = active_jobs + 1, updated_at = $3
			WHERE tenant_id = $1 AND active_jobs < $2
		`, job.TenantID, maxActiveJobs, job.CreatedAt)
		if err != nil {
			return fmt.Errorf("count active job: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("%w: at most %d active ingestion jobs", model.ErrQuotaExceeded, maxActiveJobs)
		}
		if _, err := lockDataset(ctx, tx, job.TenantID, job.DatasetID); err != nil {
			return err
		}
		stored, err = scanIngestionJob(tx.QueryRow(ctx, `
			INSERT INTO ingestion_jobs (id, tenant_id, dataset_id, created_by, idempotency_key, state,
			                            max_concurrency, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, 'PENDING_VALIDATION', $6, $7, $7)
			RETURNING `+ingestionJobColumns,
			job.ID, job.TenantID, job.DatasetID, job.CreatedBy, job.IdempotencyKey, job.MaxConcurrency, job.CreatedAt))
		if err != nil {
			return fmt.Errorf("insert ingestion job: %w", err)
		}
		created = true
		return refreshDataset(ctx, tx, job.DatasetID, job.CreatedAt)
	})
	if isUniqueViolation(err) {
		// A concurrent request with the same key won; return its job.
		existing, getErr := scanIngestionJob(r.db.Pool.QueryRow(ctx,
			`SELECT `+ingestionJobColumns+` FROM ingestion_jobs WHERE tenant_id = $1 AND idempotency_key = $2`,
			job.TenantID, job.IdempotencyKey))
		if getErr != nil {
			return model.IngestionJob{}, false, fmt.Errorf("read concurrently created job: %w", getErr)
		}
		if existing.DatasetID != job.DatasetID || existing.MaxConcurrency != job.MaxConcurrency {
			return model.IngestionJob{}, false, model.ErrIngestionConflict
		}
		return existing, false, nil
	}
	if err != nil {
		return model.IngestionJob{}, false, err
	}
	return stored, created, nil
}

func (r *IngestionRepository) GetJob(ctx context.Context, tenantID, jobID string) (model.IngestionJob, error) {
	j, err := scanIngestionJob(r.db.Pool.QueryRow(ctx,
		`SELECT `+ingestionJobColumns+` FROM ingestion_jobs WHERE tenant_id = $1 AND id = $2`, tenantID, jobID))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.IngestionJob{}, model.ErrIngestionNotFound
	}
	if err != nil {
		return model.IngestionJob{}, fmt.Errorf("get ingestion job: %w", err)
	}
	return j, nil
}

// ListJobs pages the tenant's jobs newest first. The cursor is
// "<created_at unix nanoseconds>:<id>" of the last job of the previous page.
func (r *IngestionRepository) ListJobs(ctx context.Context, tenantID, datasetID, after string, limit int) ([]model.IngestionJob, error) {
	afterTime, afterID := time.Now().Add(100*365*24*time.Hour), ""
	if after != "" {
		nanos, id, ok := strings.Cut(after, ":")
		n, err := strconv.ParseInt(nanos, 10, 64)
		if !ok || err != nil {
			return nil, fmt.Errorf("invalid job cursor")
		}
		afterTime, afterID = time.Unix(0, n).UTC(), id
	}
	rows, err := r.db.Pool.Query(ctx, `
		SELECT `+ingestionJobColumns+` FROM ingestion_jobs
		WHERE tenant_id = $1 AND ($2 = '' OR dataset_id = $2)
		  AND (created_at, id) < ($3, $4)
		ORDER BY created_at DESC, id DESC
		LIMIT $5
	`, tenantID, datasetID, afterTime, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("list ingestion jobs: %w", err)
	}
	return collect(rows, scanIngestionJob)
}

// ─── Manifests ────────────────────────────────────────────────────────

func (r *IngestionRepository) StartManifest(ctx context.Context, tenantID, jobID string, m interfaces.ManifestSource, now time.Time, events ...model.OutboxEvent) (model.IngestionJob, error) {
	var job model.IngestionJob
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		var err error
		job, err = scanIngestionJob(tx.QueryRow(ctx, `
			UPDATE ingestion_jobs
			SET state = 'VALIDATING', manifest_source = $3, manifest_object_key = NULLIF($4, ''),
			    manifest_etag = NULLIF($5, ''), manifest_bytes = $6, manifest_sha256 = NULLIF($8, ''), updated_at = $7
			WHERE tenant_id = $1 AND id = $2 AND state = 'PENDING_VALIDATION'
			RETURNING `+ingestionJobColumns,
			tenantID, jobID, m.Source, m.ObjectKey, m.ETag, m.SizeBytes, now, m.SHA256))
		if errors.Is(err, pgx.ErrNoRows) {
			job, err = scanIngestionJob(tx.QueryRow(ctx,
				`SELECT `+ingestionJobColumns+` FROM ingestion_jobs WHERE tenant_id = $1 AND id = $2`, tenantID, jobID))
			if errors.Is(err, pgx.ErrNoRows) {
				return model.ErrIngestionNotFound
			}
			if err != nil {
				return fmt.Errorf("read ingestion job: %w", err)
			}
			// Starting the same manifest again changes nothing.
			if job.State == model.JobValidating && job.ManifestSource == m.Source && job.ManifestObjectKey == m.ObjectKey {
				return nil
			}
			return model.ErrInvalidJobState
		}
		if err != nil {
			return fmt.Errorf("start manifest: %w", err)
		}
		return insertOutbox(ctx, tx, events...)
	})
	return job, err
}

func (r *IngestionRepository) AppendEntries(ctx context.Context, job model.IngestionJob, entries []model.IngestionEntry, cp interfaces.Checkpoint, maxPending int64, now time.Time) (interfaces.AppendResult, error) {
	var result interfaces.AppendResult
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		// Lock order: tenant usage, then job. Batches of one job are
		// serialized by the job row; the checkpoint makes a replayed batch
		// a no-op.
		if err := lockUsage(ctx, tx, job.TenantID, now); err != nil {
			return err
		}
		var state string
		var parsed int64
		err := tx.QueryRow(ctx, `SELECT state, parsed_lines FROM ingestion_jobs WHERE id = $1 FOR UPDATE`, job.ID).
			Scan(&state, &parsed)
		if err != nil {
			return fmt.Errorf("lock ingestion job: %w", err)
		}
		if model.JobState(state) != model.JobValidating {
			return model.ErrInvalidJobState
		}

		var valid, invalid []model.IngestionEntry
		for _, e := range entries {
			if e.State == model.EntryInvalid {
				invalid = append(invalid, e)
			} else {
				valid = append(valid, e)
			}
		}

		inserted, err := insertEntries(ctx, tx, job, valid, now)
		if err != nil {
			return err
		}
		// A valid line that was not inserted was stored before (a replay),
		// or its path is live in the dataset (a duplicate).
		var missed []int64
		for _, e := range valid {
			if _, ok := inserted[e.Position]; !ok {
				missed = append(missed, e.Position)
			}
		}
		stored, err := existingPositions(ctx, tx, job.ID, missed)
		if err != nil {
			return err
		}
		for _, e := range valid {
			if _, ok := inserted[e.Position]; ok {
				continue
			}
			if stored[e.Position] {
				result.Replayed++
				continue
			}
			result.Duplicates++
			dup := e
			dup.State = model.EntryInvalid
			dup.ErrorCode = model.EntryErrDuplicatePath
			dup.ErrorDetail = "the path is already in this dataset"
			invalid = append(invalid, dup)
		}
		invalidInserted, err := insertEntries(ctx, tx, job, invalid, now)
		if err != nil {
			return err
		}
		// An invalid line not inserted was stored by an earlier submission.
		result.Replayed += len(invalid) - len(invalidInserted)

		var accepted, expected, invalidCount, requiredFailed int64
		for _, e := range valid {
			if _, ok := inserted[e.Position]; ok {
				accepted++
				expected += e.SizeBytes
			}
		}
		for _, e := range invalid {
			if _, ok := invalidInserted[e.Position]; ok {
				invalidCount++
				if e.Required {
					requiredFailed++
				}
			}
		}
		result.Accepted, result.Invalid = int(accepted), int(invalidCount)

		if accepted > 0 {
			tag, err := tx.Exec(ctx, `
				UPDATE ingestion_tenant_usage SET pending_entries = pending_entries + $2, updated_at = $4
				WHERE tenant_id = $1 AND pending_entries + $2 <= $3
			`, job.TenantID, accepted, maxPending, now)
			if err != nil {
				return fmt.Errorf("count pending entries: %w", err)
			}
			if tag.RowsAffected() == 0 {
				return fmt.Errorf("%w: at most %d pending entries per tenant", model.ErrQuotaExceeded, maxPending)
			}
		}
		_, err = tx.Exec(ctx, `
			UPDATE ingestion_jobs
			SET total_entries = total_entries + $2 + $3, invalid_entries = invalid_entries + $3,
			    pending_entries = pending_entries + $2, expected_bytes = expected_bytes + $4,
			    required_failed = required_failed + $5,
			    parsed_lines = GREATEST(parsed_lines, $6), parsed_bytes = GREATEST(parsed_bytes, $7),
			    updated_at = $8
			WHERE id = $1
		`, job.ID, accepted, invalidCount, expected, requiredFailed, cp.Lines, cp.Bytes, now)
		if err != nil {
			return fmt.Errorf("count appended entries: %w", err)
		}
		return nil
	})
	return result, err
}

// insertEntries inserts entries with one statement and returns the
// positions it inserted. Rows that conflict (same position, or a live path
// already in the dataset) are skipped.
func insertEntries(ctx context.Context, tx pgx.Tx, job model.IngestionJob, entries []model.IngestionEntry, now time.Time) (map[int64]struct{}, error) {
	inserted := make(map[int64]struct{}, len(entries))
	if len(entries) == 0 {
		return inserted, nil
	}
	positions := make([]int64, len(entries))
	paths := make([]string, len(entries))
	keys := make([]string, len(entries))
	sizes := make([]int64, len(entries))
	sums := make([]string, len(entries))
	types := make([]string, len(entries))
	metadata := make([]string, len(entries))
	required := make([]bool, len(entries))
	strategies := make([]string, len(entries))
	states := make([]string, len(entries))
	codes := make([]string, len(entries))
	details := make([]string, len(entries))
	for i, e := range entries {
		positions[i], paths[i], keys[i], sizes[i] = e.Position, e.Path, e.PathKey, e.SizeBytes
		sums[i], types[i], metadata[i], required[i] = e.ChecksumSHA256, e.ContentType, string(e.Metadata), e.Required
		strategies[i], states[i], codes[i], details[i] = e.Strategy, string(e.State), e.ErrorCode, e.ErrorDetail
	}
	rows, err := tx.Query(ctx, `
		INSERT INTO ingestion_entries (job_id, position, tenant_id, dataset_id, path, path_key, size_bytes,
		                               checksum_sha256, content_type, metadata, required, strategy, state,
		                               error_code, error_detail, created_at, updated_at)
		SELECT $1, u.position, $2, $3, NULLIF(u.path, ''), NULLIF(u.path_key, ''), u.size_bytes,
		       NULLIF(u.checksum, ''), NULLIF(u.content_type, ''), NULLIF(u.metadata, '')::jsonb, u.required,
		       NULLIF(u.strategy, ''), u.state, NULLIF(u.code, ''), NULLIF(u.detail, ''), $4, $4
		FROM unnest($5::bigint[], $6::text[], $7::text[], $8::bigint[], $9::text[], $10::text[], $11::text[],
		            $12::boolean[], $13::text[], $14::text[], $15::text[], $16::text[])
		     AS u(position, path, path_key, size_bytes, checksum, content_type, metadata, required, strategy, state, code, detail)
		ON CONFLICT DO NOTHING
		RETURNING position
	`, job.ID, job.TenantID, job.DatasetID, now, positions, paths, keys, sizes, sums, types, metadata,
		required, strategies, states, codes, details)
	if err != nil {
		return nil, fmt.Errorf("insert ingestion entries: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var p int64
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("scan inserted position: %w", err)
		}
		inserted[p] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("insert ingestion entries: %w", err)
	}
	return inserted, nil
}

func existingPositions(ctx context.Context, tx pgx.Tx, jobID string, positions []int64) (map[int64]bool, error) {
	found := map[int64]bool{}
	if len(positions) == 0 {
		return found, nil
	}
	rows, err := tx.Query(ctx, `SELECT position FROM ingestion_entries WHERE job_id = $1 AND position = ANY($2)`, jobID, positions)
	if err != nil {
		return nil, fmt.Errorf("find stored positions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var p int64
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		found[p] = true
	}
	return found, rows.Err()
}

func (r *IngestionRepository) CompleteManifest(ctx context.Context, jobID, sha256 string, size int64, now time.Time) error {
	tag, err := r.db.Pool.Exec(ctx, `
		UPDATE ingestion_jobs
		SET manifest_complete = TRUE, manifest_sha256 = COALESCE(NULLIF($2, ''), manifest_sha256),
		    manifest_bytes = $3, updated_at = $4
		WHERE id = $1 AND state = 'VALIDATING'
	`, jobID, sha256, size, now)
	if err != nil {
		return fmt.Errorf("complete manifest: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return model.ErrInvalidJobState
	}
	return nil
}

// FailManifest ends a job whose manifest was rejected. With no entries
// stored it is FAILED at once; otherwise it is CANCELLING with the error
// set, the worker cancels the stored entries, and it settles as FAILED.
func (r *IngestionRepository) FailManifest(ctx context.Context, jobID, reason string, now time.Time) error {
	return r.inTx(ctx, func(tx pgx.Tx) error {
		job, err := scanIngestionJob(tx.QueryRow(ctx, `SELECT `+ingestionJobColumns+` FROM ingestion_jobs WHERE id = $1`, jobID))
		if err != nil {
			return fmt.Errorf("read ingestion job: %w", err)
		}
		if err := lockUsage(ctx, tx, job.TenantID, now); err != nil {
			return err
		}
		if _, err := lockDataset(ctx, tx, job.TenantID, job.DatasetID); err != nil {
			return err
		}
		job, err = scanIngestionJob(tx.QueryRow(ctx, `
			UPDATE ingestion_jobs SET state = 'CANCELLING', error = $2, cancelled_at = $3, updated_at = $3
			WHERE id = $1 AND state IN ('PENDING_VALIDATION', 'VALIDATING')
			RETURNING `+ingestionJobColumns, jobID, clipText(reason), now))
		if errors.Is(err, pgx.ErrNoRows) {
			return model.ErrInvalidJobState
		}
		if err != nil {
			return fmt.Errorf("fail manifest: %w", err)
		}
		_, err = settle(ctx, tx, job, now)
		return err
	})
}

func (r *IngestionRepository) Finalize(ctx context.Context, tenantID, jobID string, maxStorageBytes int64, now time.Time) (model.IngestionJob, error) {
	var job model.IngestionJob
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		var err error
		job, err = scanIngestionJob(tx.QueryRow(ctx,
			`SELECT `+ingestionJobColumns+` FROM ingestion_jobs WHERE tenant_id = $1 AND id = $2`, tenantID, jobID))
		if errors.Is(err, pgx.ErrNoRows) {
			return model.ErrIngestionNotFound
		}
		if err != nil {
			return fmt.Errorf("read ingestion job: %w", err)
		}
		usage, err := lockUsageRow(ctx, tx, tenantID, now)
		if err != nil {
			return err
		}
		dataset, err := lockDataset(ctx, tx, tenantID, job.DatasetID)
		if err != nil {
			return err
		}
		job, err = scanIngestionJob(tx.QueryRow(ctx, `SELECT `+ingestionJobColumns+` FROM ingestion_jobs WHERE id = $1 FOR UPDATE`, jobID))
		if err != nil {
			return fmt.Errorf("lock ingestion job: %w", err)
		}
		switch {
		case job.FinalizedAt != nil:
			return nil // finalized before: the same answer
		case job.State != model.JobValidating:
			return model.ErrInvalidJobState
		case !job.ManifestComplete:
			return model.ErrManifestIncomplete
		}

		if job.Counters.Valid() == 0 {
			job, err = finish(ctx, tx, job, model.JobFailed, "the manifest has no valid entries", now)
			return err
		}
		// A first check of the whole job against the quota. The hard limit
		// is enforced entry by entry when slots are granted.
		if usage.StoredBytes+usage.ReservedBytes+job.Counters.ExpectedBytes > maxStorageBytes {
			return fmt.Errorf("%w: the job's %d bytes do not fit the tenant's remaining quota", model.ErrQuotaExceeded, job.Counters.ExpectedBytes)
		}
		if dataset.MaxBytes > 0 && dataset.StoredBytes+dataset.ReservedBytes+job.Counters.ExpectedBytes > dataset.MaxBytes {
			return fmt.Errorf("%w: the job's %d bytes do not fit the dataset's remaining size", model.ErrQuotaExceeded, job.Counters.ExpectedBytes)
		}
		job, err = scanIngestionJob(tx.QueryRow(ctx, `
			UPDATE ingestion_jobs SET state = 'READY', finalized_at = $2, updated_at = $2
			WHERE id = $1 RETURNING `+ingestionJobColumns, jobID, now))
		if err != nil {
			return fmt.Errorf("finalize ingestion job: %w", err)
		}
		return refreshDataset(ctx, tx, job.DatasetID, now)
	})
	return job, err
}

func (r *IngestionRepository) ClaimManifests(ctx context.Context, owner, jobID string, now time.Time, lease time.Duration, limit int) ([]model.IngestionJob, error) {
	rows, err := r.db.Pool.Query(ctx, `
		WITH due AS (
			SELECT id AS due_id FROM ingestion_jobs
			WHERE state = 'VALIDATING' AND manifest_source = 'STAGED' AND NOT manifest_complete
			  AND (lease_expires_at IS NULL OR lease_expires_at <= $2)
			  AND ($4 = '' OR id = $4)
			ORDER BY updated_at
			LIMIT $5
			FOR UPDATE SKIP LOCKED
		)
		UPDATE ingestion_jobs
		SET lease_owner = $1, lease_expires_at = $2::timestamptz + make_interval(secs => $3), updated_at = $2
		FROM due WHERE id = due.due_id
		RETURNING `+ingestionJobColumns,
		owner, now, lease.Seconds(), jobID, limit)
	if err != nil {
		return nil, fmt.Errorf("claim manifests: %w", err)
	}
	return collect(rows, scanIngestionJob)
}

func (r *IngestionRepository) ExtendManifest(ctx context.Context, jobID, owner string, now time.Time, lease time.Duration) error {
	tag, err := r.db.Pool.Exec(ctx, `
		UPDATE ingestion_jobs SET lease_expires_at = $3::timestamptz + make_interval(secs => $4), updated_at = $3
		WHERE id = $1 AND lease_owner = $2 AND state = 'VALIDATING'
	`, jobID, owner, now, lease.Seconds())
	if err != nil {
		return fmt.Errorf("extend manifest lease: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return model.ErrLeaseLost
	}
	return nil
}

func (r *IngestionRepository) ReleaseManifest(ctx context.Context, jobID, owner string) error {
	_, err := r.db.Pool.Exec(ctx, `
		UPDATE ingestion_jobs SET lease_owner = NULL, lease_expires_at = NULL
		WHERE id = $1 AND lease_owner = $2
	`, jobID, owner)
	if err != nil {
		return fmt.Errorf("release manifest lease: %w", err)
	}
	return nil
}

// ─── Entries ──────────────────────────────────────────────────────────

func (r *IngestionRepository) AttachUpload(ctx context.Context, jobID string, position int64, generation int, uploadID string, now time.Time) error {
	tag, err := r.db.Pool.Exec(ctx, `
		UPDATE ingestion_entries SET upload_id = $4, updated_at = $5
		WHERE job_id = $1 AND position = $2 AND upload_generation = $3
		  AND state IN ('SCHEDULED', 'UPLOADING') AND (upload_id IS NULL OR upload_id = $4)
	`, jobID, position, generation, uploadID, now)
	if err != nil {
		return fmt.Errorf("attach upload: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return model.ErrInvalidEntryState
	}
	return nil
}

func (r *IngestionRepository) GetEntry(ctx context.Context, tenantID, jobID string, position int64) (model.IngestionEntry, error) {
	e, err := scanEntry(r.db.Pool.QueryRow(ctx, `
		SELECT `+entryColumns+` FROM ingestion_entries WHERE tenant_id = $1 AND job_id = $2 AND position = $3
	`, tenantID, jobID, position))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.IngestionEntry{}, model.ErrEntryNotFound
	}
	if err != nil {
		return model.IngestionEntry{}, fmt.Errorf("get ingestion entry: %w", err)
	}
	return e, nil
}

func (r *IngestionRepository) EntryByUpload(ctx context.Context, uploadID string) (model.IngestionEntry, error) {
	e, err := scanEntry(r.db.Pool.QueryRow(ctx,
		`SELECT `+entryColumns+` FROM ingestion_entries WHERE upload_id = $1`, uploadID))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.IngestionEntry{}, model.ErrEntryNotFound
	}
	if err != nil {
		return model.IngestionEntry{}, fmt.Errorf("get entry by upload: %w", err)
	}
	return e, nil
}

func (r *IngestionRepository) ListEntries(ctx context.Context, tenantID, jobID string, states []model.EntryState, after int64, limit int) ([]model.IngestionEntry, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT `+entryColumns+` FROM ingestion_entries
		WHERE tenant_id = $1 AND job_id = $2 AND position > $3
		  AND (cardinality($4::text[]) = 0 OR state = ANY($4))
		ORDER BY position
		LIMIT $5
	`, tenantID, jobID, after, stateStrings(states), limit)
	if err != nil {
		return nil, fmt.Errorf("list ingestion entries: %w", err)
	}
	return collect(rows, scanEntry)
}

func stateStrings(states []model.EntryState) []string {
	out := make([]string, len(states))
	for i, s := range states {
		out[i] = string(s)
	}
	return out
}

// ─── Worker sweeps ────────────────────────────────────────────────────

func (r *IngestionRepository) entries(ctx context.Context, sql string, args ...any) ([]model.IngestionEntry, error) {
	rows, err := r.db.Pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("list ingestion entries: %w", err)
	}
	return collect(rows, scanEntry)
}

func (r *IngestionRepository) ExpiredLeases(ctx context.Context, now time.Time, limit int) ([]model.IngestionEntry, error) {
	return r.entries(ctx, `
		SELECT `+entryColumns+` FROM ingestion_entries
		WHERE state IN ('SCHEDULED', 'UPLOADING') AND lease_expires_at <= $1
		ORDER BY lease_expires_at LIMIT $2
	`, now, limit)
}

func (r *IngestionRepository) DueRetries(ctx context.Context, now time.Time, limit int) ([]model.IngestionEntry, error) {
	return r.entries(ctx, `
		SELECT `+entryColumns+` FROM ingestion_entries
		WHERE state = 'FAILED_RETRYABLE' AND next_attempt_at <= $1
		ORDER BY next_attempt_at LIMIT $2
	`, now, limit)
}

func (r *IngestionRepository) AwaitingOutcome(ctx context.Context, before time.Time, limit int) ([]model.IngestionEntry, error) {
	return r.entries(ctx, `
		SELECT `+entryColumns+` FROM ingestion_entries
		WHERE state IN ('UPLOADED', 'PROCESSING') AND updated_at <= $1
		ORDER BY updated_at LIMIT $2
	`, before, limit)
}

func (r *IngestionRepository) InFlight(ctx context.Context, jobID string, limit int) ([]model.IngestionEntry, error) {
	return r.entries(ctx, `
		SELECT `+entryColumns+` FROM ingestion_entries
		WHERE job_id = $1 AND state IN ('SCHEDULED', 'UPLOADING')
		ORDER BY position LIMIT $2
	`, jobID, limit)
}

func (r *IngestionRepository) JobsInState(ctx context.Context, state model.JobState, limit int) ([]model.IngestionJob, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT `+ingestionJobColumns+` FROM ingestion_jobs WHERE state = $1 ORDER BY updated_at LIMIT $2
	`, string(state), limit)
	if err != nil {
		return nil, fmt.Errorf("list ingestion jobs by state: %w", err)
	}
	return collect(rows, scanIngestionJob)
}

func (r *IngestionRepository) TenantsWithWork(ctx context.Context, limit int) ([]string, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT DISTINCT tenant_id FROM ingestion_jobs
		WHERE state IN ('PENDING_VALIDATION', 'VALIDATING', 'READY', 'RUNNING', 'CANCELLING')
		ORDER BY tenant_id LIMIT $1
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("list tenants with ingestion work: %w", err)
	}
	defer rows.Close()
	var tenants []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		tenants = append(tenants, t)
	}
	return tenants, rows.Err()
}

func clipText(s string) string {
	if len(s) > 500 {
		return s[:500]
	}
	return s
}
