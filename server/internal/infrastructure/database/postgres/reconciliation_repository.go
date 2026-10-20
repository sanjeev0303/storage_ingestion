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

// dueOperation and dueSessions define which sessions need reconciliation
// and with which operation ($1 = now, $2 = grace seconds). EnqueueDue and
// the dry run's ListDue share them, so a dry run reports exactly what a
// real run would do. COMPLETED sessions are left to the processing
// worker, whose VERIFY stage checks the bytes.
const dueOperation = `
	CASE
		WHEN s.status = 'COMPLETING' THEN 'COMPLETE'
		WHEN s.status = 'ABORTING' THEN 'ABORT'
		WHEN s.expires_at <= $1 THEN 'EXPIRE'
		ELSE 'INITIATE'
	END`

const dueSessions = `
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
		)`

// enqueueBatch bounds the tasks one cycle creates; the rest follow on the
// next cycles.
const enqueueBatch = 1000

func (r *ReconciliationRepository) EnqueueDue(
	ctx context.Context,
	now time.Time,
	grace time.Duration,
	storageProvider string,
	maxAttempts int,
) (int, error) {
	// The operation comes from the session's durable state, so a request
	// that crashed mid-operation is always picked up. EXHAUSTED tasks block
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
			gen_random_uuid()::text, s.tenant_id, s.id, `+dueOperation+`,
			'PENDING', $3, s.object_key, s.storage_upload_id,
			0, $4, $1, $1, $1
		`+dueSessions+`
		ORDER BY s.updated_at
		LIMIT $5
		ON CONFLICT DO NOTHING
	`, now, grace.Seconds(), storageProvider, maxAttempts, enqueueBatch)
	if err != nil {
		return 0, fmt.Errorf("enqueue reconciliation tasks: %w", err)
	}

	return int(tag.RowsAffected()), nil
}

func (r *ReconciliationRepository) ListDue(ctx context.Context, now time.Time, grace time.Duration, limit int) ([]model.DueReconciliation, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT s.tenant_id, s.id, `+dueOperation+`, s.status
		`+dueSessions+`
		ORDER BY s.updated_at
		LIMIT $3
	`, now, grace.Seconds(), limit)
	if err != nil {
		return nil, fmt.Errorf("list due reconciliation: %w", err)
	}
	defer rows.Close()

	var due []model.DueReconciliation
	for rows.Next() {
		var d model.DueReconciliation
		var operation, status string
		if err := rows.Scan(&d.TenantID, &d.UploadID, &operation, &status); err != nil {
			return nil, fmt.Errorf("scan due reconciliation: %w", err)
		}
		d.Operation, d.Status = model.ReconciliationOperation(operation), model.UploadStatus(status)
		due = append(due, d)
	}
	return due, rows.Err()
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
	outcome string,
	lastError string,
) error {
	if state != model.ReconcileSucceeded && state != model.ReconcileExhausted {
		return fmt.Errorf("finish state must be SUCCEEDED or EXHAUSTED, got %s", state)
	}

	tag, err := r.db.Pool.Exec(ctx, `
		UPDATE reconciliation_tasks
		SET state = $4,
		    last_error = NULLIF($5, ''),
		    outcome = NULLIF($6, ''),
		    lease_owner = NULL,
		    lease_expires_at = NULL,
		    updated_at = $3
		WHERE id = $1
		  AND state = 'PENDING'
		  AND lease_owner = $2
		  AND lease_expires_at > $3
	`, taskID, owner, now, string(state), lastError, outcome)
	if err != nil {
		return fmt.Errorf("finish reconciliation task: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return model.ErrLeaseLost
	}

	return nil
}

func (r *ReconciliationRepository) RecordFinding(ctx context.Context, f model.ReconciliationFinding) error {
	_, err := r.db.Pool.Exec(ctx, `
		INSERT INTO reconciliation_findings
			(id, kind, tenant_id, upload_id, object_key, storage_upload_id, action, outcome, reason, created_at)
		VALUES ($1, $2, NULLIF($3, ''), NULLIF($4, ''), NULLIF($5, ''), NULLIF($6, ''), $7, $8, $9, $10)
	`, f.ID, f.Kind, f.TenantID, f.UploadID, f.ObjectKey, f.StorageUploadID, f.Action, f.Outcome, f.Reason, f.CreatedAt)
	if err != nil {
		return fmt.Errorf("record reconciliation finding: %w", err)
	}
	return nil
}

func (r *ReconciliationRepository) StaleParts(ctx context.Context, olderThan time.Time, limit int) ([]model.UploadRef, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT s.tenant_id, s.id
		FROM upload_sessions AS s
		WHERE s.status IN ('ABORTED', 'FAILED', 'EXPIRED')
		  AND s.updated_at <= $1
		  AND EXISTS (
				SELECT 1 FROM upload_parts AS p
				WHERE p.upload_id = s.id AND p.tenant_id = s.tenant_id
			)
		ORDER BY s.updated_at
		LIMIT $2
	`, olderThan, limit)
	if err != nil {
		return nil, fmt.Errorf("list stale parts: %w", err)
	}
	defer rows.Close()

	var refs []model.UploadRef
	for rows.Next() {
		var ref model.UploadRef
		if err := rows.Scan(&ref.TenantID, &ref.UploadID); err != nil {
			return nil, fmt.Errorf("scan stale parts: %w", err)
		}
		refs = append(refs, ref)
	}
	return refs, rows.Err()
}

func (r *ReconciliationRepository) CountStuck(ctx context.Context, olderThan time.Time) (map[model.UploadStatus]int, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT status, COUNT(*)
		FROM upload_sessions
		WHERE status IN ('PENDING', 'UPLOADING', 'COMPLETING', 'ABORTING')
		  AND updated_at <= $1
		GROUP BY status
	`, olderThan)
	if err != nil {
		return nil, fmt.Errorf("count stuck uploads: %w", err)
	}
	defer rows.Close()

	counts := map[model.UploadStatus]int{}
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return nil, fmt.Errorf("scan stuck uploads: %w", err)
		}
		counts[model.UploadStatus(status)] = n
	}
	return counts, rows.Err()
}

func (r *ReconciliationRepository) FinishedSessions(ctx context.Context, since, until time.Time, afterID string, limit int) ([]model.UploadSession, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT tenant_id, id, object_key, COALESCE(storage_upload_id, ''), status
		FROM upload_sessions
		WHERE status IN ('ABORTED', 'FAILED', 'EXPIRED')
		  AND updated_at > $1
		  AND updated_at <= $2
		  AND id > $3
		ORDER BY id
		LIMIT $4
	`, since, until, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("list finished sessions: %w", err)
	}
	defer rows.Close()

	var sessions []model.UploadSession
	for rows.Next() {
		var s model.UploadSession
		var status string
		if err := rows.Scan(&s.TenantID, &s.ID, &s.ObjectKey, &s.StorageUploadID, &status); err != nil {
			return nil, fmt.Errorf("scan finished session: %w", err)
		}
		s.Status = model.UploadStatus(status)
		sessions = append(sessions, s)
	}
	return sessions, rows.Err()
}
