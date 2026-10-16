package memory

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// ProcessingJobRepository mirrors the PostgreSQL processing queue: assets
// and jobs are derived from completed uploads, claims use expiring leases,
// and only the lease holder may write. For tests only.
type ProcessingJobRepository struct {
	mu      sync.Mutex
	uploads *UploadRepository
	assets  *AssetRepository
	jobs    map[string]model.ProcessingJob
	nextID  int
}

var _ interfaces.ProcessingJobRepository = (*ProcessingJobRepository)(nil)

func NewProcessingJobRepository(uploads *UploadRepository, assets *AssetRepository) *ProcessingJobRepository {
	return &ProcessingJobRepository{uploads: uploads, assets: assets, jobs: make(map[string]model.ProcessingJob)}
}

func (r *ProcessingJobRepository) EnqueueDue(ctx context.Context, now time.Time, maxAttempts int) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	r.uploads.mu.RLock()
	var completed []model.UploadSession
	for _, s := range r.uploads.uploads {
		if s.Status == model.UploadStatusCompleted || s.Status == model.UploadStatusVerified {
			completed = append(completed, s)
		}
	}
	r.uploads.mu.RUnlock()

	r.mu.Lock()
	defer r.mu.Unlock()

	created := 0
	for _, s := range completed {
		asset, exists := r.assets.byUpload(s.ID)
		if !exists {
			r.nextID++
			asset = model.Asset{
				ID: fmt.Sprintf("asset-%d", r.nextID), TenantID: s.TenantID, OwnerID: s.OwnerID, UploadID: s.ID,
				ObjectKey: s.ObjectKey, FileName: s.FileName, ContentType: s.ContentType, SizeBytes: s.ExpectedSize,
				ChecksumSHA256: s.ChecksumSHA256, Status: model.AssetStatusQuarantined, CreatedAt: now, UpdatedAt: now,
			}
			if err := r.assets.Create(ctx, &asset); err != nil {
				return created, err
			}
		}
		if asset.Status != model.AssetStatusQuarantined && asset.Status != model.AssetStatusProcessing {
			continue
		}
		if r.hasJob(asset.ID) {
			continue
		}

		r.nextID++
		id := fmt.Sprintf("job-%d", r.nextID)
		r.jobs[id] = model.ProcessingJob{
			ID: id, TenantID: asset.TenantID, AssetID: asset.ID, UploadID: asset.UploadID,
			Stage: model.StageVerify, State: model.ProcessingPending, MaxAttempts: maxAttempts,
			NextAttemptAt: now, CreatedAt: now, UpdatedAt: now,
		}
		created++
	}
	return created, nil
}

func (r *ProcessingJobRepository) hasJob(assetID string) bool {
	for _, job := range r.jobs {
		if job.AssetID == assetID {
			return true
		}
	}
	return false
}

func (r *ProcessingJobRepository) Claim(ctx context.Context, owner string, now time.Time, lease time.Duration, limit int) ([]model.ProcessingJob, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	var due []model.ProcessingJob
	for _, job := range r.jobs {
		leaseFree := job.LeaseExpiresAt == nil || !job.LeaseExpiresAt.After(now)
		if job.State == model.ProcessingPending && !job.NextAttemptAt.After(now) && leaseFree {
			due = append(due, job)
		}
	}
	sort.Slice(due, func(i, j int) bool { return due[i].NextAttemptAt.Before(due[j].NextAttemptAt) })
	if len(due) > limit {
		due = due[:limit]
	}

	expires := now.Add(lease)
	for i := range due {
		started := now
		due[i].LeaseOwner = owner
		due[i].LeaseExpiresAt = &expires
		due[i].AttemptCount++
		if due[i].StartedAt == nil {
			due[i].StartedAt = &started
		}
		due[i].UpdatedAt = now
		r.jobs[due[i].ID] = due[i]
	}
	return due, nil
}

func (r *ProcessingJobRepository) SaveProgress(ctx context.Context, job model.ProcessingJob, owner string, now time.Time) error {
	return r.update(ctx, job.ID, owner, now, func(stored *model.ProcessingJob) {
		lease := stored.LeaseExpiresAt
		attempts, started := stored.AttemptCount, stored.StartedAt
		*stored = job
		stored.LeaseOwner, stored.LeaseExpiresAt = owner, lease
		stored.AttemptCount, stored.StartedAt = attempts, started
	})
}

func (r *ProcessingJobRepository) Reschedule(ctx context.Context, jobID string, owner string, now time.Time, next time.Time, lastError string) error {
	return r.update(ctx, jobID, owner, now, func(stored *model.ProcessingJob) {
		stored.NextAttemptAt = next
		stored.LastError = lastError
		stored.LeaseOwner, stored.LeaseExpiresAt = "", nil
	})
}

func (r *ProcessingJobRepository) Finish(ctx context.Context, jobID string, owner string, now time.Time, state model.ProcessingState, lastError string) error {
	if state == model.ProcessingPending {
		return fmt.Errorf("finish state must be final, got %s", state)
	}
	return r.update(ctx, jobID, owner, now, func(stored *model.ProcessingJob) {
		completed := now
		stored.State = state
		stored.LastError = lastError
		stored.CompletedAt = &completed
		stored.LeaseOwner, stored.LeaseExpiresAt = "", nil
	})
}

// update applies change only while owner holds an unexpired lease.
func (r *ProcessingJobRepository) update(ctx context.Context, jobID, owner string, now time.Time, change func(*model.ProcessingJob)) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	job, ok := r.jobs[jobID]
	if !ok || job.State != model.ProcessingPending || job.LeaseOwner != owner ||
		job.LeaseExpiresAt == nil || !job.LeaseExpiresAt.After(now) {
		return model.ErrLeaseLost
	}

	change(&job)
	job.UpdatedAt = now
	r.jobs[jobID] = job
	return nil
}

// Jobs returns a snapshot of every job, for assertions.
func (r *ProcessingJobRepository) Jobs() []model.ProcessingJob {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]model.ProcessingJob, 0, len(r.jobs))
	for _, job := range r.jobs {
		out = append(out, job)
	}
	return out
}
