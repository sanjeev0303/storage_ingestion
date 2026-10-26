package ingestion

import (
	"context"
	"errors"
	"runtime"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/retry"
	"github.com/sanjeev0303/object-storage-platform/internal/service/upload"
)

// Sweeps are the worker's bounded passes over durable state. Each handles
// at most one batch, so a pass stays short whatever the backlog, and each
// is safe to run on several workers at once: every change is a conditional
// transition.

// ExpireLeases handles entries whose client stopped before completing:
// the upload is finished if the object is in fact there, and otherwise the
// entry's slot is released and it is retried with a new session.
func (s *Service) ExpireLeases(ctx context.Context) (int, error) {
	entries, err := s.deps.Repo.ExpiredLeases(ctx, s.now(), s.cfg.WorkerBatch)
	if err != nil {
		return 0, err
	}
	for _, e := range entries {
		if ctx.Err() != nil {
			break
		}
		s.expire(ctx, e)
	}
	return len(entries), nil
}

func (s *Service) expire(ctx context.Context, e model.IngestionEntry) {
	from := []model.EntryState{model.EntryScheduled, model.EntryUploading}
	if e.UploadID != "" {
		session, err := s.deps.Sessions.GetByID(ctx, e.TenantID, e.UploadID)
		if err != nil && !errors.Is(err, model.ErrUploadNotFound) {
			s.deps.Log.Warn("read expired entry's upload", "job_id", e.JobID, "position", e.Position, "error", retry.Sanitize(err))
			return // try again next pass
		}
		if err == nil {
			switch session.Status {
			case model.UploadStatusCompleted, model.UploadStatusVerified:
				// Completed, but the client never told us.
				_, _ = s.uploaded(ctx, e)
				return
			case model.UploadStatusCompleting:
				// Reconciliation is confirming the provider's answer.
				lease := s.now().Add(s.cfg.SlotLease)
				_, _ = s.deps.Repo.Transition(ctx, interfaces.EntryChange{
					JobID: e.JobID, Position: e.Position, From: from, To: e.State, Now: s.now(),
					UploadID: e.UploadID, LeaseExpiresAt: &lease,
				})
				return
			case model.UploadStatusPending:
				if e.Strategy == model.StrategySingle {
					// The client may have uploaded and stopped before
					// completing: look for the object.
					var status model.UploadStatus
					err := s.onStorage(ctx, func(ctx context.Context) error {
						var err error
						status, err = s.deps.Uploads.Complete(ctx, e.TenantID, e.UploadID)
						return err
					})
					if err == nil && (status == model.UploadStatusCompleted || status == model.UploadStatusVerified) {
						_, _ = s.uploaded(ctx, e)
						return
					}
					if errors.Is(err, upload.ErrObjectSizeMismatch) {
						s.fail(ctx, e, from, model.EntryErrSizeMismatch, "the stored object's size differs from the manifest", true)
						return
					}
				}
			}
			if e.Strategy == model.StrategyMultipart && !session.Status.IsTerminal() {
				s.abort(ctx, e)
			}
		}
	}
	s.count("ingestion_leases_expired", 1)
	s.fail(ctx, e, from, model.EntryErrLeaseExpired, "the client stopped before completing the upload", false)
}

// abort ends an entry's multipart upload at the provider, best effort:
// reconciliation cleans up whatever this misses.
func (s *Service) abort(ctx context.Context, e model.IngestionEntry) {
	job, err := s.deps.Repo.GetJob(ctx, e.TenantID, e.JobID)
	if err != nil {
		return
	}
	err = s.onStorage(ctx, func(ctx context.Context) error {
		_, err := s.deps.Multipart.Abort(ctx, e.TenantID, job.CreatedBy, e.UploadID)
		return err
	})
	if err != nil {
		s.deps.Log.Warn("abort entry's multipart upload", "job_id", e.JobID, "position", e.Position, "error", retry.Sanitize(err))
		return
	}
	s.count("ingestion_multipart_aborted", 1)
}

// RunDueRetries makes retryable entries whose backoff is over PENDING
// again, or requeues their held processing.
func (s *Service) RunDueRetries(ctx context.Context) (int, error) {
	entries, err := s.deps.Repo.DueRetries(ctx, s.now(), s.cfg.WorkerBatch)
	if err != nil {
		return 0, err
	}
	for _, e := range entries {
		if ctx.Err() != nil {
			break
		}
		if e.ErrorCode == model.EntryErrProcessingHeld && e.AssetID != "" {
			if err := s.requeue(ctx, e); err != nil {
				s.deps.Log.Warn("requeue held entry", "job_id", e.JobID, "position", e.Position, "error", retry.Sanitize(err))
			}
			continue
		}
		_, err := s.deps.Repo.Transition(ctx, interfaces.EntryChange{
			JobID: e.JobID, Position: e.Position, From: []model.EntryState{model.EntryRetryable},
			To: model.EntryPending, Now: s.now(),
		})
		if err != nil && !errors.Is(err, model.ErrInvalidEntryState) {
			return 0, err
		}
	}
	return len(entries), nil
}

// SyncOutcomes checks entries waiting for processing longer than
// OutcomeSyncAfter. Normally upload.processed records the outcome; this
// covers a lost event or a stack running without a queue.
func (s *Service) SyncOutcomes(ctx context.Context) (int, error) {
	entries, err := s.deps.Repo.AwaitingOutcome(ctx, s.now().Add(-s.cfg.OutcomeSyncAfter), s.cfg.WorkerBatch)
	if err != nil {
		return 0, err
	}
	for _, e := range entries {
		if ctx.Err() != nil {
			break
		}
		if _, err := s.settleOutcome(ctx, e); err != nil {
			s.deps.Log.Warn("sync entry outcome", "job_id", e.JobID, "position", e.Position, "error", retry.Sanitize(err))
		}
	}
	return len(entries), nil
}

// maxCancelRounds bounds the waiting entries one pass cancels per job.
const maxCancelRounds = 20

// AdvanceCancellations works through cancelling jobs: waiting entries are
// cancelled in batches, and uploads in progress are aborted and cancelled.
// Stored uploads keep going; the job is CANCELLED once they finish.
//
// Waiting entries are cancelled in position order from where the last
// batch stopped, across passes, so a batch never reads past the entries
// already cancelled. After a pass reaches the end, the next one starts
// over, but only if the job's counters still show entries waiting: an
// upload that failed meanwhile can be waiting again behind the cursor.
func (s *Service) AdvanceCancellations(ctx context.Context) (int, error) {
	jobs, err := s.deps.Repo.JobsInState(ctx, model.JobCancelling, s.cfg.WorkerBatch)
	if err != nil {
		return 0, err
	}
	s.cancelMu.Lock()
	reached := make(map[string]int64, len(jobs))
	for _, job := range jobs {
		reached[job.ID] = s.cancelledTo[job.ID]
	}
	s.cancelledTo = reached // jobs no longer cancelling are forgotten
	s.cancelMu.Unlock()

	total := 0
	for _, job := range jobs {
		after := reached[job.ID]
		waiting := job.Counters.Pending + job.Counters.Retryable
		for round := 0; waiting > 0 && round < maxCancelRounds && ctx.Err() == nil; round++ {
			n, last, err := s.deps.Repo.CancelWaiting(ctx, job.ID, after, s.cfg.WorkerBatch, s.now())
			if err != nil {
				return total, err
			}
			total += n
			after = last
			if n < s.cfg.WorkerBatch {
				after = 0
				break
			}
		}
		s.cancelMu.Lock()
		s.cancelledTo[job.ID] = after
		s.cancelMu.Unlock()
		inFlight, err := s.deps.Repo.InFlight(ctx, job.ID, s.cfg.WorkerBatch)
		if err != nil {
			return total, err
		}
		for _, e := range inFlight {
			if e.Strategy == model.StrategyMultipart && e.UploadID != "" {
				s.abort(ctx, e)
			}
			_, err := s.deps.Repo.Transition(ctx, interfaces.EntryChange{
				JobID: e.JobID, Position: e.Position, From: []model.EntryState{model.EntryScheduled, model.EntryUploading},
				To: model.EntryCancelled, Now: s.now(), UploadID: e.UploadID,
				ErrorCode: model.EntryErrCancelled, ErrorDetail: "the job was cancelled during the upload",
			})
			if err != nil && !errors.Is(err, model.ErrInvalidEntryState) {
				return total, err
			}
			total++
		}
	}
	return total, nil
}

// ResumeManifests reads staged manifests whose reader stopped, or that no
// event started (a stack running without a queue).
func (s *Service) ResumeManifests(ctx context.Context, owner string) (int, error) {
	jobs, err := s.deps.Repo.ClaimManifests(ctx, owner, "", s.now(), s.cfg.ManifestLease, s.cfg.ParseConcurrency)
	if err != nil {
		return 0, err
	}
	for _, job := range jobs {
		note, err := s.readStaged(ctx, owner, job)
		if err != nil {
			s.deps.Log.Warn("read staged manifest", "job_id", job.ID, "error", retry.Sanitize(err))
			continue
		}
		s.deps.Log.Info("staged manifest", "job_id", job.ID, "outcome", note)
	}
	return len(jobs), nil
}

// ReconcileReport counts what reconciliation looked at and repaired.
type ReconcileReport struct {
	JobsChecked     int
	JobsRepaired    int
	TenantsChecked  int
	TenantsRepaired int
}

// Reconcile recounts the counters of jobs with work in progress from their
// entries, then each such tenant's usage. It repairs drift (a counter left wrong by a bug or a manual change) without
// any request ever having to scan entries.
func (s *Service) Reconcile(ctx context.Context) (ReconcileReport, error) {
	var report ReconcileReport
	for _, state := range []model.JobState{model.JobReady, model.JobRunning, model.JobCancelling, model.JobValidating} {
		jobs, err := s.deps.Repo.JobsInState(ctx, state, s.cfg.WorkerBatch)
		if err != nil {
			return report, err
		}
		for _, job := range jobs {
			before, after, err := s.deps.Repo.Recount(ctx, job.ID, s.now())
			if err != nil {
				return report, err
			}
			report.JobsChecked++
			if before != after {
				report.JobsRepaired++
				s.count("ingestion_reconcile_job_repairs", 1)
				s.deps.Log.Warn("ingestion job counters repaired", "job_id", job.ID, "tenant_id", job.TenantID)
			}
		}
	}
	tenants, err := s.deps.Repo.TenantsWithWork(ctx, s.cfg.WorkerBatch)
	if err != nil {
		return report, err
	}
	for _, tenant := range tenants {
		before, after, err := s.deps.Repo.ReconcileUsage(ctx, tenant, s.now())
		if err != nil {
			return report, err
		}
		report.TenantsChecked++
		if before != after {
			report.TenantsRepaired++
			s.count("ingestion_reconcile_usage_repairs", 1)
			s.deps.Log.Warn("ingestion tenant usage repaired", "tenant_id", tenant,
				"reserved_before", before.ReservedBytes, "reserved_after", after.ReservedBytes,
				"uploads_before", before.ActiveUploads, "uploads_after", after.ActiveUploads)
		}
	}
	return report, nil
}

// Gauges refreshes the gauges that tell where a bottleneck is: slots in
// use (the upload side), storage calls in flight, and this process's
// memory and goroutines.
func (s *Service) Gauges(ctx context.Context) {
	if slots, err := s.deps.Repo.SlotsInUse(ctx); err == nil {
		s.deps.Metrics.Set("ingestion_slots_in_use", int64(slots))
	}
	s.deps.Metrics.Set("ingestion_storage_calls_in_flight", int64(s.storage.InUse()))
	s.deps.Metrics.Set("ingestion_manifest_reads_in_flight", int64(s.parsing.InUse()))
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	s.deps.Metrics.Set("go_heap_alloc_bytes", int64(mem.HeapAlloc))
	s.deps.Metrics.Set("go_heap_inuse_bytes", int64(mem.HeapInuse))
	s.deps.Metrics.Set("go_sys_bytes", int64(mem.Sys))
	s.deps.Metrics.Set("go_goroutines", int64(runtime.NumGoroutine()))
}
