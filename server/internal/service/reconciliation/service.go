package reconciliation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/metrics"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/retry"
	uploadservice "github.com/sanjeev0303/object-storage-platform/internal/service/upload"
)

// Resolver finishes interrupted multipart operations and scans for
// discrepancies. *upload.MultipartService implements it.
type Resolver interface {
	ResolveInitiation(ctx context.Context, tenantID, uploadID string) (model.UploadStatus, error)
	ResolveCompletion(ctx context.Context, tenantID, uploadID string) (model.UploadStatus, error)
	ResolveAbort(ctx context.Context, tenantID, uploadID string) (model.UploadStatus, error)
	Expire(ctx context.Context, tenantID, uploadID string) (model.UploadStatus, error)
	VerifyIntegrity(ctx context.Context, tenantID, uploadID string) (model.UploadStatus, error)

	ScanOrphanSessions(ctx context.Context, cursor string, opts uploadservice.ScanOptions) ([]model.ReconciliationFinding, string, error)
	ScanOrphanObjects(ctx context.Context, cursor string, opts uploadservice.ScanOptions) ([]model.ReconciliationFinding, string, error)
	CleanStaleParts(ctx context.Context, refs []model.UploadRef, dryRun bool) ([]model.ReconciliationFinding, error)
	AbortLeftoverSessions(ctx context.Context, sessions []model.UploadSession, dryRun bool) ([]model.ReconciliationFinding, error)
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

	// DryRun reports what a cycle would do and changes nothing: no tasks
	// are created or claimed, no provider session or object is touched,
	// and no finding is written.
	DryRun bool
	// ScanPageSize and ScanPages bound each discrepancy scan per cycle;
	// the scan continues from where it stopped on the next cycle.
	ScanPageSize int
	ScanPages    int
	// OrphanMinAge is how old an unowned provider session or object must
	// be before it is removed (retention against in-flight requests).
	OrphanMinAge time.Duration
	// StuckAfter reports non-terminal sessions unchanged for this long.
	StuckAfter time.Duration
	// SessionScan picks how orphaned provider sessions are found:
	// SessionScanProvider lists every provider session under quarantine/
	// (AWS S3); SessionScanDatabase checks recently finished sessions key by
	// key (MinIO, which cannot list by prefix); SessionScanOff skips it.
	SessionScan string
	// SessionLookback bounds the database scan to sessions finished this
	// recently; older leftovers are left to a provider lifecycle rule.
	SessionLookback time.Duration
	// DeleteUnrecorded lets the scans remove provider sessions and objects
	// that have no upload session at all. Off by default: they are only
	// reported, since another environment may share the bucket.
	DeleteUnrecorded bool
}

const (
	SessionScanProvider = "provider"
	SessionScanDatabase = "database"
	SessionScanOff      = "off"
)

// Service runs reconciliation cycles. Several instances may run at once:
// claims are atomic and every outcome is written under the claim's lease.
type Service struct {
	tasks    interfaces.ReconciliationRepository
	resolver Resolver
	cfg      Config
	log      *slog.Logger
	metrics  *metrics.Registry
	now      func() time.Time

	// Where each listing scan continues next cycle (this process only).
	sessionCursor string
	objectCursor  string
}

func NewService(
	tasks interfaces.ReconciliationRepository,
	resolver Resolver,
	cfg Config,
	log *slog.Logger,
	registry *metrics.Registry,
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
	if cfg.ScanPageSize <= 0 || cfg.ScanPageSize > 1000 || cfg.ScanPages <= 0 || cfg.OrphanMinAge <= 0 || cfg.StuckAfter <= 0 {
		return nil, errors.New("reconciliation scan settings must be positive (page size at most 1000)")
	}
	switch cfg.SessionScan {
	case "":
		cfg.SessionScan = SessionScanProvider
	case SessionScanProvider, SessionScanOff:
	case SessionScanDatabase:
		if cfg.SessionLookback <= cfg.OrphanMinAge {
			return nil, errors.New("the database session scan needs a lookback longer than the orphan minimum age")
		}
	default:
		return nil, fmt.Errorf("unknown session scan strategy %q", cfg.SessionScan)
	}

	return &Service{
		tasks:    tasks,
		resolver: resolver,
		cfg:      cfg,
		log:      log,
		metrics:  registry,
		now:      func() time.Time { return time.Now().UTC() },
	}, nil
}

// CycleStats is what one cycle did; a dry run fills Due and Findings with
// what it would have done.
type CycleStats struct {
	DryRun    bool
	Enqueued  int
	Claimed   int
	Succeeded int
	Retried   int
	Exhausted int
	LeaseLost int
	Due       []model.DueReconciliation
	Findings  []model.ReconciliationFinding
	Stuck     map[model.UploadStatus]int
	Duration  time.Duration
}

// RunOnce runs one cycle: live tasks first, then the discrepancy scans and
// the stuck-upload report. A failing step is reported but does not stop
// the others.
func (s *Service) RunOnce(ctx context.Context) (CycleStats, error) {
	started := time.Now()
	stats := CycleStats{DryRun: s.cfg.DryRun}
	var errs []error

	if s.cfg.DryRun {
		due, err := s.tasks.ListDue(ctx, s.now(), s.cfg.Grace, s.cfg.BatchSize)
		stats.Due = due
		errs = append(errs, err)
	} else {
		errs = append(errs, s.runTasks(ctx, &stats))
	}

	if ctx.Err() == nil {
		errs = append(errs, s.scan(ctx, &stats))
	}
	if ctx.Err() == nil {
		stuck, err := s.tasks.CountStuck(ctx, s.now().Add(-s.cfg.StuckAfter))
		stats.Stuck = stuck
		errs = append(errs, err)
		s.reportStuck(stuck)
	}

	stats.Duration = time.Since(started)
	s.metrics.Set("reconciliation_last_cycle_ms", stats.Duration.Milliseconds())
	s.metrics.Add("reconciliation_cycles", 1)
	return stats, errors.Join(errs...)
}

func (s *Service) runTasks(ctx context.Context, stats *CycleStats) error {
	enqueued, err := s.tasks.EnqueueDue(ctx, s.now(), s.cfg.Grace, s.cfg.StorageProvider, s.cfg.Retry.MaxAttempts)
	if err != nil {
		return fmt.Errorf("enqueue due work: %w", err)
	}
	stats.Enqueued = enqueued

	tasks, err := s.tasks.Claim(ctx, s.cfg.Owner, s.now(), s.cfg.Lease, s.cfg.BatchSize)
	if err != nil {
		return fmt.Errorf("claim work: %w", err)
	}
	stats.Claimed = len(tasks)

	// One task at a time: bounded work and no goroutine per task. Run more
	// worker processes for more throughput; claims keep them apart.
	for _, task := range tasks {
		if ctx.Err() != nil {
			// Unprocessed claims simply expire and are picked up again.
			break
		}
		s.process(ctx, task, stats)
	}
	return nil
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
	started := time.Now()

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
	op := string(task.Operation)
	var releaseErr error

	switch {
	case err == nil:
		releaseErr = s.tasks.Finish(ctx, task.ID, s.cfg.Owner, now, model.ReconcileSucceeded, string(status), "")
		if releaseErr == nil {
			stats.Succeeded++
			s.metrics.Add("reconciliation_succeeded_"+op, 1)
			s.metrics.Add("reconciliation_ms_"+op, time.Since(started).Milliseconds())
			logOutcome(log, task, status)
		}

	case retry.IsPermanent(err) || task.AttemptCount >= task.MaxAttempts:
		releaseErr = s.tasks.Finish(ctx, task.ID, s.cfg.Owner, now, model.ReconcileExhausted, "", retry.Sanitize(err))
		if releaseErr == nil {
			stats.Exhausted++
			s.metrics.Add("reconciliation_exhausted_"+op, 1)
			log.Error("reconciliation exhausted; operator action required",
				"permanent", retry.IsPermanent(err), "error", retry.Sanitize(err))
		}

	default:
		next := now.Add(s.cfg.Retry.Delay(task.AttemptCount))
		releaseErr = s.tasks.Reschedule(ctx, task.ID, s.cfg.Owner, now, next, retry.Sanitize(err))
		if releaseErr == nil {
			stats.Retried++
			s.metrics.Add("reconciliation_retried_"+op, 1)
			log.Warn("reconciliation will retry", "next_attempt_at", next, "error", retry.Sanitize(err))
		}
	}

	if errors.Is(releaseErr, model.ErrLeaseLost) {
		stats.LeaseLost++
		s.metrics.Add("reconciliation_lease_lost", 1)
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

// scan runs the bounded discrepancy scans: orphaned provider sessions,
// orphaned quarantine objects, and stale part rows. Each listing scan
// takes at most ScanPages pages per cycle and resumes from its cursor.
func (s *Service) scan(ctx context.Context, stats *CycleStats) error {
	opts := uploadservice.ScanOptions{
		PageSize: s.cfg.ScanPageSize, MinAge: s.cfg.OrphanMinAge, DryRun: s.cfg.DryRun, DeleteUnrecorded: s.cfg.DeleteUnrecorded,
	}
	var errs []error

	var err error
	switch s.cfg.SessionScan {
	case SessionScanProvider:
		s.sessionCursor, err = s.scanPages(ctx, stats, s.sessionCursor, opts, s.resolver.ScanOrphanSessions)
	case SessionScanDatabase:
		err = s.scanFinishedSessions(ctx, stats)
	}
	errs = append(errs, err)

	s.objectCursor, err = s.scanPages(ctx, stats, s.objectCursor, opts, s.resolver.ScanOrphanObjects)
	errs = append(errs, err)

	refs, err := s.tasks.StaleParts(ctx, s.now().Add(-s.cfg.OrphanMinAge), s.cfg.ScanPageSize)
	if err == nil {
		var findings []model.ReconciliationFinding
		findings, err = s.resolver.CleanStaleParts(ctx, refs, s.cfg.DryRun)
		s.record(ctx, stats, findings)
	}
	errs = append(errs, err)

	return errors.Join(errs...)
}

// scanFinishedSessions pages through recently finished sessions in the
// database (keyset on ID, resuming next cycle) and has the provider checked
// for leftover multipart sessions under each one's key.
func (s *Service) scanFinishedSessions(ctx context.Context, stats *CycleStats) error {
	until := s.now().Add(-s.cfg.OrphanMinAge)
	since := s.now().Add(-s.cfg.SessionLookback)

	for page := 0; page < s.cfg.ScanPages; page++ {
		if ctx.Err() != nil {
			return nil
		}
		sessions, err := s.tasks.FinishedSessions(ctx, since, until, s.sessionCursor, s.cfg.ScanPageSize)
		if err != nil {
			return err
		}
		findings, err := s.resolver.AbortLeftoverSessions(ctx, sessions, s.cfg.DryRun)
		s.record(ctx, stats, findings)
		if err != nil {
			return err
		}
		if len(sessions) < s.cfg.ScanPageSize {
			s.sessionCursor = "" // done; the next cycle starts over
			return nil
		}
		s.sessionCursor = sessions[len(sessions)-1].ID
	}
	return nil
}

type scanFunc func(ctx context.Context, cursor string, opts uploadservice.ScanOptions) ([]model.ReconciliationFinding, string, error)

func (s *Service) scanPages(ctx context.Context, stats *CycleStats, cursor string, opts uploadservice.ScanOptions, scan scanFunc) (string, error) {
	for page := 0; page < s.cfg.ScanPages; page++ {
		if ctx.Err() != nil {
			return cursor, nil
		}
		findings, next, err := scan(ctx, cursor, opts)
		s.record(ctx, stats, findings)
		if err != nil {
			return cursor, err
		}
		cursor = next
		if cursor == "" {
			break // the listing is done; next cycle starts over
		}
	}
	return cursor, nil
}

// record keeps a cycle's findings: stored and counted on a real run, only
// reported on a dry run.
func (s *Service) record(ctx context.Context, stats *CycleStats, findings []model.ReconciliationFinding) {
	for _, f := range findings {
		stats.Findings = append(stats.Findings, f)
		s.metrics.Add("discrepancy_"+f.Kind+"_"+f.Outcome, 1)

		log := s.log.With("kind", f.Kind, "action", f.Action, "outcome", f.Outcome,
			"tenant_id", f.TenantID, "upload_id", f.UploadID, "reason", f.Reason)
		switch f.Outcome {
		case model.FindingPlanned:
			log.Info("reconciliation dry run: would act")
			continue
		case model.FindingReported:
			log.Warn("unrecorded item left in place; set RECONCILIATION_UNRECORDED_ACTION=delete to remove such items")
			continue
		case model.FindingFailed:
			log.Error("reconciliation action failed; retried next cycle")
		default:
			log.Info("reconciliation action done")
		}

		if err := s.tasks.RecordFinding(ctx, f); err != nil {
			// The action already happened; only its record is missing.
			s.log.Error("record reconciliation finding", "kind", f.Kind, "error", err)
		}
	}
}

func (s *Service) reportStuck(stuck map[model.UploadStatus]int) {
	for _, status := range []model.UploadStatus{
		model.UploadStatusPending, model.UploadStatusUploading, model.UploadStatusCompleting, model.UploadStatusAborting,
	} {
		n := stuck[status]
		s.metrics.Set("uploads_stuck_"+string(status), int64(n))
		if n > 0 {
			s.log.Warn("uploads stuck beyond threshold", "status", string(status), "count", n,
				"threshold", s.cfg.StuckAfter.String())
		}
	}
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
