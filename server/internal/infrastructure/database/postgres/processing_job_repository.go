package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

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
	COALESCE(checksum_algorithm, ''), checksum_verified_at, COALESCE(detected_content_type, ''),
	COALESCE(scanner, ''), COALESCE(scan_result, ''), COALESCE(scan_signature, ''), scanned_at,
	COALESCE(promotion_key, ''), promoted_at,
	started_at, completed_at, created_at, updated_at
`

func scanJob(row pgx.Row) (model.ProcessingJob, error) {
	var job model.ProcessingJob
	var stage, state string

	err := row.Scan(
		&job.ID, &job.TenantID, &job.AssetID, &job.UploadID, &stage, &state,
		&job.AttemptCount, &job.MaxAttempts, &job.NextAttemptAt, &job.LastError,
		&job.LeaseOwner, &job.LeaseExpiresAt,
		&job.ChecksumAlgorithm, &job.ChecksumVerifiedAt, &job.DetectedContentType,
		&job.Scanner, &job.ScanResult, &job.ScanSignature, &job.ScannedAt,
		&job.PromotionKey, &job.PromotedAt,
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

	// Asset first, then its job, in one transaction: a completed upload
	// never has an asset without a job. The UNIQUE upload_id / asset_id
	// constraints make repeated or concurrent runs harmless.
	_, err = tx.Exec(ctx, `
		INSERT INTO assets (
			id, tenant_id, owner_id, upload_id, object_key, file_name,
			content_type, size_bytes, checksum_sha256, status, created_at, updated_at
		)
		SELECT gen_random_uuid()::text, s.tenant_id, s.owner_id, s.id, s.object_key, s.file_name,
		       s.content_type, s.expected_size, s.checksum_sha256, 'QUARANTINED', $1, $1
		FROM upload_sessions AS s
		WHERE s.status IN ('COMPLETED', 'VERIFIED')
		  AND NOT EXISTS (SELECT 1 FROM assets AS a WHERE a.upload_id = s.id)
		ON CONFLICT DO NOTHING
	`, now)
	if err != nil {
		return 0, fmt.Errorf("create assets for completed uploads: %w", err)
	}

	tag, err := tx.Exec(ctx, `
		INSERT INTO processing_jobs (
			id, tenant_id, asset_id, upload_id, stage, state,
			max_attempts, next_attempt_at, created_at, updated_at
		)
		SELECT gen_random_uuid()::text, a.tenant_id, a.id, a.upload_id, 'VERIFY', 'PENDING', $2, $1, $1, $1
		FROM assets AS a
		WHERE a.status IN ('QUARANTINED', 'PROCESSING')
		  AND NOT EXISTS (SELECT 1 FROM processing_jobs AS j WHERE j.asset_id = a.id)
		ON CONFLICT DO NOTHING
	`, now, maxAttempts)
	if err != nil {
		return 0, fmt.Errorf("create processing jobs: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit enqueue: %w", err)
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
		    updated_at = $3
		WHERE id = $1
		  AND state = 'PENDING'
		  AND lease_owner = $2
		  AND lease_expires_at > $3
	`, job.ID, owner, now, string(job.Stage),
		job.ChecksumAlgorithm, job.ChecksumVerifiedAt, job.DetectedContentType,
		job.Scanner, job.ScanResult, job.ScanSignature, job.ScannedAt,
		job.PromotionKey, job.PromotedAt)
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

	tag, err := r.db.Pool.Exec(ctx, `
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
	`, jobID, owner, now, string(state), lastError)
	if err != nil {
		return fmt.Errorf("finish processing job: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return model.ErrLeaseLost
	}
	return nil
}
