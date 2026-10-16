package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// ReconciliationRepository stores recovery tasks in Neon PostgreSQL.
type ReconciliationRepository struct {
	db *DB
}

var _ interfaces.ReconciliationRepository = (*ReconciliationRepository)(nil)

func NewReconciliationRepository(db *DB) *ReconciliationRepository {
	return &ReconciliationRepository{db: db}
}

const taskColumns = `
	id,
	tenant_id,
	upload_id,
	operation,
	state,
	storage_provider,
	object_key,
	COALESCE(storage_upload_id, ''),
	attempt_count,
	max_attempts,
	last_attempt_at,
	next_attempt_at,
	COALESCE(last_error, ''),
	COALESCE(lease_owner, ''),
	lease_expires_at,
	created_at,
	updated_at
`

func scanTask(row pgx.Row) (model.ReconciliationTask, error) {
	var task model.ReconciliationTask
	var operation, state string

	err := row.Scan(
		&task.ID,
		&task.TenantID,
		&task.UploadID,
		&operation,
		&state,
		&task.StorageProvider,
		&task.ObjectKey,
		&task.StorageUploadID,
		&task.AttemptCount,
		&task.MaxAttempts,
		&task.LastAttemptAt,
		&task.NextAttemptAt,
		&task.LastError,
		&task.LeaseOwner,
		&task.LeaseExpiresAt,
		&task.CreatedAt,
		&task.UpdatedAt,
	)
	task.Operation = model.ReconciliationOperation(operation)
	task.State = model.ReconciliationState(state)
	return task, err
}

func (r *ReconciliationRepository) EnqueueDue(
	ctx context.Context,
	now time.Time,
	grace time.Duration,
	storageProvider string,
	maxAttempts int,
) (int, error) {
	// The operation comes from the session's durable state, so a request
	// that crashed mid-operation is always picked up. COMPLETED sessions are
	// left to the processing worker, whose VERIFY stage checks the bytes
	// (VERIFY tasks are no longer created here). EXHAUSTED tasks block
	// re-enqueueing: they wait for an operator instead of looping forever.
	// ON CONFLICT DO NOTHING lets concurrent sweepers race safely against
	// ux_reconciliation_tasks_open_upload.
	tag, err := r.db.Pool.Exec(ctx, `
		INSERT INTO reconciliation_tasks (
			id, tenant_id, upload_id, operation, state,
			storage_provider, object_key, storage_upload_id,
			attempt_count, max_attempts, next_attempt_at,
			created_at, updated_at
		)
		SELECT
			gen_random_uuid()::text,
			s.tenant_id,
			s.id,
			CASE
				WHEN s.status = 'COMPLETING' THEN 'COMPLETE'
				WHEN s.status = 'ABORTING' THEN 'ABORT'
				WHEN s.expires_at <= $1 THEN 'EXPIRE'
				ELSE 'INITIATE'
			END,
			'PENDING',
			$3,
			s.object_key,
			s.storage_upload_id,
			0,
			$4,
			$1,
			$1,
			$1
		FROM upload_sessions AS s
		WHERE (
				(s.status IN ('COMPLETING', 'ABORTING')
					AND s.updated_at <= $1 - make_interval(secs => $2))
				OR (s.status IN ('PENDING', 'UPLOADING') AND s.expires_at <= $1)
				OR (s.status = 'PENDING'
					AND s.storage_upload_id IS NULL
					AND s.updated_at <= $1 - make_interval(secs => $2))
			)
		  AND NOT EXISTS (
				SELECT 1
				FROM reconciliation_tasks AS t
				WHERE t.upload_id = s.id
				  AND t.state IN ('PENDING', 'EXHAUSTED')
			)
		ON CONFLICT DO NOTHING
	`, now, grace.Seconds(), storageProvider, maxAttempts)
	if err != nil {
		return 0, fmt.Errorf("enqueue reconciliation tasks: %w", err)
	}

	return int(tag.RowsAffected()), nil
}

func (r *ReconciliationRepository) Claim(
	ctx context.Context,
	owner string,
	now time.Time,
	lease time.Duration,
	limit int,
) ([]model.ReconciliationTask, error) {
	if owner == "" || lease <= 0 || limit <= 0 {
		return nil, fmt.Errorf("claim needs an owner, a positive lease and a positive limit")
	}

	// SKIP LOCKED: concurrent workers each get different rows instead of
	// waiting on, or double-claiming, the same ones. An expired lease is
	// claimable again, so a crashed worker never blocks recovery.
	rows, err := r.db.Pool.Query(ctx, `
		WITH due AS (
			SELECT id AS due_id
			FROM reconciliation_tasks
			WHERE state = 'PENDING'
			  AND next_attempt_at <= $2
			  AND (lease_expires_at IS NULL OR lease_expires_at <= $2)
			ORDER BY next_attempt_at
			LIMIT $4
			FOR UPDATE SKIP LOCKED
		)
		UPDATE reconciliation_tasks AS t
		SET lease_owner = $1,
		    lease_expires_at = $2 + make_interval(secs => $3),
		    attempt_count = t.attempt_count + 1,
		    last_attempt_at = $2,
		    updated_at = $2
		FROM due
		WHERE t.id = due.due_id
		RETURNING `+taskColumns,
		owner, now, lease.Seconds(), limit)
	if err != nil {
		return nil, fmt.Errorf("claim reconciliation tasks: %w", err)
	}
	defer rows.Close()

	tasks := make([]model.ReconciliationTask, 0, limit)
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			return nil, fmt.Errorf("scan claimed task: %w", err)
		}
		tasks = append(tasks, task)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate claimed tasks: %w", err)
	}

	return tasks, nil
}

func (r *ReconciliationRepository) Reschedule(
	ctx context.Context,
	taskID string,
	owner string,
	now time.Time,
	next time.Time,
	lastError string,
) error {
	tag, err := r.db.Pool.Exec(ctx, `
		UPDATE reconciliation_tasks
		SET next_attempt_at = $4,
		    last_error = NULLIF($5, ''),
		    lease_owner = NULL,
		    lease_expires_at = NULL,
		    updated_at = $3
		WHERE id = $1
		  AND state = 'PENDING'
		  AND lease_owner = $2
		  AND lease_expires_at > $3
	`, taskID, owner, now, next, lastError)
	if err != nil {
		return fmt.Errorf("reschedule reconciliation task: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return model.ErrLeaseLost
	}

	return nil
}

func (r *ReconciliationRepository) Finish(
	ctx context.Context,
	taskID string,
	owner string,
	now time.Time,
	state model.ReconciliationState,
	lastError string,
) error {
	if state != model.ReconcileSucceeded && state != model.ReconcileExhausted {
		return fmt.Errorf("finish state must be SUCCEEDED or EXHAUSTED, got %s", state)
	}

	tag, err := r.db.Pool.Exec(ctx, `
		UPDATE reconciliation_tasks
		SET state = $4,
		    last_error = NULLIF($5, ''),
		    lease_owner = NULL,
		    lease_expires_at = NULL,
		    updated_at = $3
		WHERE id = $1
		  AND state = 'PENDING'
		  AND lease_owner = $2
		  AND lease_expires_at > $3
	`, taskID, owner, now, string(state), lastError)
	if err != nil {
		return fmt.Errorf("finish reconciliation task: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return model.ErrLeaseLost
	}

	return nil
}
