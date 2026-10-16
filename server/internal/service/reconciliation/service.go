package reconciliation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/retry"
	uploadservice "github.com/sanjeev0303/object-storage-platform/internal/service/upload"
)

// Resolver finishes one kind of interrupted multipart operation.
// *upload.MultipartService implements it.
type Resolver interface {
	ResolveInitiation(ctx context.Context, tenantID, uploadID string) (model.UploadStatus, error)
	ResolveCompletion(ctx context.Context, tenantID, uploadID string) (model.UploadStatus, error)
	ResolveAbort(ctx context.Context, tenantID, uploadID string) (model.UploadStatus, error)
	Expire(ctx context.Context, tenantID, uploadID string) (model.UploadStatus, error)
	VerifyIntegrity(ctx context.Context, tenantID, uploadID string) (model.UploadStatus, error)
}

type Config struct {
	// Owner identifies this worker on the leases it takes.
	Owner           string
	StorageProvider string
	BatchSize       int
	// Lease is how long a claim lasts. Each task's work is cut off at the
	// lease, so a slow task never outlives its claim.
	Lease time.Duration
	// Grace leaves recently changed sessions to the request handling them.
	Grace time.Duration
	Retry retry.Policy
}

// Service runs reconciliation cycles. Several instances may run at once:
// claims are atomic and every outcome is written under the claim's lease.
type Service struct {
	tasks    interfaces.ReconciliationRepository
	resolver Resolver
	cfg      Config
	log      *slog.Logger
	now      func() time.Time
}

func NewService(
	tasks interfaces.ReconciliationRepository,
	resolver Resolver,
	cfg Config,
	log *slog.Logger,
) (*Service, error) {
	if tasks == nil || resolver == nil || log == nil {
		return nil, errors.New("reconciliation dependencies are required")
	}
	if cfg.Owner == "" || cfg.StorageProvider == "" {
		return nil, errors.New("reconciliation owner and storage provider are required")
	}
	if cfg.BatchSize <= 0 || cfg.Lease <= 0 || cfg.Grace < 0 {
		return nil, errors.New("reconciliation batch size and lease must be positive")
	}
	if cfg.Retry.MaxAttempts <= 0 || cfg.Retry.BaseDelay <= 0 || cfg.Retry.MaxDelay < cfg.Retry.BaseDelay {
		return nil, errors.New("reconciliation retry policy is invalid")
	}

	return &Service{
		tasks:    tasks,
		resolver: resolver,
		cfg:      cfg,
		log:      log,
		now:      func() time.Time { return time.Now().UTC() },
	}, nil
}

// CycleStats counts what one cycle did, for the cycle's log line.
type CycleStats struct {
	Enqueued  int
	Claimed   int
	Succeeded int
	Retried   int
	Exhausted int
	LeaseLost int
}

// RunOnce enqueues newly due work, claims a batch and processes it.
func (s *Service) RunOnce(ctx context.Context) (CycleStats, error) {
	var stats CycleStats

	enqueued, err := s.tasks.EnqueueDue(ctx, s.now(), s.cfg.Grace, s.cfg.StorageProvider, s.cfg.Retry.MaxAttempts)
	if err != nil {
		return stats, fmt.Errorf("enqueue due work: %w", err)
	}
	stats.Enqueued = enqueued

	tasks, err := s.tasks.Claim(ctx, s.cfg.Owner, s.now(), s.cfg.Lease, s.cfg.BatchSize)
	if err != nil {
		return stats, fmt.Errorf("claim work: %w", err)
	}
	stats.Claimed = len(tasks)

	for _, task := range tasks {
		if ctx.Err() != nil {
			// Unprocessed claims simply expire and are picked up again.
			break
		}
		s.process(ctx, task, &stats)
	}

	return stats, nil
}

func (s *Service) process(ctx context.Context, task model.ReconciliationTask, stats *CycleStats) {
	log := s.log.With(
		"task_id", task.ID,
		"tenant_id", task.TenantID,
		"upload_id", task.UploadID,
		"operation", string(task.Operation),
		"attempt", task.AttemptCount,
		"max_attempts", task.MaxAttempts,
	)

	workCtx, cancel := context.WithTimeout(ctx, s.cfg.Lease)
	status, err := s.resolve(workCtx, task)
	cancel()

	if ctx.Err() != nil {
		// Shutting down: the outcome is unknown, so write nothing. The
		// lease expires and another worker repeats the (idempotent) work.
		log.Info("reconciliation interrupted by shutdown")
		return
	}

	now := s.now()
	var releaseErr error

	switch {
	case err == nil:
		releaseErr = s.tasks.Finish(ctx, task.ID, s.cfg.Owner, now, model.ReconcileSucceeded, "")
		if releaseErr == nil {
			stats.Succeeded++
			logOutcome(log, task, status)
		}

	case retry.IsPermanent(err) || task.AttemptCount >= task.MaxAttempts:
		releaseErr = s.tasks.Finish(ctx, task.ID, s.cfg.Owner, now, model.ReconcileExhausted, retry.Sanitize(err))
		if releaseErr == nil {
			stats.Exhausted++
			log.Error("reconciliation exhausted; operator action required",
				"permanent", retry.IsPermanent(err), "error", retry.Sanitize(err))
		}

	default:
		next := now.Add(s.cfg.Retry.Delay(task.AttemptCount))
		releaseErr = s.tasks.Reschedule(ctx, task.ID, s.cfg.Owner, now, next, retry.Sanitize(err))
		if releaseErr == nil {
			stats.Retried++
			log.Warn("reconciliation will retry", "next_attempt_at", next, "error", retry.Sanitize(err))
		}
	}

	if errors.Is(releaseErr, model.ErrLeaseLost) {
		stats.LeaseLost++
		log.Warn("reconciliation lease lost; another worker owns the task")
	} else if releaseErr != nil {
		// The lease will expire and the task will run again; the work is idempotent.
		log.Error("record reconciliation outcome", "error", releaseErr)
	}
}

func (s *Service) resolve(ctx context.Context, task model.ReconciliationTask) (model.UploadStatus, error) {
	var status model.UploadStatus
	var err error

	switch task.Operation {
	case model.ReconcileInitiate:
		status, err = s.resolver.ResolveInitiation(ctx, task.TenantID, task.UploadID)
	case model.ReconcileComplete:
		status, err = s.resolver.ResolveCompletion(ctx, task.TenantID, task.UploadID)
	case model.ReconcileAbort:
		status, err = s.resolver.ResolveAbort(ctx, task.TenantID, task.UploadID)
	case model.ReconcileExpire:
		status, err = s.resolver.Expire(ctx, task.TenantID, task.UploadID)
	case model.ReconcileVerify:
		status, err = s.resolver.VerifyIntegrity(ctx, task.TenantID, task.UploadID)
	default:
		return "", retry.Permanent(fmt.Errorf("unknown reconciliation operation %q", task.Operation))
	}

	// A session deleted since the task was created needs nothing more.
	if errors.Is(err, uploadservice.ErrMultipartUploadNotFound) {
		return "", nil
	}
	return status, err
}

func logOutcome(log *slog.Logger, task model.ReconciliationTask, status model.UploadStatus) {
	if status != model.UploadStatusFailed {
		log.Info("reconciliation succeeded", "upload_status", string(status))
		return
	}

	// The session ended FAILED: worth a warning, and for VERIFY it means
	// the stored bytes did not pass the integrity check.
	if task.Operation == model.ReconcileVerify {
		log.Warn("upload failed integrity verification; kept in quarantine", "upload_status", string(status))
		return
	}
	log.Warn("upload finalized as failed", "upload_status", string(status))
}
