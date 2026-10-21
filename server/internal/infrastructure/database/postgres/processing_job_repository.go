package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sanjeev0303/object-storage-platform/internal/dto/event"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// ProcessingJobRepository is the processing queue in Neon PostgreSQL.
type ProcessingJobRepository struct {
	db *DB
}

var _ interfaces.ProcessingJobRepository = (*ProcessingJobRepository)(nil)

func NewProcessingJobRepository(db *DB) *ProcessingJobRepository {
	return &ProcessingJobRepository{db: db}
}

const jobColumns = `
	id, tenant_id, asset_id, upload_id, stage, state,
	attempt_count, max_attempts, next_attempt_at, COALESCE(last_error, ''),
	COALESCE(lease_owner, ''), lease_expires_at,
	COALESCE(verification_status, ''), COALESCE(checksum_source, ''),
	COALESCE(checksum_algorithm, ''), checksum_verified_at,
	COALESCE(validation_status, ''), COALESCE(validation_reason, ''), COALESCE(detected_content_type, ''),
	COALESCE(scanner, ''), COALESCE(scan_result, ''), COALESCE(scan_signature, ''), COALESCE(scan_error, ''), scanned_at,
	COALESCE(promotion_status, ''), COALESCE(promotion_key, ''), promoted_at,
	started_at, completed_at, created_at, updated_at
`

func scanJob(row pgx.Row) (model.ProcessingJob, error) {
	var job model.ProcessingJob
	var stage, state string

	err := row.Scan(
		&job.ID, &job.TenantID, &job.AssetID, &job.UploadID, &stage, &state,
		&job.AttemptCount, &job.MaxAttempts, &job.NextAttemptAt, &job.LastError,
		&job.LeaseOwner, &job.LeaseExpiresAt,
		&job.VerificationStatus, &job.ChecksumSource,
		&job.ChecksumAlgorithm, &job.ChecksumVerifiedAt,
		&job.ValidationStatus, &job.ValidationReason, &job.DetectedContentType,
		&job.Scanner, &job.ScanResult, &job.ScanSignature, &job.ScanError, &job.ScannedAt,
		&job.PromotionStatus, &job.PromotionKey, &job.PromotedAt,
		&job.StartedAt, &job.CompletedAt, &job.CreatedAt, &job.UpdatedAt,
	)
	job.Stage = model.ProcessingStage(stage)
	job.State = model.ProcessingState(state)
	return job, err
}

func (r *ProcessingJobRepository) EnqueueDue(ctx context.Context, now time.Time, maxAttempts int) (int, error) {
	tx, err := r.db.Pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin enqueue: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	created, err := enqueue(ctx, tx, now, maxAttempts, "")
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit enqueue: %w", err)
	}
	return created, nil
}

func (r *ProcessingJobRepository) EnqueueUpload(ctx context.Context, tenantID, uploadID string, now time.Time, maxAttempts int) (model.ProcessingJob, error) {
	tx, err := r.db.Pool.Begin(ctx)
	if err != nil {
		return model.ProcessingJob{}, fmt.Errorf("begin enqueue: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	var status string
	err = tx.QueryRow(ctx, `SELECT status FROM upload_sessions WHERE tenant_id = $1 AND id = $2`,
		tenantID, uploadID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.ProcessingJob{}, model.ErrUploadNotFound
	}
	if err != nil {
		return model.ProcessingJob{}, fmt.Errorf("read upload: %w", err)
	}
	if status != string(model.UploadStatusCompleted) && status != string(model.UploadStatusVerified) {
		return model.ProcessingJob{}, model.ErrInvalidUploadState
	}

	if _, err := enqueue(ctx, tx, now, maxAttempts, uploadID); err != nil {
		return model.ProcessingJob{}, err
	}
	job, err := scanJob(tx.QueryRow(ctx, `
		SELECT `+jobColumns+` FROM processing_jobs WHERE tenant_id = $1 AND upload_id = $2
	`, tenantID, uploadID))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.ProcessingJob{}, model.ErrAssetNotFound
	}
	if err != nil {
		return model.ProcessingJob{}, fmt.Errorf("read processing job: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return model.ProcessingJob{}, fmt.Errorf("commit enqueue: %w", err)
	}
	return job, nil
}

// enqueue creates the missing assets and jobs of completed uploads, or of
// one upload when uploadID is not empty, and returns how many jobs it made.
func enqueue(ctx context.Context, tx pgx.Tx, now time.Time, maxAttempts int, uploadID string) (int, error) {
	// Asset first, then its job, in one transaction: a completed upload
	// never has an asset without a job. The UNIQUE upload_id / asset_id
	// constraints make repeated or concurrent runs harmless.
	_, err := tx.Exec(ctx, `
		INSERT INTO assets (
			id, tenant_id, owner_id, upload_id, object_key, file_name,
			content_type, size_bytes, checksum_sha256, dataset, status, created_at, updated_at,
			object_id, metadata, storage_connection_id, staging_connection_id
		)
		SELECT gen_random_uuid()::text, s.tenant_id, s.owner_id, s.id, s.object_key, s.file_name,
		       s.content_type, s.expected_size, s.checksum_sha256, s.dataset, 'QUARANTINED', $1, $1,
		       s.object_id, s.metadata, s.storage_connection_id, s.staging_connection_id
		FROM upload_sessions AS s
		WHERE s.status IN ('COMPLETED', 'VERIFIED')
		  AND ($2 = '' OR s.id = $2)
		  AND NOT EXISTS (SELECT 1 FROM assets AS a WHERE a.upload_id = s.id)
		  -- The binding trigger refuses a deleted connection, and one refused
		  -- row would fail this statement for every tenant. Such an upload
		  -- stays COMPLETED, without an asset, for an operator to look at.
		  AND NOT EXISTS (SELECT 1 FROM storage_connections AS c
		                  WHERE c.id IN (s.storage_connection_id, s.staging_connection_id) AND c.status = 'DELETED')
		ON CONFLICT DO NOTHING
	`, now, uploadID)
	if err != nil {
		return 0, fmt.Errorf("create assets for completed uploads: %w", err)
	}

	tag, err := tx.Exec(ctx, `
		INSERT INTO processing_jobs (
			id, tenant_id, asset_id, upload_id, stage, state,
			max_attempts, next_attempt_at, created_at, updated_at, storage_connection_id
		)
		SELECT gen_random_uuid()::text, a.tenant_id, a.id, a.upload_id, 'VERIFY', 'PENDING', $2, $1, $1, $1,
		       a.storage_connection_id
		FROM assets AS a
		WHERE a.status IN ('QUARANTINED', 'PROCESSING')
		  AND ($3 = '' OR a.upload_id = $3)
		  AND NOT EXISTS (SELECT 1 FROM processing_jobs AS j WHERE j.asset_id = a.id)
		ON CONFLICT DO NOTHING
	`, now, maxAttempts, uploadID)
	if err != nil {
		return 0, fmt.Errorf("create processing jobs: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

func (r *ProcessingJobRepository) Claim(ctx context.Context, owner string, now time.Time, lease time.Duration, limit int) ([]model.ProcessingJob, error) {
	if owner == "" || lease <= 0 || limit <= 0 {
		return nil, fmt.Errorf("claim needs an owner, a positive lease and a positive limit")
	}

	// Same claim as reconciliation: SKIP LOCKED keeps concurrent workers
	// apart, and an expired lease makes a crashed worker's job claimable.
	rows, err := r.db.Pool.Query(ctx, `
		WITH due AS (
			SELECT id AS due_id
			FROM processing_jobs
			WHERE state = 'PENDING'
			  AND next_attempt_at <= $2
			  AND (lease_expires_at IS NULL OR lease_expires_at <= $2)
			ORDER BY next_attempt_at
			LIMIT $4
			FOR UPDATE SKIP LOCKED
		)
		UPDATE processing_jobs AS j
		SET lease_owner = $1,
		    lease_expires_at = $2 + make_interval(secs => $3),
		    attempt_count = j.attempt_count + 1,
		    started_at = COALESCE(j.started_at, $2),
		    updated_at = $2
		FROM due
		WHERE j.id = due.due_id
		RETURNING `+jobColumns,
		owner, now, lease.Seconds(), limit)
	if err != nil {
		return nil, fmt.Errorf("claim processing jobs: %w", err)
	}
	defer rows.Close()

	jobs := make([]model.ProcessingJob, 0, limit)
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, fmt.Errorf("scan claimed job: %w", err)
		}
		jobs = append(jobs, job)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate claimed jobs: %w", err)
	}
	return jobs, nil
}

func (r *ProcessingJobRepository) ClaimJob(ctx context.Context, jobID, owner string, now time.Time, lease time.Duration) (model.ProcessingJob, bool, error) {
	if owner == "" || lease <= 0 {
		return model.ProcessingJob{}, false, fmt.Errorf("claim needs an owner and a positive lease")
	}
	// Concurrent claims of the same row wait on each other; the loser
	// re-checks the lease condition and matches nothing.
	job, err := scanJob(r.db.Pool.QueryRow(ctx, `
		UPDATE processing_jobs AS j
		SET lease_owner = $2,
		    lease_expires_at = $3::timestamptz + make_interval(secs => $4),
		    attempt_count = j.attempt_count + 1,
		    started_at = COALESCE(j.started_at, $3),
		    updated_at = $3
		WHERE j.id = $1
		  AND j.state = 'PENDING'
		  AND j.next_attempt_at <= $3
		  AND (j.lease_expires_at IS NULL OR j.lease_expires_at <= $3)
		RETURNING `+jobColumns,
		jobID, owner, now, lease.Seconds()))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.ProcessingJob{}, false, nil
	}
	if err != nil {
		return model.ProcessingJob{}, false, fmt.Errorf("claim processing job: %w", err)
	}
	return job, true, nil
}

func (r *ProcessingJobRepository) SaveProgress(ctx context.Context, job model.ProcessingJob, owner string, now time.Time) error {
	tag, err := r.db.Pool.Exec(ctx, `
		UPDATE processing_jobs
		SET stage = $4,
		    checksum_algorithm = NULLIF($5, ''),
		    checksum_verified_at = $6,
		    detected_content_type = NULLIF($7, ''),
		    scanner = NULLIF($8, ''),
		    scan_result = NULLIF($9, ''),
		    scan_signature = NULLIF($10, ''),
		    scanned_at = $11,
		    promotion_key = NULLIF($12, ''),
		    promoted_at = $13,
		    verification_status = NULLIF($14, ''),
		    checksum_source = NULLIF($15, ''),
		    validation_status = NULLIF($16, ''),
		    validation_reason = NULLIF($17, ''),
		    scan_error = NULLIF($18, ''),
		    promotion_status = NULLIF($19, ''),
		    updated_at = $3
		WHERE id = $1
		  AND state = 'PENDING'
		  AND lease_owner = $2
		  AND lease_expires_at > $3
	`, job.ID, owner, now, string(job.Stage),
		job.ChecksumAlgorithm, job.ChecksumVerifiedAt, job.DetectedContentType,
		job.Scanner, job.ScanResult, job.ScanSignature, job.ScannedAt,
		job.PromotionKey, job.PromotedAt,
		job.VerificationStatus, job.ChecksumSource, job.ValidationStatus, job.ValidationReason,
		job.ScanError, job.PromotionStatus)
	if err != nil {
		return fmt.Errorf("save processing progress: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return model.ErrLeaseLost
	}
	return nil
}

func (r *ProcessingJobRepository) Reschedule(ctx context.Context, jobID string, owner string, now time.Time, next time.Time, lastError string) error {
	tag, err := r.db.Pool.Exec(ctx, `
		UPDATE processing_jobs
		SET next_attempt_at = $4,
		    last_error = NULLIF($5, ''),
		    lease_owner = NULL,
		    lease_expires_at = NULL,
		    updated_at = $3
		WHERE id = $1
		  AND state = 'PENDING'
		  AND lease_owner = $2
		  AND lease_expires_at > $3
	`, jobID, owner, now, next, lastError)
	if err != nil {
		return fmt.Errorf("reschedule processing job: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return model.ErrLeaseLost
	}
	return nil
}

func (r *ProcessingJobRepository) Finish(ctx context.Context, jobID string, owner string, now time.Time, state model.ProcessingState, lastError string) error {
	if state == model.ProcessingPending {
		return fmt.Errorf("finish state must be final, got %s", state)
	}

	tx, err := r.db.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin finish processing job: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	var tenantID, uploadID string
	err = tx.QueryRow(ctx, `
		UPDATE processing_jobs
		SET state = $4,
		    last_error = NULLIF($5, ''),
		    lease_owner = NULL,
		    lease_expires_at = NULL,
		    completed_at = $3,
		    updated_at = $3
		WHERE id = $1
		  AND state = 'PENDING'
		  AND lease_owner = $2
		  AND lease_expires_at > $3
		RETURNING tenant_id, upload_id
	`, jobID, owner, now, string(state), lastError).Scan(&tenantID, &uploadID)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.ErrLeaseLost
	}
	if err != nil {
		return fmt.Errorf("finish processing job: %w", err)
	}

	// A bulk ingestion entry waits for this outcome. Its event commits with
	// the outcome, so the entry cannot miss it; other uploads get none.
	var bulk bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM ingestion_entries WHERE upload_id = $1)`, uploadID).Scan(&bulk); err != nil {
		return fmt.Errorf("find ingestion entry of upload: %w", err)
	}
	if bulk {
		if err := insertOutbox(ctx, tx, event.NewUploadProcessed(tenantID, uploadID, now, "")); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit finish processing job: %w", err)
	}
	return nil
}

func (r *ProcessingJobRepository) GetByAsset(ctx context.Context, tenantID, assetID string) (model.ProcessingJob, error) {
	job, err := scanJob(r.db.Pool.QueryRow(ctx, `
		SELECT `+jobColumns+`
		FROM processing_jobs
		WHERE tenant_id = $1 AND asset_id = $2
	`, tenantID, assetID))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.ProcessingJob{}, model.ErrAssetNotFound
	}
	if err != nil {
		return model.ProcessingJob{}, fmt.Errorf("get processing job: %w", err)
	}
	return job, nil
}

func (r *ProcessingJobRepository) Requeue(ctx context.Context, tenantID, assetID string, now time.Time) (model.ProcessingJob, error) {
	job, err := scanJob(r.db.Pool.QueryRow(ctx, `
		UPDATE processing_jobs
		SET state = 'PENDING',
		    attempt_count = 0,
		    next_attempt_at = $3,
		    last_error = NULL,
		    completed_at = NULL,
		    updated_at = $3
		WHERE tenant_id = $1
		  AND asset_id = $2
		  AND state = 'EXHAUSTED'
		RETURNING `+jobColumns,
		tenantID, assetID, now))
	if errors.Is(err, pgx.ErrNoRows) {
		if _, getErr := r.GetByAsset(ctx, tenantID, assetID); getErr != nil {
			return model.ProcessingJob{}, getErr
		}
		return model.ProcessingJob{}, model.ErrInvalidAssetState
	}
	if err != nil {
		return model.ProcessingJob{}, fmt.Errorf("requeue processing job: %w", err)
	}
	return job, nil
}

func (r *ProcessingJobRepository) Backlog(ctx context.Context, now time.Time) (map[string]int, error) {
	var due, leased, expired, exhausted int
	err := r.db.Pool.QueryRow(ctx, `
		SELECT
			COUNT(*) FILTER (WHERE state = 'PENDING' AND next_attempt_at <= $1 AND lease_owner IS NULL),
			COUNT(*) FILTER (WHERE state = 'PENDING' AND lease_expires_at > $1),
			COUNT(*) FILTER (WHERE state = 'PENDING' AND lease_expires_at <= $1),
			COUNT(*) FILTER (WHERE state = 'EXHAUSTED')
		FROM processing_jobs
	`, now).Scan(&due, &leased, &expired, &exhausted)
	if err != nil {
		return nil, fmt.Errorf("count processing backlog: %w", err)
	}
	return map[string]int{"due": due, "leased": leased, "lease_expired": expired, "exhausted": exhausted}, nil
}
