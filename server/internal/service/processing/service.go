package processing

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/retry"
	"github.com/sanjeev0303/object-storage-platform/internal/service/promotion"
)

// ScanPolicy says what happens when no malware scanner is configured.
// A missing scanner is never reported as a clean scan.
type ScanPolicy string

const (
	// ScanPolicyHold keeps the asset PROCESSING (never AVAILABLE) and marks
	// the job EXHAUSTED for an operator. The default.
	ScanPolicyHold ScanPolicy = "hold"
	// ScanPolicyReject rejects the asset (scanner_unavailable).
	ScanPolicyReject ScanPolicy = "reject"
	// ScanPolicyAllowUnscanned promotes without a scan, recording
	// NOT_SCANNED. Only allowed in development (see config).
	ScanPolicyAllowUnscanned ScanPolicy = "allow_unscanned"
)

var errNoScanner = errors.New("no malware scanner is configured; asset held in processing")

// UploadVerifier checks a completed upload's stored bytes (existence, key,
// size, SHA-256). *upload.MultipartService implements it.
type UploadVerifier interface {
	VerifyIntegrity(ctx context.Context, tenantID, uploadID string) (model.UploadStatus, error)
}

type Dependencies struct {
	Jobs      interfaces.ProcessingJobRepository
	Assets    interfaces.AssetRepository
	Uploads   interfaces.UploadRepository
	Verifier  UploadVerifier
	Storage   interfaces.MultipartStorage
	Validator interfaces.FileValidator
	// Scanner is nil when no scanner is configured; ScanPolicy then applies.
	Scanner  interfaces.MalwareScanner
	Promoter *promotion.Service
}

type Config struct {
	// Owner identifies this worker on the leases it takes.
	Owner       string
	Bucket      string
	BatchSize   int
	Concurrency int
	// Lease bounds each job's processing time as well as its claim.
	Lease        time.Duration
	Retry        retry.Policy
	MaxFileBytes int64
	ScanPolicy   ScanPolicy
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
	if deps.Jobs == nil || deps.Assets == nil || deps.Uploads == nil || deps.Verifier == nil ||
		deps.Storage == nil || deps.Validator == nil || deps.Promoter == nil || log == nil {
		return nil, errors.New("processing dependencies are required")
	}
	if cfg.Owner == "" || cfg.Bucket == "" {
		return nil, errors.New("processing owner and bucket are required")
	}
	if cfg.BatchSize <= 0 || cfg.Concurrency <= 0 || cfg.Lease <= 0 || cfg.MaxFileBytes <= 0 {
		return nil, errors.New("processing batch size, concurrency, lease and file limit must be positive")
	}
	if cfg.Retry.MaxAttempts <= 0 || cfg.Retry.BaseDelay <= 0 || cfg.Retry.MaxDelay < cfg.Retry.BaseDelay {
		return nil, errors.New("processing retry policy is invalid")
	}
	switch cfg.ScanPolicy {
	case ScanPolicyHold, ScanPolicyReject, ScanPolicyAllowUnscanned:
	default:
		return nil, fmt.Errorf("unknown scan policy %q", cfg.ScanPolicy)
	}

	return &Service{deps: deps, cfg: cfg, log: log, now: func() time.Time { return time.Now().UTC() }}, nil
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
}

// RunOnce creates jobs for newly completed uploads, claims a batch and
// processes it with at most Concurrency jobs at a time.
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

	return stats, nil
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

	count := func(field *int) {
		mu.Lock()
		*field++
		mu.Unlock()
	}

	if ctx.Err() != nil {
		// Shutting down: write nothing; the lease expires and the job resumes.
		log.Info("processing interrupted by shutdown", "reached_stage", string(job.Stage))
		return
	}
	if errors.Is(err, model.ErrLeaseLost) {
		count(&stats.LeaseLost)
		log.Warn("processing lease lost; another worker owns the job")
		return
	}

	now := s.now()
	duration := time.Since(started).Milliseconds()
	var releaseErr error

	switch {
	case err == nil:
		releaseErr = s.deps.Jobs.Finish(ctx, job.ID, s.cfg.Owner, now, state, "")
		if releaseErr == nil && state == model.ProcessingSucceeded {
			count(&stats.Succeeded)
			log.Info("asset promoted to AVAILABLE", "duration_ms", duration)
		} else if releaseErr == nil {
			count(&stats.Failed)
			log.Warn("asset processing ended without promotion", "state", string(state), "duration_ms", duration)
		}

	case retry.IsPermanent(err) || job.AttemptCount >= job.MaxAttempts:
		releaseErr = s.deps.Jobs.Finish(ctx, job.ID, s.cfg.Owner, now, model.ProcessingExhausted, retry.Sanitize(err))
		if releaseErr == nil {
			count(&stats.Exhausted)
			log.Error("processing exhausted; asset stays unavailable until an operator acts",
				"reached_stage", string(job.Stage), "permanent", retry.IsPermanent(err), "error", retry.Sanitize(err))
		}

	default:
		next := now.Add(s.cfg.Retry.Delay(job.AttemptCount))
		releaseErr = s.deps.Jobs.Reschedule(ctx, job.ID, s.cfg.Owner, now, next, retry.Sanitize(err))
		if releaseErr == nil {
			count(&stats.Retried)
			log.Warn("processing will retry", "reached_stage", string(job.Stage),
				"next_attempt_at", next, "error", retry.Sanitize(err))
		}
	}

	if errors.Is(releaseErr, model.ErrLeaseLost) {
		count(&stats.LeaseLost)
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
		if asset.Status == model.AssetStatusRejected || asset.Status == model.AssetStatusDeleted {
			if job.PromotionKey != "" {
				if err := s.deps.Promoter.RemoveDestination(ctx, job.PromotionKey); err != nil {
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
			state, err = s.cleanup(ctx, *asset)
		default:
			return "", retry.Permanent(fmt.Errorf("unknown processing stage %q", job.Stage))
		}
		if err != nil || state != "" {
			return state, err
		}
	}
}

// verify checks the stored bytes and the file type, then moves the asset
// QUARANTINED -> PROCESSING.
func (s *Service) verify(ctx context.Context, job *model.ProcessingJob, asset model.Asset) (model.ProcessingState, error) {
	if asset.UploadID != job.UploadID || asset.ObjectKey != quarantineKey(asset.TenantID, asset.UploadID) {
		return "", retry.Permanent(errors.New("asset does not match its upload's quarantine key"))
	}
	if asset.SizeBytes > s.cfg.MaxFileBytes {
		return s.reject(ctx, asset, model.RejectFileTooLarge)
	}

	// Existence, key, size and SHA-256 of the actual bytes (streamed, not
	// the multipart ETag). Repeating it on a verified upload is a no-op.
	status, err := s.deps.Verifier.VerifyIntegrity(ctx, asset.TenantID, asset.UploadID)
	if err != nil {
		return "", fmt.Errorf("verify upload: %w", err)
	}
	switch status {
	case model.UploadStatusVerified:
	case model.UploadStatusFailed:
		reason := model.RejectVerificationFailed
		if session, err := s.deps.Uploads.GetByID(ctx, asset.TenantID, asset.UploadID); err == nil && session.FailureReason != "" {
			reason = session.FailureReason
		}
		s.log.Warn("asset failed verification", "asset_id", asset.ID, "tenant_id", asset.TenantID, "reason", reason)
		return s.reject(ctx, asset, reason)
	default:
		return "", fmt.Errorf("upload is %s, not yet verifiable", status)
	}

	head, err := s.readHead(ctx, asset.ObjectKey)
	if err != nil {
		return "", err
	}
	detected, reason := s.deps.Validator.Validate(head, asset.ContentType)
	job.DetectedContentType = detected
	if reason != "" {
		return s.reject(ctx, asset, reason)
	}

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

	verifiedAt := s.now()
	job.ChecksumAlgorithm = "SHA-256"
	job.ChecksumVerifiedAt = &verifiedAt
	job.Stage = model.StageScan
	return s.save(ctx, job)
}

// scan runs the configured malware scanner, or applies ScanPolicy when
// there is none.
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
			return s.reject(ctx, asset, model.RejectScannerUnavailable)
		case ScanPolicyAllowUnscanned:
			job.ScanResult = model.ScanNotScanned
			job.Stage = model.StagePromote
			s.log.Warn("asset promoted without a malware scan (development policy)", "asset_id", asset.ID)
			return s.save(ctx, job)
		default:
			return "", retry.Permanent(errNoScanner)
		}
	}

	body, err := s.deps.Storage.OpenObject(ctx, s.cfg.Bucket, asset.ObjectKey)
	if err != nil {
		return "", fmt.Errorf("open object to scan: %w", err)
	}
	verdict, err := s.deps.Scanner.Scan(ctx, body)
	body.Close()
	if err != nil {
		return "", fmt.Errorf("malware scan: %w", err)
	}

	scannedAt := s.now()
	job.Scanner = s.deps.Scanner.Name()
	job.ScannedAt = &scannedAt

	if verdict.Infected {
		job.ScanResult = model.ScanInfected
		job.ScanSignature = truncate(verdict.Signature, 200)
		if _, err := s.save(ctx, job); err != nil {
			return "", err
		}
		s.log.Warn("malware detected; asset rejected", "asset_id", asset.ID, "tenant_id", asset.TenantID,
			"scanner", job.Scanner, "signature", job.ScanSignature)
		return s.reject(ctx, asset, model.RejectMalwareDetected)
	}

	job.ScanResult = model.ScanClean
	job.Stage = model.StagePromote
	return s.save(ctx, job)
}

// promote records the destination key before copying, so a crash after the
// copy always knows which object it may have left behind.
func (s *Service) promote(ctx context.Context, job *model.ProcessingJob, asset model.Asset) (model.ProcessingState, error) {
	if job.ScanResult != model.ScanClean && job.ScanResult != model.ScanNotScanned {
		return "", retry.Permanent(errors.New("promotion requires a recorded scan result"))
	}

	if job.PromotionKey == "" {
		job.PromotionKey = s.deps.Promoter.DestinationKey(asset)
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
	case model.AssetStatusRejected, model.AssetStatusDeleted:
		return "", nil // finished by someone else meanwhile: run() removes the copy
	default:
		return "", retry.Permanent(fmt.Errorf("asset is %s at the promotion stage", status))
	}

	promotedAt := s.now()
	job.PromotedAt = &promotedAt
	job.Stage = model.StageCleanup
	return s.save(ctx, job)
}

// cleanup deletes the quarantine source, only after the asset is AVAILABLE
// and that state is committed.
func (s *Service) cleanup(ctx context.Context, asset model.Asset) (model.ProcessingState, error) {
	if err := s.deps.Promoter.RemoveSource(ctx, asset); err != nil {
		return "", err
	}
	return model.ProcessingSucceeded, nil
}

func (s *Service) reject(ctx context.Context, asset model.Asset, reason string) (model.ProcessingState, error) {
	err := s.deps.Assets.TransitionStatus(ctx, asset.TenantID, asset.ID,
		[]model.AssetStatus{model.AssetStatusQuarantined, model.AssetStatusProcessing},
		model.AssetStatusRejected, reason, "", s.now())
	if err != nil && !errors.Is(err, model.ErrInvalidAssetState) {
		return "", fmt.Errorf("reject asset: %w", err)
	}
	s.log.Warn("asset rejected", "asset_id", asset.ID, "tenant_id", asset.TenantID, "reason", reason)
	// Already rejected or deleted by someone else ends the job the same way.
	return model.ProcessingFailed, nil
}

func (s *Service) save(ctx context.Context, job *model.ProcessingJob) (model.ProcessingState, error) {
	return "", s.deps.Jobs.SaveProgress(ctx, *job, s.cfg.Owner, s.now())
}

// readHead reads at most sniffBytes from the start of the object.
func (s *Service) readHead(ctx context.Context, objectKey string) ([]byte, error) {
	body, err := s.deps.Storage.OpenObject(ctx, s.cfg.Bucket, objectKey)
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
