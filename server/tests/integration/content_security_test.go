package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/scanner/clamav"
	s3storage "github.com/sanjeev0303/object-storage-platform/internal/infrastructure/storage/s3"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/retry"
	"github.com/sanjeev0303/object-storage-platform/internal/service/processing"
	"github.com/sanjeev0303/object-storage-platform/internal/service/promotion"
	uploadservice "github.com/sanjeev0303/object-storage-platform/internal/service/upload"
)

// Phase 5.4 against the local stack: PostgreSQL (TEST_DATABASE_URL with
// migrations through 000007), MinIO (TEST_S3_BUCKET) and ClamAV
// (TEST_CLAMAV_ADDRESS, e.g. 127.0.0.1:3310). Only the EICAR test string is
// used as "malware"; it is harmless by design.

const promotionPrefix = "assets-it"

// eicar is assembled at run time so the source file itself does not
// trip antivirus software on the developer's machine.
func eicar() []byte {
	parts := []string{`X5O!P%@AP[4\PZX54(P^)7CC)7}$`, `EICAR-STANDARD-ANTIVIRUS-TEST-FILE!`, `$H+H*`}
	return []byte(strings.Join(parts, ""))
}

type contentStack struct {
	env       *processingEnv
	provider  *s3storage.ObjectStorage
	bucket    string
	multipart *uploadservice.MultipartService
	clamAddr  string
}

func openContentStack(t *testing.T) *contentStack {
	t.Helper()
	clamAddr := os.Getenv("TEST_CLAMAV_ADDRESS")
	if clamAddr == "" {
		t.Skip("TEST_CLAMAV_ADDRESS is not set; content security tests were not run")
	}
	env := openProcessingEnv(t)
	provider, bucket := openTestProvider(t)

	multipart, err := uploadservice.NewMultipartService(env.uploads, postgres.NewMultipartUploadRepository(env.db), provider,
		uploadservice.MultipartServiceConfig{Bucket: bucket, MaxSize: 1 << 30, SessionTTL: time.Hour, PartURLTTL: 5 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = env.db.Pool.Exec(ctx, `DELETE FROM processing_events WHERE tenant_id = $1`, env.tenant)
		_, _ = env.db.Pool.Exec(ctx, `DELETE FROM tenant_content_policies WHERE tenant_id = $1`, env.tenant)
	})
	return &contentStack{env: env, provider: provider, bucket: bucket, multipart: multipart, clamAddr: clamAddr}
}

type serviceOptions struct {
	scanner     interfaces.MalwareScanner
	storage     interfaces.MultipartStorage
	maxAttempts int
	retention   time.Duration
}

func (s *contentStack) service(t *testing.T, opts serviceOptions) *processing.Service {
	t.Helper()
	if opts.scanner == nil {
		opts.scanner = clamav.New(s.clamAddr, 30*time.Second)
	}
	if opts.storage == nil {
		opts.storage = s.provider
	}
	if opts.maxAttempts == 0 {
		opts.maxAttempts = 3
	}
	if opts.retention == 0 {
		opts.retention = 24 * time.Hour
	}

	assets := postgres.NewAssetRepository(s.env.db)
	promoter, err := promotion.NewService(assets, opts.storage, promotion.Config{
		SourceBucket: s.bucket, DestinationBucket: s.bucket, Prefix: promotionPrefix,
	})
	if err != nil {
		t.Fatal(err)
	}
	svc, err := processing.NewService(processing.Dependencies{
		Jobs:       s.env.jobs,
		Assets:     assets,
		Uploads:    s.env.uploads,
		Quarantine: postgres.NewQuarantineRepository(s.env.db),
		Events:     postgres.NewProcessingEventRepository(s.env.db),
		Policies:   postgres.NewContentPolicyRepository(s.env.db),
		Verifier:   s.multipart,
		Storage:    opts.storage,
		Validators: []interfaces.FileValidator{processing.BasicValidator{}, processing.TypeValidator{}},
		Scanner:    opts.scanner,
		Promoter:   promoter,
	}, processing.Config{
		Owner: "it-processing", Bucket: s.bucket, BatchSize: 100, Concurrency: 2, Lease: 2 * time.Minute,
		Retry:               retry.Policy{BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond, MaxAttempts: opts.maxAttempts},
		Policy:              model.ContentPolicy{MaxSizeBytes: 1 << 30, UnknownTypeAction: "allow"},
		ScanPolicy:          processing.ScanPolicyHold,
		ScanTimeout:         time.Minute,
		QuarantineRetention: opts.retention,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

// upload sends parts through presigned URLs and completes the upload, as a
// client would. It returns the upload ID.
func (s *contentStack) upload(t *testing.T, name, contentType string, parts ...[]byte) string {
	t.Helper()
	ctx := context.Background()
	whole := bytes.Join(parts, nil)
	digest := sha256.Sum256(whole)

	started, err := s.multipart.Start(ctx, uploadservice.StartMultipartInput{
		TenantID: s.env.tenant, OwnerID: "owner", FileName: name, ContentType: contentType,
		SizeBytes: int64(len(whole)), ChecksumSHA256: hex.EncodeToString(digest[:]), IdempotencyKey: "it-" + name,
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	id := started.Session.ID
	t.Cleanup(func() { _ = s.provider.DeleteObject(context.Background(), s.bucket, started.Session.ObjectKey) })

	numbers := make([]int32, len(parts))
	for i := range parts {
		numbers[i] = int32(i + 1)
	}
	urls, err := s.multipart.PresignParts(ctx, s.env.tenant, "owner", id, numbers)
	if err != nil {
		t.Fatal(err)
	}
	var recorded []uploadservice.RecordPartInput
	var completed []uploadservice.CompletedPartInput
	for i, part := range parts {
		etag := putPart(t, urls[i].URL, part)
		recorded = append(recorded, uploadservice.RecordPartInput{PartNumber: numbers[i], ETag: etag, SizeBytes: int64(len(part))})
		completed = append(completed, uploadservice.CompletedPartInput{PartNumber: numbers[i], ETag: etag})
	}
	if err := s.multipart.RecordParts(ctx, s.env.tenant, "owner", id, recorded); err != nil {
		t.Fatalf("record parts: %v", err)
	}
	if _, err := s.multipart.Complete(ctx, s.env.tenant, "owner", id, uploadservice.CompleteMultipartInput{Parts: completed}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	return id
}

func (s *contentStack) assetFor(t *testing.T, uploadID string) (model.Asset, model.ProcessingJob) {
	t.Helper()
	job, ok := s.env.ourJobs(t)[uploadID]
	if !ok {
		t.Fatalf("no job for upload %s", uploadID)
	}
	asset, err := s.env.assets.GetByID(context.Background(), s.env.tenant, job.AssetID)
	if err != nil {
		t.Fatal(err)
	}
	full, err := s.env.jobs.GetByAsset(context.Background(), s.env.tenant, job.AssetID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = s.provider.DeleteObject(context.Background(), s.bucket, promotionPrefix+"/"+s.env.tenant+"/"+asset.ID)
	})
	return *asset, full
}

func (s *contentStack) records(t *testing.T, assetID string) []model.QuarantineRecord {
	t.Helper()
	records, err := postgres.NewQuarantineRepository(s.env.db).ListForAsset(context.Background(), s.env.tenant, assetID)
	if err != nil {
		t.Fatal(err)
	}
	return records
}

func runCycle(t *testing.T, svc *processing.Service) processing.CycleStats {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	stats, err := svc.RunOnce(ctx)
	if err != nil {
		t.Fatalf("processing cycle: %v", err)
	}
	return stats
}

func TestLocalStackCleanMultipartUploadBecomesAvailable(t *testing.T) {
	stack := openContentStack(t)
	first := append([]byte("%PDF-1.7\n"), bytes.Repeat([]byte("p"), 5*1024*1024)...)
	second := []byte("\n%%EOF\n")
	id := stack.upload(t, "report.pdf", "application/pdf", first, second)

	// The provider's ETag for a multipart object is not a content hash.
	meta, err := stack.provider.HeadObject(context.Background(), stack.bucket, "quarantine/"+stack.env.tenant+"/"+id)
	if err != nil || !strings.HasSuffix(strings.Trim(meta.ETag, `"`), "-2") {
		t.Fatalf("expected a multipart ETag ending in -2, got %q (%v)", meta.ETag, err)
	}

	runCycle(t, stack.service(t, serviceOptions{}))

	asset, job := stack.assetFor(t, id)
	if asset.Status != model.AssetStatusAvailable || asset.ObjectKey != promotionPrefix+"/"+stack.env.tenant+"/"+asset.ID {
		t.Fatalf("expected AVAILABLE under the delivery key, got %s at %s", asset.Status, asset.ObjectKey)
	}
	if job.State != model.ProcessingSucceeded || job.VerificationStatus != model.StagePassed ||
		job.ChecksumSource != model.ChecksumDeclared || job.ChecksumAlgorithm != "SHA-256" ||
		job.ValidationStatus != model.StagePassed || job.DetectedContentType != "application/pdf" ||
		job.ScanResult != model.ScanClean || job.Scanner != "clamav" || job.PromotionStatus != model.PromotionPublished {
		t.Fatalf("stage results not durable: %+v", job)
	}
	if strings.Contains(asset.ChecksumSHA256, "-") || asset.ChecksumSHA256 == strings.Trim(meta.ETag, `"`) {
		t.Fatal("the multipart ETag must never stand in for the checksum")
	}
	if _, err := stack.provider.HeadObject(context.Background(), stack.bucket, "quarantine/"+stack.env.tenant+"/"+id); !errors.Is(err, interfaces.ErrObjectNotFound) {
		t.Fatalf("the quarantine source must be removed, got %v", err)
	}
	if records := stack.records(t, asset.ID); len(records) != 0 {
		t.Fatalf("a clean file must not be quarantined: %+v", records)
	}
}

func TestLocalStackEicarIsRejectedQuarantinedAndPurged(t *testing.T) {
	stack := openContentStack(t)
	id := stack.upload(t, "eicar.txt", "text/plain", eicar())

	svc := stack.service(t, serviceOptions{retention: time.Millisecond})
	runCycle(t, svc)

	asset, job := stack.assetFor(t, id)
	if asset.Status != model.AssetStatusRejected || asset.RejectionReason != model.RejectMalwareDetected {
		t.Fatalf("expected REJECTED/malware_detected, got %s/%s", asset.Status, asset.RejectionReason)
	}
	if job.ScanResult != model.ScanInfected || !strings.Contains(strings.ToUpper(job.ScanSignature), "EICAR") {
		t.Fatalf("scan result not recorded: %s %q", job.ScanResult, job.ScanSignature)
	}
	records := stack.records(t, asset.ID)
	if len(records) != 1 || records[0].Category != model.QuarantineMalwareDetected || records[0].RetainUntil == nil {
		t.Fatalf("expected one malware quarantine record, got %+v", records)
	}

	time.Sleep(10 * time.Millisecond)
	if _, err := svc.PurgeExpired(context.Background()); err != nil {
		t.Fatal(err)
	}
	if records := stack.records(t, asset.ID); records[0].Status != model.QuarantinePurged {
		t.Fatalf("expected PURGED after retention, got %s", records[0].Status)
	}
	if _, err := stack.provider.HeadObject(context.Background(), stack.bucket, "quarantine/"+stack.env.tenant+"/"+id); !errors.Is(err, interfaces.ErrObjectNotFound) {
		t.Fatalf("the infected object must be deleted after retention, got %v", err)
	}
}

func TestLocalStackScannerOutageHoldsThenRequeueReleases(t *testing.T) {
	stack := openContentStack(t)
	id := stack.upload(t, "notes.txt", "text/plain", []byte("plain text notes\n"))

	// Nothing listens on port 1: the health check fails, never a clean scan.
	down := stack.service(t, serviceOptions{scanner: clamav.New("127.0.0.1:1", time.Second), maxAttempts: 1})
	if stats := runCycle(t, down); stats.Exhausted < 1 {
		t.Fatalf("expected exhaustion, got %+v", stats)
	}
	asset, job := stack.assetFor(t, id)
	if asset.Status != model.AssetStatusProcessing || job.State != model.ProcessingExhausted || job.ScanResult != model.ScanError {
		t.Fatalf("expected a held PROCESSING asset, got %s, job %s, scan %s", asset.Status, job.State, job.ScanResult)
	}
	if records := stack.records(t, asset.ID); len(records) != 1 || records[0].Status != model.QuarantineHeld ||
		records[0].Category != model.QuarantineScanIncomplete {
		t.Fatalf("expected a HELD scan_incomplete record, got %+v", records)
	}

	up := stack.service(t, serviceOptions{})
	if _, err := up.Requeue(context.Background(), stack.env.tenant, asset.ID, "operator:it"); err != nil {
		t.Fatal(err)
	}
	runCycle(t, up)

	asset, job = stack.assetFor(t, id)
	if asset.Status != model.AssetStatusAvailable || job.ScanResult != model.ScanClean {
		t.Fatalf("expected AVAILABLE after requeue, got %s / %s", asset.Status, job.ScanResult)
	}
	if records := stack.records(t, asset.ID); records[0].Status != model.QuarantineReleased {
		t.Fatalf("expected the hold RELEASED, got %+v", records)
	}
	inspection, err := up.Inspect(context.Background(), stack.env.tenant, asset.ID)
	if err != nil || len(inspection.Events) == 0 {
		t.Fatalf("inspect: %v, %d events", err, len(inspection.Events))
	}
}

// lostCopy completes the copy at the provider but reports a failure once,
// as a worker crash right after the copy would look.
type lostCopy struct {
	*s3storage.ObjectStorage
	calls int
}

func (l *lostCopy) CopyObject(ctx context.Context, sourceBucket, sourceKey, destinationBucket, destinationKey string) error {
	l.calls++
	if err := l.ObjectStorage.CopyObject(ctx, sourceBucket, sourceKey, destinationBucket, destinationKey); err != nil {
		return err
	}
	if l.calls == 1 {
		return errors.New("copy object: connection reset")
	}
	return nil
}

func TestLocalStackCrashAfterCopyResumesWithoutCopyingAgain(t *testing.T) {
	stack := openContentStack(t)
	id := stack.upload(t, "data.csv", "text/csv", []byte("a,b\n1,2\n"))

	storage := &lostCopy{ObjectStorage: stack.provider}
	svc := stack.service(t, serviceOptions{storage: storage})
	if stats := runCycle(t, svc); stats.Retried < 1 {
		t.Fatalf("expected a retry, got %+v", stats)
	}
	if asset, _ := stack.assetFor(t, id); asset.Status != model.AssetStatusProcessing {
		t.Fatalf("must not be AVAILABLE before the copy is confirmed, got %s", asset.Status)
	}

	time.Sleep(20 * time.Millisecond) // past the retry backoff
	runCycle(t, svc)
	asset, job := stack.assetFor(t, id)
	if asset.Status != model.AssetStatusAvailable || job.State != model.ProcessingSucceeded || storage.calls != 1 {
		t.Fatalf("expected AVAILABLE from the verified leftover copy, got %s, %d copies", asset.Status, storage.calls)
	}
}

func TestPostgresQuarantineRecordsStaySingleAndTenantScoped(t *testing.T) {
	env := openProcessingEnv(t)
	ctx := context.Background()
	env.session(t, "q", model.UploadStatusCompleted)
	if _, err := env.jobs.EnqueueDue(ctx, time.Now().UTC(), 3); err != nil {
		t.Fatal(err)
	}
	job := env.ourJobs(t)[env.tenant+"-q"]
	repo := postgres.NewQuarantineRepository(env.db)
	t.Cleanup(func() {
		_, _ = env.db.Pool.Exec(context.Background(), `DELETE FROM processing_events WHERE tenant_id = $1`, env.tenant)
	})

	record := model.QuarantineRecord{
		TenantID: env.tenant, AssetID: job.AssetID, JobID: job.ID, UploadID: job.UploadID,
		ObjectKey: "quarantine/" + env.tenant + "/q", Category: model.QuarantineScanIncomplete, Reason: "scanner down",
		ScanResult: model.ScanError, CreatedAt: time.Now().UTC(),
	}
	first, err := repo.Hold(ctx, record)
	if err != nil {
		t.Fatal(err)
	}
	record.Category, record.Reason = model.QuarantineProcessingExhausted, "retries exhausted"
	second, err := repo.Hold(ctx, record)
	if err != nil || second.ID != first.ID || second.Category != model.QuarantineProcessingExhausted {
		t.Fatalf("a second hold must refresh the open record: %+v, %v", second, err)
	}

	if other, _ := repo.ListForAsset(ctx, "another-tenant", job.AssetID); len(other) != 0 {
		t.Fatal("another tenant must not see quarantine records")
	}
	if released, _ := repo.Release(ctx, "another-tenant", job.AssetID, time.Now().UTC()); released {
		t.Fatal("another tenant must not release a hold")
	}

	retain := time.Now().UTC().Add(time.Hour)
	record.Category, record.Reason, record.RetainUntil = model.QuarantineMalwareDetected, model.RejectMalwareDetected, &retain
	for i := 0; i < 2; i++ {
		if err := repo.Reject(ctx, record); err != nil {
			t.Fatal(err)
		}
	}
	records, err := repo.ListForAsset(ctx, env.tenant, job.AssetID)
	if err != nil || len(records) != 1 || records[0].Status != model.QuarantineRejected || records[0].ID != first.ID {
		t.Fatalf("rejection must close the hold once: %+v, %v", records, err)
	}
	if released, _ := repo.Release(ctx, env.tenant, job.AssetID, time.Now().UTC()); released {
		t.Fatal("a rejected record must not be released")
	}

	if _, err := env.jobs.Requeue(ctx, env.tenant, job.AssetID, time.Now().UTC()); !errors.Is(err, model.ErrInvalidAssetState) {
		t.Fatalf("only an EXHAUSTED job may be requeued, got %v", err)
	}
	backlog, err := env.jobs.Backlog(ctx, time.Now().UTC())
	if err != nil || len(backlog) != 4 {
		t.Fatalf("backlog: %v, %v", backlog, err)
	}
}

func TestPostgresContentPolicyIsReadPerTenant(t *testing.T) {
	env := openProcessingEnv(t)
	ctx := context.Background()
	t.Cleanup(func() {
		_, _ = env.db.Pool.Exec(context.Background(), `DELETE FROM tenant_content_policies WHERE tenant_id = $1`, env.tenant)
	})
	repo := postgres.NewContentPolicyRepository(env.db)

	if _, ok, err := repo.Get(ctx, env.tenant); ok || err != nil {
		t.Fatalf("no row must mean the default policy: %v, %v", ok, err)
	}
	_, err := env.db.Pool.Exec(ctx, `
		INSERT INTO tenant_content_policies (tenant_id, allowed_types, denied_types, max_size_bytes, unknown_type_action)
		VALUES ($1, '{image/*}', '{image/gif}', 1000, 'reject')
	`, env.tenant)
	if err != nil {
		t.Fatal(err)
	}
	policy, ok, err := repo.Get(ctx, env.tenant)
	if err != nil || !ok || strings.Join(policy.AllowedTypes, ",") != "image/*" || strings.Join(policy.DeniedTypes, ",") != "image/gif" ||
		policy.MaxSizeBytes != 1000 || policy.UnknownTypeAction != "reject" {
		t.Fatalf("policy not read back: %+v, %v, %v", policy, ok, err)
	}
}
