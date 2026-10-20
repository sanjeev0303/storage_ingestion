package interfaces

import (
	"context"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// ReconciliationRepository persists recovery work and the leases that
// keep two workers from processing the same task at once.
type ReconciliationRepository interface {
	// EnqueueDue creates one PENDING task for every upload session whose
	// state needs reconciliation and has no open or exhausted task. Sessions
	// changed within `grace` are left to the request still handling them.
	// It returns the number of tasks created.
	EnqueueDue(
		ctx context.Context,
		now time.Time,
		grace time.Duration,
		storageProvider string,
		maxAttempts int,
	) (int, error)

	// Claim leases up to `limit` due tasks to `owner` until now+lease and
	// counts the attempt. A task whose lease expired can be claimed again.
	Claim(
		ctx context.Context,
		owner string,
		now time.Time,
		lease time.Duration,
		limit int,
	) ([]model.ReconciliationTask, error)

	// Reschedule releases the lease and keeps the task PENDING until next.
	// It returns model.ErrLeaseLost if `owner` no longer holds the lease.
	Reschedule(
		ctx context.Context,
		taskID string,
		owner string,
		now time.Time,
		next time.Time,
		lastError string,
	) error

	// Finish releases the lease and moves the task to SUCCEEDED or
	// EXHAUSTED, recording the upload status it left behind (outcome).
	// It returns model.ErrLeaseLost if `owner` lost the lease.
	Finish(
		ctx context.Context,
		taskID string,
		owner string,
		now time.Time,
		state model.ReconciliationState,
		outcome string,
		lastError string,
	) error

	// ListDue reports, without writing anything, the sessions EnqueueDue
	// would create tasks for. Used by dry runs.
	ListDue(ctx context.Context, now time.Time, grace time.Duration, limit int) ([]model.DueReconciliation, error)

	// RecordFinding stores one discrepancy and the action taken.
	RecordFinding(ctx context.Context, finding model.ReconciliationFinding) error

	// StaleParts returns up to limit finished sessions (ABORTED, FAILED,
	// EXPIRED) last changed before olderThan that still have part rows.
	StaleParts(ctx context.Context, olderThan time.Time, limit int) ([]model.UploadRef, error)

	// FinishedSessions pages through ABORTED, FAILED and EXPIRED sessions
	// last changed between since and until, ordered by ID, after afterID.
	// The database-driven orphan-session scan uses it on providers that
	// cannot list multipart sessions by prefix.
	FinishedSessions(ctx context.Context, since, until time.Time, afterID string, limit int) ([]model.UploadSession, error)

	// CountStuck counts non-terminal sessions unchanged since olderThan,
	// by status, for stuck-upload reporting.
	CountStuck(ctx context.Context, olderThan time.Time) (map[model.UploadStatus]int, error)
}
