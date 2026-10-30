package adoption

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/dto/event"
	"github.com/sanjeev0303/object-storage-platform/internal/dto/request"
	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/netguard"
	"github.com/sanjeev0303/object-storage-platform/internal/service/audit"
	"github.com/sanjeev0303/object-storage-platform/internal/service/processing"
	uploadservice "github.com/sanjeev0303/object-storage-platform/internal/service/upload"
)

// Store is the adoption repository (postgres.AdoptionRepository).
type Store interface {
	CreateJob(ctx context.Context, j model.AdoptionJob) error
	GetJob(ctx context.Context, tenantID, id string) (model.AdoptionJob, error)
	ListJobs(ctx context.Context, tenantID string, limit int) ([]model.AdoptionJob, error)
	ClaimJob(ctx context.Context, owner string, now time.Time, lease time.Duration) (model.AdoptionJob, bool, error)
	SetState(ctx context.Context, id, owner, state, message string, now time.Time) error
	Release(ctx context.Context, id, owner string) error
	Cancel(ctx context.Context, tenantID, id string, now time.Time) error
	AddEntries(ctx context.Context, entries []model.AdoptionEntry, now time.Time) error
	PendingEntries(ctx context.Context, jobID string, limit int) ([]model.AdoptionEntry, error)
	Entries(ctx context.Context, tenantID, jobID string, after int64, limit int) ([]model.AdoptionEntry, error)
	FinishEntry(ctx context.Context, e model.AdoptionEntry, now time.Time) error
	Count(ctx context.Context, jobID string, now time.Time) (model.AdoptionJob, error)
	AdoptObject(ctx context.Context, o postgres.AdoptedObject) (string, error)
	AdoptedKey(ctx context.Context, tenantID, connectionID, key string) (string, error)
}

// Uploads creates and completes the upload sessions imports go through
// (upload.Service).
type Uploads interface {
	Create(ctx context.Context, in uploadservice.CreateInput) (uploadservice.CreateResult, error)
	Complete(ctx context.Context, tenantID, uploadID string) (model.UploadStatus, error)
}

type Dependencies struct {
	Store  Store
	Router interfaces.StorageRouter
	// Platform storage and bucket hold imports staged on the platform.
	Platform       interfaces.StorageProvider
	PlatformBucket string
	Validators     []interfaces.FileValidator
	// Scanner is nil when no scanner is configured: adoption then refuses
	// to run, as processing would hold every upload.
	Scanner  interfaces.MalwareScanner
	Policies interfaces.ContentPolicyRepository
	Checks   interfaces.ContentCheckRepository
	Uploads  Uploads
	// Net fetches URLs, refusing private and metadata addresses.
	Net   netguard.Policy
	Audit interfaces.AuditRecorder
}

type Config struct {
	Owner            string
	Policy           model.ContentPolicy
	ScanTimeout      time.Duration
	ScanSegmentBytes int64
	// MaxImportBytes bounds one imported file.
	MaxImportBytes int64
	Lease          time.Duration
	Batch          int
	Concurrency    int
	MaxAttempts    int
	TempDir        string
}

type Service struct {
	deps Dependencies
	cfg  Config
	log  *slog.Logger
	now  func() time.Time
}

func NewService(deps Dependencies, cfg Config, log *slog.Logger) *Service {
	if cfg.Lease <= 0 {
		cfg.Lease = 5 * time.Minute
	}
	if cfg.Batch <= 0 {
		cfg.Batch = 50
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 4
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 3
	}
	if cfg.ScanSegmentBytes <= 0 {
		cfg.ScanSegmentBytes = 64 << 20
	}
	if cfg.ScanTimeout <= 0 {
		cfg.ScanTimeout = 2 * time.Minute
	}
	if cfg.MaxImportBytes <= 0 {
		cfg.MaxImportBytes = 5 << 30
	}
	return &Service{deps: deps, cfg: cfg, log: log, now: func() time.Time { return time.Now().UTC() }}
}

// ─── API ──────────────────────────────────────────────────────────────

// CreateInput is a new adoption job.
type CreateInput struct {
	ConnectionID string
	Dataset      string
	Source       string
	// SourceRef: an object key on the connection for MANIFEST,
	// S3_INVENTORY (its manifest.json), MHL and BAGIT (the bag's folder);
	// a URL for STAC. Inline: a MANIFEST or URL_LIST sent with the request.
	SourceRef     string
	Inline        string
	Mode          string
	RequireDigest bool
}

const maxInline = 16 << 20

// CreateFor starts a job. Adopting objects in place needs the connection
// to be one of the organization's own; imports may use the platform.
func (s *Service) CreateFor(ctx context.Context, p model.Principal, in CreateInput) (model.AdoptionJob, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermIngestionWrite, "adoption.create", "adoption", ""); err != nil {
		return model.AdoptionJob{}, err
	}
	in.Source = strings.ToUpper(in.Source)
	if in.Mode == "" {
		in.Mode = model.AdoptionDetach
	}
	in.Mode = strings.ToUpper(in.Mode)
	bad := func(detail string) (model.AdoptionJob, error) {
		return model.AdoptionJob{}, fmt.Errorf("%w: %s", model.ErrInvalidAdoption, detail)
	}
	switch {
	case !model.ValidAdoptionSource(in.Source):
		return bad("source must be MANIFEST, S3_INVENTORY, MHL, BAGIT, URL_LIST or STAC")
	case in.Mode != model.AdoptionDetach && in.Mode != model.AdoptionManage:
		return bad("mode must be DETACH or MANAGE")
	case in.Dataset != "" && !model.ValidDataset(in.Dataset):
		return bad("invalid dataset name")
	case len(in.Inline) > maxInline:
		return bad("an inline source is limited to 16 MiB; put larger ones in the bucket")
	case in.Inline != "" && in.Source != model.AdoptManifest && in.Source != model.AdoptURLList:
		return bad("only MANIFEST and URL_LIST sources may be sent inline")
	case in.Inline == "" && in.SourceRef == "":
		return bad("give the source's object key (or URL), or send it inline")
	case in.Source == model.AdoptSTAC:
		if _, err := s.deps.Net.ValidateURLWithPath(in.SourceRef); err != nil {
			return bad("the STAC URL is not allowed: " + err.Error())
		}
	}
	if in.ConnectionID == "" {
		in.ConnectionID = model.PlatformConnectionID
	}
	if !model.AdoptionImports(in.Source) {
		if model.IsPlatformConnection(in.ConnectionID) {
			return bad("objects are adopted from one of the organization's own storage connections")
		}
		if err := p.Require(model.PermStorageUse); err != nil {
			return model.AdoptionJob{}, err
		}
	}
	if !model.IsPlatformConnection(in.ConnectionID) {
		if s.deps.Router == nil {
			return model.AdoptionJob{}, model.ErrStorageNotConfigured
		}
		if _, err := s.deps.Router.Locate(ctx, p.TenantID, in.ConnectionID); err != nil {
			return model.AdoptionJob{}, err
		}
	}
	now := s.now()
	job := model.AdoptionJob{ID: event.NewID(), TenantID: p.TenantID, ConnectionID: in.ConnectionID, Dataset: in.Dataset,
		Source: in.Source, SourceRef: in.SourceRef, InlineSource: in.Inline, Mode: in.Mode, RequireDigest: in.RequireDigest,
		State: model.AdoptionQueued, CreatedBy: p.UserID, CreatedAt: now, UpdatedAt: now}
	if err := s.deps.Store.CreateJob(ctx, job); err != nil {
		return model.AdoptionJob{}, err
	}
	if s.deps.Audit != nil {
		e := audit.From(ctx, p, "adoption.create", "adoption", job.ID)
		e.Details = map[string]any{"source": job.Source, "connection_id": job.ConnectionID, "mode": job.Mode}
		s.deps.Audit.Record(ctx, e)
	}
	return job, nil
}

func (s *Service) JobsFor(ctx context.Context, p model.Principal) ([]model.AdoptionJob, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermIngestionRead, "adoption.read", "adoption", ""); err != nil {
		return nil, err
	}
	return s.deps.Store.ListJobs(ctx, p.TenantID, 100)
}

func (s *Service) JobFor(ctx context.Context, p model.Principal, id string) (model.AdoptionJob, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermIngestionRead, "adoption.read", "adoption", id); err != nil {
		return model.AdoptionJob{}, err
	}
	return s.deps.Store.GetJob(ctx, p.TenantID, id)
}

// EntriesFor pages through a job's per-entry results.
func (s *Service) EntriesFor(ctx context.Context, p model.Principal, id string, after int64, limit int) ([]model.AdoptionEntry, error) {
	if _, err := s.JobFor(ctx, p, id); err != nil {
		return nil, err
	}
	return s.deps.Store.Entries(ctx, p.TenantID, id, after, min(max(limit, 1), 5000))
}

func (s *Service) CancelFor(ctx context.Context, p model.Principal, id string) error {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermIngestionWrite, "adoption.cancel", "adoption", id); err != nil {
		return err
	}
	return s.deps.Store.Cancel(ctx, p.TenantID, id, s.now())
}

// ─── Runner ───────────────────────────────────────────────────────────

// RunStats counts one cycle's work.
type RunStats struct {
	Jobs     int `json:"jobs"`
	Adopted  int `json:"adopted"`
	Imported int `json:"imported"`
	Rejected int `json:"rejected"`
	Failed   int `json:"failed"`
}

// RunOnce claims one job with work and moves it on: reads its source into
// entries, or processes a batch of its pending entries.
func (s *Service) RunOnce(ctx context.Context) (RunStats, error) {
	var stats RunStats
	if s.deps.Store == nil {
		return stats, nil
	}
	job, ok, err := s.deps.Store.ClaimJob(ctx, s.cfg.Owner, s.now(), s.cfg.Lease)
	if err != nil || !ok {
		return stats, err
	}
	stats.Jobs = 1
	switch job.State {
	case model.AdoptionQueued, model.AdoptionReading:
		err = s.read(ctx, job)
	default:
		err = s.process(ctx, job, &stats)
	}
	if err != nil && !errors.Is(err, model.ErrLeaseLost) {
		s.log.Warn("adoption job failed", "job_id", job.ID, "tenant_id", job.TenantID, "error", err)
		_ = s.deps.Store.SetState(ctx, job.ID, s.cfg.Owner, model.AdoptionFailed, truncate(err.Error(), 500), s.now())
	}
	return stats, nil
}

// read turns the job's source into entries. Reading again after a crash
// stores nothing twice.
func (s *Service) read(ctx context.Context, job model.AdoptionJob) error {
	if err := s.deps.Store.SetState(ctx, job.ID, s.cfg.Owner, model.AdoptionReading, "", s.now()); err != nil {
		return err
	}
	var batch []model.AdoptionEntry
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		err := s.deps.Store.AddEntries(ctx, batch, s.now())
		batch = batch[:0]
		return err
	}
	e := &emitter{emit: func(entry model.AdoptionEntry) error {
		entry.JobID, entry.TenantID = job.ID, job.TenantID
		if entry.Path != "" && job.Source != model.AdoptURLList && job.Source != model.AdoptSTAC && job.Dataset == "" {
			entry.Path = strings.TrimPrefix(entry.Path, "/")
		}
		batch = append(batch, entry)
		if len(batch) >= 1000 {
			return flush()
		}
		return nil
	}}

	reader, bucket, err := s.reader(ctx, job)
	if err != nil {
		return err
	}
	switch job.Source {
	case model.AdoptManifest, model.AdoptURLList:
		var src io.ReadCloser = io.NopCloser(strings.NewReader(job.InlineSource))
		if job.InlineSource == "" {
			if src, err = reader.Open(ctx, job.SourceRef); err != nil {
				return fmt.Errorf("open the source: %w", err)
			}
		}
		err = readLines(src, job.Source == model.AdoptURLList, e)
		src.Close()
	case model.AdoptS3Inventory:
		err = readInventory(ctx, reader, job.SourceRef, bucket, e)
	case model.AdoptMHL:
		err = readMHL(ctx, reader, job.SourceRef, e)
	case model.AdoptBagIt:
		err = readBag(ctx, reader, job.SourceRef, e)
	case model.AdoptSTAC:
		err = readSTAC(ctx, jsonFetcher{s.deps.Net.Client()}, job.SourceRef, e)
	}
	if err != nil {
		return err
	}
	if err := flush(); err != nil {
		return err
	}
	if _, err := s.deps.Store.Count(ctx, job.ID, s.now()); err != nil {
		return err
	}
	if err := s.deps.Store.SetState(ctx, job.ID, s.cfg.Owner, model.AdoptionRunning, "", s.now()); err != nil {
		return err
	}
	return s.deps.Store.Release(ctx, job.ID, s.cfg.Owner)
}

// connReader opens objects in the connection's bucket.
type connReader struct {
	storage interfaces.MultipartStorage
	bucket  string
}

func (c connReader) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	return c.storage.OpenObject(ctx, c.bucket, key)
}

// place is the job's connection: its storage, the bucket adopted objects
// are in (where reads are served from) and the prefixes the platform owns.
func (s *Service) place(ctx context.Context, job model.AdoptionJob) (interfaces.StorageLocation, error) {
	if model.IsPlatformConnection(job.ConnectionID) {
		return interfaces.StorageLocation{Storage: s.deps.Platform, StoragePlace: interfaces.StoragePlace{
			ConnectionID: model.PlatformConnectionID, ReadyBucket: s.deps.PlatformBucket, StagingBucket: s.deps.PlatformBucket}}, nil
	}
	if s.deps.Router == nil {
		return interfaces.StorageLocation{}, model.ErrStorageNotConfigured
	}
	return s.deps.Router.Locate(ctx, job.TenantID, job.ConnectionID)
}

func (s *Service) reader(ctx context.Context, job model.AdoptionJob) (objectReader, string, error) {
	loc, err := s.place(ctx, job)
	if err != nil {
		return nil, "", err
	}
	return connReader{storage: loc.Storage, bucket: loc.ReadyBucket}, loc.ReadyBucket, nil
}

// process works through a batch of pending entries, Concurrency at a time,
// and completes the job when none is left.
func (s *Service) process(ctx context.Context, job model.AdoptionJob, stats *RunStats) error {
	if !model.AdoptionImports(job.Source) && s.deps.Scanner == nil {
		return errors.New("no malware scanner is configured: adopted objects could not be scanned")
	}
	entries, err := s.deps.Store.PendingEntries(ctx, job.ID, s.cfg.Batch)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		if _, err := s.deps.Store.Count(ctx, job.ID, s.now()); err != nil {
			return err
		}
		return s.deps.Store.SetState(ctx, job.ID, s.cfg.Owner, model.AdoptionCompleted, "", s.now())
	}
	loc, err := s.place(ctx, job)
	if err != nil {
		return err
	}
	policy, err := processing.PolicyFor(ctx, s.cfg.Policy, s.deps.Policies, job.TenantID)
	if err != nil {
		return err
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	slots := make(chan struct{}, s.cfg.Concurrency)
	for _, entry := range entries {
		slots <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			var done model.AdoptionEntry
			if model.AdoptionImports(job.Source) {
				done = s.importURL(ctx, job, loc, entry)
			} else {
				done = s.adopt(ctx, job, loc, policy, entry)
			}
			if done.State == model.AdoptFailed && done.Attempts < s.cfg.MaxAttempts {
				done.State = model.AdoptPending // tried again next cycle
			}
			if err := s.deps.Store.FinishEntry(ctx, done, s.now()); err != nil {
				s.log.Warn("record adoption entry", "job_id", job.ID, "line", entry.Line, "error", err)
			}
			mu.Lock()
			defer mu.Unlock()
			switch done.State {
			case model.AdoptAdopted:
				stats.Adopted++
			case model.AdoptImported:
				stats.Imported++
			case model.AdoptRejected:
				stats.Rejected++
			case model.AdoptFailed:
				stats.Failed++
			}
		}()
	}
	wg.Wait()
	if _, err := s.deps.Store.Count(ctx, job.ID, s.now()); err != nil {
		return err
	}
	return s.deps.Store.Release(ctx, job.ID, s.cfg.Owner)
}

func reject(e model.AdoptionEntry, code, detail string) model.AdoptionEntry {
	e.State, e.Code, e.Detail = model.AdoptRejected, code, detail
	return e
}

func fail(e model.AdoptionEntry, detail string) model.AdoptionEntry {
	e.State, e.Code, e.Detail = model.AdoptFailed, "read_failed", detail
	e.Attempts++
	return e
}

// adopt checks one object where it is and publishes it in place: its
// size, every expected digest, the content checks and the malware scan,
// on the exact version read.
func (s *Service) adopt(ctx context.Context, job model.AdoptionJob, loc interfaces.StorageLocation, policy model.ContentPolicy, e model.AdoptionEntry) model.AdoptionEntry {
	key := e.ObjectKey
	switch {
	case key == "" || strings.HasPrefix(key, "/"):
		return reject(e, "invalid_key", "")
	case owned(loc.StoragePlace, key):
		return reject(e, "platform_key", "the key is under a prefix the platform manages")
	case job.RequireDigest && len(e.Expected) == 0:
		return reject(e, "digest_required", "no expected digest; set require_digest false to adopt by computed SHA-256 alone")
	}
	if existing, err := s.deps.Store.AdoptedKey(ctx, job.TenantID, job.ConnectionID, key); err != nil {
		return fail(e, err.Error())
	} else if existing != "" {
		e.State, e.Code, e.AssetID = model.AdoptSkipped, "already_adopted", existing
		return e
	}
	meta, err := loc.Storage.HeadObject(ctx, loc.ReadyBucket, key)
	if errors.Is(err, interfaces.ErrObjectNotFound) {
		return reject(e, "not_found", "no such object")
	}
	if err != nil {
		return fail(e, err.Error())
	}
	if e.SizeBytes > 0 && meta.SizeBytes != e.SizeBytes {
		return reject(e, "size_mismatch", fmt.Sprintf("stored %d bytes, expected %d", meta.SizeBytes, e.SizeBytes))
	}
	if meta.SizeBytes <= 0 {
		return reject(e, "empty_file", "")
	}
	if policy.MaxSizeBytes > 0 && meta.SizeBytes > policy.MaxSizeBytes {
		return reject(e, model.RejectFileTooLarge, "")
	}
	e.SizeBytes = meta.SizeBytes
	delivery, ok := loc.Storage.(interfaces.ObjectDelivery)
	if !ok {
		return fail(e, "the connection cannot serve ranged reads")
	}
	// Every read below is of exactly the version measured.
	cond := interfaces.ReadCondition{IfMatch: meta.ETag, VersionID: meta.VersionID}
	open := func(ctx context.Context) (io.ReadCloser, error) {
		return delivery.OpenRange(ctx, loc.ReadyBucket, key, 0, -1, cond)
	}

	// One pass digests the bytes while the scanner reads them.
	digests := newDigester(e.Expected)
	scan, err := s.scan(ctx, delivery, loc.ReadyBucket, key, meta.SizeBytes, cond, digests)
	if err != nil {
		return fail(e, err.Error())
	}
	computed := digests.Digests()
	e.SHA256 = computed["sha256"]
	if bad := mismatch(e.Expected, computed); len(bad) > 0 {
		return reject(e, "digest_mismatch", "the stored bytes do not match the expected "+strings.Join(bad, ", "))
	}
	switch scan.Outcome {
	case model.ScanClean:
	case model.ScanInfected:
		return reject(e, model.RejectMalwareDetected, "found by "+s.deps.Scanner.Name())
	default:
		return fail(e, "scan "+scan.Outcome+": "+scan.Detail)
	}

	head, err := readHead(ctx, open)
	if err != nil {
		return fail(e, err.Error())
	}
	fileName := path.Base(e.Path)
	contentType := e.ContentType
	if contentType == "" {
		contentType = meta.ContentType
	}
	sample := interfaces.ContentSample{Head: head, DeclaredType: contentType, FileName: fileName, SizeBytes: meta.SizeBytes, Open: open}
	result, err := processing.RunValidators(ctx, s.deps.Validators, sample, policy)
	if err != nil {
		return fail(e, err.Error())
	}
	if result.RejectReason != "" {
		return reject(e, result.RejectReason, result.Detail)
	}
	if contentType == "" || contentType == "binary/octet-stream" {
		contentType = result.DetectedType
	}
	coverage := "FULL"
	if len(result.Notes) > 0 {
		coverage = "PARTIAL"
	}
	version := ""
	if versioned, ok := s.deps.Scanner.(interfaces.VersionedScanner); ok {
		version, _ = versioned.Version(ctx)
	}
	assetID, err := s.deps.Store.AdoptObject(ctx, postgres.AdoptedObject{
		TenantID: job.TenantID, OwnerID: job.CreatedBy, ConnectionID: job.ConnectionID, Dataset: job.Dataset,
		Path: e.Path, ObjectKey: key, FileName: fileName, ContentType: contentType, SizeBytes: meta.SizeBytes,
		SHA256: e.SHA256, ETag: meta.ETag, VersionID: meta.VersionID, Metadata: e.Metadata, Mode: job.Mode,
		Digests: computed, DetectedType: result.DetectedType, Scanner: s.deps.Scanner.Name(), ScannerVersion: version,
		Coverage: coverage, At: s.now(),
	})
	if errors.Is(err, model.ErrPathExists) || errors.Is(err, model.ErrPathBusy) {
		return reject(e, "path_conflict", err.Error())
	}
	if err != nil {
		return fail(e, err.Error())
	}
	if s.deps.Checks != nil && len(result.Findings) > 0 {
		_ = s.deps.Checks.ReplaceFindings(ctx, job.TenantID, assetID, "", result.Findings, s.now())
		codes := make([]string, len(result.Findings))
		for i, f := range result.Findings {
			codes[i] = f.Code
		}
		if class := model.ClassForFindings(codes); class != "" {
			_ = s.deps.Checks.RaiseClassification(ctx, job.TenantID, assetID, class)
		}
	}
	e.State, e.Code, e.Detail, e.AssetID = model.AdoptAdopted, "", "", assetID
	return e
}

// owned reports whether key is under a prefix the platform writes.
func owned(p interfaces.StoragePlace, key string) bool {
	for _, prefix := range []string{p.ReadyPrefix, p.StagingPrefix, "quarantine", "manifests"} {
		if prefix != "" && strings.HasPrefix(key, strings.TrimSuffix(prefix, "/")+"/") {
			return true
		}
	}
	return false
}

func readHead(ctx context.Context, open func(context.Context) (io.ReadCloser, error)) ([]byte, error) {
	body, err := open(ctx)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	return io.ReadAll(io.LimitReader(body, 512))
}

// scan reads the whole version once, into the digests and the scanner. An
// object larger than one scanner stream is scanned as overlapping segments,
// with the digests taken from one more full read.
func (s *Service) scan(ctx context.Context, r interfaces.ObjectDelivery, bucket, key string, size int64,
	cond interfaces.ReadCondition, digests io.Writer) (interfaces.ScanResult, error) {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.ScanTimeout+time.Duration(size/(16<<20))*time.Second)
	defer cancel()
	if size <= s.cfg.ScanSegmentBytes {
		body, err := r.OpenRange(ctx, bucket, key, 0, -1, cond)
		if err != nil {
			return interfaces.ScanResult{}, err
		}
		defer body.Close()
		counted := &countWriter{}
		result := s.deps.Scanner.Scan(ctx, io.TeeReader(body, io.MultiWriter(digests, counted)))
		if result.Outcome == model.ScanClean && counted.n != size {
			return interfaces.ScanResult{}, errors.New("the scanner did not read the whole object")
		}
		return result, nil
	}
	const overlap = 1 << 20
	for start := int64(0); start < size; start += s.cfg.ScanSegmentBytes {
		first := max(start-overlap, 0)
		last := min(start+s.cfg.ScanSegmentBytes, size) - 1
		body, err := r.OpenRange(ctx, bucket, key, first, last, cond)
		if err != nil {
			return interfaces.ScanResult{}, err
		}
		result := s.deps.Scanner.Scan(ctx, body)
		body.Close()
		if result.Outcome != model.ScanClean {
			return result, nil
		}
	}
	body, err := r.OpenRange(ctx, bucket, key, 0, -1, cond)
	if err != nil {
		return interfaces.ScanResult{}, err
	}
	defer body.Close()
	n, err := io.Copy(digests, body)
	if err != nil || n != size {
		return interfaces.ScanResult{}, fmt.Errorf("read the whole object: %v", err)
	}
	return interfaces.ScanResult{Outcome: model.ScanClean}, nil
}

type countWriter struct{ n int64 }

func (c *countWriter) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return len(p), nil
}

// importURL fetches one URL, checks it against its expected digests, and
// uploads it through an ordinary upload session: the processing pipeline
// then verifies, checks, scans and publishes it like any upload.
func (s *Service) importURL(ctx context.Context, job model.AdoptionJob, loc interfaces.StorageLocation, e model.AdoptionEntry) model.AdoptionEntry {
	if e.UploadID != "" {
		e.State = model.AdoptImported // completed before a crash
		return e
	}
	if _, err := s.deps.Net.ValidateURLWithPath(e.URL); err != nil {
		return reject(e, "url_not_allowed", err.Error())
	}
	spool, err := os.CreateTemp(s.cfg.TempDir, "import-*")
	if err != nil {
		return fail(e, err.Error())
	}
	defer func() {
		spool.Close()
		os.Remove(spool.Name())
	}()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.URL, nil)
	if err != nil {
		return reject(e, "invalid_url", err.Error())
	}
	resp, err := s.deps.Net.Client().Do(req)
	if err != nil {
		return fail(e, err.Error())
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
			return reject(e, "not_found", resp.Status)
		}
		return fail(e, resp.Status)
	}
	digests := newDigester(e.Expected)
	n, err := io.Copy(io.MultiWriter(spool, digests), io.LimitReader(resp.Body, s.cfg.MaxImportBytes+1))
	if err != nil {
		return fail(e, err.Error())
	}
	if n > s.cfg.MaxImportBytes {
		return reject(e, model.RejectFileTooLarge, "")
	}
	if n == 0 {
		return reject(e, "empty_file", "")
	}
	if e.SizeBytes > 0 && n != e.SizeBytes {
		return reject(e, "size_mismatch", fmt.Sprintf("fetched %d bytes, expected %d", n, e.SizeBytes))
	}
	computed := digests.Digests()
	if bad := mismatch(e.Expected, computed); len(bad) > 0 {
		return reject(e, "digest_mismatch", "the fetched bytes do not match the expected "+strings.Join(bad, ", "))
	}
	e.SizeBytes, e.SHA256 = n, computed["sha256"]
	contentType := e.ContentType
	if contentType == "" {
		contentType = resp.Header.Get("Content-Type")
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	created, err := s.deps.Uploads.Create(ctx, uploadservice.CreateInput{TenantID: job.TenantID, OwnerID: job.CreatedBy, Dataset: job.Dataset,
		Request: request.CreateUploadRequest{FileName: path.Base(e.Path), ContentType: contentType, SizeBytes: n,
			ChecksumSHA256: e.SHA256, IdempotencyKey: fmt.Sprintf("adoption-%s-%d", job.ID, e.Line), Dataset: job.Dataset,
			Path: e.Path, Metadata: e.Metadata, StorageConnectionID: connectionFor(job)}})
	if err != nil {
		return reject(e, "upload_refused", err.Error())
	}
	session := created.Upload
	if _, err := spool.Seek(0, io.SeekStart); err != nil {
		return fail(e, err.Error())
	}
	if err := s.write(ctx, job, session, spool, n, contentType); err != nil {
		return fail(e, err.Error())
	}
	if _, err := s.deps.Uploads.Complete(ctx, job.TenantID, session.ID); err != nil {
		return fail(e, "complete the upload: "+err.Error())
	}
	e.State, e.UploadID, e.Code, e.Detail = model.AdoptImported, session.ID, "", ""
	return e
}

func connectionFor(job model.AdoptionJob) string {
	if model.IsPlatformConnection(job.ConnectionID) {
		return ""
	}
	return job.ConnectionID
}

// write puts the bytes where the upload session expects them.
func (s *Service) write(ctx context.Context, job model.AdoptionJob, session *model.UploadSession, body io.Reader, size int64, contentType string) error {
	if model.IsPlatformConnection(session.StagingConnectionID) {
		_, err := s.deps.Platform.WriteObject(ctx, s.deps.PlatformBucket, session.ObjectKey, body, size, contentType)
		return err
	}
	loc, err := s.deps.Router.Locate(ctx, job.TenantID, session.StagingConnectionID)
	if err != nil {
		return err
	}
	_, err = loc.Storage.WriteObject(ctx, loc.StagingBucket, session.ObjectKey, body, size, contentType)
	return err
}

type jsonFetcher struct{ client *http.Client }

func (f jsonFetcher) JSON(ctx context.Context, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json, application/geo+json")
	resp, err := f.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %s", url, resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(out)
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
