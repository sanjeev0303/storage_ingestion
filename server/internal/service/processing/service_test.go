package processing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	storagememory "github.com/sanjeev0303/object-storage-platform/internal/infrastructure/storage/memory"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/retry"
	"github.com/sanjeev0303/object-storage-platform/internal/repository/memory"
	"github.com/sanjeev0303/object-storage-platform/internal/service/promotion"
	uploadservice "github.com/sanjeev0303/object-storage-platform/internal/service/upload"
)

const tenant = "tenant-1"

const retention = 24 * time.Hour

var quietLog = slog.New(slog.NewTextHandler(io.Discard, nil))

// fakeScanner answers with the queued results, then CLEAN, after running
// during(). An error from during() is reported as a TIMEOUT when the scan
// deadline passed and as an ERROR otherwise, as the ClamAV adapter does.
type fakeScanner struct {
	mu      sync.Mutex
	results []interfaces.ScanResult
	pingErr error
	calls   int
	during  func(ctx context.Context) error
	onPing  func()
	partial int64 // when set, read only this many bytes before answering
}

func (f *fakeScanner) Name() string { return "fake" }

func (f *fakeScanner) Ping(context.Context) error {
	if f.onPing != nil {
		f.onPing()
	}
	return f.pingErr
}

func (f *fakeScanner) Scan(ctx context.Context, content io.Reader) interfaces.ScanResult {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	if f.partial > 0 {
		content = io.LimitReader(content, f.partial)
	}
	if _, err := io.Copy(io.Discard, content); err != nil {
		return interfaces.ScanResult{Outcome: model.ScanError, Detail: "read failed"}
	}
	if f.during != nil {
		if err := f.during(ctx); err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return interfaces.ScanResult{Outcome: model.ScanTimeout, Detail: "timed out"}
			}
			return interfaces.ScanResult{Outcome: model.ScanError, Detail: "failed"}
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.results) > 0 {
		result := f.results[0]
		f.results = f.results[1:]
		return result
	}
	return interfaces.ScanResult{Outcome: model.ScanClean}
}

type harness struct {
	uploads  *memory.UploadRepository
	assets   interfaces.AssetRepository
	jobs     interfaces.ProcessingJobRepository
	memJobs  *memory.ProcessingJobRepository
	memAsset *memory.AssetRepository
	storage  *storagememory.ObjectStorage
	scanner  *fakeScanner
	held     *memory.QuarantineRepository
	events   *memory.ProcessingEventRepository
	policies *memory.ContentPolicyRepository
	clock    time.Time
	policy   ScanPolicy
	noScan   bool
	attempts int
	lease    time.Duration
}

func newHarness() *harness {
	uploads := memory.NewUploadRepository()
	assets := memory.NewAssetRepository()
	jobs := memory.NewProcessingJobRepository(uploads, assets)
	return &harness{
		uploads: uploads, assets: assets, memAsset: assets, jobs: jobs, memJobs: jobs,
		storage:  storagememory.NewObjectStorage(),
		scanner:  &fakeScanner{},
		held:     memory.NewQuarantineRepository(),
		events:   memory.NewProcessingEventRepository(),
		policies: memory.NewContentPolicyRepository(),
		clock:    time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC),
		policy:   ScanPolicyHold,
		attempts: 3,
		lease:    5 * time.Minute,
	}
}

func (h *harness) service(t *testing.T, owner string) *Service {
	t.Helper()
	multipart, err := uploadservice.NewMultipartService(h.uploads, memory.NewMultipartUploadRepository(), h.storage,
		uploadservice.MultipartServiceConfig{Bucket: "bucket", MaxSize: 1 << 30, SessionTTL: time.Hour, PartURLTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	promoter, err := promotion.NewService(h.assets, h.storage, promotion.Config{
		SourceBucket: "bucket", DestinationBucket: "bucket", Prefix: "assets",
	})
	if err != nil {
		t.Fatal(err)
	}

	deps := Dependencies{
		Jobs: h.jobs, Assets: h.assets, Uploads: h.uploads, Verifier: multipart, Storage: h.storage,
		Quarantine: h.held, Events: h.events, Policies: h.policies,
		Validators: []interfaces.FileValidator{BasicValidator{}, TypeValidator{}},
		Promoter:   promoter,
	}
	if !h.noScan {
		deps.Scanner = h.scanner
	}
	svc, err := NewService(deps, Config{
		Owner: owner, Bucket: "bucket", BatchSize: 10, Concurrency: 2, Lease: h.lease,
		Policy:     model.ContentPolicy{MaxSizeBytes: 1 << 20, UnknownTypeAction: "allow"},
		ScanPolicy: h.policy, ScanTimeout: h.lease / 2, QuarantineRetention: retention,
		Retry: retry.Policy{BaseDelay: time.Minute, MaxDelay: 10 * time.Minute, MaxAttempts: h.attempts},
	}, quietLog)
	if err != nil {
		t.Fatal(err)
	}
	svc.now = func() time.Time { return h.clock }
	return svc
}

// completedUpload stores bytes as a finished multipart upload would and
// records a COMPLETED session declaring checksum and size.
func (h *harness) completedUpload(t *testing.T, id string, stored []byte, declaredSum string, declaredSize int64, contentType string) {
	t.Helper()
	key := "quarantine/" + tenant + "/" + id
	if stored != nil {
		h.storage.PutObject(key, stored)
	}
	session := model.UploadSession{
		ID: id, TenantID: tenant, OwnerID: "user-1", ObjectKey: key, FileName: "file", ContentType: contentType,
		ExpectedSize: declaredSize, ChecksumSHA256: declaredSum, Status: model.UploadStatusCompleted,
		StorageUploadID: "provider-" + id, CreatedAt: h.clock, UpdatedAt: h.clock, ExpiresAt: h.clock.Add(time.Hour),
	}
	if err := h.uploads.Create(context.Background(), &session); err != nil {
		t.Fatal(err)
	}
}

func sum(data []byte) string {
	s := sha256.Sum256(data)
	return hex.EncodeToString(s[:])
}

var pdfBytes = []byte("%PDF-1.7\n" + strings.Repeat("x", 600))

// exeBytes starts like a Windows PE file; it is never executed.
var exeBytes = []byte("MZ" + strings.Repeat("\x00", 60))

func (h *harness) goodUpload(t *testing.T, id string) {
	h.completedUpload(t, id, pdfBytes, sum(pdfBytes), int64(len(pdfBytes)), "application/pdf")
}

func (h *harness) onlyAsset(t *testing.T) model.Asset {
	t.Helper()
	jobs := h.memJobs.Jobs()
	if len(jobs) != 1 {
		t.Fatalf("expected one job, got %d", len(jobs))
	}
	asset, err := h.assets.GetByID(context.Background(), tenant, jobs[0].AssetID)
	if err != nil {
		t.Fatal(err)
	}
	return *asset
}

func (h *harness) onlyJob(t *testing.T) model.ProcessingJob {
	t.Helper()
	jobs := h.memJobs.Jobs()
	if len(jobs) != 1 {
		t.Fatalf("expected one job, got %d", len(jobs))
	}
	return jobs[0]
}

// onlyRecord returns the asset's single quarantine record.
func (h *harness) onlyRecord(t *testing.T, assetID string) model.QuarantineRecord {
	t.Helper()
	records, err := h.held.ListForAsset(context.Background(), tenant, assetID)
	if err != nil || len(records) != 1 {
		t.Fatalf("expected one quarantine record, got %d (%v)", len(records), err)
	}
	return records[0]
}

func (h *harness) eventNames(t *testing.T, assetID string) string {
	t.Helper()
	events, err := h.events.ListForAsset(context.Background(), tenant, assetID)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range events {
		names = append(names, e.Event)
	}
	return strings.Join(names, ",")
}

func (h *harness) run(t *testing.T, svc *Service) CycleStats {
	t.Helper()
	stats, err := svc.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return stats
}

// ─── Happy path and ordering ─────────────────────────────────────────

func TestCleanUploadIsVerifiedScannedAndPromoted(t *testing.T) {
	h := newHarness()
	h.goodUpload(t, "up-1")
	svc := h.service(t, "worker-a")

	stats := h.run(t, svc)
	if stats.Succeeded != 1 {
		t.Fatalf("stats %+v", stats)
	}

	asset := h.onlyAsset(t)
	job := h.onlyJob(t)
	if asset.Status != model.AssetStatusAvailable || asset.ObjectKey != "assets/"+tenant+"/"+asset.ID {
		t.Fatalf("expected AVAILABLE under the delivery key, got %s at %s", asset.Status, asset.ObjectKey)
	}
	if job.State != model.ProcessingSucceeded || job.ChecksumAlgorithm != "SHA-256" || job.ChecksumVerifiedAt == nil ||
		job.ScanResult != model.ScanClean || job.Scanner != "fake" || job.PromotedAt == nil || job.DetectedContentType != "application/pdf" ||
		job.VerificationStatus != model.StagePassed || job.ChecksumSource != model.ChecksumDeclared ||
		job.ValidationStatus != model.StagePassed || job.PromotionStatus != model.PromotionPublished {
		t.Fatalf("stage results not recorded: %+v", job)
	}
	if got := h.eventNames(t, asset.ID); got != "verified,scanned,promoted" {
		t.Fatalf("unexpected audit events %q", got)
	}
	if got, ok := h.storage.Object(asset.ObjectKey); !ok || string(got) != string(pdfBytes) {
		t.Fatal("the delivery copy is missing or wrong")
	}
	if _, ok := h.storage.Object("quarantine/" + tenant + "/up-1"); ok {
		t.Fatal("the quarantine source must be removed after promotion")
	}
	session, _ := h.uploads.GetByID(context.Background(), tenant, "up-1")
	if session.Status != model.UploadStatusVerified {
		t.Fatalf("expected the upload VERIFIED, got %s", session.Status)
	}
}

func TestCompletionAloneNeverMakesAnAssetAvailable(t *testing.T) {
	h := newHarness()
	h.goodUpload(t, "up-1")
	if _, err := h.jobs.EnqueueDue(context.Background(), h.clock, 3); err != nil {
		t.Fatal(err)
	}
	if got := h.onlyAsset(t); got.Status != model.AssetStatusQuarantined {
		t.Fatalf("a completed upload must start QUARANTINED, got %s", got.Status)
	}
}

func TestDuplicateEventsCreateOneAssetAndOneJob(t *testing.T) {
	h := newHarness()
	h.goodUpload(t, "up-1")
	for i := 0; i < 3; i++ {
		if _, err := h.jobs.EnqueueDue(context.Background(), h.clock, 3); err != nil {
			t.Fatal(err)
		}
	}
	h.onlyJob(t)

	// A repeated run after success changes nothing.
	svc := h.service(t, "worker-a")
	h.run(t, svc)
	stats := h.run(t, svc)
	if stats.Enqueued != 0 || stats.Claimed != 0 || h.storage.CopyCalls != 1 {
		t.Fatalf("a finished job ran again: %+v, copies %d", stats, h.storage.CopyCalls)
	}
}

// ─── Verification ────────────────────────────────────────────────────

func TestVerificationRejects(t *testing.T) {
	cases := []struct {
		name     string
		stored   []byte
		sum      string
		size     int64
		ctype    string
		reason   string
		category string
	}{
		{"checksum mismatch", pdfBytes, sum([]byte("other")), int64(len(pdfBytes)), "application/pdf", model.FailureChecksumMismatch, model.QuarantineVerificationFailed},
		{"wrong size", pdfBytes, sum(pdfBytes), int64(len(pdfBytes)) + 1, "application/pdf", model.FailureObjectSizeMismatch, model.QuarantineVerificationFailed},
		{"missing object", nil, sum(pdfBytes), int64(len(pdfBytes)), "application/pdf", model.FailureUploadMissing, model.QuarantineVerificationFailed},
		{"type contradicts bytes", pdfBytes, sum(pdfBytes), int64(len(pdfBytes)), "video/mp4", model.RejectContentMismatch, model.QuarantineValidationRejected},
		{"html is never accepted", []byte("<!DOCTYPE html><html></html>"), sum([]byte("<!DOCTYPE html><html></html>")), 28, "text/plain", model.RejectContentType, model.QuarantineValidationRejected},
		{"executable is never accepted", exeBytes, sum(exeBytes), int64(len(exeBytes)), "application/pdf", model.RejectContentType, model.QuarantineValidationRejected},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness()
			h.completedUpload(t, "up-1", c.stored, c.sum, c.size, c.ctype)
			h.run(t, h.service(t, "worker-a"))

			asset := h.onlyAsset(t)
			if asset.Status != model.AssetStatusRejected || asset.RejectionReason != c.reason {
				t.Fatalf("expected REJECTED/%s, got %s/%s", c.reason, asset.Status, asset.RejectionReason)
			}
			if job := h.onlyJob(t); job.State != model.ProcessingFailed || job.ScanResult != "" {
				t.Fatalf("a rejected asset must not be scanned or promoted: %+v", job)
			}
			if h.storage.CopyCalls != 0 {
				t.Fatal("a rejected asset must never be copied out of quarantine")
			}
			record := h.onlyRecord(t, asset.ID)
			if record.Status != model.QuarantineRejected || record.Category != c.category || record.Reason != c.reason ||
				record.RetainUntil == nil || !record.RetainUntil.Equal(h.clock.Add(retention)) {
				t.Fatalf("quarantine record not written: %+v", record)
			}
		})
	}
}

func TestOversizedFileIsRejectedBeforeReading(t *testing.T) {
	h := newHarness()
	big := make([]byte, (1<<20)+1)
	h.completedUpload(t, "up-1", big, sum(big), int64(len(big)), "application/octet-stream")
	h.run(t, h.service(t, "worker-a"))
	if asset := h.onlyAsset(t); asset.RejectionReason != model.RejectFileTooLarge {
		t.Fatalf("expected file_too_large, got %s/%s", asset.Status, asset.RejectionReason)
	}
}

// ─── Scanning ────────────────────────────────────────────────────────

// Bytes swapped in quarantine after verification are never published as
// scanned: here a clean object is scanned in place of the verified one,
// which is put back before the copy.
func TestContentSwappedAfterVerificationIsNotPromoted(t *testing.T) {
	h := newHarness()
	h.goodUpload(t, "up-1")
	key := "quarantine/" + tenant + "/up-1"
	scanned := []byte("%PDF-1.7\n" + strings.Repeat("y", 600))
	h.scanner.onPing = func() { h.storage.PutObject(key, scanned) }
	h.scanner.during = func(context.Context) error { h.storage.PutObject(key, pdfBytes); return nil }
	h.run(t, h.service(t, "worker-a"))

	asset := h.onlyAsset(t)
	if asset.Status != model.AssetStatusRejected || asset.RejectionReason != model.FailureChecksumMismatch {
		t.Fatalf("expected REJECTED/%s, got %s/%s", model.FailureChecksumMismatch, asset.Status, asset.RejectionReason)
	}
	if h.storage.CopyCalls != 0 {
		t.Fatal("bytes the scanner did not see must never be copied out of quarantine")
	}
}

// A scanner that answers CLEAN without reading the whole object has not
// scanned it: that is an incomplete scan, retried and finally held.
func TestAScanThatStopsEarlyIsIncomplete(t *testing.T) {
	h := newHarness()
	h.goodUpload(t, "up-1")
	h.scanner.partial = 10
	h.run(t, h.service(t, "worker-a"))

	if asset := h.onlyAsset(t); asset.Status == model.AssetStatusAvailable {
		t.Fatal("an object scanned only in part must not be promoted")
	}
	if job := h.onlyJob(t); job.ScanResult != model.ScanError || h.storage.CopyCalls != 0 {
		t.Fatalf("expected an incomplete scan and no copy, got %s (copies %d)", job.ScanResult, h.storage.CopyCalls)
	}
}

func TestMalwareIsRejectedAndRecorded(t *testing.T) {
	h := newHarness()
	h.goodUpload(t, "up-1")
	h.scanner.results = []interfaces.ScanResult{{Outcome: model.ScanInfected, Signature: "Eicar-Signature"}}
	h.run(t, h.service(t, "worker-a"))

	asset, job := h.onlyAsset(t), h.onlyJob(t)
	if asset.Status != model.AssetStatusRejected || asset.RejectionReason != model.RejectMalwareDetected {
		t.Fatalf("expected malware rejection, got %s/%s", asset.Status, asset.RejectionReason)
	}
	if job.ScanResult != model.ScanInfected || job.ScanSignature != "Eicar-Signature" || job.State != model.ProcessingFailed {
		t.Fatalf("scan result not recorded: %+v", job)
	}
	record := h.onlyRecord(t, asset.ID)
	if record.Status != model.QuarantineRejected || record.Category != model.QuarantineMalwareDetected ||
		record.ScanSignature != "Eicar-Signature" || record.ScanResult != model.ScanInfected {
		t.Fatalf("malware quarantine record not written: %+v", record)
	}
}

func TestScannerErrorsRetryThenExhaustWithoutPromotion(t *testing.T) {
	h := newHarness()
	h.goodUpload(t, "up-1")
	h.attempts = 2
	h.scanner.results = []interfaces.ScanResult{{Outcome: model.ScanError}, {Outcome: model.ScanError}}
	svc := h.service(t, "worker-a")

	if stats := h.run(t, svc); stats.Retried != 1 {
		t.Fatalf("expected a retry, got %+v", stats)
	}
	if job := h.onlyJob(t); job.Stage != model.StageScan || job.ChecksumVerifiedAt == nil {
		t.Fatalf("verification must be kept and the job resume at SCAN: %+v", job)
	}

	h.clock = h.clock.Add(time.Hour)
	if stats := h.run(t, svc); stats.Exhausted != 1 {
		t.Fatalf("expected exhaustion, got %+v", stats)
	}
	asset := h.onlyAsset(t)
	if asset.Status != model.AssetStatusProcessing {
		t.Fatalf("an unscanned asset must stay PROCESSING, got %s", asset.Status)
	}
	if record := h.onlyRecord(t, asset.ID); record.Status != model.QuarantineHeld || record.Category != model.QuarantineScanIncomplete {
		t.Fatalf("an exhausted scan must be held: %+v", record)
	}
	if h.scanner.calls != 2 {
		t.Fatalf("scanner called %d times, want 2", h.scanner.calls)
	}
}

func TestNoScannerPolicies(t *testing.T) {
	cases := []struct {
		policy    ScanPolicy
		asset     model.AssetStatus
		job       model.ProcessingState
		scanState string
	}{
		{ScanPolicyHold, model.AssetStatusProcessing, model.ProcessingExhausted, ""},
		{ScanPolicyReject, model.AssetStatusRejected, model.ProcessingFailed, model.ScanNotScanned},
		{ScanPolicyAllowUnscanned, model.AssetStatusAvailable, model.ProcessingSucceeded, model.ScanNotScanned},
	}
	for _, c := range cases {
		t.Run(string(c.policy), func(t *testing.T) {
			h := newHarness()
			h.goodUpload(t, "up-1")
			h.noScan = true
			h.policy = c.policy
			h.run(t, h.service(t, "worker-a"))

			asset, job := h.onlyAsset(t), h.onlyJob(t)
			if asset.Status != c.asset || job.State != c.job || job.ScanResult != c.scanState {
				t.Fatalf("got asset %s, job %s, scan %q", asset.Status, job.State, job.ScanResult)
			}
			if job.ScanResult == model.ScanClean {
				t.Fatal("no scanner must never be recorded as a clean scan")
			}
		})
	}
}

// ─── Promotion ───────────────────────────────────────────────────────

func TestPromotionFailuresAreRetriedSafely(t *testing.T) {
	cases := []struct {
		name   string
		inject func(*storagememory.ObjectStorage)
	}{
		{"copy failed", func(s *storagememory.ObjectStorage) { s.CopyErr = errors.New("timeout") }},
		{"copy response lost", func(s *storagememory.ObjectStorage) { s.CopyLost = errors.New("timeout") }},
		{"copy corrupted", func(s *storagememory.ObjectStorage) { s.CopyCorrupts = true }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness()
			h.goodUpload(t, "up-1")
			c.inject(h.storage)
			svc := h.service(t, "worker-a")

			if stats := h.run(t, svc); stats.Retried != 1 {
				t.Fatalf("expected a retry, got %+v", stats)
			}
			asset := h.onlyAsset(t)
			if asset.Status == model.AssetStatusAvailable {
				t.Fatal("must not be available before a verified copy")
			}
			if _, ok := h.storage.Object("quarantine/" + tenant + "/up-1"); !ok {
				t.Fatal("the source must survive a failed promotion")
			}
			if c.name == "copy corrupted" {
				if _, ok := h.storage.Object("assets/" + tenant + "/" + asset.ID); ok {
					t.Fatal("a copy that failed verification must be removed")
				}
			}

			h.clock = h.clock.Add(time.Hour)
			h.run(t, svc)
			if got := h.onlyAsset(t); got.Status != model.AssetStatusAvailable {
				t.Fatalf("expected AVAILABLE after retry, got %s", got.Status)
			}
		})
	}
}

// failCleanupSave fails the first save of the CLEANUP stage, as a database
// outage right after the asset became AVAILABLE would.
type failCleanupSave struct {
	*memory.ProcessingJobRepository
	failed bool
}

func (f *failCleanupSave) SaveProgress(ctx context.Context, job model.ProcessingJob, owner string, now time.Time) error {
	if job.Stage == model.StageCleanup && !f.failed {
		f.failed = true
		return errors.New("database unavailable")
	}
	return f.ProcessingJobRepository.SaveProgress(ctx, job, owner, now)
}

func TestDatabaseFailureAfterPromotionDoesNotCopyAgain(t *testing.T) {
	h := newHarness()
	h.goodUpload(t, "up-1")
	h.jobs = &failCleanupSave{ProcessingJobRepository: h.memJobs}
	svc := h.service(t, "worker-a")

	if stats := h.run(t, svc); stats.Retried != 1 {
		t.Fatalf("expected a retry, got %+v", stats)
	}
	if job := h.onlyJob(t); job.Stage != model.StagePromote || job.PromotionKey == "" {
		t.Fatalf("the promotion key must be durable before the copy: %+v", job)
	}

	h.clock = h.clock.Add(time.Hour)
	h.run(t, svc)
	if job := h.onlyJob(t); job.State != model.ProcessingSucceeded || h.storage.CopyCalls != 1 {
		t.Fatalf("expected success without a second copy: %s, copies %d", job.State, h.storage.CopyCalls)
	}
}

// deleteBeforePromotion deletes the asset just before it would become
// AVAILABLE, as a concurrent deletion request would.
type deleteBeforePromotion struct {
	*memory.AssetRepository
}

func (d deleteBeforePromotion) TransitionStatus(ctx context.Context, tenantID, assetID string, from []model.AssetStatus, to model.AssetStatus, reason, key string, at time.Time) error {
	if to == model.AssetStatusAvailable {
		_ = d.AssetRepository.TransitionStatus(ctx, tenantID, assetID,
			[]model.AssetStatus{model.AssetStatusProcessing}, model.AssetStatusDeleted, "", "", at)
	}
	return d.AssetRepository.TransitionStatus(ctx, tenantID, assetID, from, to, reason, key, at)
}

func TestConcurrentDeletionWinsAndLeavesNoDeliveryCopy(t *testing.T) {
	h := newHarness()
	h.goodUpload(t, "up-1")
	h.assets = deleteBeforePromotion{h.memAsset}
	h.run(t, h.service(t, "worker-a"))

	asset, job := h.onlyAsset(t), h.onlyJob(t)
	if asset.Status != model.AssetStatusDeleted || job.State != model.ProcessingFailed {
		t.Fatalf("expected DELETED and a failed job, got %s / %s", asset.Status, job.State)
	}
	if _, ok := h.storage.Object("assets/" + tenant + "/" + asset.ID); ok {
		t.Fatal("the copy of a deleted asset must be removed")
	}
}

func TestRejectedAssetNeverBecomesAvailable(t *testing.T) {
	h := newHarness()
	h.goodUpload(t, "up-1")
	h.scanner.during = func(context.Context) error {
		// An operator rejects the asset while it is being scanned.
		asset := h.onlyAsset(t)
		return h.assets.TransitionStatus(context.Background(), tenant, asset.ID,
			[]model.AssetStatus{model.AssetStatusProcessing}, model.AssetStatusRejected, "operator", "", h.clock)
	}
	h.run(t, h.service(t, "worker-a"))

	if asset := h.onlyAsset(t); asset.Status != model.AssetStatusRejected || h.storage.CopyCalls != 0 {
		t.Fatalf("expected REJECTED and no copy, got %s, copies %d", asset.Status, h.storage.CopyCalls)
	}
	if !model.CanTransitionAsset(model.AssetStatusQuarantined, model.AssetStatusRejected) ||
		model.CanTransitionAsset(model.AssetStatusRejected, model.AssetStatusAvailable) {
		t.Fatal("the lifecycle must never lead from REJECTED to AVAILABLE")
	}
}

// ─── Crashes, timeouts, cancellation ─────────────────────────────────

func TestCrashedWorkerJobIsRecoveredAndStaleWriteRefused(t *testing.T) {
	h := newHarness()
	h.goodUpload(t, "up-1")
	ctx := context.Background()

	if _, err := h.jobs.EnqueueDue(ctx, h.clock, 3); err != nil {
		t.Fatal(err)
	}
	claimed, err := h.jobs.Claim(ctx, "worker-a", h.clock, h.lease, 10)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: %v, %d", err, len(claimed))
	}
	// worker-a crashes. Within its lease nobody else gets the job.
	if again, _ := h.jobs.Claim(ctx, "worker-b", h.clock.Add(time.Minute), h.lease, 10); len(again) != 0 {
		t.Fatal("an active lease was claimed twice")
	}

	h.clock = h.clock.Add(h.lease + time.Second)
	h.run(t, h.service(t, "worker-b"))
	if asset := h.onlyAsset(t); asset.Status != model.AssetStatusAvailable {
		t.Fatalf("expected recovery to AVAILABLE, got %s", asset.Status)
	}
	if err := h.jobs.Finish(ctx, claimed[0].ID, "worker-a", h.clock, model.ProcessingExhausted, "stale"); !errors.Is(err, model.ErrLeaseLost) {
		t.Fatalf("the crashed worker must not write, got %v", err)
	}
}

func TestProcessingTimeoutIsRetried(t *testing.T) {
	h := newHarness()
	h.goodUpload(t, "up-1")
	h.lease = 50 * time.Millisecond
	h.scanner.during = func(ctx context.Context) error {
		<-ctx.Done() // a scan that never finishes on its own
		return ctx.Err()
	}

	stats := h.run(t, h.service(t, "worker-a"))
	if stats.Retried != 1 || h.onlyAsset(t).Status == model.AssetStatusAvailable {
		t.Fatalf("a timed-out job must be retried, not promoted: %+v", stats)
	}
}

func TestShutdownWritesNoOutcome(t *testing.T) {
	h := newHarness()
	h.goodUpload(t, "up-1")
	ctx, cancel := context.WithCancel(context.Background())
	h.scanner.during = func(context.Context) error {
		cancel()
		return context.Canceled
	}

	stats, _ := h.service(t, "worker-a").RunOnce(ctx)
	job := h.onlyJob(t)
	if stats.Succeeded+stats.Retried+stats.Exhausted+stats.Failed != 0 || job.LeaseOwner != "worker-a#1" || job.State != model.ProcessingPending {
		t.Fatalf("an interrupted job must keep its lease to expire: %+v / %+v", stats, job)
	}
}

// ─── Tenant isolation ────────────────────────────────────────────────

func TestAssetsAreTenantScoped(t *testing.T) {
	h := newHarness()
	h.goodUpload(t, "up-1")
	h.run(t, h.service(t, "worker-a"))
	asset := h.onlyAsset(t)

	if _, err := h.assets.GetByID(context.Background(), "tenant-2", asset.ID); !errors.Is(err, model.ErrAssetNotFound) {
		t.Fatalf("another tenant must not read the asset, got %v", err)
	}
	err := h.assets.TransitionStatus(context.Background(), "tenant-2", asset.ID,
		[]model.AssetStatus{model.AssetStatusAvailable}, model.AssetStatusDeleted, "", "", h.clock)
	if !errors.Is(err, model.ErrAssetNotFound) {
		t.Fatalf("another tenant must not change the asset, got %v", err)
	}
	if !strings.HasPrefix(asset.ObjectKey, "assets/"+tenant+"/") {
		t.Fatalf("the delivery key must be derived from the tenant, got %s", asset.ObjectKey)
	}
}

// ─── Scan outcomes, quarantine and operators ─────────────────────────

func TestIncompleteScansAreNeverClean(t *testing.T) {
	cases := []struct {
		outcome string
		stats   func(CycleStats) bool
	}{
		// Unsupported cannot improve by retrying: held at once.
		{model.ScanUnsupported, func(s CycleStats) bool { return s.Exhausted == 1 }},
		{model.ScanTimeout, func(s CycleStats) bool { return s.Retried == 1 }},
		{model.ScanError, func(s CycleStats) bool { return s.Retried == 1 }},
	}
	for _, c := range cases {
		t.Run(c.outcome, func(t *testing.T) {
			h := newHarness()
			h.goodUpload(t, "up-1")
			h.scanner.results = []interfaces.ScanResult{{Outcome: c.outcome, Detail: "detail"}}
			stats := h.run(t, h.service(t, "worker-a"))

			asset, job := h.onlyAsset(t), h.onlyJob(t)
			if !c.stats(stats) || asset.Status != model.AssetStatusProcessing || h.storage.CopyCalls != 0 {
				t.Fatalf("an incomplete scan must not promote: %+v, %s", stats, asset.Status)
			}
			if job.ScanResult != c.outcome || job.ScanError != "detail" || job.Stage != model.StageScan {
				t.Fatalf("scan attempt not recorded: %+v", job)
			}
		})
	}
}

func TestScannerHealthCheckFailureRetriesWithoutScanning(t *testing.T) {
	h := newHarness()
	h.goodUpload(t, "up-1")
	h.scanner.pingErr = errors.New("connection refused")
	stats := h.run(t, h.service(t, "worker-a"))

	if stats.Retried != 1 || h.scanner.calls != 0 {
		t.Fatalf("expected a retry and no scan, got %+v, %d scans", stats, h.scanner.calls)
	}
	if job := h.onlyJob(t); job.ScanResult != model.ScanError || job.ScanError != "scanner unavailable" {
		t.Fatalf("scanner outage not recorded: %+v", job)
	}
}

func TestHeldAssetIsReleasedAfterRequeue(t *testing.T) {
	h := newHarness()
	h.goodUpload(t, "up-1")
	h.attempts = 1
	h.scanner.results = []interfaces.ScanResult{{Outcome: model.ScanError}}
	svc := h.service(t, "worker-a")

	if stats := h.run(t, svc); stats.Exhausted != 1 {
		t.Fatalf("expected exhaustion, got %+v", stats)
	}
	asset := h.onlyAsset(t)
	if record := h.onlyRecord(t, asset.ID); record.Status != model.QuarantineHeld {
		t.Fatalf("expected HELD, got %+v", record)
	}

	// Requeue refuses anything but an exhausted job, and other tenants.
	if _, err := svc.Requeue(context.Background(), "tenant-2", asset.ID, "operator:test"); !errors.Is(err, model.ErrAssetNotFound) {
		t.Fatalf("another tenant must not requeue, got %v", err)
	}
	if _, err := svc.Requeue(context.Background(), tenant, asset.ID, "operator:test"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Requeue(context.Background(), tenant, asset.ID, "operator:test"); !errors.Is(err, model.ErrInvalidAssetState) {
		t.Fatalf("a pending job must not be requeued again, got %v", err)
	}

	h.run(t, svc)
	if got := h.onlyAsset(t); got.Status != model.AssetStatusAvailable {
		t.Fatalf("expected AVAILABLE after requeue, got %s", got.Status)
	}
	if record := h.onlyRecord(t, asset.ID); record.Status != model.QuarantineReleased || record.ResolvedAt == nil {
		t.Fatalf("the hold must be released: %+v", record)
	}
	if got := h.eventNames(t, asset.ID); !strings.Contains(got, "quarantine_held,requeued,") || !strings.HasSuffix(got, "quarantine_released") {
		t.Fatalf("operator actions not audited: %q", got)
	}

	inspection, err := svc.Inspect(context.Background(), tenant, asset.ID)
	if err != nil || inspection.Job.State != model.ProcessingSucceeded || len(inspection.Quarantine) != 1 || len(inspection.Events) == 0 {
		t.Fatalf("inspect: %+v, %v", inspection, err)
	}
}

func TestRejectedObjectIsPurgedAfterRetention(t *testing.T) {
	h := newHarness()
	h.goodUpload(t, "up-1")
	h.scanner.results = []interfaces.ScanResult{{Outcome: model.ScanInfected, Signature: "Eicar-Signature"}}
	svc := h.service(t, "worker-a")
	source := "quarantine/" + tenant + "/up-1"

	h.run(t, svc)
	if _, ok := h.storage.Object(source); !ok {
		t.Fatal("a rejected object is kept for investigation during retention")
	}

	h.clock = h.clock.Add(retention - time.Minute)
	if stats := h.run(t, svc); stats.Purged != 0 {
		t.Fatal("purged before retention ended")
	}

	h.clock = h.clock.Add(2 * time.Minute)
	if stats := h.run(t, svc); stats.Purged != 1 {
		t.Fatalf("expected one purge, got %+v", stats)
	}
	if _, ok := h.storage.Object(source); ok {
		t.Fatal("the rejected object must be deleted after retention")
	}
	asset := h.onlyAsset(t)
	if record := h.onlyRecord(t, asset.ID); record.Status != model.QuarantinePurged {
		t.Fatalf("expected PURGED, got %+v", record)
	}
	if stats := h.run(t, svc); stats.Purged != 0 {
		t.Fatal("a purged record must not be purged again")
	}
}

func TestPromotionReusesOrReplacesALeftoverCopy(t *testing.T) {
	cases := []struct {
		name     string
		leftover []byte
		copies   int
	}{
		{"verified leftover is reused", pdfBytes, 0},
		{"wrong leftover is replaced", []byte("partial"), 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness()
			h.goodUpload(t, "up-1")
			if _, err := h.jobs.EnqueueDue(context.Background(), h.clock, 3); err != nil {
				t.Fatal(err)
			}
			// A worker crashed right after copying, before recording it.
			destination := "assets/" + tenant + "/" + h.onlyJob(t).AssetID
			h.storage.PutObject(destination, c.leftover)

			h.run(t, h.service(t, "worker-a"))
			if got := h.onlyAsset(t); got.Status != model.AssetStatusAvailable || h.storage.CopyCalls != c.copies {
				t.Fatalf("got %s with %d copies, want AVAILABLE with %d", got.Status, h.storage.CopyCalls, c.copies)
			}
			if got, _ := h.storage.Object(destination); string(got) != string(pdfBytes) {
				t.Fatal("the delivered object must be the verified bytes")
			}
		})
	}
}

func TestTenantPolicyNarrowsValidation(t *testing.T) {
	cases := []struct {
		name   string
		policy model.ContentPolicy
		reason string
	}{
		{"denied type", model.ContentPolicy{DeniedTypes: []string{"application/pdf"}}, model.RejectContentType},
		{"smaller size limit", model.ContentPolicy{MaxSizeBytes: 100}, model.RejectFileTooLarge},
		{"allow list", model.ContentPolicy{AllowedTypes: []string{"image/*"}}, model.RejectContentType},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness()
			h.goodUpload(t, "up-1")
			h.policies.Set(tenant, c.policy)
			h.run(t, h.service(t, "worker-a"))

			asset, job := h.onlyAsset(t), h.onlyJob(t)
			if asset.Status != model.AssetStatusRejected || asset.RejectionReason != c.reason {
				t.Fatalf("expected REJECTED/%s, got %s/%s", c.reason, asset.Status, asset.RejectionReason)
			}
			if job.ValidationStatus != model.StageRejected || job.ValidationReason != c.reason || h.scanner.calls != 0 {
				t.Fatalf("validation result not recorded or scanned anyway: %+v", job)
			}
		})
	}
}

// A quarantine record without its asset (impossible: they cascade) is
// never purged from the platform's bucket in place of the asset's own
// connection.
func TestPurgeNeverFallsBackToThePlatformBucket(t *testing.T) {
	h := newHarness()
	ctx := context.Background()
	key := "quarantine/" + tenant + "/up-gone"
	h.storage.PutObject(key, []byte("rejected bytes"))
	retain := h.clock.Add(-time.Hour)
	if err := h.held.Reject(ctx, model.QuarantineRecord{
		ID: "q-gone", TenantID: tenant, AssetID: "gone", JobID: "job-gone", UploadID: "up-gone", ObjectKey: key,
		Category: model.QuarantineMalwareDetected, Reason: model.RejectMalwareDetected, RetainUntil: &retain, CreatedAt: retain,
	}); err != nil {
		t.Fatal(err)
	}
	if n, err := h.service(t, "worker-a").PurgeExpired(ctx); err != nil || n != 0 {
		t.Fatalf("purged %d: %v", n, err)
	}
	if _, ok := h.storage.Object(key); !ok {
		t.Fatal("an object was deleted from the platform bucket for a record without its asset")
	}
}
