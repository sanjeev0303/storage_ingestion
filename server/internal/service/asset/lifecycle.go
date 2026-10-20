package asset

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/retry"
)

// dryRunPages bounds how far a dry run's retention evaluation reads.
const dryRunPages = 20

// CycleStats is what one lifecycle cycle did, or would do on a dry run.
type CycleStats struct {
	DryRun    bool
	Evaluated int
	// Expired counts expirations scheduled; on a dry run or with
	// expiration off, the ones that would be.
	Expired int
	// ExpiryReport lists assets that expired but were only reported.
	ExpiryReport []string
	// HeldFromExpiry counts expired assets a legal hold protects.
	HeldFromExpiry int

	Claimed   int
	Protected int
	Completed int
	Retried   int
	Failed    int
	Cancelled int
	LeaseLost int
	// DeletionReport describes, on a dry run, what each due deletion
	// would do.
	DeletionReport []string
}

// RunOnce evaluates retention for one batch of assets and works on due
// deletions. A failing step is reported but does not stop the other.
func (s *Service) RunOnce(ctx context.Context) (CycleStats, error) {
	stats := CycleStats{DryRun: s.cfg.DryRun}
	err := s.EvaluateRetention(ctx, &stats)
	if ctx.Err() != nil {
		return stats, err
	}
	return stats, errors.Join(err, s.ProcessDeletions(ctx, &stats))
}

// EvaluateRetention checks the next batch of AVAILABLE and REJECTED
// assets against their policy, all against one reference time, and
// schedules the expired ones for deletion (or reports them). The batch
// cursor is saved, so evaluation resumes after a restart; at the end of
// the assets it starts over.
func (s *Service) EvaluateRetention(ctx context.Context, stats *CycleStats) error {
	cursorName := "retention:" + s.scopeName()
	cursor := ""
	if !s.cfg.DryRun {
		var err error
		if cursor, err = s.deps.Cursors.Get(ctx, cursorName); err != nil {
			return fmt.Errorf("load retention cursor: %w", err)
		}
	}

	asOf := s.now()
	statuses := []model.AssetStatus{model.AssetStatusAvailable, model.AssetStatusRejected}
	for page := 0; ; page++ {
		assets, err := s.deps.Assets.ListAfter(ctx, cursor, s.cfg.TenantID, statuses, s.cfg.BatchSize)
		if err != nil {
			return fmt.Errorf("list assets for retention: %w", err)
		}
		for _, asset := range assets {
			if ctx.Err() != nil {
				return nil
			}
			if err := s.evaluate(ctx, asset, asOf, stats); err != nil {
				return err
			}
			cursor = asset.ID
		}

		done := len(assets) < s.cfg.BatchSize
		if done {
			cursor = "" // every asset was evaluated; start over next time
			s.deps.Metrics.Add("retention_passes_completed", 1)
		}
		if !s.cfg.DryRun {
			if err := s.deps.Cursors.Set(ctx, cursorName, cursor, s.now()); err != nil {
				return fmt.Errorf("save retention cursor: %w", err)
			}
			return nil // one batch per cycle
		}
		if done || page+1 >= dryRunPages {
			return nil
		}
	}
}

func (s *Service) evaluate(ctx context.Context, asset model.Asset, asOf time.Time, stats *CycleStats) error {
	stats.Evaluated++
	s.deps.Metrics.Add("retention_evaluated", 1)

	policy, err := s.policyFor(ctx, asset, asOf)
	if err != nil {
		return err
	}
	if !policy.Expired(asset.CreatedAt, asOf) {
		return nil
	}

	holds, err := s.deps.Holds.Active(ctx, asset.TenantID, asset.Dataset, asset.ID)
	if err != nil {
		return fmt.Errorf("check legal holds: %w", err)
	}
	if len(holds) > 0 {
		stats.HeldFromExpiry++
		s.deps.Metrics.Add("retention_protected_by_hold", 1)
		return nil
	}

	stats.Expired++
	if s.cfg.DryRun || !s.cfg.ExpireAssets {
		if len(stats.ExpiryReport) < 1000 {
			stats.ExpiryReport = append(stats.ExpiryReport, asset.TenantID+"/"+asset.ID)
		}
		s.deps.Metrics.Add("retention_expiry_reported", 1)
		// Found again on every pass: debug level; the metric counts it.
		s.log.Debug("asset expired; expiration is report-only", "asset_id", asset.ID, "tenant_id", asset.TenantID,
			"max_age", policy.MaxAge.String(), "from", policy.MaxAgeFrom, "dry_run", s.cfg.DryRun)
		return nil
	}

	_, err = s.requestDeletion(ctx, asset, model.DeletionReasonExpired, "policy", "")
	if errors.Is(err, model.ErrInvalidAssetState) {
		return nil // changed since it was listed; evaluated again next pass
	}
	if err != nil {
		return fmt.Errorf("schedule expired asset: %w", err)
	}
	s.deps.Metrics.Add("retention_expired_scheduled", 1)
	return nil
}

// ProcessDeletions claims due deletions and advances each one, at most
// Concurrency at a time.
func (s *Service) ProcessDeletions(ctx context.Context, stats *CycleStats) error {
	limit := min(s.cfg.BatchSize, s.cfg.MaxDeletionsPerRun)
	if s.cfg.DryRun {
		return s.reportDeletions(ctx, limit, stats)
	}

	ops, err := s.deps.Deletions.Claim(ctx, s.cfg.Owner, s.now(), s.cfg.Lease, limit, s.cfg.TenantID)
	if err != nil {
		return fmt.Errorf("claim deletions: %w", err)
	}
	stats.Claimed += len(ops)

	var mu sync.Mutex
	var wg sync.WaitGroup
	slots := make(chan struct{}, s.cfg.Concurrency)
	for _, op := range ops {
		if ctx.Err() != nil {
			break // unprocessed claims expire and are picked up again
		}
		slots <- struct{}{}
		wg.Add(1)
		go func(op model.DeletionOperation) {
			defer wg.Done()
			defer func() { <-slots }()
			outcome := s.processDeletion(ctx, op)
			mu.Lock()
			count(stats, outcome)
			mu.Unlock()
		}(op)
	}
	wg.Wait()
	return nil
}

// RunDeletion runs one scheduled deletion now when it is already due; the
// asset.deletion_scheduled event calls it. A deletion still in its grace
// period is left to the deletion schedule, which runs it at its purge
// time. It returns a short note on what happened.
func (s *Service) RunDeletion(ctx context.Context, tenantID, operationID string) (string, error) {
	op, err := s.deps.Deletions.Get(ctx, tenantID, operationID)
	if errors.Is(err, model.ErrDeletionNotFound) {
		return "", retry.Permanent(err)
	}
	if err != nil {
		return "", fmt.Errorf("load deletion: %w", err)
	}
	if !op.State.IsLive() {
		return "", fmt.Errorf("%w: the deletion is %s", model.ErrStaleEvent, op.State)
	}
	if op.NextAttemptAt.After(s.now()) {
		return "not due until " + op.NextAttemptAt.Format(time.RFC3339) + "; the deletion schedule runs it then", nil
	}

	claimed, ok, err := s.deps.Deletions.ClaimOne(ctx, tenantID, operationID, s.cfg.Owner, s.now(), s.cfg.Lease)
	if err != nil {
		return "", fmt.Errorf("claim deletion: %w", err)
	}
	if !ok {
		return "held by another worker", nil
	}
	return "deletion " + s.processDeletion(ctx, claimed), nil
}

// ResumeAfterHold makes the deletions a released hold was protecting due
// now instead of at their next recheck; the legal_hold.released event
// calls it. The deletion schedule then runs them, and checks every blocker
// again first, other holds included.
func (s *Service) ResumeAfterHold(ctx context.Context, tenantID, holdID string) (string, error) {
	n, err := s.deps.Deletions.ExpediteProtected(ctx, tenantID, holdID, s.now())
	if err != nil {
		return "", fmt.Errorf("expedite protected deletions: %w", err)
	}
	s.deps.Metrics.Add("deletions_expedited", int64(n))
	return fmt.Sprintf("%d protected deletions due now", n), nil
}

// Outcomes of one deletion step.
const (
	outcomeProtected = "protected"
	outcomeCompleted = "completed"
	outcomeRetried   = "retried"
	outcomeFailed    = "failed"
	outcomeCancelled = "cancelled"
	outcomeLeaseLost = "lease_lost"
	outcomeStopped   = "stopped"
)

func count(stats *CycleStats, outcome string) {
	switch outcome {
	case outcomeProtected:
		stats.Protected++
	case outcomeCompleted:
		stats.Completed++
	case outcomeRetried:
		stats.Retried++
	case outcomeFailed:
		stats.Failed++
	case outcomeCancelled:
		stats.Cancelled++
	case outcomeLeaseLost:
		stats.LeaseLost++
	}
}

func (s *Service) processDeletion(ctx context.Context, op model.DeletionOperation) string {
	log := s.log.With("operation_id", op.ID, "tenant_id", op.TenantID, "asset_id", op.AssetID, "state", string(op.State))

	workCtx, cancel := context.WithTimeout(ctx, s.cfg.Lease)
	outcome, attempts, err := s.advance(workCtx, op)
	cancel()

	if ctx.Err() != nil {
		// Shutting down: write nothing. The lease expires and another
		// worker repeats the (idempotent) step.
		log.Info("deletion interrupted by shutdown")
		return outcomeStopped
	}
	if errors.Is(err, model.ErrLeaseLost) {
		s.deps.Metrics.Add("deletions_lease_lost", 1)
		log.Warn("deletion lease lost; another worker owns it")
		return outcomeLeaseLost
	}
	if err == nil {
		s.deps.Metrics.Add("deletions_"+outcome, 1)
		return outcome
	}

	now := s.now()
	s.deps.Metrics.Add("storage_delete_failures", 1)
	if retry.IsPermanent(err) || attempts >= op.MaxAttempts {
		event := s.newEvent(op.TenantID, op.AssetID, op.ID, "deletion_failed", "FAILED", retry.Sanitize(err), s.actor())
		if failErr := s.deps.Deletions.Fail(ctx, op.ID, s.cfg.Owner, now, retry.Sanitize(err), event); failErr != nil {
			log.Error("record deletion failure", "error", failErr)
			return outcomeLeaseLost
		}
		s.deps.Metrics.Add("deletions_failed", 1)
		log.Error("deletion failed; an operator must requeue it", "attempts", attempts,
			"permanent", retry.IsPermanent(err), "error", retry.Sanitize(err))
		return outcomeFailed
	}

	next := now.Add(s.cfg.Retry.Delay(max(attempts, 1)))
	if retryErr := s.deps.Deletions.Retry(ctx, op.ID, s.cfg.Owner, now, next, retry.Sanitize(err)); retryErr != nil {
		log.Error("schedule deletion retry", "error", retryErr)
		return outcomeLeaseLost
	}
	s.deps.Metrics.Add("deletions_retried", 1)
	log.Warn("deletion will retry", "next_attempt_at", next, "attempts", attempts, "error", retry.Sanitize(err))
	return outcomeRetried
}

// advance moves one claimed deletion as far as it can go. It returns the
// outcome, the attempts counted so far, and an error to retry or fail on.
//
// The order makes every crash recoverable: storage deletion is recorded
// (STORAGE_DELETED) before the metadata is tombstoned, and every storage
// step is idempotent, so a repeated step finds the objects already gone.
func (s *Service) advance(ctx context.Context, op model.DeletionOperation) (string, int, error) {
	attempts := op.AttemptCount
	asset, err := s.deps.Assets.GetByID(ctx, op.TenantID, op.AssetID)
	if err != nil {
		return "", attempts, fmt.Errorf("load asset: %w", err)
	}

	if op.State == model.DeletionStorageDeleted {
		return s.complete(ctx, op)
	}

	if op.State != model.DeletionInProgress {
		if asset.Status != model.AssetStatusSoftDeleted {
			return s.cancel(ctx, op, "asset is "+string(asset.Status))
		}

		// The fresh check, immediately before anything is removed: a hold,
		// a longer retention or a new reference that appeared since the
		// deletion was scheduled all still apply.
		reasons, until, err := s.blockers(ctx, *asset)
		if err != nil {
			return "", attempts, err
		}
		if len(reasons) > 0 {
			return s.protect(ctx, op, reasons, until)
		}

		started, err := s.deps.Deletions.Start(ctx, op.ID, s.cfg.Owner, s.now())
		if err != nil {
			return "", attempts, err
		}
		if !started {
			// A hold arrived, or the asset was restored, in the moment
			// between the check and the start.
			current, err := s.deps.Assets.GetByID(ctx, op.TenantID, op.AssetID)
			if err == nil && current.Status != model.AssetStatusSoftDeleted {
				return outcomeCancelled, attempts, nil
			}
			return s.protect(ctx, op, []string{"legal hold placed while the deletion was starting"}, s.now().Add(s.cfg.RecheckInterval))
		}
		attempts++
		s.record(ctx, s.newEvent(op.TenantID, op.AssetID, op.ID, "deletion_started", "IN_PROGRESS", "", s.actor()))
	}

	if err := s.removeObjects(ctx, *asset); err != nil {
		return "", attempts, err
	}
	if err := s.deps.Deletions.MarkStorageDeleted(ctx, op.ID, s.cfg.Owner, s.now()); err != nil {
		return "", attempts, err
	}
	s.record(ctx, s.newEvent(op.TenantID, op.AssetID, op.ID, "storage_deleted", "STORAGE_DELETED", "every object confirmed gone", s.actor()))
	return s.complete(ctx, op)
}

func (s *Service) complete(ctx context.Context, op model.DeletionOperation) (string, int, error) {
	event := s.newEvent(op.TenantID, op.AssetID, op.ID, "deletion_completed", "COMPLETED", "asset tombstoned", s.actor())
	if err := s.deps.Deletions.Complete(ctx, op.ID, s.cfg.Owner, s.now(), event); err != nil {
		return "", op.AttemptCount, err
	}
	s.log.Info("asset permanently deleted", "asset_id", op.AssetID, "tenant_id", op.TenantID, "operation_id", op.ID)
	return outcomeCompleted, op.AttemptCount, nil
}

func (s *Service) cancel(ctx context.Context, op model.DeletionOperation, reason string) (string, int, error) {
	if err := s.deps.Deletions.Cancel(ctx, op.ID, s.cfg.Owner, s.now(), reason); err != nil {
		return "", op.AttemptCount, err
	}
	s.record(ctx, s.newEvent(op.TenantID, op.AssetID, op.ID, "deletion_cancelled", "CANCELLED", reason, s.actor()))
	return outcomeCancelled, op.AttemptCount, nil
}

func (s *Service) protect(ctx context.Context, op model.DeletionOperation, reasons []string, until time.Time) (string, int, error) {
	reason := truncate(strings.Join(reasons, "; "), 500)
	next := until
	if latest := s.now().Add(s.cfg.RecheckInterval); next.After(latest) {
		next = latest
	}
	if err := s.deps.Deletions.Protect(ctx, op.ID, s.cfg.Owner, s.now(), next, reason); err != nil {
		return "", op.AttemptCount, err
	}
	// Recorded when the protection starts or changes, not on every check.
	if op.State != model.DeletionProtected || op.ProtectedReason != reason {
		s.record(ctx, s.newEvent(op.TenantID, op.AssetID, op.ID, "deletion_protected", "PROTECTED", reason, s.actor()))
		s.log.Info("deletion protected", "operation_id", op.ID, "asset_id", op.AssetID, "reason", reason, "next_check", next)
	}
	return outcomeProtected, op.AttemptCount, nil
}

// blockers lists what must keep the asset's objects for now, and when to
// look again: legal holds, the retention period, quarantine evidence
// retention, active work, and any other live record pointing at the
// objects.
func (s *Service) blockers(ctx context.Context, asset model.Asset) ([]string, time.Time, error) {
	now := s.now()
	until := now.Add(s.cfg.RecheckInterval)
	var reasons []string

	holds, err := s.deps.Holds.Active(ctx, asset.TenantID, asset.Dataset, asset.ID)
	if err != nil {
		return nil, until, fmt.Errorf("check legal holds: %w", err)
	}
	for _, hold := range holds {
		reasons = append(reasons, "legal hold "+hold.ID)
	}

	policy, err := s.policyFor(ctx, asset, now)
	if err != nil {
		return nil, until, err
	}
	if retainUntil := policy.RetainUntil(asset.CreatedAt); now.Before(retainUntil) {
		reasons = append(reasons, fmt.Sprintf("retention until %s (%s)", retainUntil.Format(time.RFC3339), policy.MinRetentionFrom))
		until = earlier(until, retainUntil)
	}

	records, err := s.deps.Quarantine.ListForAsset(ctx, asset.TenantID, asset.ID)
	if err != nil {
		return nil, until, fmt.Errorf("check quarantine records: %w", err)
	}
	for _, record := range records {
		if record.Status == model.QuarantineRejected && record.RetainUntil != nil && now.Before(*record.RetainUntil) {
			reasons = append(reasons, "quarantine evidence kept until "+record.RetainUntil.Format(time.RFC3339))
			until = earlier(until, *record.RetainUntil)
		}
	}

	job, err := s.deps.Jobs.GetByAsset(ctx, asset.TenantID, asset.ID)
	switch {
	case errors.Is(err, model.ErrAssetNotFound):
	case err != nil:
		return nil, until, fmt.Errorf("check processing job: %w", err)
	case job.State == model.ProcessingPending:
		reasons = append(reasons, "processing job is active")
	}

	upload, err := s.deps.Uploads.GetByID(ctx, asset.TenantID, asset.UploadID)
	switch {
	case errors.Is(err, model.ErrUploadNotFound):
	case err != nil:
		return nil, until, fmt.Errorf("check upload session: %w", err)
	case uploadActive(upload.Status):
		reasons = append(reasons, "upload is "+string(upload.Status))
	}

	refs, err := s.deps.Deletions.References(ctx, asset, s.objectKeys(asset, job))
	if err != nil {
		return nil, until, fmt.Errorf("check references: %w", err)
	}
	reasons = append(reasons, refs...)
	return reasons, until, nil
}

// target is one physical object an asset owns.
type target struct {
	bucket    string
	key       string
	versionID string
}

// targets are the exact objects an asset owns, derived from its own
// tenant and IDs, never from a client or a listing: its delivery copy (at
// the recorded version, where the bucket is versioned), its quarantine
// source, and any other promotion copy its job recorded.
func (s *Service) targets(asset model.Asset, job model.ProcessingJob) []target {
	delivery := s.deliveryKey(asset)
	version := ""
	if asset.ObjectKey == delivery {
		version = asset.StorageVersionID
	}
	targets := []target{
		{bucket: s.cfg.DeliveryBucket, key: delivery, versionID: version},
		{bucket: s.cfg.QuarantineBucket, key: quarantineKey(asset.TenantID, asset.UploadID)},
	}
	if job.PromotionKey != "" && job.PromotionKey != delivery {
		targets = append(targets, target{bucket: s.cfg.DeliveryBucket, key: job.PromotionKey})
	}
	return targets
}

func (s *Service) objectKeys(asset model.Asset, job model.ProcessingJob) []string {
	var keys []string
	for _, t := range s.targets(asset, job) {
		keys = append(keys, t.key)
	}
	return keys
}

// removeObjects deletes each of the asset's objects and confirms it is
// gone. An object already missing counts as removed, so a repeated step
// (after a crash or a lost response) is safe.
func (s *Service) removeObjects(ctx context.Context, asset model.Asset) error {
	job, err := s.deps.Jobs.GetByAsset(ctx, asset.TenantID, asset.ID)
	if err != nil && !errors.Is(err, model.ErrAssetNotFound) {
		return fmt.Errorf("load processing job: %w", err)
	}
	for _, t := range s.targets(asset, job) {
		if err := s.removeExact(ctx, t); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) removeExact(ctx context.Context, t target) error {
	exists := func() (bool, error) {
		var err error
		if t.versionID != "" {
			_, err = s.deps.Storage.HeadObjectVersion(ctx, t.bucket, t.key, t.versionID)
		} else {
			_, err = s.deps.Storage.HeadObject(ctx, t.bucket, t.key)
		}
		if errors.Is(err, interfaces.ErrObjectNotFound) {
			return false, nil
		}
		return err == nil, err
	}

	present, err := exists()
	if err != nil {
		return fmt.Errorf("inspect object before delete: %w", err)
	}
	if !present {
		return nil
	}

	if t.versionID != "" {
		err = s.deps.Storage.DeleteObjectVersion(ctx, t.bucket, t.key, t.versionID)
	} else {
		err = s.deps.Storage.DeleteObject(ctx, t.bucket, t.key)
	}
	if err != nil {
		return fmt.Errorf("delete object: %w", err)
	}

	// Confirm with the provider; a deletion is recorded only once it is
	// verified.
	present, err = exists()
	if err != nil {
		return fmt.Errorf("confirm object deletion: %w", err)
	}
	if present {
		return errors.New("object still present after delete")
	}
	s.deps.Metrics.Add("storage_objects_deleted", 1)
	return nil
}

// reportDeletions is the dry run of ProcessDeletions: it evaluates due
// deletions without claiming or changing them.
func (s *Service) reportDeletions(ctx context.Context, limit int, stats *CycleStats) error {
	ops, err := s.deps.Deletions.Due(ctx, s.now(), limit, s.cfg.TenantID)
	if err != nil {
		return fmt.Errorf("list due deletions: %w", err)
	}
	for _, op := range ops {
		asset, err := s.deps.Assets.GetByID(ctx, op.TenantID, op.AssetID)
		if err != nil {
			return err
		}
		line := op.TenantID + "/" + op.AssetID + ": "
		switch {
		case op.State == model.DeletionStorageDeleted:
			line += "would tombstone the asset"
		case op.State != model.DeletionInProgress && asset.Status != model.AssetStatusSoftDeleted:
			line += "would cancel (asset is " + string(asset.Status) + ")"
		default:
			reasons, _, err := s.blockers(ctx, *asset)
			if err != nil {
				return err
			}
			if len(reasons) > 0 && op.State != model.DeletionInProgress {
				line += "would stay protected: " + strings.Join(reasons, "; ")
			} else {
				line += "would delete its objects and tombstone the asset"
			}
		}
		stats.DeletionReport = append(stats.DeletionReport, line)
	}
	return nil
}

func (s *Service) deliveryKey(asset model.Asset) string {
	return fmt.Sprintf("%s/%s/%s", s.cfg.DeliveryPrefix, asset.TenantID, asset.ID)
}

func (s *Service) scopeName() string {
	if s.cfg.TenantID == "" {
		return "*"
	}
	return s.cfg.TenantID
}

func (s *Service) actor() string {
	return "worker:" + s.cfg.Owner
}

// quarantineKey matches the key the upload service gives every session.
func quarantineKey(tenantID, uploadID string) string {
	return fmt.Sprintf("quarantine/%s/%s", tenantID, uploadID)
}

func uploadActive(status model.UploadStatus) bool {
	switch status {
	case model.UploadStatusPending, model.UploadStatusUploading, model.UploadStatusCompleting, model.UploadStatusAborting:
		return true
	}
	return false
}

func earlier(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}
