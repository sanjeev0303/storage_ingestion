package processing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/metrics"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/retry"
	"github.com/sanjeev0303/object-storage-platform/internal/service/promotion"
)

// ScanPolicy says what happens when no malware scanner is configured.
// A missing scanner is never reported as a clean scan.
type ScanPolicy string

const (
	// ScanPolicyHold keeps the asset out of delivery with a HELD quarantine
	// record and marks the job EXHAUSTED for an operator. The default.
	ScanPolicyHold ScanPolicy = "hold"
	// ScanPolicyReject rejects the asset (scanner_unavailable).
	ScanPolicyReject ScanPolicy = "reject"
	// ScanPolicyAllowUnscanned promotes without a scan, recording
	// NOT_SCANNED. Only allowed in development (see config).
	ScanPolicyAllowUnscanned ScanPolicy = "allow_unscanned"
)

var errNoScanner = errors.New("no malware scanner is configured; asset held in quarantine")

// UploadVerifier checks a completed upload's stored bytes (existence, key,
// size, SHA-256). *upload.MultipartService implements it.
type UploadVerifier interface {
	VerifyIntegrity(ctx context.Context, tenantID, uploadID string) (model.UploadStatus, error)
}

type Dependencies struct {
	Jobs       interfaces.ProcessingJobRepository
	Assets     interfaces.AssetRepository
	Uploads    interfaces.UploadRepository
	Quarantine interfaces.QuarantineRepository
	Events     interfaces.ProcessingEventRepository
	// Holds is optional; with it, a legal hold stops quarantine purges.
	Holds interfaces.LegalHoldRepository
	// Retention is optional; with it, a tenant, dataset or asset policy may
	// keep rejected uploads longer than Config.QuarantineRetention (never
	// shorter).
	Retention RetentionPolicy
	// Policies is optional; without it every tenant uses Config.Policy.
	Policies   interfaces.ContentPolicyRepository
	Verifier   UploadVerifier
	Storage    interfaces.MultipartStorage
	Validators []interfaces.FileValidator
	// Scanner is nil when no scanner is configured; ScanPolicy then applies.
	Scanner  interfaces.MalwareScanner
	Promoter *promotion.Service
	// Metrics is optional.
	Metrics *metrics.Registry
	// Router reaches tenants' storage connections; without it every asset
	// is processed on the platform's quarantine bucket.
	Router interfaces.StorageRouter
}

type Config struct {
	// Owner identifies this worker on the leases it takes.
	Owner       string
	Bucket      string
	BatchSize   int
	Concurrency int
	// Lease bounds each job's processing time as well as its claim.
	Lease time.Duration
	Retry retry.Policy
	// Policy is the platform content policy. A tenant policy can only
	// narrow it: denied types are added, the size limit is the smaller one.
	Policy     model.ContentPolicy
	ScanPolicy ScanPolicy
	// ScanTimeout bounds one scan; it must be shorter than Lease.
	ScanTimeout time.Duration
	// QuarantineRetention is how long a rejected object is kept for
	// investigation before it is purged.
	QuarantineRetention time.Duration
}

// Service runs assets through VERIFY -> SCAN -> PROMOTE -> CLEANUP. Each
// stage's result is saved under the job's lease before the next stage
// starts, and every stage is idempotent, so a repeated, delayed or
// crashed job resumes safely at its recorded stage.
type Service struct {
	deps Dependencies
	cfg  Config
	log  *slog.Logger
	now  func() time.Time
}

func NewService(deps Dependencies, cfg Config, log *slog.Logger) (*Service, error) {
	if deps.Jobs == nil || deps.Assets == nil || deps.Uploads == nil || deps.Quarantine == nil || deps.Events == nil ||
		deps.Verifier == nil || deps.Storage == nil || len(deps.Validators) == 0 || deps.Promoter == nil || log == nil {
		return nil, errors.New("processing dependencies are required")
	}
	if cfg.Owner == "" || cfg.Bucket == "" {
		return nil, errors.New("processing owner and bucket are required")
	}
	if cfg.BatchSize <= 0 || cfg.Concurrency <= 0 || cfg.Lease <= 0 || cfg.Policy.MaxSizeBytes <= 0 {
		return nil, errors.New("processing batch size, concurrency, lease and file limit must be positive")
	}
	if cfg.ScanTimeout <= 0 || cfg.ScanTimeout >= cfg.Lease {
		return nil, errors.New("processing scan timeout must be positive and shorter than the lease")
	}
	if cfg.QuarantineRetention <= 0 {
		return nil, errors.New("quarantine retention must be positive")
	}
	if cfg.Retry.MaxAttempts <= 0 || cfg.Retry.BaseDelay <= 0 || cfg.Retry.MaxDelay < cfg.Retry.BaseDelay {
		return nil, errors.New("processing retry policy is invalid")
	}
	switch cfg.ScanPolicy {
	case ScanPolicyHold, ScanPolicyReject, ScanPolicyAllowUnscanned:
	default:
		return nil, fmt.Errorf("unknown scan policy %q", cfg.ScanPolicy)
	}
	switch cfg.Policy.UnknownTypeAction {
	case "allow", "reject":
	default:
		return nil, fmt.Errorf("unknown type action must be allow or reject, got %q", cfg.Policy.UnknownTypeAction)
	}

	return &Service{deps: deps, cfg: cfg, log: log, now: func() time.Time { return time.Now().UTC() }}, nil
}

// Route lets processing reach assets on tenants' storage connections; its
// promoter is routed the same way.
func (s *Service) Route(router interfaces.StorageRouter) *Service {
	s.deps.Router = router
	s.deps.Promoter.Route(router)
	return s
}

// CycleStats counts what one cycle did, for the cycle's log line.
type CycleStats struct {
	Enqueued  int
	Claimed   int
	Succeeded int
	Failed    int
	Retried   int
	Exhausted int
	LeaseLost int
	Purged    int
}

// RunOnce creates jobs for newly completed uploads, claims a batch and
// processes it with at most Concurrency jobs at a time, then purges
// rejected objects whose retention ended.
func (s *Service) RunOnce(ctx context.Context) (CycleStats, error) {
	var stats CycleStats

	enqueued, err := s.deps.Jobs.EnqueueDue(ctx, s.now(), s.cfg.Retry.MaxAttempts)
	if err != nil {
		return stats, fmt.Errorf("enqueue processing jobs: %w", err)
	}
	stats.Enqueued = enqueued

	jobs, err := s.deps.Jobs.Claim(ctx, s.cfg.Owner, s.now(), s.cfg.Lease, s.cfg.BatchSize)
	if err != nil {
		return stats, fmt.Errorf("claim processing jobs: %w", err)
	}
	stats.Claimed = len(jobs)

	var mu sync.Mutex
	var wg sync.WaitGroup
	slots := make(chan struct{}, s.cfg.Concurrency)
	for _, job := range jobs {
		if ctx.Err() != nil {
			break // unprocessed claims expire and are picked up again
		}
		slots <- struct{}{}
		wg.Add(1)
		go func(job model.ProcessingJob) {
			defer wg.Done()
			defer func() { <-slots }()
			s.process(ctx, job, &stats, &mu)
		}(job)
	}
	wg.Wait()

	if ctx.Err() != nil {
		return stats, nil
	}
	purged, err := s.PurgeExpired(ctx)
	stats.Purged = purged
	if err != nil {
		return stats, err
	}
	s.recordBacklog(ctx)
	return stats, nil
}

// Intake starts processing one completed upload now instead of at the
// next poll; the upload.completed event calls it. It creates the upload's
// asset and job when they do not exist yet and runs the job when it is due
// and free. A job that another worker holds, that waits for a retry or
// that already ended is left to its own schedule, so repeating Intake is
// safe. It returns a short note on what happened.
func (s *Service) Intake(ctx context.Context, tenantID, uploadID string) (string, error) {
	job, err := s.deps.Jobs.EnqueueUpload(ctx, tenantID, uploadID, s.now(), s.cfg.Retry.MaxAttempts)
	switch {
	case errors.Is(err, model.ErrUploadNotFound):
		return "", retry.Permanent(err)
	case errors.Is(err, model.ErrInvalidUploadState):
		return "", fmt.Errorf("%w: the upload is no longer completed", model.ErrStaleEvent)
	case errors.Is(err, model.ErrAssetNotFound):
		return "", fmt.Errorf("%w: the upload has no asset left to process", model.ErrStaleEvent)
	case err != nil:
		return "", fmt.Errorf("enqueue upload: %w", err)
	}

	if job.State != model.ProcessingPending {
		return "processing already ended: " + string(job.State), nil
	}
	claimed, ok, err := s.deps.Jobs.ClaimJob(ctx, job.ID, s.cfg.Owner, s.now(), s.cfg.Lease)
	if err != nil {
		return "", fmt.Errorf("claim processing job: %w", err)
	}
	if !ok {
		return "job is held by another worker or waiting for its retry", nil
	}

	var stats CycleStats
	var mu sync.Mutex
	s.process(ctx, claimed, &stats, &mu)
	switch {
	case stats.Succeeded > 0:
		return "asset promoted", nil
	case stats.Failed > 0:
		return "processing ended without promotion", nil
	case stats.Exhausted > 0:
		return "processing exhausted; asset held in quarantine", nil
	case stats.Retried > 0:
		return "processing failed; the job retries on its own schedule", nil
	}
	return "processing interrupted; the job resumes when its lease expires", nil
}

func (s *Service) process(ctx context.Context, job model.ProcessingJob, stats *CycleStats, mu *sync.Mutex) {
	log := s.log.With(
		"job_id", job.ID,
		"tenant_id", job.TenantID,
		"asset_id", job.AssetID,
		"stage", string(job.Stage),
		"attempt", job.AttemptCount,
		"max_attempts", job.MaxAttempts,
	)
	started := time.Now()

	workCtx, cancel := context.WithTimeout(ctx, s.cfg.Lease)
	state, err := s.run(workCtx, &job)
	cancel()

	count := func(field *int, metric string) {
		mu.Lock()
		*field++
		mu.Unlock()
		s.deps.Metrics.Add(metric, 1)
	}

	if ctx.Err() != nil {
		// Shutting down: write nothing; the lease expires and the job resumes.
		log.Info("processing interrupted by shutdown", "reached_stage", string(job.Stage))
		return
	}
	if errors.Is(err, model.ErrLeaseLost) {
		count(&stats.LeaseLost, "jobs_lease_lost")
		log.Warn("processing lease lost; another worker owns the job")
		return
	}

	now := s.now()
	duration := time.Since(started).Milliseconds()
	exhausted := err != nil && (retry.IsPermanent(err) || job.AttemptCount >= job.MaxAttempts)

	// A job that stops without promotion keeps its asset out of delivery
	// under a HELD quarantine record until an operator requeues or rejects
	// it. If the hold cannot be written, the job is retried instead.
	if exhausted {
		if holdErr := s.hold(ctx, job, retry.Sanitize(err)); holdErr != nil {
			log.Error("record quarantine hold", "error", holdErr)
			exhausted = false
		}
	}

	var releaseErr error
	switch {
	case err == nil:
		releaseErr = s.deps.Jobs.Finish(ctx, job.ID, s.cfg.Owner, now, state, "")
		if releaseErr == nil && state == model.ProcessingSucceeded {
			count(&stats.Succeeded, "jobs_succeeded")
			log.Info("asset promoted to AVAILABLE", "duration_ms", duration)
		} else if releaseErr == nil {
			count(&stats.Failed, "jobs_rejected")
			log.Warn("asset processing ended without promotion", "state", string(state), "duration_ms", duration)
		}

	case exhausted:
		releaseErr = s.deps.Jobs.Finish(ctx, job.ID, s.cfg.Owner, now, model.ProcessingExhausted, retry.Sanitize(err))
		if releaseErr == nil {
			count(&stats.Exhausted, "jobs_exhausted")
			log.Error("processing exhausted; asset held in quarantine until an operator acts",
				"reached_stage", string(job.Stage), "permanent", retry.IsPermanent(err), "error", retry.Sanitize(err))
		}

	default:
		next := now.Add(s.cfg.Retry.Delay(job.AttemptCount))
		releaseErr = s.deps.Jobs.Reschedule(ctx, job.ID, s.cfg.Owner, now, next, retry.Sanitize(err))
		if releaseErr == nil {
			count(&stats.Retried, "jobs_retried")
			log.Warn("processing will retry", "reached_stage", string(job.Stage),
				"next_attempt_at", next, "error", retry.Sanitize(err))
		}
	}

	if errors.Is(releaseErr, model.ErrLeaseLost) {
		count(&stats.LeaseLost, "jobs_lease_lost")
		log.Warn("processing lease lost before the outcome was recorded")
	} else if releaseErr != nil {
		log.Error("record processing outcome", "error", releaseErr)
	}
}

// run advances the job stage by stage until it reaches a final state. A
// nil error with a state means the job is finished; an error means retry
// (or exhaustion, by the caller's rules).
func (s *Service) run(ctx context.Context, job *model.ProcessingJob) (model.ProcessingState, error) {
	for {
		asset, err := s.deps.Assets.GetByID(ctx, job.TenantID, job.AssetID)
		if errors.Is(err, model.ErrAssetNotFound) {
			return model.ProcessingFailed, nil
		}
		if err != nil {
			return "", err
		}

		// Rejected or deleted by anyone, at any time: stop, and never leave
		// a delivery copy behind for it.
		if asset.Status == model.AssetStatusRejected || asset.Status == model.AssetStatusDeleted ||
			asset.Status == model.AssetStatusSoftDeleted {
			if job.PromotionKey != "" {
				if err := s.deps.Promoter.RemoveDestination(ctx, *asset, job.PromotionKey); err != nil {
					return "", err
				}
			}
			return model.ProcessingFailed, nil
		}

		var state model.ProcessingState
		switch job.Stage {
		case model.StageVerify:
			state, err = s.verify(ctx, job, *asset)
		case model.StageScan:
			state, err = s.scan(ctx, job, *asset)
		case model.StagePromote:
			state, err = s.promote(ctx, job, *asset)
		case model.StageCleanup:
			state, err = s.cleanup(ctx, job, *asset)
		default:
			return "", retry.Permanent(fmt.Errorf("unknown processing stage %q", job.Stage))
		}
		if err != nil || state != "" {
			return state, err
		}
	}
}

// verify checks the stored bytes against the declared size and SHA-256,
// then runs the content validators, and moves the asset QUARANTINED ->
// PROCESSING. Verification and validation are recorded separately.
func (s *Service) verify(ctx context.Context, job *model.ProcessingJob, asset model.Asset) (model.ProcessingState, error) {
	storage, bucket, key, err := s.staging(ctx, asset)
	if err != nil {
		return "", err
	}
	if asset.UploadID != job.UploadID || asset.ObjectKey != key {
		return "", retry.Permanent(errors.New("asset does not match its upload's quarantine key"))
	}

	policy, err := s.policyFor(ctx, asset.TenantID)
	if err != nil {
		return "", err
	}
	// Refuse an oversized file before hashing all of it.
	if asset.SizeBytes > policy.MaxSizeBytes {
		job.ValidationStatus = model.StageRejected
		job.ValidationReason = model.RejectFileTooLarge
		if _, err := s.save(ctx, job); err != nil {
			return "", err
		}
		return s.reject(ctx, job, asset, model.QuarantineValidationRejected, model.RejectFileTooLarge)
	}

	// Existence, key, size and SHA-256 of the actual bytes, streamed. The
	// upload requires a client-declared SHA-256, so the comparison is
	// always against DECLARED; a multipart ETag is never used as a
	// checksum. Repeating it on a verified upload is a no-op.
	status, err := s.deps.Verifier.VerifyIntegrity(ctx, asset.TenantID, asset.UploadID)
	if err != nil {
		return "", fmt.Errorf("verify upload: %w", err)
	}
	job.ChecksumSource = model.ChecksumDeclared
	switch status {
	case model.UploadStatusVerified:
	case model.UploadStatusFailed:
		reason := model.RejectVerificationFailed
		if session, err := s.deps.Uploads.GetByID(ctx, asset.TenantID, asset.UploadID); err == nil && session.FailureReason != "" {
			reason = session.FailureReason
		}
		job.VerificationStatus = model.StageFailed
		if _, err := s.save(ctx, job); err != nil {
			return "", err
		}
		s.deps.Metrics.Add("verification_failed", 1)
		return s.reject(ctx, job, asset, model.QuarantineVerificationFailed, reason)
	default:
		return "", fmt.Errorf("upload is %s, not yet verifiable", status)
	}

	verifiedAt := s.now()
	job.VerificationStatus = model.StagePassed
	job.ChecksumAlgorithm = "SHA-256"
	job.ChecksumVerifiedAt = &verifiedAt

	head, err := readHead(ctx, storage, bucket, key)
	if err != nil {
		return "", err
	}
	sample := interfaces.ContentSample{
		Head:         head,
		DeclaredType: asset.ContentType,
		FileName:     asset.FileName,
		SizeBytes:    asset.SizeBytes,
		Open: func(ctx context.Context) (io.ReadCloser, error) {
			return storage.OpenObject(ctx, bucket, key)
		},
	}
	result, err := RunValidators(ctx, s.deps.Validators, sample, policy)
	if err != nil {
		return "", fmt.Errorf("validate content: %w", err)
	}
	job.DetectedContentType = result.DetectedType
	if result.RejectReason != "" {
		job.ValidationStatus = model.StageRejected
		job.ValidationReason = result.RejectReason
		if _, err := s.save(ctx, job); err != nil {
			return "", err
		}
		s.deps.Metrics.Add("validation_rejected", 1)
		s.log.Warn("asset failed validation", "asset_id", asset.ID, "tenant_id", asset.TenantID,
			"reason", result.RejectReason, "detail", result.Detail)
		return s.reject(ctx, job, asset, model.QuarantineValidationRejected, result.RejectReason)
	}
	job.ValidationStatus = model.StagePassed
	job.ValidationReason = ""

	if asset.Status == model.AssetStatusQuarantined {
		err := s.deps.Assets.TransitionStatus(ctx, asset.TenantID, asset.ID,
			[]model.AssetStatus{model.AssetStatusQuarantined}, model.AssetStatusProcessing, "", "", s.now())
		if errors.Is(err, model.ErrInvalidAssetState) {
			return "", nil // someone else moved it; re-read and continue from there
		}
		if err != nil {
			return "", fmt.Errorf("move asset to processing: %w", err)
		}
	}

	job.Stage = model.StageScan
	s.event(ctx, *job, "verified", "checksum DECLARED; detected "+result.DetectedType)
	return s.save(ctx, job)
}

// scan runs the configured malware scanner, or applies ScanPolicy when
// there is none. Only CLEAN moves on; an infected object is rejected, and
// an incomplete scan (unsupported, timeout, error) is retried and ends
// HELD, never promoted.
func (s *Service) scan(ctx context.Context, job *model.ProcessingJob, asset model.Asset) (model.ProcessingState, error) {
	if asset.Status != model.AssetStatusProcessing {
		return "", retry.Permanent(fmt.Errorf("asset is %s at the scan stage", asset.Status))
	}

	if s.deps.Scanner == nil {
		switch s.cfg.ScanPolicy {
		case ScanPolicyReject:
			job.ScanResult = model.ScanNotScanned
			if _, err := s.save(ctx, job); err != nil {
				return "", err
			}
			return s.reject(ctx, job, asset, model.QuarantineScanIncomplete, model.RejectScannerUnavailable)
		case ScanPolicyAllowUnscanned:
			job.ScanResult = model.ScanNotScanned
			job.Stage = model.StagePromote
			s.log.Warn("asset promoted without a malware scan (development policy)", "asset_id", asset.ID)
			s.event(ctx, *job, "scan_skipped", "allow_unscanned development policy")
			return s.save(ctx, job)
		default:
			return "", retry.Permanent(errNoScanner)
		}
	}

	job.Scanner = s.deps.Scanner.Name()
	if err := s.deps.Scanner.Ping(ctx); err != nil {
		s.deps.Metrics.Set("scanner_up", 0)
		return s.scanIncomplete(ctx, job, interfaces.ScanResult{Outcome: model.ScanError, Detail: "scanner unavailable"})
	}
	s.deps.Metrics.Set("scanner_up", 1)

	storage, bucket, key, err := s.staging(ctx, asset)
	if err != nil {
		return "", err
	}
	body, err := storage.OpenObject(ctx, bucket, key)
	if err != nil {
		return "", fmt.Errorf("open object to scan: %w", err)
	}
	// Hash and count exactly what the scanner reads. Quarantine is not
	// pinned to the verified bytes, so a write credential still valid after
	// verification could swap them for the scan; only bytes matching the
	// declared SHA-256, which promotion checks the copy against too, may
	// pass as scanned.
	hasher := sha256.New()
	var read byteCount
	scanCtx, cancel := context.WithTimeout(ctx, s.cfg.ScanTimeout)
	started := time.Now()
	result := s.deps.Scanner.Scan(scanCtx, io.TeeReader(body, io.MultiWriter(hasher, &read)))
	cancel()
	body.Close()

	scannedAt := s.now()
	job.ScannedAt = &scannedAt
	job.ScanResult = result.Outcome
	job.ScanError = truncate(result.Detail, 200)
	s.deps.Metrics.Add("scan_"+result.Outcome, 1)
	s.deps.Metrics.Add("scan_duration_ms_total", time.Since(started).Milliseconds())

	switch result.Outcome {
	case model.ScanClean:
		if int64(read) < asset.SizeBytes {
			return s.scanIncomplete(ctx, job, interfaces.ScanResult{Outcome: model.ScanError, Detail: "the scanner did not read the whole object"})
		}
		if int64(read) != asset.SizeBytes || !strings.EqualFold(hex.EncodeToString(hasher.Sum(nil)), asset.ChecksumSHA256) {
			job.VerificationStatus = model.StageFailed
			if _, err := s.save(ctx, job); err != nil {
				return "", err
			}
			s.deps.Metrics.Add("verification_failed", 1)
			s.log.Warn("quarantined object changed after verification; asset rejected", "asset_id", asset.ID, "tenant_id", asset.TenantID)
			return s.reject(ctx, job, asset, model.QuarantineVerificationFailed, model.FailureChecksumMismatch)
		}
		job.ScanSignature = ""
		job.Stage = model.StagePromote
		s.event(ctx, *job, "scanned", "CLEAN")
		return s.save(ctx, job)
	case model.ScanInfected:
		job.ScanSignature = truncate(result.Signature, 200)
		if _, err := s.save(ctx, job); err != nil {
			return "", err
		}
		s.log.Warn("malware detected; asset rejected", "asset_id", asset.ID, "tenant_id", asset.TenantID,
			"scanner", job.Scanner, "signature", job.ScanSignature)
		return s.reject(ctx, job, asset, model.QuarantineMalwareDetected, model.RejectMalwareDetected)
	default:
		return s.scanIncomplete(ctx, job, result)
	}
}

// byteCount counts the bytes written to it.
type byteCount int64

func (c *byteCount) Write(p []byte) (int, error) {
	*c += byteCount(len(p))
	return len(p), nil
}

// scanIncomplete records the attempt and returns an error: TIMEOUT and
// ERROR are retried, UNSUPPORTED cannot improve and exhausts at once. Both
// end with the asset HELD, never promoted.
func (s *Service) scanIncomplete(ctx context.Context, job *model.ProcessingJob, result interfaces.ScanResult) (model.ProcessingState, error) {
	job.ScanResult = result.Outcome
	job.ScanError = truncate(result.Detail, 200)
	if _, err := s.save(ctx, job); err != nil {
		return "", err
	}
	s.event(ctx, *job, "scan_incomplete", result.Outcome+": "+job.ScanError)

	err := fmt.Errorf("malware scan incomplete (%s): %s", result.Outcome, job.ScanError)
	if result.Outcome == model.ScanUnsupported {
		return "", retry.Permanent(err)
	}
	return "", err
}

// promote records the destination key before copying, so a crash after the
// copy always knows which object it may have left behind.
func (s *Service) promote(ctx context.Context, job *model.ProcessingJob, asset model.Asset) (model.ProcessingState, error) {
	if job.ScanResult != model.ScanClean && job.ScanResult != model.ScanNotScanned {
		return "", retry.Permanent(errors.New("promotion requires a clean scan result"))
	}
	if job.VerificationStatus != model.StagePassed || job.ValidationStatus != model.StagePassed {
		return "", retry.Permanent(errors.New("promotion requires passed verification and validation"))
	}

	if job.PromotionKey == "" {
		key, err := s.deps.Promoter.DestinationKey(ctx, asset)
		if err != nil {
			return "", err
		}
		job.PromotionKey = key
		job.PromotionStatus = model.PromotionPending
		if _, err := s.save(ctx, job); err != nil {
			return "", err
		}
	}

	status, err := s.deps.Promoter.Promote(ctx, asset, job.PromotionKey)
	if err != nil {
		return "", err
	}
	switch status {
	case model.AssetStatusAvailable:
	case model.AssetStatusRejected, model.AssetStatusDeleted, model.AssetStatusSoftDeleted:
		return "", nil // finished by someone else meanwhile: run() removes the copy
	default:
		return "", retry.Permanent(fmt.Errorf("asset is %s at the promotion stage", status))
	}

	promotedAt := s.now()
	job.PromotedAt = &promotedAt
	job.PromotionStatus = model.PromotionPublished
	job.Stage = model.StageCleanup
	s.event(ctx, *job, "promoted", "")
	return s.save(ctx, job)
}

// cleanup deletes the quarantine source, only after the asset is AVAILABLE
// and that state is committed, and closes any earlier hold.
func (s *Service) cleanup(ctx context.Context, job *model.ProcessingJob, asset model.Asset) (model.ProcessingState, error) {
	if err := s.deps.Promoter.RemoveSource(ctx, asset); err != nil {
		return "", err
	}
	released, err := s.deps.Quarantine.Release(ctx, asset.TenantID, asset.ID, s.now())
	if err != nil {
		return "", err
	}
	if released {
		s.deps.Metrics.Add("quarantine_released", 1)
		s.event(ctx, *job, "quarantine_released", "")
	}
	return model.ProcessingSucceeded, nil
}

// reject records a permanent quarantine rejection, then moves the asset to
// REJECTED. The record comes first so a rejected asset always has one.
func (s *Service) reject(ctx context.Context, job *model.ProcessingJob, asset model.Asset, category, reason string) (model.ProcessingState, error) {
	now := s.now()
	retainUntil := now.Add(s.quarantineRetention(ctx, asset))
	err := s.deps.Quarantine.Reject(ctx, model.QuarantineRecord{
		TenantID:      asset.TenantID,
		AssetID:       asset.ID,
		JobID:         job.ID,
		UploadID:      asset.UploadID,
		ObjectKey:     recordKey(asset),
		Category:      category,
		Reason:        reason,
		ScanResult:    job.ScanResult,
		ScanSignature: job.ScanSignature,
		RetainUntil:   &retainUntil,
		CreatedAt:     now,
	})
	if err != nil {
		return "", err
	}

	err = s.deps.Assets.TransitionStatus(ctx, asset.TenantID, asset.ID,
		[]model.AssetStatus{model.AssetStatusQuarantined, model.AssetStatusProcessing},
		model.AssetStatusRejected, reason, "", now)
	if err != nil && !errors.Is(err, model.ErrInvalidAssetState) {
		return "", fmt.Errorf("reject asset: %w", err)
	}
	s.event(ctx, *job, "rejected", category+": "+reason)
	s.log.Warn("asset rejected", "asset_id", asset.ID, "tenant_id", asset.TenantID, "category", category, "reason", reason)
	// Already rejected or deleted by someone else ends the job the same way.
	return model.ProcessingFailed, nil
}

// hold opens (or refreshes) the asset's HELD quarantine record for a job
// that stops without an outcome.
func (s *Service) hold(ctx context.Context, job model.ProcessingJob, reason string) error {
	asset, err := s.deps.Assets.GetByID(ctx, job.TenantID, job.AssetID)
	if errors.Is(err, model.ErrAssetNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if asset.Status != model.AssetStatusQuarantined && asset.Status != model.AssetStatusProcessing {
		return nil
	}

	category := model.QuarantineProcessingExhausted
	if job.Stage == model.StageScan {
		category = model.QuarantineScanIncomplete
	}
	_, err = s.deps.Quarantine.Hold(ctx, model.QuarantineRecord{
		TenantID:   asset.TenantID,
		AssetID:    asset.ID,
		JobID:      job.ID,
		UploadID:   asset.UploadID,
		ObjectKey:  recordKey(*asset),
		Category:   category,
		Reason:     truncate(reason, 500),
		ScanResult: job.ScanResult,
		CreatedAt:  s.now(),
	})
	if err != nil {
		return err
	}
	s.deps.Metrics.Add("quarantine_held", 1)
	s.event(ctx, job, "quarantine_held", category)
	return nil
}

// PurgeExpired deletes rejected quarantine objects whose retention ended
// and marks their records PURGED. An object already gone counts as purged.
func (s *Service) PurgeExpired(ctx context.Context) (int, error) {
	due, err := s.deps.Quarantine.DuePurge(ctx, s.now(), s.cfg.BatchSize)
	if err != nil {
		return 0, fmt.Errorf("list quarantine purges: %w", err)
	}

	purged := 0
	for _, record := range due {
		held, err := s.purgeHeld(ctx, record)
		if err != nil {
			s.log.Error("check legal holds before purge", "record_id", record.ID, "error", err)
			continue
		}
		if held {
			// Kept until the hold is released; checked again next cycle.
			s.deps.Metrics.Add("quarantine_purge_held", 1)
			continue
		}

		storage, bucket, err := s.recordStorage(ctx, record)
		if err != nil {
			s.log.Error("resolve storage of quarantined object", "record_id", record.ID, "error", err)
			continue
		}
		err = storage.DeleteObject(ctx, bucket, record.ObjectKey)
		if err != nil && !errors.Is(err, interfaces.ErrObjectNotFound) {
			s.log.Error("purge quarantined object", "record_id", record.ID, "error", err)
			continue
		}
		if err := s.deps.Quarantine.MarkPurged(ctx, record.ID, s.now()); err != nil {
			s.log.Error("mark quarantine record purged", "record_id", record.ID, "error", err)
			continue
		}
		purged++
		s.deps.Metrics.Add("quarantine_purged", 1)
		s.event(ctx, model.ProcessingJob{TenantID: record.TenantID, AssetID: record.AssetID, ID: record.JobID}, "purged", "")
	}
	return purged, nil
}

// Inspection is everything recorded about one asset's processing.
type Inspection struct {
	Asset      *model.Asset
	Job        model.ProcessingJob
	Quarantine []model.QuarantineRecord
	Events     []model.ProcessingEvent
}

// Inspect returns an asset's processing record for operators.
func (s *Service) Inspect(ctx context.Context, tenantID, assetID string) (Inspection, error) {
	return s.quarantine().Inspect(ctx, tenantID, assetID)
}

func (s *Service) quarantine() *Quarantine {
	q := NewQuarantine(s.deps.Assets, s.deps.Jobs, s.deps.Quarantine, s.deps.Events, nil)
	q.now = s.now
	return q
}

// purgeHeld reports whether a legal hold covers the record's asset.
func (s *Service) purgeHeld(ctx context.Context, record model.QuarantineRecord) (bool, error) {
	if s.deps.Holds == nil {
		return false, nil
	}
	dataset := ""
	if asset, err := s.deps.Assets.GetByID(ctx, record.TenantID, record.AssetID); err == nil {
		dataset = asset.Dataset
	}
	holds, err := s.deps.Holds.Active(ctx, record.TenantID, dataset, record.AssetID)
	return len(holds) > 0, err
}

// RetentionPolicy resolves a tenant's quarantine retention for an asset.
type RetentionPolicy interface {
	QuarantineRetention(ctx context.Context, asset model.Asset) (time.Duration, error)
}

// quarantineRetention is the platform retention, or the asset's policy
// when that keeps rejected uploads longer.
func (s *Service) quarantineRetention(ctx context.Context, asset model.Asset) time.Duration {
	retention := s.cfg.QuarantineRetention
	if s.deps.Retention == nil {
		return retention
	}
	policy, err := s.deps.Retention.QuarantineRetention(ctx, asset)
	if err != nil {
		s.log.Warn("load quarantine retention policy; using the platform retention", "asset_id", asset.ID, "error", err)
		return retention
	}
	return max(retention, policy)
}

// Requeue gives an EXHAUSTED job a fresh attempt budget; see
// Quarantine.Requeue.
func (s *Service) Requeue(ctx context.Context, tenantID, assetID, actor string) (model.ProcessingJob, error) {
	return s.quarantine().Requeue(ctx, tenantID, assetID, actor)
}

// recordBacklog publishes queue gauges. lease_expired is the stuck count:
// claimed work whose worker stopped renewing it.
func (s *Service) recordBacklog(ctx context.Context) {
	if s.deps.Metrics == nil {
		return
	}
	backlog, err := s.deps.Jobs.Backlog(ctx, s.now())
	if err != nil {
		s.log.Warn("count processing backlog", "error", err)
		return
	}
	for name, value := range backlog {
		s.deps.Metrics.Set("backlog_"+name, int64(value))
	}
}

func (s *Service) policyFor(ctx context.Context, tenantID string) (model.ContentPolicy, error) {
	policy := s.cfg.Policy
	if s.deps.Policies == nil {
		return policy, nil
	}
	tenant, ok, err := s.deps.Policies.Get(ctx, tenantID)
	if err != nil {
		return policy, fmt.Errorf("load content policy: %w", err)
	}
	if !ok {
		return policy, nil
	}
	return mergePolicy(policy, tenant)
}

// mergePolicy narrows the platform policy with a tenant's own: the tenant
// can add denied types, set its own allow list, lower the size limit and
// reject unknown types, but cannot loosen any platform rule.
func mergePolicy(platform, tenant model.ContentPolicy) (model.ContentPolicy, error) {
	merged := platform
	merged.DeniedTypes = append(append([]string(nil), platform.DeniedTypes...), tenant.DeniedTypes...)

	if len(tenant.AllowedTypes) > 0 {
		// With both lists, keep only the tenant entries the platform allows.
		merged.AllowedTypes = nil
		for _, t := range tenant.AllowedTypes {
			if len(platform.AllowedTypes) == 0 || matchesAny(t, platform.AllowedTypes) {
				merged.AllowedTypes = append(merged.AllowedTypes, t)
			}
		}
		if len(merged.AllowedTypes) == 0 {
			// Retried, then held: an operator must fix the policy.
			return merged, errors.New("tenant content policy allows no type the platform allows")
		}
	}

	if tenant.MaxSizeBytes > 0 && tenant.MaxSizeBytes < platform.MaxSizeBytes {
		merged.MaxSizeBytes = tenant.MaxSizeBytes
	}
	if tenant.UnknownTypeAction == "reject" {
		merged.UnknownTypeAction = "reject"
	}
	return merged, nil
}

func (s *Service) event(ctx context.Context, job model.ProcessingJob, name, detail string) {
	s.recordEvent(ctx, job, name, detail, "worker:"+s.cfg.Owner)
}

// recordEvent appends to the audit trail. A failed write is logged, not
// fatal: the job's own columns and the quarantine record stay the source
// of truth.
func (s *Service) recordEvent(ctx context.Context, job model.ProcessingJob, name, detail, actor string) {
	err := s.deps.Events.Record(ctx, model.ProcessingEvent{
		TenantID:  job.TenantID,
		AssetID:   job.AssetID,
		JobID:     job.ID,
		Event:     name,
		Detail:    truncate(detail, 500),
		Actor:     actor,
		CreatedAt: s.now(),
	})
	if err != nil {
		s.deps.Metrics.Add("event_write_failures", 1)
		s.log.Warn("record processing event", "event", name, "asset_id", job.AssetID, "error", err)
	}
}

func (s *Service) save(ctx context.Context, job *model.ProcessingJob) (model.ProcessingState, error) {
	return "", s.deps.Jobs.SaveProgress(ctx, *job, s.cfg.Owner, s.now())
}

// staging resolves where an asset's upload landed: the platform's
// quarantine bucket, or its tenant connection's staging prefix.
func (s *Service) staging(ctx context.Context, asset model.Asset) (interfaces.MultipartStorage, string, string, error) {
	if model.IsPlatformConnection(asset.StagingConnectionID) {
		return s.deps.Storage, s.cfg.Bucket, quarantineKey(asset.TenantID, asset.UploadID), nil
	}
	if s.deps.Router == nil {
		return nil, "", "", model.ErrStorageNotConfigured
	}
	loc, err := s.deps.Router.Locate(ctx, asset.TenantID, asset.StagingConnectionID)
	if err != nil {
		return nil, "", "", fmt.Errorf("resolve storage connection: %w", err)
	}
	return loc.Storage, loc.StagingBucket, loc.StagingKey(asset.TenantID, asset.UploadID), nil
}

// recordStorage is where a quarantine record's object is: its asset's
// staging connection.
func (s *Service) recordStorage(ctx context.Context, record model.QuarantineRecord) (interfaces.MultipartStorage, string, error) {
	asset, err := s.deps.Assets.GetByID(ctx, record.TenantID, record.AssetID)
	if errors.Is(err, model.ErrAssetNotFound) {
		return s.deps.Storage, s.cfg.Bucket, nil
	}
	if err != nil {
		return nil, "", err
	}
	storage, bucket, _, err := s.staging(ctx, *asset)
	return storage, bucket, err
}

// recordKey is the quarantine object a record names: the platform's key,
// or the staging key the asset was uploaded to.
func recordKey(asset model.Asset) string {
	if model.IsPlatformConnection(asset.StagingConnectionID) {
		return quarantineKey(asset.TenantID, asset.UploadID)
	}
	return asset.ObjectKey
}

// readHead reads at most sniffBytes from the start of the object.
func readHead(ctx context.Context, storage interfaces.MultipartStorage, bucket, objectKey string) ([]byte, error) {
	body, err := storage.OpenObject(ctx, bucket, objectKey)
	if err != nil {
		return nil, fmt.Errorf("open object for type check: %w", err)
	}
	defer body.Close()

	head := make([]byte, sniffBytes)
	n, err := io.ReadFull(body, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("read object head: %w", err)
	}
	return head[:n], nil
}

func quarantineKey(tenantID, uploadID string) string {
	return fmt.Sprintf("quarantine/%s/%s", tenantID, uploadID)
}

func truncate(text string, limit int) string {
	if len(text) > limit {
		return text[:limit]
	}
	return text
}
