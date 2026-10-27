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
	return r.enqueue(ctx, now, maxAttempts, "")
}

func (r *ProcessingJobRepository) EnqueueUpload(ctx context.Context, tenantID, uploadID string, now time.Time, maxAttempts int) (model.ProcessingJob, error) {
	if err := ctx.Err(); err != nil {
		return model.ProcessingJob{}, err
	}
	session, err := r.uploads.GetByID(ctx, tenantID, uploadID)
	if err != nil {
		return model.ProcessingJob{}, err
	}
	if session.Status != model.UploadStatusCompleted && session.Status != model.UploadStatusVerified {
		return model.ProcessingJob{}, model.ErrInvalidUploadState
	}
	if _, err := r.enqueue(ctx, now, maxAttempts, uploadID); err != nil {
		return model.ProcessingJob{}, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	for _, job := range r.jobs {
		if job.TenantID == tenantID && job.UploadID == uploadID {
			return job, nil
		}
	}
	return model.ProcessingJob{}, model.ErrAssetNotFound
}

// enqueue creates the missing assets and jobs of completed uploads, or of
// one upload when uploadID is not empty.
func (r *ProcessingJobRepository) enqueue(ctx context.Context, now time.Time, maxAttempts int, uploadID string) (int, error) {
	r.uploads.mu.RLock()
	var completed []model.UploadSession
	for _, s := range r.uploads.uploads {
		if uploadID != "" && s.ID != uploadID {
			continue
		}
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
				ChecksumSHA256: s.ChecksumSHA256, Dataset: s.Dataset, Status: model.AssetStatusQuarantined, CreatedAt: now, UpdatedAt: now,
				StorageConnectionID: s.StorageConnectionID, StagingConnectionID: s.StagingConnectionID,
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
	return r.ClaimDue(ctx, owner, now, now, lease, limit)
}

func (r *ProcessingJobRepository) ClaimDue(ctx context.Context, owner string, dueBy, now time.Time, lease time.Duration, limit int) ([]model.ProcessingJob, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	var due []model.ProcessingJob
	for _, job := range r.jobs {
		leaseFree := job.LeaseExpiresAt == nil || !job.LeaseExpiresAt.After(now)
		if job.State == model.ProcessingPending && !job.NextAttemptAt.After(dueBy) && leaseFree {
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
		due[i].AttemptCount++
		due[i].LeaseOwner = fmt.Sprintf("%s#%d", owner, due[i].AttemptCount)
		due[i].LeaseExpiresAt = &expires
		if due[i].StartedAt == nil {
			due[i].StartedAt = &started
		}
		due[i].UpdatedAt = now
		r.jobs[due[i].ID] = due[i]
	}
	return due, nil
}

func (r *ProcessingJobRepository) ClaimJob(ctx context.Context, jobID, owner string, now time.Time, lease time.Duration) (model.ProcessingJob, bool, error) {
	if err := ctx.Err(); err != nil {
		return model.ProcessingJob{}, false, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	job, ok := r.jobs[jobID]
	leaseFree := job.LeaseExpiresAt == nil || !job.LeaseExpiresAt.After(now)
	if !ok || job.State != model.ProcessingPending || job.NextAttemptAt.After(now) || !leaseFree {
		return model.ProcessingJob{}, false, nil
	}

	expires := now.Add(lease)
	started := now
	job.AttemptCount++
	job.LeaseOwner, job.LeaseExpiresAt = fmt.Sprintf("%s#%d", owner, job.AttemptCount), &expires
	if job.StartedAt == nil {
		job.StartedAt = &started
	}
	job.UpdatedAt = now
	r.jobs[jobID] = job
	return job, true, nil
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

func (r *ProcessingJobRepository) Renew(ctx context.Context, jobID, owner string, now time.Time, lease time.Duration) error {
	return r.update(ctx, jobID, owner, now, func(stored *model.ProcessingJob) {
		expires := now.Add(lease)
		stored.LeaseExpiresAt = &expires
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

func (r *ProcessingJobRepository) GetByAsset(ctx context.Context, tenantID, assetID string) (model.ProcessingJob, error) {
	if err := ctx.Err(); err != nil {
		return model.ProcessingJob{}, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	for _, job := range r.jobs {
		if job.TenantID == tenantID && job.AssetID == assetID {
			return job, nil
		}
	}
	return model.ProcessingJob{}, model.ErrAssetNotFound
}

func (r *ProcessingJobRepository) Requeue(ctx context.Context, tenantID, assetID string, now time.Time) (model.ProcessingJob, error) {
	if err := ctx.Err(); err != nil {
		return model.ProcessingJob{}, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	for id, job := range r.jobs {
		if job.TenantID != tenantID || job.AssetID != assetID {
			continue
		}
		if job.State != model.ProcessingExhausted {
			return model.ProcessingJob{}, model.ErrInvalidAssetState
		}
		job.State = model.ProcessingPending
		job.AttemptCount = 0
		job.NextAttemptAt = now
		job.LastError = ""
		job.CompletedAt = nil
		job.UpdatedAt = now
		r.jobs[id] = job
		return job, nil
	}
	return model.ProcessingJob{}, model.ErrAssetNotFound
}

func (r *ProcessingJobRepository) Backlog(ctx context.Context, now time.Time) (map[string]int, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	counts := map[string]int{"due": 0, "leased": 0, "lease_expired": 0, "exhausted": 0}
	for _, job := range r.jobs {
		switch {
		case job.State == model.ProcessingExhausted:
			counts["exhausted"]++
		case job.State != model.ProcessingPending:
		case job.LeaseExpiresAt == nil:
			if !job.NextAttemptAt.After(now) {
				counts["due"]++
			}
		case job.LeaseExpiresAt.After(now):
			counts["leased"]++
		default:
			counts["lease_expired"]++
		}
	}
	return counts, nil
}

// Put stores a job as given, to set up test states.
func (r *ProcessingJobRepository) Put(job model.ProcessingJob) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.jobs[job.ID] = job
}
