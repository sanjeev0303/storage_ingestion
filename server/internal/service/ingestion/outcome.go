package ingestion

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/retry"
)

// OnUploadProcessed handles upload.processed: the processing of an
// entry's upload ended. It is safe to repeat: the outcome is read from the
// asset and recorded by a conditional transition.
func (s *Service) OnUploadProcessed(ctx context.Context, tenantID, uploadID string) (string, error) {
	e, err := s.deps.Repo.EntryByUpload(ctx, uploadID)
	if errors.Is(err, model.ErrEntryNotFound) {
		return "no ingestion entry uses this upload any more", nil
	}
	if err != nil {
		return "", err
	}
	if e.TenantID != tenantID {
		return "", retry.Permanent(fmt.Errorf("upload %s belongs to another tenant", uploadID))
	}
	return s.settleOutcome(ctx, e)
}

// settleOutcome records what processing made of an entry's upload: its
// asset's state is the truth.
func (s *Service) settleOutcome(ctx context.Context, e model.IngestionEntry) (string, error) {
	held := e.State == model.EntryRetryable && e.ErrorCode == model.EntryErrProcessingHeld
	if e.UploadID == "" || !(e.State.AwaitingProcessing() || held) {
		return "the entry is " + string(e.State) + "; nothing to record", nil
	}
	now := s.now()
	asset, err := s.deps.Assets.GetByUpload(ctx, e.TenantID, e.UploadID)
	if errors.Is(err, model.ErrAssetNotFound) {
		// Processing has not taken the upload in yet. Touch the entry so
		// the outcome sweep moves on to others.
		s.touch(ctx, e, now)
		return "processing has not started", nil
	}
	if err != nil {
		return "", err
	}

	change := interfaces.EntryChange{
		JobID: e.JobID, Position: e.Position, Now: now, UploadID: e.UploadID, AssetID: asset.ID,
		From: []model.EntryState{model.EntryUploaded, model.EntryProcessing, model.EntryRetryable},
	}
	switch asset.Status {
	case model.AssetStatusAvailable:
		change.To, change.StoredBytes = model.EntryPublished, asset.SizeBytes
		if _, err := s.deps.Repo.Transition(ctx, change); err != nil {
			return s.lost(err, "published")
		}
		s.count("ingestion_entries_published", 1)
		s.count("ingestion_entry_latency_ms_total", now.Sub(e.CreatedAt).Milliseconds())
		return "published", nil

	case model.AssetStatusRejected, model.AssetStatusSoftDeleted, model.AssetStatusDeleted:
		detail := asset.RejectionReason
		if asset.Status != model.AssetStatusRejected {
			detail = "the asset was deleted before it was published"
		}
		change.To, change.ErrorCode, change.ErrorDetail = model.EntryFailed, model.EntryErrRejected, detail
		if _, err := s.deps.Repo.Transition(ctx, change); err != nil {
			return s.lost(err, "rejected")
		}
		s.count("ingestion_entries_failed", 1)
		return "rejected: " + detail, nil
	}

	job, err := s.deps.Jobs.GetByAsset(ctx, e.TenantID, asset.ID)
	if err != nil && !errors.Is(err, model.ErrAssetNotFound) {
		return "", err
	}
	if err == nil && job.State == model.ProcessingExhausted {
		if held {
			return "processing is held; retried later", nil
		}
		// Processing gave up for now (a scanner outage, say). The upload
		// and asset stay; a retry requeues processing, not the upload.
		change.CountAttempt = true
		change.ErrorCode, change.ErrorDetail = model.EntryErrProcessingHeld, job.LastError
		if e.Attempts+1 >= s.cfg.MaxAttempts {
			change.To, change.ErrorCode = model.EntryFailed, model.EntryErrAttemptsExhausted
		} else {
			next := now.Add(s.backoff(e.Attempts + 1))
			change.To, change.NextAttemptAt = model.EntryRetryable, &next
		}
		if _, err := s.deps.Repo.Transition(ctx, change); err != nil {
			return s.lost(err, "held")
		}
		return "processing held", nil
	}

	// Still being processed.
	if e.State == model.EntryUploaded {
		change.To = model.EntryProcessing
		change.From = []model.EntryState{model.EntryUploaded}
		if _, err := s.deps.Repo.Transition(ctx, change); err != nil {
			return s.lost(err, "processing")
		}
	} else {
		s.touch(ctx, e, now)
	}
	return "processing", nil
}

// touch moves an entry's updated_at without changing its state.
func (s *Service) touch(ctx context.Context, e model.IngestionEntry, now time.Time) {
	_, _ = s.deps.Repo.Transition(ctx, interfaces.EntryChange{
		JobID: e.JobID, Position: e.Position, From: []model.EntryState{e.State}, To: e.State, Now: now,
		UploadID: e.UploadID, ErrorCode: e.ErrorCode, ErrorDetail: e.ErrorDetail, NextAttemptAt: e.NextAttemptAt,
	})
}

// lost turns a transition that found the entry already moved into a
// note: another delivery or the sweep recorded the outcome first.
func (s *Service) lost(err error, outcome string) (string, error) {
	if errors.Is(err, model.ErrInvalidEntryState) {
		s.count("ingestion_outcomes_duplicate", 1)
		return outcome + " (already recorded)", nil
	}
	return "", err
}

// RetryResult counts what a retry did.
type RetryResult struct {
	// Retried entries are PENDING again and will be uploaded again.
	Retried int
	// Requeued entries had their processing requeued; their upload stays.
	Requeued int
}

// maxRetryPages bounds one retry request; the client calls again for more.
const maxRetryPages = 100

// Retry tries failed entries again: the listed positions, or every
// eligible failure when all is set. Published entries are never touched,
// so a retry cannot duplicate a successful asset.
func (s *Service) Retry(ctx context.Context, p model.Principal, jobID string, positions []int64, all bool) (RetryResult, error) {
	if err := s.authorize(ctx, p, model.PermIngestionManage, "ingestion.retry", "ingestion_job", jobID); err != nil {
		return RetryResult{}, err
	}
	var result RetryResult
	if !all && (len(positions) == 0 || len(positions) > 1000) {
		return result, fmt.Errorf("%w: list 1 to 1000 positions, or retry all", ErrInvalidInput)
	}
	if _, err := s.deps.Repo.GetJob(ctx, p.TenantID, jobID); err != nil {
		return result, err
	}
	q, err := s.quota(ctx, p.TenantID)
	if err != nil {
		return result, err
	}

	// Held processing is not uploaded again: the entry is made due now, and
	// the worker, which runs processing, requeues it on its next pass.
	wanted := map[int64]bool{}
	for _, pos := range positions {
		wanted[pos] = true
	}
	var after int64
	for page := 0; page < maxRetryPages; page++ {
		held, err := s.deps.Repo.ListEntries(ctx, p.TenantID, jobID, []model.EntryState{model.EntryRetryable}, after, s.cfg.WorkerBatch)
		if err != nil {
			return result, err
		}
		for _, e := range held {
			if e.ErrorCode == model.EntryErrProcessingHeld && (all || wanted[e.Position]) {
				due := s.now()
				_, err := s.deps.Repo.Transition(ctx, interfaces.EntryChange{
					JobID: e.JobID, Position: e.Position, From: []model.EntryState{model.EntryRetryable},
					To: model.EntryRetryable, Now: due, UploadID: e.UploadID,
					ErrorCode: e.ErrorCode, ErrorDetail: e.ErrorDetail, NextAttemptAt: &due,
				})
				if err == nil {
					result.Requeued++
				}
			}
		}
		if len(held) < s.cfg.WorkerBatch {
			break
		}
		after = held[len(held)-1].Position
	}

	for page := 0; page < maxRetryPages; page++ {
		n, err := s.deps.Repo.Retry(ctx, interfaces.RetryRequest{
			TenantID: p.TenantID, JobID: jobID, Positions: positions, All: all, Limit: s.cfg.WorkerBatch,
			MaxManualRetries: s.cfg.MaxManualRetries, MaxActiveJobs: q.MaxActiveJobs, Now: s.now(),
		})
		if err != nil {
			return result, err
		}
		result.Retried += n
		if n < s.cfg.WorkerBatch {
			break
		}
	}
	s.count("ingestion_entries_retried", int64(result.Retried+result.Requeued))
	s.record(ctx, p, "ingestion.retry", "ingestion_job", jobID, map[string]any{
		"all": all, "positions": len(positions), "retried": result.Retried, "requeued": result.Requeued,
	})
	return result, nil
}

// requeue gives a held processing job a fresh budget and moves its entry
// back to PROCESSING.
func (s *Service) requeue(ctx context.Context, e model.IngestionEntry) error {
	if s.deps.Requeue == nil {
		return errors.New("this process cannot requeue processing; the worker does")
	}
	if err := s.deps.Requeue(ctx, e.TenantID, e.AssetID, "ingestion"); err != nil {
		// Already requeued by an operator, or running again: the entry
		// waits for the outcome either way.
		s.deps.Log.Info("requeue processing", "job_id", e.JobID, "position", e.Position, "error", retry.Sanitize(err))
	}
	_, err := s.deps.Repo.Transition(ctx, interfaces.EntryChange{
		JobID: e.JobID, Position: e.Position, From: []model.EntryState{model.EntryRetryable},
		To: model.EntryProcessing, Now: s.now(), UploadID: e.UploadID,
	})
	if err != nil && !errors.Is(err, model.ErrInvalidEntryState) {
		return err
	}
	return nil
}

// Cancel stops a job. No more slots are granted from now on; the worker
// cancels waiting entries, aborts uploads in progress, and lets stored
// uploads finish processing, so assets already made are kept. Deleting
// published assets is a separate request.
func (s *Service) Cancel(ctx context.Context, p model.Principal, jobID string) (model.IngestionJob, error) {
	if err := s.authorize(ctx, p, model.PermIngestionManage, "ingestion.cancel", "ingestion_job", jobID); err != nil {
		return model.IngestionJob{}, err
	}
	job, err := s.deps.Repo.RequestCancel(ctx, p.TenantID, jobID, s.now())
	if err == nil {
		s.count("ingestion_jobs_cancelled", 1)
		s.record(ctx, p, "ingestion.cancel", "ingestion_job", jobID, nil)
	}
	return job, err
}

func (s *Service) GetEntry(ctx context.Context, p model.Principal, jobID string, position int64) (model.IngestionEntry, error) {
	if err := s.authorize(ctx, p, model.PermIngestionRead, "ingestion.read", "ingestion_job", jobID); err != nil {
		return model.IngestionEntry{}, err
	}
	return s.deps.Repo.GetEntry(ctx, p.TenantID, jobID, position)
}

// ListEntries pages a job's entries by position, optionally by state.
func (s *Service) ListEntries(ctx context.Context, p model.Principal, jobID string, states []model.EntryState, after int64, limit int) ([]model.IngestionEntry, error) {
	if err := s.authorize(ctx, p, model.PermIngestionRead, "ingestion.read", "ingestion_job", jobID); err != nil {
		return nil, err
	}
	if _, err := s.deps.Repo.GetJob(ctx, p.TenantID, jobID); err != nil {
		return nil, err
	}
	return s.deps.Repo.ListEntries(ctx, p.TenantID, jobID, states, after, PageSize(limit))
}

// FailedStates are the states the failures listing shows.
var FailedStates = []model.EntryState{model.EntryInvalid, model.EntryRetryable, model.EntryFailed, model.EntryCancelled}
