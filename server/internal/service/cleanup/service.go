// Package cleanup finds and removes stored objects nothing owns, and
// reports records whose objects are missing. It works as mark and sweep:
// a resumable inventory scan marks unowned objects, and a later sweep
// deletes only those seen repeatedly, past a safety interval, that a
// fresh check still finds unowned.
package cleanup

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/metrics"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/retry"
)

// reportLimit bounds the report lines one cycle keeps.
const reportLimit = 1000

type Dependencies struct {
	Storage   interfaces.LifecycleStorage
	Inventory interfaces.InventoryRepository
	Cursors   interfaces.CursorRepository
	Uploads   interfaces.UploadRepository
	Assets    interfaces.AssetRepository
	Jobs      interfaces.ProcessingJobRepository
	Holds     interfaces.LegalHoldRepository
	Events    interfaces.LifecycleEventRepository
	// Metrics is optional.
	Metrics *metrics.Registry
}

type Config struct {
	// Owner identifies this worker on the scan leases it takes.
	Owner            string
	QuarantineBucket string
	DeliveryBucket   string
	DeliveryPrefix   string
	// TenantID limits every step to one tenant's objects and records.
	TenantID string

	PageSize    int
	PagesPerRun int
	Lease       time.Duration
	// MinAge: younger objects are never candidates (in-flight writes).
	MinAge time.Duration
	// MinObservations and SafetyInterval gate the sweep: a candidate must
	// be seen in that many scans, the first that long ago.
	MinObservations int
	SafetyInterval  time.Duration
	// FailedUploadRetention keeps a failed upload's object as evidence.
	FailedUploadRetention time.Duration

	// DeleteOrphans lets the sweep delete candidates that a record proves
	// unowned. Off, they are only reported.
	DeleteOrphans bool
	// DeleteUnrecorded also lets it delete objects with no record at all.
	// Only safe when this database is the bucket's sole writer.
	DeleteUnrecorded bool

	MaxDeletionsPerRun int
	MaxAttempts        int
	// MaxScanErrors ends a scan as FAILED after that many listing errors
	// in a row; until then it resumes from its cursor.
	MaxScanErrors int
	// CheckBatch is how many assets one missing-object check reads.
	CheckBatch int
	// DryRun reports and writes nothing.
	DryRun bool
}

type Service struct {
	deps Dependencies
	cfg  Config
	log  *slog.Logger
	now  func() time.Time
}

func NewService(deps Dependencies, cfg Config, log *slog.Logger) (*Service, error) {
	if deps.Storage == nil || deps.Inventory == nil || deps.Cursors == nil || deps.Uploads == nil || deps.Assets == nil ||
		deps.Jobs == nil || deps.Holds == nil || deps.Events == nil || log == nil {
		return nil, errors.New("cleanup dependencies are required")
	}
	cfg.DeliveryPrefix = strings.Trim(cfg.DeliveryPrefix, "/")
	if cfg.Owner == "" || cfg.QuarantineBucket == "" || cfg.DeliveryBucket == "" || cfg.DeliveryPrefix == "" || cfg.DeliveryPrefix == "quarantine" {
		return nil, errors.New("cleanup owner, buckets and a non-quarantine delivery prefix are required")
	}
	if cfg.PageSize <= 0 || cfg.PageSize > 1000 || cfg.PagesPerRun <= 0 || cfg.Lease <= 0 || cfg.CheckBatch <= 0 {
		return nil, errors.New("cleanup page size (at most 1000), pages, lease and check batch must be positive")
	}
	if cfg.MinAge <= 0 || cfg.SafetyInterval <= 0 || cfg.MinObservations < 2 || cfg.FailedUploadRetention <= 0 {
		return nil, errors.New("cleanup needs a positive grace and safety interval and at least two observations")
	}
	if cfg.MaxDeletionsPerRun <= 0 || cfg.MaxAttempts <= 0 || cfg.MaxScanErrors <= 0 {
		return nil, errors.New("cleanup deletion, attempt and scan error limits must be positive")
	}
	if cfg.TenantID != "" && strings.Contains(cfg.TenantID, "/") {
		return nil, errors.New("cleanup tenant scope must be a tenant ID")
	}
	return &Service{deps: deps, cfg: cfg, log: log, now: func() time.Time { return time.Now().UTC() }}, nil
}

// CycleStats is what one cycle did, or would do on a dry run.
type CycleStats struct {
	DryRun         bool
	PagesScanned   int
	ObjectsSeen    int
	Marked         int
	Skipped        map[string]int
	ScansCompleted int
	ScanErrors     int

	Deleted     int
	Cleared     int
	Kept        int
	SweepFailed int

	MissingChecked int
	Missing        int
	// Report lists what a dry run would do, what report-only mode kept,
	// and missing objects.
	Report []string
}

func (st *CycleStats) note(line string) {
	if len(st.Report) < reportLimit {
		st.Report = append(st.Report, line)
	}
}

// area is one bucket prefix the platform writes to.
type area struct {
	bucket string
	prefix string
	kind   string
}

func (s *Service) areas() []area {
	scope := ""
	if s.cfg.TenantID != "" {
		scope = s.cfg.TenantID + "/"
	}
	return []area{
		{bucket: s.cfg.QuarantineBucket, prefix: "quarantine/" + scope, kind: kindQuarantine},
		{bucket: s.cfg.DeliveryBucket, prefix: s.cfg.DeliveryPrefix + "/" + scope, kind: kindDelivery},
	}
}

// RunOnce scans, sweeps and checks for missing objects. A failing step is
// reported but does not stop the others.
func (s *Service) RunOnce(ctx context.Context) (CycleStats, error) {
	stats := CycleStats{DryRun: s.cfg.DryRun}
	var errs []error
	errs = append(errs, s.Scan(ctx, &stats))
	if ctx.Err() == nil {
		errs = append(errs, s.Sweep(ctx, &stats))
	}
	if ctx.Err() == nil {
		errs = append(errs, s.CheckMissing(ctx, &stats))
	}
	return stats, errors.Join(errs...)
}

// Scan continues each area's inventory scan for up to PagesPerRun pages,
// marking unowned objects. The scan's cursor is saved after every page.
func (s *Service) Scan(ctx context.Context, stats *CycleStats) error {
	var errs []error
	for _, a := range s.areas() {
		if ctx.Err() != nil {
			break
		}
		if s.cfg.DryRun {
			errs = append(errs, s.preview(ctx, a, stats))
			continue
		}
		errs = append(errs, s.scanArea(ctx, a, stats))
	}
	return errors.Join(errs...)
}

func (s *Service) scanArea(ctx context.Context, a area, stats *CycleStats) error {
	scan, ok, err := s.deps.Inventory.OpenScan(ctx, a.bucket, a.prefix, s.cfg.Owner, newID(), s.now(), s.cfg.Lease)
	if err != nil {
		return fmt.Errorf("open inventory scan: %w", err)
	}
	if !ok {
		return nil // another worker is scanning this prefix
	}

	for page := 0; page < s.cfg.PagesPerRun; page++ {
		if ctx.Err() != nil {
			return nil // the saved cursor resumes it
		}
		listing, err := s.deps.Storage.ListObjectsPage(ctx, a.bucket, a.prefix, scan.Cursor, s.cfg.PageSize)
		if err != nil {
			return s.scanError(ctx, scan, a, stats, err)
		}

		for _, object := range listing.Objects {
			if err := s.inspectObject(ctx, a, &scan, object, stats); err != nil {
				return s.scanError(ctx, scan, a, stats, err)
			}
		}
		scan.Cursor = listing.Cursor
		scan.Pages++
		scan.ObjectsSeen += int64(len(listing.Objects))
		scan.Errors = 0
		stats.PagesScanned++
		s.deps.Metrics.Add("inventory_pages_scanned", 1)

		if listing.Cursor == "" {
			if err := s.deps.Inventory.FinishScan(ctx, scan, s.cfg.Owner, s.now(), model.ScanCompleted); err != nil {
				return fmt.Errorf("finish inventory scan: %w", err)
			}
			stats.ScansCompleted++
			s.deps.Metrics.Add("inventory_scans_completed", 1)
			s.log.Info("inventory scan completed", "bucket", a.bucket, "prefix", a.prefix, "pages", scan.Pages,
				"objects", scan.ObjectsSeen, "candidates", scan.CandidatesMarked)
			return nil
		}
		if err := s.deps.Inventory.SaveProgress(ctx, scan, s.cfg.Owner, s.now(), s.cfg.Lease); err != nil {
			return fmt.Errorf("save inventory scan progress: %w", err)
		}
	}
	return nil
}

// scanError keeps the scan resumable from its last saved cursor; after
// MaxScanErrors in a row it ends the scan as FAILED. A failed scan never
// counts towards sweeping: candidates must be seen in completed scans.
func (s *Service) scanError(ctx context.Context, scan model.InventoryScan, a area, stats *CycleStats, cause error) error {
	stats.ScanErrors++
	s.deps.Metrics.Add("inventory_scan_errors", 1)
	scan.Errors++
	scan.LastError = retry.Sanitize(cause)

	if scan.Errors >= s.cfg.MaxScanErrors {
		if err := s.deps.Inventory.FinishScan(ctx, scan, s.cfg.Owner, s.now(), model.ScanFailed); err != nil {
			return errors.Join(cause, err)
		}
		s.deps.Metrics.Add("inventory_scans_failed", 1)
		s.log.Error("inventory scan failed; the next scan starts over", "bucket", a.bucket, "prefix", a.prefix, "error", scan.LastError)
		return fmt.Errorf("inventory scan of %s failed: %w", a.prefix, cause)
	}
	if err := s.deps.Inventory.SaveProgress(ctx, scan, s.cfg.Owner, s.now(), s.cfg.Lease); err != nil {
		return errors.Join(cause, err)
	}
	s.log.Warn("inventory scan page failed; resuming next cycle", "bucket", a.bucket, "prefix", a.prefix,
		"errors", scan.Errors, "error", scan.LastError)
	return fmt.Errorf("inventory scan of %s: %w", a.prefix, cause)
}

func (s *Service) inspectObject(ctx context.Context, a area, scan *model.InventoryScan, object interfaces.StoredObject, stats *CycleStats) error {
	stats.ObjectsSeen++
	v, err := s.classify(ctx, a, object, true)
	if err != nil {
		return err
	}
	if !v.orphan {
		s.countSkip(stats, v.skip)
		return nil
	}

	now := s.now()
	marked, err := s.deps.Inventory.Mark(ctx, model.OrphanCandidate{
		ID: newID(), Bucket: a.bucket, ObjectKey: object.ObjectKey, ETag: object.ETag, SizeBytes: object.SizeBytes,
		Category: v.category, TenantID: v.tenantID, Reason: v.reason, Status: model.CandidateMarked,
		Observations: 1, FirstSeenAt: now, LastSeenAt: now, LastScanID: scan.ID, CreatedAt: now, UpdatedAt: now,
	}, scan.ID, now)
	if err != nil {
		return fmt.Errorf("mark orphan candidate: %w", err)
	}
	stats.Marked++
	scan.CandidatesMarked++
	s.deps.Metrics.Add("orphan_candidates_marked", 1)
	if marked.Observations == 1 {
		s.record(ctx, v.tenantID, v.assetID, "orphan_marked", model.CandidateMarked, v.category+": "+v.reason+" ("+object.ObjectKey+")")
	}
	return nil
}

// preview is the dry run of a scan: it lists from the start, without a
// checkpoint, and reports what would be marked.
func (s *Service) preview(ctx context.Context, a area, stats *CycleStats) error {
	cursor := ""
	for page := 0; page < s.cfg.PagesPerRun; page++ {
		listing, err := s.deps.Storage.ListObjectsPage(ctx, a.bucket, a.prefix, cursor, s.cfg.PageSize)
		if err != nil {
			stats.ScanErrors++
			return fmt.Errorf("list %s: %w", a.prefix, err)
		}
		stats.PagesScanned++
		for _, object := range listing.Objects {
			stats.ObjectsSeen++
			v, err := s.classify(ctx, a, object, true)
			if err != nil {
				return err
			}
			if !v.orphan {
				s.countSkip(stats, v.skip)
				continue
			}
			stats.Marked++
			stats.note(fmt.Sprintf("would mark %s/%s: %s (%s)", a.bucket, object.ObjectKey, v.category, v.reason))
		}
		cursor = listing.Cursor
		if cursor == "" {
			return nil
		}
	}
	return nil
}

// Sweep deletes due candidates that a fresh check still finds unowned,
// within MaxDeletionsPerRun. Every candidate is re-verified first: still
// present, still the same object (ETag), still unowned, not under a legal
// hold.
func (s *Service) Sweep(ctx context.Context, stats *CycleStats) error {
	budget := s.cfg.MaxDeletionsPerRun
	seenBefore := s.now().Add(-s.cfg.SafetyInterval)

	for _, a := range s.areas() {
		if ctx.Err() != nil || budget == 0 {
			return nil
		}
		candidates, err := s.deps.Inventory.DueCandidates(ctx, a.bucket, a.prefix, seenBefore, s.cfg.MinObservations, s.cfg.MaxAttempts, budget)
		if err != nil {
			return fmt.Errorf("list due orphan candidates: %w", err)
		}
		for _, candidate := range candidates {
			if ctx.Err() != nil || budget == 0 {
				return nil
			}
			deleted, err := s.sweepOne(ctx, a, candidate, stats)
			if err != nil {
				return err
			}
			if deleted {
				budget--
			}
		}
	}
	return nil
}

func (s *Service) sweepOne(ctx context.Context, a area, c model.OrphanCandidate, stats *CycleStats) (bool, error) {
	line := fmt.Sprintf("%s/%s (%s)", c.Bucket, c.ObjectKey, c.Category)
	if !s.cfg.DeleteOrphans || (model.Unrecorded(c.Category) && !s.cfg.DeleteUnrecorded) {
		stats.Kept++
		stats.note("report only, kept " + line)
		s.deps.Metrics.Add("orphan_candidates_reported", 1)
		return false, nil
	}

	meta, err := s.deps.Storage.HeadObject(ctx, c.Bucket, c.ObjectKey)
	if errors.Is(err, interfaces.ErrObjectNotFound) {
		return false, s.clear(ctx, c, "already gone", stats)
	}
	if err != nil {
		return false, s.failCandidate(ctx, c, err, stats)
	}
	if meta.ETag != c.ETag {
		return false, s.clear(ctx, c, "a different object is stored under this key now", stats)
	}

	v, err := s.classify(ctx, a, interfaces.StoredObject{ObjectKey: c.ObjectKey, ETag: meta.ETag}, false)
	if err != nil {
		return false, s.failCandidate(ctx, c, err, stats)
	}
	if !v.orphan {
		return false, s.clear(ctx, c, "owned again: "+v.reason, stats)
	}

	holds, err := s.deps.Holds.Active(ctx, v.tenantID, v.dataset, v.assetID)
	if err != nil {
		return false, s.failCandidate(ctx, c, err, stats)
	}
	if len(holds) > 0 {
		stats.Kept++
		s.deps.Metrics.Add("orphan_candidates_held", 1)
		stats.note("legal hold, kept " + line)
		return false, nil
	}

	if s.cfg.DryRun {
		stats.Deleted++
		stats.note("would delete " + line + ": " + v.reason)
		return true, nil
	}

	if err := s.deps.Storage.DeleteObject(ctx, c.Bucket, c.ObjectKey); err != nil {
		return false, s.failCandidate(ctx, c, err, stats)
	}
	if _, err := s.deps.Storage.HeadObject(ctx, c.Bucket, c.ObjectKey); !errors.Is(err, interfaces.ErrObjectNotFound) {
		if err == nil {
			err = errors.New("object still present after delete")
		}
		return false, s.failCandidate(ctx, c, err, stats)
	}

	if err := s.deps.Inventory.Resolve(ctx, c.ID, model.CandidateDeleted, v.reason, s.now()); err != nil {
		return true, fmt.Errorf("record orphan deletion: %w", err)
	}
	stats.Deleted++
	s.deps.Metrics.Add("orphan_objects_deleted", 1)
	s.record(ctx, c.TenantID, v.assetID, "orphan_deleted", model.CandidateDeleted, c.Category+": "+v.reason+" ("+c.ObjectKey+")")
	s.log.Info("orphan object deleted", "bucket", c.Bucket, "key", c.ObjectKey, "category", c.Category,
		"observations", c.Observations, "first_seen_at", c.FirstSeenAt)
	return true, nil
}

func (s *Service) clear(ctx context.Context, c model.OrphanCandidate, reason string, stats *CycleStats) error {
	stats.Cleared++
	if s.cfg.DryRun {
		stats.note(fmt.Sprintf("would clear %s/%s: %s", c.Bucket, c.ObjectKey, reason))
		return nil
	}
	if err := s.deps.Inventory.Resolve(ctx, c.ID, model.CandidateCleared, reason, s.now()); err != nil {
		return fmt.Errorf("clear orphan candidate: %w", err)
	}
	s.deps.Metrics.Add("orphan_candidates_cleared", 1)
	s.record(ctx, c.TenantID, "", "orphan_cleared", model.CandidateCleared, reason+" ("+c.ObjectKey+")")
	return nil
}

func (s *Service) failCandidate(ctx context.Context, c model.OrphanCandidate, cause error, stats *CycleStats) error {
	stats.SweepFailed++
	s.deps.Metrics.Add("orphan_delete_failures", 1)
	s.log.Warn("orphan sweep step failed; retried next sweep", "key", c.ObjectKey, "error", retry.Sanitize(cause))
	if s.cfg.DryRun {
		return nil
	}
	if err := s.deps.Inventory.FailCandidate(ctx, c.ID, retry.Sanitize(cause), s.now()); err != nil {
		return fmt.Errorf("record orphan sweep failure: %w", err)
	}
	return nil
}

// CheckMissing heads the expected object of the next batch of assets and
// reports the ones that are missing, classified. It never deletes or
// changes metadata: a missing object may still be recoverable.
func (s *Service) CheckMissing(ctx context.Context, stats *CycleStats) error {
	cursorName := "missing:" + s.scopeName()
	cursor := ""
	if !s.cfg.DryRun {
		var err error
		if cursor, err = s.deps.Cursors.Get(ctx, cursorName); err != nil {
			return fmt.Errorf("load missing-object cursor: %w", err)
		}
	}

	statuses := []model.AssetStatus{model.AssetStatusAvailable, model.AssetStatusQuarantined, model.AssetStatusProcessing}
	assets, err := s.deps.Assets.ListAfter(ctx, cursor, s.cfg.TenantID, statuses, s.cfg.CheckBatch)
	if err != nil {
		return fmt.Errorf("list assets to check: %w", err)
	}
	for _, asset := range assets {
		if ctx.Err() != nil {
			return nil
		}
		if err := s.checkAsset(ctx, asset, stats); err != nil {
			return err
		}
		cursor = asset.ID
	}
	if len(assets) < s.cfg.CheckBatch {
		cursor = "" // pass complete; start over next time
	}
	if s.cfg.DryRun {
		return nil
	}
	return s.deps.Cursors.Set(ctx, cursorName, cursor, s.now())
}

func (s *Service) checkAsset(ctx context.Context, asset model.Asset, stats *CycleStats) error {
	stats.MissingChecked++
	bucket, key := s.cfg.QuarantineBucket, "quarantine/"+asset.TenantID+"/"+asset.UploadID
	if asset.Status == model.AssetStatusAvailable {
		bucket, key = s.cfg.DeliveryBucket, asset.ObjectKey
	}

	_, err := s.deps.Storage.HeadObject(ctx, bucket, key)
	if err == nil {
		return nil
	}
	if !errors.Is(err, interfaces.ErrObjectNotFound) {
		// Storage temporarily unavailable: no conclusion; checked next pass.
		s.deps.Metrics.Add("missing_check_errors", 1)
		return nil
	}

	class := "inconsistent: an available asset has no delivery object; delivery answers 503 for it"
	if asset.Status != model.AssetStatusAvailable {
		job, err := s.deps.Jobs.GetByAsset(ctx, asset.TenantID, asset.ID)
		switch {
		case err == nil && job.State == model.ProcessingPending:
			class = "awaiting processing: verification will reject the asset"
		case err == nil && job.State == model.ProcessingExhausted:
			class = "held asset without its quarantine object"
		default:
			class = "inconsistent: " + strings.ToLower(string(asset.Status)) + " asset without its quarantine object"
		}
	}

	stats.Missing++
	stats.note(fmt.Sprintf("missing %s/%s for %s/%s: %s", bucket, key, asset.TenantID, asset.ID, class))
	s.deps.Metrics.Add("missing_objects", 1)
	if s.cfg.DryRun {
		return nil
	}

	// Every pass finds it again; record and warn at most once a day.
	recent, err := s.deps.Events.RecordedSince(ctx, asset.TenantID, asset.ID, "object_missing", s.now().Add(-24*time.Hour))
	if err != nil || recent {
		return nil
	}
	s.log.Warn("object missing for a record; metadata kept", "asset_id", asset.ID, "tenant_id", asset.TenantID,
		"status", string(asset.Status), "classification", class)
	s.record(ctx, asset.TenantID, asset.ID, "object_missing", "REPORTED", class)
	return nil
}

func (s *Service) countSkip(stats *CycleStats, counter string) {
	if stats.Skipped == nil {
		stats.Skipped = map[string]int{}
	}
	stats.Skipped[counter]++
	if counter == "uncertain" {
		s.deps.Metrics.Add("orphan_skipped_uncertain", 1)
	}
}

// record appends an audit event; a failed write is logged and counted. A
// dry run records nothing.
func (s *Service) record(ctx context.Context, tenantID, assetID, name, outcome, reason string) {
	if s.cfg.DryRun {
		return
	}
	err := s.deps.Events.Record(ctx, model.LifecycleEvent{
		ID: newID(), TenantID: tenantID, AssetID: assetID, Event: name, Outcome: outcome,
		Reason: truncate(reason, 500), Actor: "worker:" + s.cfg.Owner, CreatedAt: s.now(),
	})
	if err != nil {
		s.deps.Metrics.Add("lifecycle_event_write_failures", 1)
		s.log.Warn("record lifecycle event", "event", name, "error", err)
	}
}

func (s *Service) scopeName() string {
	if s.cfg.TenantID == "" {
		return "*"
	}
	return s.cfg.TenantID
}

func truncate(text string, limit int) string {
	if len(text) > limit {
		return text[:limit]
	}
	return text
}

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
