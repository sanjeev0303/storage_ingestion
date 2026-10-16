package processing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"strings"
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

var quietLog = slog.New(slog.NewTextHandler(io.Discard, nil))

// fakeScanner answers with its verdict or error, after running during().
type fakeScanner struct {
	verdict interfaces.ScanVerdict
	errs    []error
	calls   int
	during  func(ctx context.Context) error
}

func (f *fakeScanner) Name() string { return "fake" }

func (f *fakeScanner) Scan(ctx context.Context, content io.Reader) (interfaces.ScanVerdict, error) {
	f.calls++
	if _, err := io.Copy(io.Discard, content); err != nil {
		return interfaces.ScanVerdict{}, err
	}
	if f.during != nil {
		if err := f.during(ctx); err != nil {
			return interfaces.ScanVerdict{}, err
		}
	}
	if len(f.errs) > 0 {
		err := f.errs[0]
		f.errs = f.errs[1:]
		return interfaces.ScanVerdict{}, err
	}
	return f.verdict, nil
}

type harness struct {
	uploads  *memory.UploadRepository
	assets   interfaces.AssetRepository
	jobs     interfaces.ProcessingJobRepository
	memJobs  *memory.ProcessingJobRepository
	memAsset *memory.AssetRepository
	storage  *storagememory.ObjectStorage
	scanner  *fakeScanner
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
		Validator: ContentValidator{}, Promoter: promoter,
	}
	if !h.noScan {
		deps.Scanner = h.scanner
	}
	svc, err := NewService(deps, Config{
		Owner: owner, Bucket: "bucket", BatchSize: 10, Concurrency: 2, Lease: h.lease,
		MaxFileBytes: 1 << 20, ScanPolicy: h.policy,
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
		job.ScanResult != model.ScanClean || job.Scanner != "fake" || job.PromotedAt == nil || job.DetectedContentType != "application/pdf" {
		t.Fatalf("stage results not recorded: %+v", job)
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
		name   string
		stored []byte
		sum    string
		size   int64
		ctype  string
		reason string
	}{
		{"checksum mismatch", pdfBytes, sum([]byte("other")), int64(len(pdfBytes)), "application/pdf", model.FailureChecksumMismatch},
		{"wrong size", pdfBytes, sum(pdfBytes), int64(len(pdfBytes)) + 1, "application/pdf", model.FailureObjectSizeMismatch},
		{"missing object", nil, sum(pdfBytes), int64(len(pdfBytes)), "application/pdf", model.FailureUploadMissing},
		{"type contradicts bytes", pdfBytes, sum(pdfBytes), int64(len(pdfBytes)), "video/mp4", model.RejectContentMismatch},
		{"html is never accepted", []byte("<!DOCTYPE html><html></html>"), sum([]byte("<!DOCTYPE html><html></html>")), 28, "text/plain", model.RejectContentType},
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

func TestMalwareIsRejectedAndRecorded(t *testing.T) {
	h := newHarness()
	h.goodUpload(t, "up-1")
	h.scanner.verdict = interfaces.ScanVerdict{Infected: true, Signature: "Eicar-Signature"}
	h.run(t, h.service(t, "worker-a"))

	asset, job := h.onlyAsset(t), h.onlyJob(t)
	if asset.Status != model.AssetStatusRejected || asset.RejectionReason != model.RejectMalwareDetected {
		t.Fatalf("expected malware rejection, got %s/%s", asset.Status, asset.RejectionReason)
	}
	if job.ScanResult != model.ScanInfected || job.ScanSignature != "Eicar-Signature" || job.State != model.ProcessingFailed {
		t.Fatalf("scan result not recorded: %+v", job)
	}
}

func TestScannerErrorsRetryThenExhaustWithoutPromotion(t *testing.T) {
	h := newHarness()
	h.goodUpload(t, "up-1")
	h.attempts = 2
	h.scanner.errs = []error{errors.New("clamd down"), errors.New("clamd down")}
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
	if asset := h.onlyAsset(t); asset.Status != model.AssetStatusProcessing {
		t.Fatalf("an unscanned asset must stay PROCESSING, got %s", asset.Status)
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
	if stats.Succeeded+stats.Retried+stats.Exhausted+stats.Failed != 0 || job.LeaseOwner != "worker-a" || job.State != model.ProcessingPending {
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
