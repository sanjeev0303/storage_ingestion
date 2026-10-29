package ingestion

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

	"github.com/sanjeev0303/object-storage-platform/internal/dto/event"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/concurrency"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/config"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/correlation"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/metrics"
	"github.com/sanjeev0303/object-storage-platform/internal/service/audit"
	"github.com/sanjeev0303/object-storage-platform/internal/service/upload"
)

// Errors the API maps to client responses.
var (
	ErrInvalidInput        = errors.New("invalid ingestion request")
	ErrManifestTooLarge    = errors.New("manifest is larger than allowed")
	ErrCompletionPending   = errors.New("completion accepted; its outcome is being confirmed")
	ErrManifestNotStaged   = errors.New("the manifest object has not been uploaded")
	ErrManifestChecksum    = errors.New("the manifest does not match its declared sha256")
	ErrUploadNotAttached   = errors.New("the entry's upload has not been created yet")
	ErrStrategyMismatch    = errors.New("this operation does not apply to the entry's upload strategy")
	errManifestBudgetSpent = errors.New("manifest read budget spent")
)

// SingleUploads is what ingestion needs from the single-request upload
// service.
type SingleUploads interface {
	Create(ctx context.Context, input upload.CreateInput) (upload.CreateResult, error)
	Plan(ctx context.Context, tenantID, uploadID string) (upload.CreateResult, error)
	Complete(ctx context.Context, tenantID, uploadID string) (model.UploadStatus, error)
}

// MultipartUploads is what ingestion needs from the multipart service.
type MultipartUploads interface {
	Start(ctx context.Context, input upload.StartMultipartInput) (upload.MultipartResult, error)
	PresignParts(ctx context.Context, tenantID, ownerID, uploadID string, partNumbers []int32) ([]upload.PartURL, error)
	RecordParts(ctx context.Context, tenantID, ownerID, uploadID string, inputs []upload.RecordPartInput) error
	Complete(ctx context.Context, tenantID, ownerID, uploadID string, input upload.CompleteMultipartInput) (model.UploadSession, error)
	Abort(ctx context.Context, tenantID, ownerID, uploadID string) (model.UploadSession, error)
}

// ManifestStore holds staged manifests: clients upload them with a
// presigned PUT and the worker reads them in ranges.
type ManifestStore interface {
	CreateUploadPlan(ctx context.Context, target interfaces.UploadTarget) (interfaces.UploadPlan, error)
	HeadObject(ctx context.Context, bucket, objectKey string) (interfaces.ObjectMetadata, error)
	OpenRange(ctx context.Context, bucket, objectKey string, first, last int64, cond interfaces.ReadCondition) (io.ReadCloser, error)
}

// Dependencies are the collaborators of the ingestion service.
type Dependencies struct {
	Repo      interfaces.IngestionRepository
	Uploads   SingleUploads
	Multipart MultipartUploads
	Sessions  interfaces.UploadRepository
	Assets    interfaces.AssetRepository
	Jobs      interfaces.ProcessingJobRepository
	// Requeue gives a held processing job a fresh budget. Only the worker,
	// which runs processing, sets it; the API leaves requeues to it.
	Requeue   func(ctx context.Context, tenantID, assetID, actor string) error
	Manifests ManifestStore
	Bucket    string
	Log       *slog.Logger
	Metrics   *metrics.Registry
	// Audit, when set, records refused attempts and the changes callers
	// make (datasets, jobs, manifests, cancellations, retries).
	Audit interfaces.AuditRecorder
}

// Service runs bulk ingestion.
type Service struct {
	deps    Dependencies
	cfg     config.Ingestion
	storage *concurrency.Limiter
	parsing *concurrency.Limiter
	now     func() time.Time

	// cancelledTo is, per cancelling job, the position its cancellation
	// has reached in this process (sweeps.go).
	cancelMu    sync.Mutex
	cancelledTo map[string]int64
}

func NewService(deps Dependencies, cfg config.Ingestion) (*Service, error) {
	if deps.Repo == nil || deps.Uploads == nil || deps.Multipart == nil || deps.Sessions == nil ||
		deps.Assets == nil || deps.Jobs == nil || deps.Manifests == nil ||
		deps.Bucket == "" || deps.Log == nil {
		return nil, errors.New("ingestion service dependencies are required")
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &Service{
		deps: deps, cfg: cfg,
		storage: concurrency.NewLimiter(cfg.StorageConcurrency),
		parsing: concurrency.NewLimiter(cfg.ParseConcurrency),
		now:     func() time.Time { return time.Now().UTC() },
	}, nil
}

func (s *Service) limits() ManifestLimits {
	return ManifestLimits{
		MaxLineBytes: s.cfg.MaxLineBytes, MaxEntries: s.cfg.MaxEntriesPerJob,
		MaxMetadataBytes: s.cfg.MaxMetadataBytes, MaxMetadataKeys: s.cfg.MaxMetadataKeys,
		MaxObjectBytes: s.cfg.MaxObjectBytes, SmallFileThreshold: s.cfg.SmallFileThreshold,
		Path: model.PathPolicy{MaxBytes: s.cfg.MaxPathBytes, MaxSegmentBytes: s.cfg.MaxSegmentBytes, MaxDepth: s.cfg.MaxPathDepth},
	}
}

// quota is the tenant's effective quota: its overrides, or the defaults.
func (s *Service) quota(ctx context.Context, tenantID string) (model.TenantQuota, error) {
	q, err := s.deps.Repo.Quota(ctx, tenantID)
	if err != nil {
		return q, err
	}
	if q.MaxStorageBytes == 0 {
		q.MaxStorageBytes = s.cfg.TenantStorageQuota
	}
	if q.MaxActiveJobs == 0 {
		q.MaxActiveJobs = s.cfg.MaxActiveJobs
	}
	if q.MaxActiveUploads == 0 || q.MaxActiveUploads > s.cfg.TenantUploadSlots {
		q.MaxActiveUploads = s.cfg.TenantUploadSlots
	}
	if q.MaxPendingEntries == 0 {
		q.MaxPendingEntries = s.cfg.MaxPendingEntries
	}
	// A tenant's manifest limits can be lower than the platform's, never
	// higher.
	if q.MaxManifestEntries == 0 || q.MaxManifestEntries > int64(s.cfg.MaxEntriesPerJob) {
		q.MaxManifestEntries = int64(s.cfg.MaxEntriesPerJob)
	}
	if q.MaxManifestBytes == 0 || q.MaxManifestBytes > s.cfg.MaxManifestBytes {
		q.MaxManifestBytes = s.cfg.MaxManifestBytes
	}
	return q, nil
}

// limitsFor is limits with the tenant's manifest entry cap.
func (s *Service) limitsFor(q model.TenantQuota) ManifestLimits {
	l := s.limits()
	l.MaxEntries = int(q.MaxManifestEntries)
	return l
}

// onStorage runs a storage call within the storage concurrency limit and
// its timeout.
func (s *Service) onStorage(ctx context.Context, fn func(context.Context) error) error {
	return s.storage.Do(ctx, func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, s.cfg.StorageTimeout)
		defer cancel()
		return fn(ctx)
	})
}

func (s *Service) count(name string, n int64) {
	s.deps.Metrics.Add(name, n)
}

// ─── Datasets ─────────────────────────────────────────────────────────

// CreateDatasetInput describes a dataset.
type CreateDatasetInput struct {
	Name             string
	CompletionPolicy model.CompletionPolicy
	MaxBytes         int64
}

// CreateDataset creates the caller's dataset, or returns the existing one
// of that name when its settings match.
func (s *Service) CreateDataset(ctx context.Context, p model.Principal, in CreateDatasetInput) (model.Dataset, bool, error) {
	if err := s.authorize(ctx, p, model.PermDatasetsWrite, "dataset.create", "dataset", in.Name); err != nil {
		return model.Dataset{}, false, err
	}
	if !model.ValidDataset(in.Name) {
		return model.Dataset{}, false, fmt.Errorf("%w: name must be 1-64 characters of a-z, 0-9, '.', '_' or '-', starting with a letter or digit", ErrInvalidInput)
	}
	if in.CompletionPolicy == "" {
		in.CompletionPolicy = model.CompleteAll
	}
	if !in.CompletionPolicy.Valid() {
		return model.Dataset{}, false, fmt.Errorf("%w: completion_policy must be ALL, REQUIRED or PARTIAL", ErrInvalidInput)
	}
	if in.MaxBytes < 0 {
		return model.Dataset{}, false, fmt.Errorf("%w: max_bytes must not be negative", ErrInvalidInput)
	}
	now := s.now()
	d, created, err := s.deps.Repo.CreateDataset(ctx, model.Dataset{
		ID: event.NewID(), TenantID: p.TenantID, Name: in.Name, CompletionPolicy: in.CompletionPolicy,
		MaxBytes: in.MaxBytes, CreatedBy: p.UserID, CreatedAt: now, UpdatedAt: now,
	})
	if err == nil && created {
		s.record(ctx, p, "dataset.create", "dataset", d.ID, map[string]any{"name": d.Name, "policy": string(d.CompletionPolicy)})
	}
	return d, created, err
}

func (s *Service) GetDataset(ctx context.Context, p model.Principal, datasetID string) (model.Dataset, error) {
	if err := s.authorize(ctx, p, model.PermDatasetsRead, "dataset.read", "dataset", datasetID); err != nil {
		return model.Dataset{}, err
	}
	return s.deps.Repo.GetDataset(ctx, p.TenantID, datasetID)
}

// ─── Jobs ─────────────────────────────────────────────────────────────

// CreateJobInput describes an ingestion job.
type CreateJobInput struct {
	DatasetID      string
	IdempotencyKey string
	// MaxConcurrency caps the job's upload slots; 0 uses the default.
	MaxConcurrency int
}

// CreateJob creates an ingestion job, or returns the caller's job with the
// same idempotency key. The tenant's active-job limit applies.
func (s *Service) CreateJob(ctx context.Context, p model.Principal, in CreateJobInput) (model.IngestionJob, bool, error) {
	if err := s.authorize(ctx, p, model.PermIngestionWrite, "ingestion.job_create", "ingestion_job", ""); err != nil {
		return model.IngestionJob{}, false, err
	}
	key := strings.TrimSpace(in.IdempotencyKey)
	if key == "" || len(key) > 128 {
		return model.IngestionJob{}, false, fmt.Errorf("%w: idempotency_key is required, at most 128 characters", ErrInvalidInput)
	}
	if in.MaxConcurrency < 0 || in.MaxConcurrency > s.cfg.JobUploadSlots {
		return model.IngestionJob{}, false, fmt.Errorf("%w: max_concurrency must be between 0 and %d", ErrInvalidInput, s.cfg.JobUploadSlots)
	}
	ds, err := s.deps.Repo.GetDataset(ctx, p.TenantID, in.DatasetID)
	if err != nil {
		return model.IngestionJob{}, false, err
	}
	if ds.ParentID != "" {
		// Files land in the parent's namespace; a branch then pins them.
		return model.IngestionJob{}, false, fmt.Errorf("%w: ingest into the dataset the branch belongs to, then add the files to the branch", ErrInvalidInput)
	}
	q, err := s.quota(ctx, p.TenantID)
	if err != nil {
		return model.IngestionJob{}, false, err
	}
	now := s.now()
	job, created, err := s.deps.Repo.CreateJob(ctx, model.IngestionJob{
		ID: event.NewID(), TenantID: p.TenantID, DatasetID: in.DatasetID, CreatedBy: p.UserID,
		IdempotencyKey: key, MaxConcurrency: in.MaxConcurrency, CreatedAt: now, UpdatedAt: now,
	}, q.MaxActiveJobs)
	if err == nil && created {
		s.record(ctx, p, "ingestion.job_create", "ingestion_job", job.ID, map[string]any{"dataset_id": job.DatasetID})
	}
	return job, created, err
}

func (s *Service) GetJob(ctx context.Context, p model.Principal, jobID string) (model.IngestionJob, error) {
	if err := s.authorize(ctx, p, model.PermIngestionRead, "ingestion.read", "ingestion_job", jobID); err != nil {
		return model.IngestionJob{}, err
	}
	return s.deps.Repo.GetJob(ctx, p.TenantID, jobID)
}

func (s *Service) ListJobs(ctx context.Context, p model.Principal, datasetID, after string, limit int) ([]model.IngestionJob, error) {
	if err := s.authorize(ctx, p, model.PermIngestionRead, "ingestion.read", "ingestion_job", ""); err != nil {
		return nil, err
	}
	return s.deps.Repo.ListJobs(ctx, p.TenantID, datasetID, after, PageSize(limit))
}

// ─── Manifests ────────────────────────────────────────────────────────

// ManifestReport is what reading a manifest found.
type ManifestReport struct {
	Lines      int64
	Accepted   int
	Invalid    int
	Duplicates int
	Replayed   int
	// Samples are the first invalid lines of this submission.
	Samples  []model.IngestionEntry
	Complete bool
	SHA256   string
}

const maxSamples = 20

// SubmitInlineManifest reads a manifest sent in the request body, in
// batches, each stored with its checkpoint. Sending the same manifest
// again is safe: stored lines are skipped, and once it is complete the
// same bytes give the same report while different bytes are refused.
// declaredSHA256, when given, must match the manifest's checksum.
func (s *Service) SubmitInlineManifest(ctx context.Context, p model.Principal, jobID string, body io.Reader, declaredSHA256 string) (ManifestReport, error) {
	if err := s.authorize(ctx, p, model.PermIngestionWrite, "ingestion.manifest_submit", "ingestion_job", jobID); err != nil {
		return ManifestReport{}, err
	}
	declaredSHA256 = strings.ToLower(strings.TrimSpace(declaredSHA256))
	if declaredSHA256 != "" {
		if _, ok := parseChecksum("sha256:" + declaredSHA256); !ok {
			return ManifestReport{}, fmt.Errorf("%w: the declared manifest sha256 must be 64 hex digits", ErrInvalidInput)
		}
	}
	job, err := s.deps.Repo.GetJob(ctx, p.TenantID, jobID)
	if err != nil {
		return ManifestReport{}, err
	}
	if job.ManifestSource == model.ManifestStaged {
		return ManifestReport{}, fmt.Errorf("%w: this job's manifest is staged", model.ErrInvalidJobState)
	}
	if declaredSHA256 != "" && job.ManifestSHA256 != "" && declaredSHA256 != job.ManifestSHA256 {
		return ManifestReport{}, model.ErrManifestConflict
	}
	q := s.mustQuota(ctx, p.TenantID)
	inlineLimit := min(s.cfg.MaxInlineManifestBytes, q.MaxManifestBytes)

	hasher := sha256.New()
	limited := &countingReader{r: io.LimitReader(body, inlineLimit+1)}
	tee := io.TeeReader(limited, hasher)

	if job.ManifestComplete {
		// Already read in full: only check that this is the same manifest.
		if _, err := io.Copy(io.Discard, tee); err != nil {
			return ManifestReport{}, fmt.Errorf("read manifest: %w", err)
		}
		sum := hex.EncodeToString(hasher.Sum(nil))
		if sum != job.ManifestSHA256 {
			return ManifestReport{}, model.ErrManifestConflict
		}
		return ManifestReport{Lines: job.ParsedLines, Replayed: int(job.Counters.Total), Complete: true, SHA256: sum}, nil
	}

	job, err = s.deps.Repo.StartManifest(ctx, p.TenantID, jobID, interfaces.ManifestSource{Source: model.ManifestInline, SHA256: declaredSHA256}, s.now())
	if err != nil {
		return ManifestReport{}, err
	}
	reader := NewManifestReader(tee, s.limitsFor(q), 0, 0, job.ParsedLines)
	report, err := s.readManifest(ctx, job, reader, func() error {
		if limited.n > inlineLimit {
			return ErrManifestTooLarge
		}
		return nil
	})
	if errors.Is(err, ErrManifestTooLarge) || errors.Is(err, ErrTooManyLines) {
		// Lines already stored are cancelled with the job.
		_ = s.deps.Repo.FailManifest(ctx, job.ID, err.Error(), s.now())
		return report, err
	}
	if err != nil {
		return report, err
	}

	sum := hex.EncodeToString(hasher.Sum(nil))
	report.SHA256 = sum
	if expected := job.ManifestSHA256; expected != "" && expected != sum {
		_ = s.deps.Repo.FailManifest(ctx, job.ID, ErrManifestChecksum.Error(), s.now())
		return report, ErrManifestChecksum
	}
	if err := s.deps.Repo.CompleteManifest(ctx, job.ID, sum, limited.n, s.now()); err != nil {
		return report, err
	}
	report.Complete = true
	s.record(ctx, p, "ingestion.manifest_submit", "ingestion_job", job.ID, map[string]any{
		"source": model.ManifestInline, "lines": report.Lines, "accepted": report.Accepted, "invalid": report.Invalid, "bytes": limited.n,
	})
	return report, nil
}

// countingReader counts the bytes read through it.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// readManifest stores entries from reader in batches of InsertBatch lines.
// check runs after each batch and stops reading with its error. It stops
// with errManifestBudgetSpent once deadline passes, if one is set.
func (s *Service) readManifest(ctx context.Context, job model.IngestionJob, reader *ManifestReader, check func() error) (ManifestReport, error) {
	var report ManifestReport
	batch := make([]model.IngestionEntry, 0, s.cfg.InsertBatch)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		started := time.Now()
		cp := interfaces.Checkpoint{Lines: reader.Position(), Bytes: reader.Offset()}
		res, err := s.deps.Repo.AppendEntries(ctx, job, batch, cp, s.mustQuota(ctx, job.TenantID).MaxPendingEntries, s.now())
		if err != nil {
			return err
		}
		s.count("ingestion_manifest_batches", 1)
		s.count("ingestion_batch_ms_total", time.Since(started).Milliseconds())
		s.count("ingestion_entries_accepted", int64(res.Accepted))
		s.count("ingestion_entries_rejected", int64(res.Invalid))
		report.Accepted += res.Accepted
		report.Invalid += res.Invalid
		report.Duplicates += res.Duplicates
		report.Replayed += res.Replayed
		for _, e := range batch {
			if e.State == model.EntryInvalid && len(report.Samples) < maxSamples {
				report.Samples = append(report.Samples, e)
			}
		}
		batch = batch[:0]
		return check()
	}

	for {
		line, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			// Keep the progress made before a read error, so a retry resumes
			// after it. A manifest with too many lines is rejected whole.
			if !errors.Is(err, ErrTooManyLines) {
				if ferr := flush(); ferr != nil && !errors.Is(ferr, errManifestBudgetSpent) {
					return report, ferr
				}
			}
			report.Lines = reader.Position()
			return report, err
		}
		line.Entry.TenantID, line.Entry.DatasetID, line.Entry.JobID = job.TenantID, job.DatasetID, job.ID
		batch = append(batch, line.Entry)
		s.count("ingestion_manifest_lines", 1)
		if len(batch) == cap(batch) {
			if err := flush(); err != nil {
				report.Lines = reader.Position()
				return report, err
			}
		}
	}
	report.Lines = reader.Position()
	if err := flush(); err != nil {
		return report, err
	}
	// Trailing blank lines move the checkpoint without storing anything.
	if _, err := s.deps.Repo.AppendEntries(ctx, job, nil,
		interfaces.Checkpoint{Lines: reader.Position(), Bytes: reader.Offset()}, 0, s.now()); err != nil {
		return report, err
	}
	return report, nil
}

// mustQuota is the tenant's quota, or the defaults when it cannot be read:
// the pending-entry check is then the configured default.
func (s *Service) mustQuota(ctx context.Context, tenantID string) model.TenantQuota {
	q, err := s.quota(ctx, tenantID)
	if err != nil {
		return model.TenantQuota{MaxPendingEntries: s.cfg.MaxPendingEntries}
	}
	return q
}

// StagedManifest is where a client uploads a large manifest.
type StagedManifest struct {
	ObjectKey string
	UploadURL string
	ExpiresAt time.Time
}

// manifestKey is generated from trusted identifiers only.
func manifestKey(tenantID, jobID string) string {
	return "manifests/" + tenantID + "/" + jobID + ".jsonl"
}

// StageManifest gives the client a presigned PUT for a manifest too large
// for one request.
func (s *Service) StageManifest(ctx context.Context, p model.Principal, jobID string, sizeBytes int64) (StagedManifest, error) {
	if err := s.authorize(ctx, p, model.PermIngestionWrite, "ingestion.manifest_stage", "ingestion_job", jobID); err != nil {
		return StagedManifest{}, err
	}
	limit := s.mustQuota(ctx, p.TenantID).MaxManifestBytes
	if sizeBytes <= 0 || sizeBytes > limit {
		return StagedManifest{}, fmt.Errorf("%w: the manifest may be at most %d bytes", ErrManifestTooLarge, limit)
	}
	job, err := s.deps.Repo.GetJob(ctx, p.TenantID, jobID)
	if err != nil {
		return StagedManifest{}, err
	}
	if job.State != model.JobPendingValidation {
		return StagedManifest{}, model.ErrInvalidJobState
	}
	key := manifestKey(job.TenantID, job.ID)
	var plan interfaces.UploadPlan
	err = s.onStorage(ctx, func(ctx context.Context) error {
		var err error
		plan, err = s.deps.Manifests.CreateUploadPlan(ctx, interfaces.UploadTarget{
			Bucket: s.deps.Bucket, ObjectKey: key, ContentType: "application/x-ndjson",
			SizeBytes: sizeBytes, ExpiresAt: s.now().Add(s.cfg.UploadURLTTL),
		})
		return err
	})
	if err != nil {
		return StagedManifest{}, fmt.Errorf("presign manifest upload: %w", err)
	}
	return StagedManifest{ObjectKey: key, UploadURL: plan.UploadURL, ExpiresAt: plan.ExpiresAt}, nil
}

// SubmitStagedManifest starts reading an uploaded manifest. The read runs
// in the worker: this records the manifest's size and ETag, and writes the
// event that starts the read in the same transaction.
func (s *Service) SubmitStagedManifest(ctx context.Context, p model.Principal, jobID, declaredSHA256 string) (model.IngestionJob, error) {
	if err := s.authorize(ctx, p, model.PermIngestionWrite, "ingestion.manifest_submit", "ingestion_job", jobID); err != nil {
		return model.IngestionJob{}, err
	}
	declaredSHA256 = strings.ToLower(strings.TrimSpace(declaredSHA256))
	job, err := s.deps.Repo.GetJob(ctx, p.TenantID, jobID)
	if err != nil {
		return job, err
	}
	key := manifestKey(job.TenantID, job.ID)
	var meta interfaces.ObjectMetadata
	err = s.onStorage(ctx, func(ctx context.Context) error {
		var err error
		meta, err = s.deps.Manifests.HeadObject(ctx, s.deps.Bucket, key)
		return err
	})
	if errors.Is(err, interfaces.ErrObjectNotFound) {
		return job, ErrManifestNotStaged
	}
	if err != nil {
		return job, fmt.Errorf("check staged manifest: %w", err)
	}
	if meta.SizeBytes > s.mustQuota(ctx, p.TenantID).MaxManifestBytes {
		return job, ErrManifestTooLarge
	}
	if meta.SizeBytes == 0 {
		return job, fmt.Errorf("%w: the staged manifest is empty", ErrInvalidInput)
	}
	now := s.now()
	s.record(ctx, p, "ingestion.manifest_submit", "ingestion_job", jobID, map[string]any{
		"source": model.ManifestStaged, "bytes": meta.SizeBytes,
	})
	return s.deps.Repo.StartManifest(ctx, p.TenantID, jobID, interfaces.ManifestSource{
		Source: model.ManifestStaged, ObjectKey: key, ETag: meta.ETag, SizeBytes: meta.SizeBytes, SHA256: declaredSHA256,
	}, now, event.NewManifestStaged(p.TenantID, jobID, now, correlation.From(ctx)))
}

// ReadStagedManifest reads a staged manifest from its checkpoint, for at
// most the configured budget, under a lease so only one reader works on
// it. A read that stops (budget, crash) is resumed by the worker's poll.
func (s *Service) ReadStagedManifest(ctx context.Context, owner, jobID string) (string, error) {
	jobs, err := s.deps.Repo.ClaimManifests(ctx, owner, jobID, s.now(), s.cfg.ManifestLease, 1)
	if err != nil {
		return "", err
	}
	if len(jobs) == 0 {
		return "not due: being read, finished or cancelled", nil
	}
	return s.readStaged(ctx, owner, jobs[0])
}

func (s *Service) readStaged(ctx context.Context, owner string, job model.IngestionJob) (string, error) {
	var note string
	err := s.parsing.Do(ctx, func(ctx context.Context) error {
		defer func() { _ = s.deps.Repo.ReleaseManifest(context.WithoutCancel(ctx), job.ID, owner) }()
		var err error
		note, err = s.readStagedLocked(ctx, owner, job)
		return err
	})
	return note, err
}

func (s *Service) readStagedLocked(ctx context.Context, owner string, job model.IngestionJob) (string, error) {
	if job.ParsedBytes < job.ManifestBytes {
		// The stream is read for the whole budget, so it gets a deadline of
		// its own rather than the per-call storage timeout. IfMatch refuses
		// a manifest replaced since it was submitted.
		readCtx, cancel := context.WithTimeout(ctx, s.cfg.ParseBudget+s.cfg.StorageTimeout)
		defer cancel()
		body, err := s.deps.Manifests.OpenRange(readCtx, s.deps.Bucket, job.ManifestObjectKey,
			job.ParsedBytes, job.ManifestBytes-1, interfaces.ReadCondition{IfMatch: job.ManifestETag})
		if errors.Is(err, interfaces.ErrObjectNotFound) || errors.Is(err, interfaces.ErrPreconditionFailed) {
			_ = s.deps.Repo.FailManifest(ctx, job.ID, "the staged manifest disappeared or was replaced", s.now())
			return "manifest gone", nil
		}
		if err != nil {
			return "", fmt.Errorf("open staged manifest: %w", err)
		}
		defer body.Close()

		deadline := time.Now().Add(s.cfg.ParseBudget)
		reader := NewManifestReader(body, s.limitsFor(s.mustQuota(ctx, job.TenantID)), job.ParsedLines, job.ParsedBytes, 0)
		report, err := s.readManifest(ctx, job, reader, func() error {
			if err := s.deps.Repo.ExtendManifest(ctx, job.ID, owner, s.now(), s.cfg.ManifestLease); err != nil {
				return err
			}
			if time.Now().After(deadline) {
				return errManifestBudgetSpent
			}
			return nil
		})
		switch {
		case errors.Is(err, errManifestBudgetSpent):
			return fmt.Sprintf("read %d lines so far; continues", report.Lines), nil
		case errors.Is(err, ErrTooManyLines), errors.Is(err, model.ErrQuotaExceeded):
			_ = s.deps.Repo.FailManifest(ctx, job.ID, err.Error(), s.now())
			return "manifest rejected: " + err.Error(), nil
		case errors.Is(err, model.ErrInvalidJobState):
			return "the job is no longer validating", nil
		case err != nil:
			return "", err
		}
	}
	if job.ManifestSHA256 != "" {
		sum, err := s.stagedChecksum(ctx, job)
		if errors.Is(err, interfaces.ErrObjectNotFound) || errors.Is(err, interfaces.ErrPreconditionFailed) {
			_ = s.deps.Repo.FailManifest(ctx, job.ID, "the staged manifest disappeared or was replaced", s.now())
			return "manifest gone", nil
		}
		if err != nil {
			return "", err
		}
		if sum != job.ManifestSHA256 {
			_ = s.deps.Repo.FailManifest(ctx, job.ID, ErrManifestChecksum.Error(), s.now())
			return "manifest rejected: " + ErrManifestChecksum.Error(), nil
		}
	}
	if err := s.deps.Repo.CompleteManifest(ctx, job.ID, "", job.ManifestBytes, s.now()); err != nil {
		if errors.Is(err, model.ErrInvalidJobState) {
			return "the job is no longer validating", nil
		}
		return "", err
	}
	return "manifest read", nil
}

// stagedChecksum hashes the whole staged manifest. Its lines are read in
// passes that resume from checkpoints, so the declared checksum gets a
// streaming pass of its own once every line is stored.
func (s *Service) stagedChecksum(ctx context.Context, job model.IngestionJob) (string, error) {
	readCtx, cancel := context.WithTimeout(ctx, s.cfg.ParseBudget+s.cfg.StorageTimeout)
	defer cancel()
	body, err := s.deps.Manifests.OpenRange(readCtx, s.deps.Bucket, job.ManifestObjectKey,
		0, job.ManifestBytes-1, interfaces.ReadCondition{IfMatch: job.ManifestETag})
	if err != nil {
		return "", err
	}
	defer body.Close()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, body); err != nil {
		return "", fmt.Errorf("hash staged manifest: %w", err)
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

// Finalize moves a fully read job to READY, so clients can claim its
// entries.
func (s *Service) Finalize(ctx context.Context, p model.Principal, jobID string) (model.IngestionJob, error) {
	if err := s.authorize(ctx, p, model.PermIngestionWrite, "ingestion.finalize", "ingestion_job", jobID); err != nil {
		return model.IngestionJob{}, err
	}
	q, err := s.quota(ctx, p.TenantID)
	if err != nil {
		return model.IngestionJob{}, err
	}
	job, err := s.deps.Repo.Finalize(ctx, p.TenantID, jobID, q.MaxStorageBytes, s.now())
	if err == nil {
		s.record(ctx, p, "ingestion.finalize", "ingestion_job", jobID, map[string]any{"state": string(job.State), "entries": job.Counters.Total})
	}
	return job, err
}

// PageSize is the page size a listing uses for the requested limit.
func PageSize(limit int) int {
	if limit <= 0 || limit > 1000 {
		return 100
	}
	return limit
}

// authorize checks p's permission; a refusal is recorded in the audit log.
func (s *Service) authorize(ctx context.Context, p model.Principal, perm model.Permission, action, resourceType, resourceID string) error {
	return audit.Require(ctx, s.deps.Audit, p, perm, action, resourceType, resourceID)
}

// record audits a change p made.
func (s *Service) record(ctx context.Context, p model.Principal, action, resourceType, resourceID string, details map[string]any) {
	if s.deps.Audit == nil {
		return
	}
	e := audit.From(ctx, p, action, resourceType, resourceID)
	e.Details = details
	s.deps.Audit.Record(ctx, e)
}
