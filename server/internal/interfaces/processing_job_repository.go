package interfaces

import (
	"context"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// ProcessingJobRepository is the durable processing queue. Jobs are derived
// from upload-session state, so a duplicate or delayed "upload completed"
// event cannot create a second asset or job, and a crashed worker's job is
// claimed again once its lease expires.
type ProcessingJobRepository interface {
	// EnqueueDue creates the asset and its single processing job for every
	// completed upload that has none yet, and returns how many jobs it made.
	EnqueueDue(ctx context.Context, now time.Time, maxAttempts int) (int, error)

	// Claim leases up to `limit` due jobs to owner until now+lease and
	// counts the attempt. A job whose lease expired can be claimed again.
	Claim(ctx context.Context, owner string, now time.Time, lease time.Duration, limit int) ([]model.ProcessingJob, error)

	// SaveProgress writes the job's stage and stage results while owner
	// still holds the lease, keeping the lease. model.ErrLeaseLost otherwise.
	SaveProgress(ctx context.Context, job model.ProcessingJob, owner string, now time.Time) error

	// Reschedule releases the lease and keeps the job PENDING until next.
	Reschedule(ctx context.Context, jobID string, owner string, now time.Time, next time.Time, lastError string) error

	// Finish releases the lease and moves the job to SUCCEEDED, FAILED or
	// EXHAUSTED.
	Finish(ctx context.Context, jobID string, owner string, now time.Time, state model.ProcessingState, lastError string) error
}
