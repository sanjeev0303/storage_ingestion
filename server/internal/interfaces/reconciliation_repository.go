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
	// EXHAUSTED. It returns model.ErrLeaseLost if `owner` lost the lease.
	Finish(
		ctx context.Context,
		taskID string,
		owner string,
		now time.Time,
		state model.ReconciliationState,
		lastError string,
	) error
}
