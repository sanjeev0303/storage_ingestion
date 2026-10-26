package reconciliation

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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

	// PlatformPlace is the platform's staging area; ScanSessionsAt and
	// ScanObjectsAt scan one page of any connection's.
	PlatformPlace() uploadservice.ScanPlace
	ScanSessionsAt(ctx context.Context, place uploadservice.ScanPlace, cursor string, opts uploadservice.ScanOptions) ([]model.ReconciliationFinding, string, error)
	ScanObjectsAt(ctx context.Context, place uploadservice.ScanPlace, cursor string, opts uploadservice.ScanOptions) ([]model.ReconciliationFinding, string, error)
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

	// Set by Route: tenants' connections for the listing scans, and the
	// inventory that keeps each scan's progress.
	connections interfaces.ConnectionLister
	router      interfaces.StorageRouter
	inventory   interfaces.InventoryRepository

	// sessionCursor is where the finished-session scan continues next
	// cycle; cursors, without an inventory, where each listing scan does
	// (this process only).
	sessionCursor string
	cursors       map[string]string
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
		cursors:  map[string]string{},
	}, nil
}

// Route extends the listing scans to tenants' storage connections that
// stage uploads in their own buckets, and keeps every scan's progress in
// inventory: its cursor, pages, findings and sanitized last error, under a
// lease, so a scan resumes after a restart and two workers never scan one
// place at once. Without it only the platform is scanned, from cursors
// kept in this process.
func (s *Service) Route(connections interfaces.ConnectionLister, router interfaces.StorageRouter, inventory interfaces.InventoryRepository) *Service {
	s.connections, s.router, s.inventory = connections, router, inventory
	return s
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
// orphaned staging objects, and stale part rows. Each listing scan runs on
// every place uploads land, takes at most ScanPages pages of each per
// cycle and resumes from that place's cursor.
func (s *Service) scan(ctx context.Context, stats *CycleStats) error {
	opts := uploadservice.ScanOptions{
		PageSize: s.cfg.ScanPageSize, MinAge: s.cfg.OrphanMinAge, DryRun: s.cfg.DryRun, DeleteUnrecorded: s.cfg.DeleteUnrecorded,
	}
	places, err := s.places(ctx)
	errs := []error{err}

	switch s.cfg.SessionScan {
	case SessionScanProvider:
		err = s.scanPlaces(ctx, stats, model.ScanUploadSessions, places, opts, s.resolver.ScanSessionsAt)
	case SessionScanDatabase:
		err = s.scanFinishedSessions(ctx, stats)
	}
	errs = append(errs, err)

	errs = append(errs, s.scanPlaces(ctx, stats, model.ScanUploadObjects, places, opts, s.resolver.ScanObjectsAt))

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

// place is a staging area to scan, or why a connection's could not be
// reached this cycle.
type place struct {
	uploadservice.ScanPlace
	err error
}

// places are where uploads land: the platform's staging area and, once
// routed, the staging prefix of every tenant connection that holds work
// (active or disabled) and stages in its own bucket. Only trusted records
// name them. A connection that cannot be reached comes back with its
// error, to be recorded; it never falls back to the platform.
func (s *Service) places(ctx context.Context) ([]place, error) {
	places := []place{{ScanPlace: s.resolver.PlatformPlace()}}
	if s.connections == nil || s.router == nil {
		return places, nil
	}
	connections, err := s.connections.ConnectionsHoldingWork(ctx, "")
	if err != nil {
		return places, fmt.Errorf("list storage connections: %w", err)
	}
	for _, c := range connections {
		if c.StagingMode != model.StagingInPlace {
			continue // its uploads land on the platform
		}
		p := place{ScanPlace: uploadservice.ScanPlace{
			ConnectionID: c.ID, Bucket: c.Bucket, Prefix: c.StagingPrefix, TenantID: c.TenantID, Customer: true,
		}}
		loc, err := s.router.Locate(ctx, c.TenantID, c.ID)
		if err != nil {
			p.err = fmt.Errorf("storage connection %s: %w", c.ID, err)
		} else {
			p.Storage, p.Bucket, p.Prefix = loc.Storage, loc.StagingBucket, loc.StagingPrefix
		}
		places = append(places, p)
	}
	return places, nil
}

type placeScan func(ctx context.Context, place uploadservice.ScanPlace, cursor string, opts uploadservice.ScanOptions) ([]model.ReconciliationFinding, string, error)

// scanPlaces runs one listing scan on every place. A place that fails is
// recorded and the others go on.
func (s *Service) scanPlaces(ctx context.Context, stats *CycleStats, kind string, places []place, opts uploadservice.ScanOptions, scan placeScan) error {
	var errs []error
	for _, p := range places {
		if ctx.Err() != nil {
			break
		}
		if s.inventory == nil || s.cfg.DryRun {
			errs = append(errs, s.scanInMemory(ctx, stats, kind, p, opts, scan))
		} else {
			errs = append(errs, s.scanPersisted(ctx, stats, kind, p, opts, scan))
		}
	}
	return errors.Join(errs...)
}

// scanInMemory scans a place from a cursor kept in this process. A dry run
// scans this way too, so it writes no scan record.
func (s *Service) scanInMemory(ctx context.Context, stats *CycleStats, kind string, p place, opts uploadservice.ScanOptions, scan placeScan) error {
	if p.err != nil {
		return p.err
	}
	key := kind + " " + p.ConnectionID
	cursor := s.cursors[key]
	defer func() { s.cursors[key] = cursor }()
	for page := 0; page < s.cfg.ScanPages; page++ {
		if ctx.Err() != nil {
			return nil
		}
		findings, next, err := scan(ctx, p.ScanPlace, cursor, opts)
		s.record(ctx, stats, findings)
		if err != nil {
			return err
		}
		cursor = next
		if cursor == "" {
			break // the listing is done; next cycle starts over
		}
	}
	return nil
}

// maxScanErrors ends a scan after that many failed pages in a row; the next
// one starts over. Until then it resumes from its saved cursor.
const maxScanErrors = 5

// scanPersisted scans a place from the cursor saved in the inventory and
// saves it after every page.
func (s *Service) scanPersisted(ctx context.Context, stats *CycleStats, kind string, p place, opts uploadservice.ScanOptions, scan placeScan) error {
	run, ok, err := s.inventory.OpenScan(ctx, kind, p.ConnectionID, p.Bucket, p.ListPrefix(), s.cfg.Owner, newScanID(), s.now(), s.cfg.Lease)
	if err != nil {
		return fmt.Errorf("open %s scan: %w", kind, err)
	}
	if !ok {
		return nil // another worker is scanning this place
	}
	if p.err != nil {
		return s.scanFailed(ctx, run, p.err)
	}
	for page := 0; page < s.cfg.ScanPages; page++ {
		if ctx.Err() != nil {
			return nil // the saved cursor resumes it
		}
		findings, next, err := scan(ctx, p.ScanPlace, run.Cursor, opts)
		s.record(ctx, stats, findings)
		if err != nil {
			return s.scanFailed(ctx, run, err)
		}
		run.Cursor = next
		run.Pages++
		run.CandidatesMarked += len(findings)
		run.Errors, run.LastError = 0, ""
		if next == "" {
			if err := s.inventory.FinishScan(ctx, run, s.cfg.Owner, s.now(), model.ScanCompleted); err != nil {
				return fmt.Errorf("finish %s scan: %w", kind, err)
			}
			return nil
		}
		if err := s.inventory.SaveProgress(ctx, run, s.cfg.Owner, s.now(), s.cfg.Lease); err != nil {
			return fmt.Errorf("save %s scan: %w", kind, err)
		}
	}
	return nil
}

// scanFailed keeps a failed scan resumable from its last saved cursor, with
// the failure recorded (sanitized), until maxScanErrors in a row end it.
func (s *Service) scanFailed(ctx context.Context, run model.InventoryScan, cause error) error {
	s.metrics.Add("reconciliation_scan_errors", 1)
	run.Errors++
	run.LastError = retry.Sanitize(cause)
	var err error
	if run.Errors >= maxScanErrors {
		err = s.inventory.FinishScan(ctx, run, s.cfg.Owner, s.now(), model.ScanFailed)
	} else {
		err = s.inventory.SaveProgress(ctx, run, s.cfg.Owner, s.now(), s.cfg.Lease)
	}
	s.log.Warn("reconciliation scan failed; it resumes next cycle", "kind", run.Kind, "connection", run.ConnectionID,
		"prefix", run.Prefix, "errors", run.Errors, "error", run.LastError)
	return errors.Join(fmt.Errorf("%s scan of %s on %s: %w", run.Kind, run.Prefix, run.ConnectionID, cause), err)
}

func newScanID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "rs_" + hex.EncodeToString(b[:])
}

// record keeps a cycle's findings: stored and counted on a real run, only
// reported on a dry run.
func (s *Service) record(ctx context.Context, stats *CycleStats, findings []model.ReconciliationFinding) {
	for _, f := range findings {
		stats.Findings = append(stats.Findings, f)
		s.metrics.Add("discrepancy_"+f.Kind+"_"+f.Outcome, 1)

		log := s.log.With("kind", f.Kind, "action", f.Action, "outcome", f.Outcome, "connection", f.ConnectionID,
			"tenant_id", f.TenantID, "upload_id", f.UploadID, "reason", f.Reason)
		switch f.Outcome {
		case model.FindingPlanned:
			log.Info("reconciliation dry run: would act")
			continue
		case model.FindingReported:
			if model.IsPlatformConnection(f.ConnectionID) {
				log.Warn("unrecorded item left in place; set RECONCILIATION_UNRECORDED_ACTION=delete to remove such items")
			} else {
				log.Warn("item on a tenant's own storage left in place: no record proves it is the platform's")
			}
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
