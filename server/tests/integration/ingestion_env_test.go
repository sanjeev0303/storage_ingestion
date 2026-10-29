package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
	memstorage "github.com/sanjeev0303/object-storage-platform/internal/infrastructure/storage/memory"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/config"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/metrics"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/retry"
	"github.com/sanjeev0303/object-storage-platform/internal/service/ingestion"
	"github.com/sanjeev0303/object-storage-platform/internal/service/processing"
	"github.com/sanjeev0303/object-storage-platform/internal/service/promotion"
	uploadservice "github.com/sanjeev0303/object-storage-platform/internal/service/upload"
)

// Bulk ingestion tests run the real ingestion, upload, multipart and
// processing services against PostgreSQL. Storage is the in-memory
// provider, which makes provider failures deterministic; the local-stack
// test repeats the main flow against MinIO and ClamAV.

// infectedMarker makes markerScanner report a file as infected.
const infectedMarker = "INFECTED-TEST-MARKER"

// markerScanner finds infectedMarker, and fails every scan while down is
// set, as a scanner outage would.
type markerScanner struct{ down *atomic.Bool }

func (markerScanner) Name() string               { return "marker" }
func (markerScanner) Ping(context.Context) error { return nil }
func (s markerScanner) Scan(_ context.Context, r io.Reader) interfaces.ScanResult {
	if s.down.Load() {
		return interfaces.ScanResult{Outcome: model.ScanError, Detail: "scanner unavailable"}
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return interfaces.ScanResult{Outcome: model.ScanError, Detail: "read failed"}
	}
	if bytes.Contains(data, []byte(infectedMarker)) {
		return interfaces.ScanResult{Outcome: model.ScanInfected, Signature: "Test.Marker"}
	}
	return interfaces.ScanResult{Outcome: model.ScanClean}
}

func testIngestionConfig() config.Ingestion {
	return config.Ingestion{
		SmallFileThreshold: 1 << 20, MaxObjectBytes: 1 << 30,
		MaxManifestBytes: 64 << 20, MaxInlineManifestBytes: 32 << 20,
		MaxEntriesPerJob: 1_000_000, MaxLineBytes: 8192, MaxPathBytes: 1024, MaxSegmentBytes: 255,
		MaxPathDepth: 32, MaxMetadataBytes: 2048, MaxMetadataKeys: 16,
		TenantStorageQuota: 1 << 40, MaxActiveJobs: 10, MaxPendingEntries: 10_000_000,
		GlobalUploadSlots: 10_000, TenantUploadSlots: 1000, JobUploadSlots: 1000, TenantMultipartSessions: 100,
		MaxPartURLs: 100, MaxClaim: 100, TenantProcessingBacklog: 1_000_000,
		MaxAttempts: 3, MaxManualRetries: 2, InsertBatch: 100, WorkerBatch: 500,
		StorageConcurrency: 8, ParseConcurrency: 2,
		SlotLease: 15 * time.Minute, UploadURLTTL: 10 * time.Minute,
		RetryBackoffBase: time.Millisecond, RetryBackoffMax: 5 * time.Millisecond,
		PollInterval: time.Second, ManifestLease: time.Minute, ParseBudget: time.Minute,
		OutcomeSyncAfter: time.Millisecond, ReconcileInterval: time.Hour, StorageTimeout: 10 * time.Second,
	}
}

type ingestEnv struct {
	*processingEnv
	repo        *postgres.IngestionRepository
	storage     *memstorage.ObjectStorage
	bucket      string
	single      *uploadservice.Service
	multi       *uploadservice.MultipartService
	proc        *processing.Service
	scannerDown *atomic.Bool
	cfg         config.Ingestion
	metrics     *metrics.Registry
	svc         *ingestion.Service
	// p is the caller: the env's tenant, or another one (see as).
	p    model.Principal
	jobs *int
}

func openIngestEnv(t *testing.T, change ...func(*config.Ingestion)) *ingestEnv {
	t.Helper()
	env := openProcessingEnv(t)
	e := &ingestEnv{
		processingEnv: env,
		repo:          postgres.NewIngestionRepository(env.db),
		storage:       memstorage.NewObjectStorage(),
		bucket:        "ingest-it",
		scannerDown:   &atomic.Bool{},
		cfg:           testIngestionConfig(),
		metrics:       metrics.New(),
		p:             member(env.tenant, "ingest-user", model.RoleDeveloper),
		jobs:          new(int),
	}
	for _, c := range change {
		c(&e.cfg)
	}
	e.single = uploadservice.NewService(env.uploads, e.storage, e.bucket, 1<<30, time.Hour)
	var err error
	e.multi, err = uploadservice.NewMultipartService(env.uploads, postgres.NewMultipartUploadRepository(env.db), e.storage,
		uploadservice.MultipartServiceConfig{Bucket: e.bucket, MaxSize: 1 << 30, SessionTTL: time.Hour, PartURLTTL: 5 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	e.proc = e.processing(t)
	e.svc = e.service(t, e.repo)
	e.cleanupTenant(t, env.tenant)
	return e
}

// newTenant returns a caller of another tenant, cleaned up with the test.
func (e *ingestEnv) newTenant(t *testing.T) model.Principal {
	t.Helper()
	tenant := fmt.Sprintf("ingest-other-%d", time.Now().UnixNano())
	e.cleanupTenant(t, tenant)
	return member(tenant, "other-user", model.RoleDeveloper)
}

// as is the env seen by caller p.
func (e *ingestEnv) as(p model.Principal) *ingestEnv {
	c := *e
	c.p = p
	return &c
}

func (e *ingestEnv) cleanupTenant(t *testing.T, tenant string) {
	t.Cleanup(func() {
		if err := cleanTenantData(context.Background(), e.db, []string{tenant}); err != nil {
			t.Errorf("clean tenant %s: %v", tenant, err)
		}
	})
}

// service builds an ingestion service over repo, so a test can wrap the
// repository to inject failures.
func (e *ingestEnv) service(t *testing.T, repo interfaces.IngestionRepository) *ingestion.Service {
	t.Helper()
	svc, err := ingestion.NewService(ingestion.Dependencies{
		Repo: repo, Uploads: e.single, Multipart: e.multi, Sessions: e.uploads, Assets: e.assets, Jobs: e.processingEnv.jobs,
		Requeue: func(ctx context.Context, tenantID, assetID, actor string) error {
			_, err := e.proc.Requeue(ctx, tenantID, assetID, actor)
			return err
		},
		Manifests: e.storage, Bucket: e.bucket,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Metrics: e.metrics,
	}, e.cfg)
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func (e *ingestEnv) processing(t *testing.T) *processing.Service {
	t.Helper()
	promoter, err := promotion.NewService(e.assets, e.storage, promotion.Config{
		SourceBucket: e.bucket, DestinationBucket: e.bucket, Prefix: "published",
	})
	if err != nil {
		t.Fatal(err)
	}
	promoter.Dedup(e.assets)
	svc, err := processing.NewService(processing.Dependencies{
		Jobs: e.processingEnv.jobs, Assets: e.assets, Uploads: e.uploads,
		Quarantine: postgres.NewQuarantineRepository(e.db), Events: postgres.NewProcessingEventRepository(e.db),
		Policies: postgres.NewContentPolicyRepository(e.db), Verifier: e.multi, Storage: e.storage,
		Validators: []interfaces.FileValidator{processing.BasicValidator{}, processing.TypeValidator{}},
		Scanner:    markerScanner{down: e.scannerDown}, Promoter: promoter,
	}, processing.Config{
		Owner: "it-ingest-processing", Bucket: e.bucket, BatchSize: 10, Concurrency: 1, Lease: time.Minute,
		Retry:               retry.Policy{BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond, MaxAttempts: 2},
		Policy:              model.ContentPolicy{MaxSizeBytes: 1 << 30, UnknownTypeAction: "allow"},
		ScanPolicy:          processing.ScanPolicyHold,
		ScanTimeout:         30 * time.Second,
		QuarantineRetention: time.Hour,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

// ─── Building manifests ───────────────────────────────────────────────

// file is one synthetic file of a manifest.
type file struct {
	path        string
	data        []byte
	contentType string
	// extra is appended to the JSON line, e.g. `,"required":false`.
	extra string
}

func textFile(path string, size int) file {
	return file{path: path, data: textData(path, size), contentType: "text/plain"}
}

// infectedFile is a small text file the scanner reports as infected.
func infectedFile(path string) file {
	return file{path: path, data: []byte("payload " + infectedMarker + " " + path + "\n"), contentType: "text/plain"}
}

// series is n text files of size bytes named prefix/NNNN.txt.
func series(prefix string, n, size int) []file {
	files := make([]file, 0, n)
	for i := 1; i <= n; i++ {
		files = append(files, textFile(fmt.Sprintf("%s/%04d.txt", prefix, i), size))
	}
	return files
}

// textData is size bytes of readable text that differ per path, so
// content checks see a text file and every file has its own checksum.
func textData(seed string, size int) []byte {
	unit := []byte("line of " + seed + " text\n")
	return bytes.Repeat(unit, size/len(unit)+1)[:size]
}

func (f file) line() string {
	sum := sha256.Sum256(f.data)
	return fmt.Sprintf(`{"path":%q,"size":%d,"content_type":%q,"checksum":"sha256:%s"%s}`,
		f.path, len(f.data), f.contentType, hex.EncodeToString(sum[:]), f.extra)
}

func manifestOf(files ...file) string {
	var b strings.Builder
	for _, f := range files {
		b.WriteString(f.line() + "\n")
	}
	return b.String()
}

// contents maps each file's path to its bytes.
func contents(files ...file) map[string][]byte {
	out := make(map[string][]byte, len(files))
	for _, f := range files {
		out[f.path] = f.data
	}
	return out
}

func totalSize(files ...file) int64 {
	var n int64
	for _, f := range files {
		n += int64(len(f.data))
	}
	return n
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// ─── Driving the service like a client ────────────────────────────────

func (e *ingestEnv) dataset(t *testing.T, name string, policy model.CompletionPolicy) model.Dataset {
	t.Helper()
	d, _, err := e.svc.CreateDataset(context.Background(), e.p, ingestion.CreateDatasetInput{Name: name, CompletionPolicy: policy})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func (e *ingestEnv) getDataset(t *testing.T, id string) model.Dataset {
	t.Helper()
	d, err := e.svc.GetDataset(context.Background(), e.p, id)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func (e *ingestEnv) newJob(t *testing.T, datasetID string) model.IngestionJob {
	t.Helper()
	*e.jobs++
	job, _, err := e.svc.CreateJob(context.Background(), e.p, ingestion.CreateJobInput{
		DatasetID: datasetID, IdempotencyKey: fmt.Sprintf("job-%d", *e.jobs),
	})
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func (e *ingestEnv) submit(t *testing.T, jobID, manifest string) ingestion.ManifestReport {
	t.Helper()
	report, err := e.svc.SubmitInlineManifest(context.Background(), e.p, jobID, strings.NewReader(manifest), "")
	if err != nil {
		t.Fatalf("submit manifest: %v", err)
	}
	return report
}

// readyJob creates a job in datasetID for files, submits its manifest and
// finalizes it.
func (e *ingestEnv) readyJob(t *testing.T, datasetID string, files ...file) model.IngestionJob {
	t.Helper()
	job := e.newJob(t, datasetID)
	e.submit(t, job.ID, manifestOf(files...))
	job, err := e.svc.Finalize(context.Background(), e.p, job.ID)
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	return job
}

func (e *ingestEnv) claim(t *testing.T, jobID string, max int) ingestion.ClaimResult {
	t.Helper()
	result, err := e.svc.Claim(context.Background(), e.p, jobID, max)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	return result
}

func (e *ingestEnv) session(t *testing.T, uploadID string) *model.UploadSession {
	t.Helper()
	session, err := e.uploads.GetByID(context.Background(), e.p.TenantID, uploadID)
	if err != nil {
		t.Fatal(err)
	}
	return session
}

// put stores the grant's bytes the way a client would: one PUT for a
// SINGLE entry, 5 MiB parts for a MULTIPART one, which are confirmed. It
// returns the parts to complete with.
func (e *ingestEnv) put(t *testing.T, g ingestion.Grant, data []byte) []uploadservice.CompletedPartInput {
	t.Helper()
	session := e.session(t, g.Entry.UploadID)
	if g.Entry.Strategy == model.StrategySingle {
		e.storage.PutObject(session.ObjectKey, data)
		return nil
	}
	const partSize = 5 << 20
	var parts []uploadservice.CompletedPartInput
	var recorded []uploadservice.RecordPartInput
	for i, n := 0, int32(1); i < len(data); i, n = i+partSize, n+1 {
		chunk := data[i:min(i+partSize, len(data))]
		etag := e.storage.PutPart(session.StorageUploadID, n, chunk)
		parts = append(parts, uploadservice.CompletedPartInput{PartNumber: n, ETag: etag})
		recorded = append(recorded, uploadservice.RecordPartInput{PartNumber: n, ETag: etag, SizeBytes: int64(len(chunk))})
	}
	if err := e.svc.ConfirmParts(context.Background(), e.p, g.Entry.JobID, g.Entry.Position, recorded); err != nil {
		t.Fatalf("confirm parts: %v", err)
	}
	return parts
}

// upload puts the grant's bytes and completes the entry.
func (e *ingestEnv) upload(t *testing.T, g ingestion.Grant, data []byte) model.IngestionEntry {
	t.Helper()
	parts := e.put(t, g, data)
	entry, err := e.svc.CompleteEntry(context.Background(), e.p, g.Entry.JobID, g.Entry.Position, parts)
	if err != nil {
		t.Fatalf("complete entry %d: %v", g.Entry.Position, err)
	}
	return entry
}

// process runs processing for an uploaded entry, then delivers its
// upload.processed outcome as the ingestion consumer would.
func (e *ingestEnv) process(t *testing.T, entry model.IngestionEntry) string {
	t.Helper()
	ctx := context.Background()
	if _, err := e.proc.Intake(ctx, e.p.TenantID, entry.UploadID); err != nil {
		t.Fatalf("intake: %v", err)
	}
	note, err := e.svc.OnUploadProcessed(ctx, e.p.TenantID, entry.UploadID)
	if err != nil {
		t.Fatalf("outcome: %v", err)
	}
	return note
}

// ingestAll claims, uploads and processes every entry the job grants,
// with the bytes in data by path.
func (e *ingestEnv) ingestAll(t *testing.T, jobID string, data map[string][]byte) {
	t.Helper()
	for round := 0; round < 100; round++ {
		result := e.claim(t, jobID, 100)
		if len(result.Grants) == 0 {
			return
		}
		for _, g := range result.Grants {
			e.process(t, e.upload(t, g, data[g.Entry.Path]))
		}
	}
	t.Fatal("the job did not run out of entries")
}

func (e *ingestEnv) job(t *testing.T, jobID string) model.IngestionJob {
	t.Helper()
	job, err := e.svc.GetJob(context.Background(), e.p, jobID)
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func (e *ingestEnv) entry(t *testing.T, jobID string, position int64) model.IngestionEntry {
	t.Helper()
	entry, err := e.svc.GetEntry(context.Background(), e.p, jobID, position)
	if err != nil {
		t.Fatal(err)
	}
	return entry
}

// entries reads all of a job's entries, a page at a time.
func (e *ingestEnv) entries(t *testing.T, jobID string, states ...model.EntryState) []model.IngestionEntry {
	t.Helper()
	var all []model.IngestionEntry
	var after int64
	for {
		page, err := e.svc.ListEntries(context.Background(), e.p, jobID, states, after, 1000)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, page...)
		if len(page) < 1000 {
			return all
		}
		after = page[len(page)-1].Position
	}
}

func (e *ingestEnv) usage(t *testing.T) model.TenantUsage {
	t.Helper()
	u, err := e.repo.Usage(context.Background(), e.p.TenantID)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func (e *ingestEnv) slots(t *testing.T) int {
	t.Helper()
	n, err := e.repo.SlotsInUse(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// expireLease makes an in-flight entry's lease overdue, as if its client
// had stopped long ago.
func (e *ingestEnv) expireLease(t *testing.T, jobID string, position int64) {
	t.Helper()
	e.exec(t, `UPDATE ingestion_entries SET lease_expires_at = now() - interval '1 minute'
	           WHERE job_id = $1 AND position = $2 AND state IN ('SCHEDULED', 'UPLOADING')`, jobID, position)
}

// waitForBackoff outlasts the test config's retry backoff (at most 5ms).
func waitForBackoff() { time.Sleep(20 * time.Millisecond) }

func (e *ingestEnv) advanceCancellations(t *testing.T) {
	t.Helper()
	if _, err := e.svc.AdvanceCancellations(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func (e *ingestEnv) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := e.db.Pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatal(err)
	}
}

func (e *ingestEnv) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := e.db.Pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// outboxEvents counts the outbox events of one type for one aggregate.
func (e *ingestEnv) outboxEvents(t *testing.T, eventType, aggregateID string) int {
	t.Helper()
	return e.count(t, `SELECT count(*) FROM outbox_events WHERE event_type = $1 AND aggregate_id = $2`, eventType, aggregateID)
}

// recount returns a job's counters as stored, and as its entries say they
// should be, repairing any difference.
func (e *ingestEnv) recount(t *testing.T, jobID string) (stored, actual model.JobCounters) {
	t.Helper()
	stored, actual, err := e.repo.Recount(context.Background(), jobID, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	return stored, actual
}
