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
	// EnqueueUpload does the same for one upload and returns its job, new
	// or existing. model.ErrUploadNotFound for an unknown upload, and
	// model.ErrInvalidUploadState when it is not COMPLETED or VERIFIED.
	EnqueueUpload(ctx context.Context, tenantID, uploadID string, now time.Time, maxAttempts int) (model.ProcessingJob, error)

	// Claim leases up to `limit` due jobs to owner until now+lease and
	// counts the attempt. A job whose lease expired can be claimed again.
	// Each claim's lease owner is owner#attempt, so a worker that lost a
	// job cannot write to it even if the same worker claims it again.
	Claim(ctx context.Context, owner string, now time.Time, lease time.Duration, limit int) ([]model.ProcessingJob, error)
	// ClaimDue is Claim for jobs due by dueBy rather than by now (the lease
	// still runs from now). A cycle that claims as it goes passes its own
	// start, so a job that failed and became due again in the same cycle
	// is left for the next one instead of spending its attempts at once.
	ClaimDue(ctx context.Context, owner string, dueBy, now time.Time, lease time.Duration, limit int) ([]model.ProcessingJob, error)
	// ClaimJob leases one job the same way, by ID. ok is false when the job
	// is finished, not due yet or leased by another worker.
	ClaimJob(ctx context.Context, jobID, owner string, now time.Time, lease time.Duration) (job model.ProcessingJob, ok bool, err error)

	// SaveProgress writes the job's stage and stage results while owner
	// still holds the lease, keeping the lease. model.ErrLeaseLost otherwise.
	SaveProgress(ctx context.Context, job model.ProcessingJob, owner string, now time.Time) error

	// FindCleanScan returns another of the tenant's assets with the same
	// SHA-256 and size whose job recorded a CLEAN verdict from scanner at
	// version. ok is false when there is none.
	FindCleanScan(ctx context.Context, tenantID, assetID, sha256 string, size int64, scanner, version string) (fromAssetID string, ok bool, err error)

	// Renew extends owner's unexpired lease to now+lease.
	// model.ErrLeaseLost when the lease expired, the job ended or another
	// claim holds it.
	Renew(ctx context.Context, jobID, owner string, now time.Time, lease time.Duration) error

	// Reschedule releases the lease and keeps the job PENDING until next.
	Reschedule(ctx context.Context, jobID string, owner string, now time.Time, next time.Time, lastError string) error

	// Finish releases the lease and moves the job to SUCCEEDED, FAILED or
	// EXHAUSTED.
	Finish(ctx context.Context, jobID string, owner string, now time.Time, state model.ProcessingState, lastError string) error

	// GetByAsset returns the tenant's job for an asset.
	GetByAsset(ctx context.Context, tenantID, assetID string) (model.ProcessingJob, error)

	// Requeue returns an EXHAUSTED job to PENDING with a fresh attempt
	// budget, from its recorded stage, for an operator retry. Jobs in any
	// other state are refused with model.ErrInvalidAssetState.
	Requeue(ctx context.Context, tenantID, assetID string, now time.Time) (model.ProcessingJob, error)

	// Backlog counts jobs by condition (due, leased, lease_expired,
	// exhausted) for backlog and stuck-job reporting.
	Backlog(ctx context.Context, now time.Time) (map[string]int, error)
}
