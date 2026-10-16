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

// ReconciliationRepository keeps tasks in memory with the same enqueue,
// claim and lease rules as the PostgreSQL adapter. For tests only.
type ReconciliationRepository struct {
	mu      sync.Mutex
	uploads *UploadRepository
	tasks   map[string]model.ReconciliationTask
	nextID  int
}

var _ interfaces.ReconciliationRepository = (*ReconciliationRepository)(nil)

// NewReconciliationRepository derives tasks from the sessions in uploads.
func NewReconciliationRepository(uploads *UploadRepository) *ReconciliationRepository {
	return &ReconciliationRepository{
		uploads: uploads,
		tasks:   make(map[string]model.ReconciliationTask),
	}
}

func (r *ReconciliationRepository) EnqueueDue(
	ctx context.Context,
	now time.Time,
	grace time.Duration,
	storageProvider string,
	maxAttempts int,
) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	r.uploads.mu.RLock()
	sessions := make([]model.UploadSession, 0, len(r.uploads.uploads))
	for _, s := range r.uploads.uploads {
		sessions = append(sessions, s)
	}
	r.uploads.mu.RUnlock()

	r.mu.Lock()
	defer r.mu.Unlock()

	created := 0
	for _, s := range sessions {
		operation, due := operationFor(s, now, grace)
		if !due || r.hasOpenOrExhausted(s.ID) {
			continue
		}

		r.nextID++
		id := fmt.Sprintf("task-%d", r.nextID)
		r.tasks[id] = model.ReconciliationTask{
			ID:              id,
			TenantID:        s.TenantID,
			UploadID:        s.ID,
			Operation:       operation,
			State:           model.ReconcilePending,
			StorageProvider: storageProvider,
			ObjectKey:       s.ObjectKey,
			StorageUploadID: s.StorageUploadID,
			MaxAttempts:     maxAttempts,
			NextAttemptAt:   now,
			CreatedAt:       now,
			UpdatedAt:       now,
		}
		created++
	}

	return created, nil
}

// operationFor is the same CASE as the PostgreSQL EnqueueDue.
func operationFor(s model.UploadSession, now time.Time, grace time.Duration) (model.ReconciliationOperation, bool) {
	settled := !s.UpdatedAt.After(now.Add(-grace))

	switch {
	case s.Status == model.UploadStatusCompleting && settled:
		return model.ReconcileComplete, true
	case s.Status == model.UploadStatusAborting && settled:
		return model.ReconcileAbort, true
	case (s.Status == model.UploadStatusPending || s.Status == model.UploadStatusUploading) &&
		!s.ExpiresAt.After(now):
		return model.ReconcileExpire, true
	case s.Status == model.UploadStatusPending && s.StorageUploadID == "" && settled:
		return model.ReconcileInitiate, true
	}

	return "", false
}

func (r *ReconciliationRepository) hasOpenOrExhausted(uploadID string) bool {
	for _, t := range r.tasks {
		if t.UploadID == uploadID &&
			(t.State == model.ReconcilePending || t.State == model.ReconcileExhausted) {
			return true
		}
	}
	return false
}

func (r *ReconciliationRepository) Claim(
	ctx context.Context,
	owner string,
	now time.Time,
	lease time.Duration,
	limit int,
) ([]model.ReconciliationTask, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	var due []model.ReconciliationTask
	for _, t := range r.tasks {
		leaseFree := t.LeaseExpiresAt == nil || !t.LeaseExpiresAt.After(now)
		if t.State == model.ReconcilePending && !t.NextAttemptAt.After(now) && leaseFree {
			due = append(due, t)
		}
	}
	sort.Slice(due, func(i, j int) bool {
		return due[i].NextAttemptAt.Before(due[j].NextAttemptAt)
	})
	if len(due) > limit {
		due = due[:limit]
	}

	expires := now.Add(lease)
	for i := range due {
		attemptAt := now
		due[i].LeaseOwner = owner
		due[i].LeaseExpiresAt = &expires
		due[i].AttemptCount++
		due[i].LastAttemptAt = &attemptAt
		due[i].UpdatedAt = now
		r.tasks[due[i].ID] = due[i]
	}

	return due, nil
}

func (r *ReconciliationRepository) Reschedule(
	ctx context.Context,
	taskID string,
	owner string,
	now time.Time,
	next time.Time,
	lastError string,
) error {
	return r.release(ctx, taskID, owner, now, func(t *model.ReconciliationTask) {
		t.NextAttemptAt = next
		t.LastError = lastError
	})
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
	return r.release(ctx, taskID, owner, now, func(t *model.ReconciliationTask) {
		t.State = state
		t.LastError = lastError
	})
}

// release applies change only while owner still holds an unexpired lease.
func (r *ReconciliationRepository) release(
	ctx context.Context,
	taskID string,
	owner string,
	now time.Time,
	change func(*model.ReconciliationTask),
) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	t, exists := r.tasks[taskID]
	if !exists || t.State != model.ReconcilePending || t.LeaseOwner != owner ||
		t.LeaseExpiresAt == nil || !t.LeaseExpiresAt.After(now) {
		return model.ErrLeaseLost
	}

	change(&t)
	t.LeaseOwner = ""
	t.LeaseExpiresAt = nil
	t.UpdatedAt = now
	r.tasks[taskID] = t
	return nil
}

// Tasks returns a snapshot of every task, for assertions.
func (r *ReconciliationRepository) Tasks() []model.ReconciliationTask {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]model.ReconciliationTask, 0, len(r.tasks))
	for _, t := range r.tasks {
		out = append(out, t)
	}
	return out
}
