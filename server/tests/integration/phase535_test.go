package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
	s3storage "github.com/sanjeev0303/object-storage-platform/internal/infrastructure/storage/s3"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/retry"
	"github.com/sanjeev0303/object-storage-platform/internal/service/reconciliation"
	uploadservice "github.com/sanjeev0303/object-storage-platform/internal/service/upload"
)

// ─── PostgreSQL: Phase 5.3.5 schema and repository behaviour ─────────

func TestPostgresAbortedStatusAndConfirmationTimes(t *testing.T) {
	env := openReconcileEnv(t)
	ctx := context.Background()
	env.session(t, "abort", model.UploadStatusUploading)
	env.session(t, "complete", model.UploadStatusCompleting)
	now := time.Now().UTC().Truncate(time.Microsecond)

	if err := env.uploads.TransitionStatus(ctx, env.tenant, env.tenant+"-abort",
		[]model.UploadStatus{model.UploadStatusUploading}, model.UploadStatusAborting, model.FailureAbortedByClient, now); err != nil {
		t.Fatal(err)
	}
	if err := env.uploads.TransitionStatus(ctx, env.tenant, env.tenant+"-abort",
		[]model.UploadStatus{model.UploadStatusAborting}, model.UploadStatusAborted, "", now); err != nil {
		t.Fatalf("ABORTED must be accepted by the schema: %v", err)
	}
	if err := env.uploads.TransitionStatus(ctx, env.tenant, env.tenant+"-complete",
		[]model.UploadStatus{model.UploadStatusCompleting}, model.UploadStatusCompleted, "", now); err != nil {
		t.Fatal(err)
	}

	aborted, _ := env.uploads.GetByID(ctx, env.tenant, env.tenant+"-abort")
	completed, _ := env.uploads.GetByID(ctx, env.tenant, env.tenant+"-complete")
	if aborted.Status != model.UploadStatusAborted || aborted.AbortedAt == nil || !aborted.AbortedAt.Equal(now) {
		t.Fatalf("abort confirmation not recorded: %+v", aborted)
	}
	if completed.CompletedAt == nil || !completed.CompletedAt.Equal(now) || completed.AbortedAt != nil {
		t.Fatalf("completion confirmation not recorded: %+v", completed)
	}

	// Terminal means terminal: ABORTED cannot be completed.
	err := env.uploads.TransitionStatus(ctx, env.tenant, env.tenant+"-abort",
		[]model.UploadStatus{model.UploadStatusAborted}, model.UploadStatusCompleted, "", now)
	if !errors.Is(err, model.ErrInvalidUploadState) {
		t.Fatalf("expected ErrInvalidUploadState, got %v", err)
	}
}

func TestPostgresDryRunListingWritesNothing(t *testing.T) {
	env := openReconcileEnv(t)
	ctx := context.Background()
	env.session(t, "due", model.UploadStatusCompleting)
	if _, err := env.db.Pool.Exec(ctx, `UPDATE upload_sessions SET updated_at = $2 WHERE id = $1`, env.tenant+"-due", farPast); err != nil {
		t.Fatal(err)
	}

	due, err := env.tasks.ListDue(ctx, time.Now().UTC(), time.Minute, 1000)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range due {
		if d.UploadID == env.tenant+"-due" {
			found = d.Operation == model.ReconcileComplete && d.Status == model.UploadStatusCompleting
		}
	}
	if !found {
		t.Fatal("ListDue must report the COMPLETE the sweep would create")
	}
	if n := countTasks(t, env, env.tenant+"-due"); n != 0 {
		t.Fatalf("ListDue must not create tasks, found %d", n)
	}
}

func TestPostgresTaskOutcomeFindingsStalePartsAndStuck(t *testing.T) {
	env := openReconcileEnv(t)
	ctx := context.Background()
	parts := postgres.NewMultipartUploadRepository(env.db)

	// A task finishes and records what it left behind.
	env.session(t, "outcome", model.UploadStatusCompleting)
	taskID := env.task(t, "outcome", 5)
	now := time.Now().UTC()
	if _, err := env.tasks.Claim(ctx, "worker-a", now, time.Minute, 50); err != nil {
		t.Fatal(err)
	}
	if err := env.tasks.Finish(ctx, taskID, "worker-a", now, model.ReconcileSucceeded, "COMPLETED", ""); err != nil {
		t.Fatal(err)
	}
	var outcome string
	if err := env.db.Pool.QueryRow(ctx, `SELECT outcome FROM reconciliation_tasks WHERE id = $1`, taskID).Scan(&outcome); err != nil || outcome != "COMPLETED" {
		t.Fatalf("task outcome not stored: %q, %v", outcome, err)
	}

	// A finding is stored with its action and result.
	findingID := env.tenant + "-finding"
	if err := env.tasks.RecordFinding(ctx, model.ReconciliationFinding{
		ID: findingID, Kind: model.FindingOrphanSession, TenantID: env.tenant, ObjectKey: "quarantine/" + env.tenant + "/x",
		StorageUploadID: "provider-x", Action: model.ActionAbortSession, Outcome: model.FindingDone,
		Reason: "no upload session exists for this key", CreatedAt: now,
	}); err != nil {
		t.Fatalf("record finding: %v", err)
	}
	t.Cleanup(func() {
		_, _ = env.db.Pool.Exec(context.Background(), `DELETE FROM reconciliation_findings WHERE id = $1`, findingID)
	})

	// Part rows on a finished session are stale; on a live one they are not.
	env.session(t, "stale", model.UploadStatusFailed)
	env.session(t, "live", model.UploadStatusUploading)
	for _, id := range []string{"stale", "live"} {
		if err := parts.UpsertPart(ctx, env.tenant, model.UploadPart{
			TenantID: env.tenant, UploadID: env.tenant + "-" + id, PartNumber: 1, ETag: `"e"`, SizeBytes: 5, Status: model.UploadPartUploaded,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := env.db.Pool.Exec(ctx, `UPDATE upload_sessions SET updated_at = $2 WHERE tenant_id = $1`, env.tenant, farPast); err != nil {
		t.Fatal(err)
	}
	refs, err := env.tasks.StaleParts(ctx, now, 1000)
	if err != nil {
		t.Fatal(err)
	}
	stale := map[string]bool{}
	for _, r := range refs {
		stale[r.UploadID] = true
	}
	if !stale[env.tenant+"-stale"] || stale[env.tenant+"-live"] {
		t.Fatalf("expected only the FAILED session's parts as stale, got %v", stale)
	}

	stuck, err := env.tasks.CountStuck(ctx, now)
	if err != nil || stuck[model.UploadStatusUploading] < 1 || stuck[model.UploadStatusCompleting] < 1 {
		t.Fatalf("expected stuck UPLOADING and COMPLETING sessions, got %v, %v", stuck, err)
	}
}

func TestPostgresClaimsPageThroughLargeBacklogs(t *testing.T) {
	env := openReconcileEnv(t)
	ctx := context.Background()
	ours := map[string]bool{}
	for i := 0; i < 25; i++ {
		id := fmt.Sprintf("bulk-%02d", i)
		env.session(t, id, model.UploadStatusCompleting)
		ours[env.task(t, id, 5)] = true
	}

	now := time.Now().UTC()
	seen := map[string]bool{}
	for page := 0; page < 3; page++ {
		got, err := env.tasks.Claim(ctx, "worker-a", now, time.Minute, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) > 10 {
			t.Fatalf("a claim returned %d tasks, more than its limit", len(got))
		}
		for _, task := range got {
			if seen[task.ID] {
				t.Fatalf("task %s claimed twice", task.ID)
			}
			seen[task.ID] = true
		}
	}
	for id := range ours {
		if !seen[id] {
			t.Fatalf("task %s was never claimed across the pages", id)
		}
	}
}

// ─── MinIO: paginated provider listings ──────────────────────────────

func TestProviderListingsArePaginated(t *testing.T) {
	storage, bucket := openTestProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	prefix := fmt.Sprintf("quarantine/it-list-%d/", time.Now().UnixNano())

	// Objects: every provider must page through a listing.
	for i := 0; i < 5; i++ {
		key := fmt.Sprintf("%sobject-%d", prefix, i)
		putObject(t, storage, bucket, key, []byte("x"))
		t.Cleanup(func() { _ = storage.DeleteObject(context.Background(), bucket, key) })
	}
	seen, pages, cursor := 0, 0, ""
	for {
		page, err := storage.ListObjectsPage(ctx, bucket, prefix, cursor, 2)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Objects) > 2 {
			t.Fatalf("page of %d exceeds the limit", len(page.Objects))
		}
		seen += len(page.Objects)
		pages++
		if page.Cursor == "" || pages > 10 {
			break
		}
		cursor = page.Cursor
	}
	if seen != 5 || pages < 3 {
		t.Fatalf("expected 5 objects over at least 3 pages, got %d over %d", seen, pages)
	}

	// Multipart sessions: listing one exact key works everywhere.
	key := prefix + "upload"
	id, err := storage.BeginMultipart(ctx, bucket, key, "application/octet-stream")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.AbortMultipart(context.Background(), bucket, key, id) })
	page, err := storage.ListMultipartUploadsPage(ctx, bucket, key, "", 10)
	if err != nil || len(page.Uploads) != 1 || page.Uploads[0].StorageUploadID != id {
		t.Fatalf("exact-key session listing: %+v, %v", page, err)
	}

	t.Run("prefix listing of sessions", func(t *testing.T) {
		if os.Getenv("TEST_S3_PREFIX_LISTING") != "true" {
			t.Skip("TEST_S3_PREFIX_LISTING is not true: this provider (e.g. MinIO) does not list multipart sessions by prefix; RECONCILIATION_SESSION_SCAN=database covers it")
		}
		page, err := storage.ListMultipartUploadsPage(ctx, bucket, prefix, "", 10)
		if err != nil || len(page.Uploads) != 1 {
			t.Fatalf("prefix listing: %+v, %v", page, err)
		}
	})
}

// putObject stores a small object through a single-part multipart upload,
// the only write path the adapter exposes.
func putObject(t *testing.T, storage *s3storage.ObjectStorage, bucket, key string, data []byte) {
	t.Helper()
	ctx := context.Background()
	id, err := storage.BeginMultipart(ctx, bucket, key, "application/octet-stream")
	if err != nil {
		t.Fatal(err)
	}
	url, err := storage.PresignPart(ctx, bucket, key, id, 1, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	etag := putPart(t, url, data)
	if err := storage.CompleteMultipart(ctx, bucket, key, id, []interfaces.CompletedPart{{PartNumber: 1, ETag: etag}}); err != nil {
		t.Fatal(err)
	}
}

// ─── PostgreSQL + MinIO: reproducible failure recovery ───────────────

// lostCompletion completes at the provider and then reports a timeout,
// like a response lost on the network.
type lostCompletion struct {
	*s3storage.ObjectStorage
	lost  bool
	calls int
}

func (l *lostCompletion) CompleteMultipart(ctx context.Context, bucket, key, uploadID string, parts []interfaces.CompletedPart) error {
	l.calls++
	if err := l.ObjectStorage.CompleteMultipart(ctx, bucket, key, uploadID, parts); err != nil {
		return err
	}
	if !l.lost {
		l.lost = true
		return errors.New("complete multipart upload: request timeout")
	}
	return nil
}

func TestLocalStackRecoversLostCompletionAndLeftoverSessions(t *testing.T) {
	env := openReconcileEnv(t)
	provider, bucket := openTestProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	storage := &lostCompletion{ObjectStorage: provider}
	multipart, err := uploadservice.NewMultipartService(env.uploads, postgres.NewMultipartUploadRepository(env.db), storage,
		uploadservice.MultipartServiceConfig{Bucket: bucket, MaxSize: 1 << 30, SessionTTL: time.Hour, PartURLTTL: 5 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}

	// 1. Upload two parts through presigned URLs, then lose the completion response.
	first := bytes.Repeat([]byte("r"), 5*1024*1024)
	second := []byte("recovered tail")
	digest := sha256.Sum256(append(append([]byte{}, first...), second...))
	started, err := multipart.Start(ctx, uploadservice.StartMultipartInput{
		TenantID: env.tenant, OwnerID: "owner", FileName: "f.bin", ContentType: "application/octet-stream",
		SizeBytes: int64(len(first) + len(second)), ChecksumSHA256: hex.EncodeToString(digest[:]), IdempotencyKey: "it-lost",
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	id, key := started.Session.ID, started.Session.ObjectKey
	t.Cleanup(func() { _ = provider.DeleteObject(context.Background(), bucket, key) })

	urls, err := multipart.PresignParts(ctx, env.tenant, "owner", id, []int32{1, 2})
	if err != nil {
		t.Fatal(err)
	}
	etag1, etag2 := putPart(t, urls[0].URL, first), putPart(t, urls[1].URL, second)
	if err := multipart.RecordParts(ctx, env.tenant, "owner", id, []uploadservice.RecordPartInput{
		{PartNumber: 1, ETag: etag1, SizeBytes: int64(len(first))},
		{PartNumber: 2, ETag: etag2, SizeBytes: int64(len(second))},
	}); err != nil {
		t.Fatalf("record parts: %v", err)
	}
	_, err = multipart.Complete(ctx, env.tenant, "owner", id, uploadservice.CompleteMultipartInput{
		Parts: []uploadservice.CompletedPartInput{{PartNumber: 1, ETag: etag1}, {PartNumber: 2, ETag: etag2}},
	})
	if !errors.Is(err, uploadservice.ErrOperationPending) {
		t.Fatalf("expected the lost response to leave the upload pending, got %v", err)
	}

	// 2. An aborted upload whose provider session survived.
	env.session(t, "leftover", model.UploadStatusAborted)
	orphanKey := "quarantine/" + env.tenant + "/leftover" // the key env.session gives it
	orphanID, err := provider.BeginMultipart(ctx, bucket, orphanKey, "application/octet-stream")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = provider.AbortMultipart(context.Background(), bucket, orphanKey, orphanID) })

	// 3. One reconciliation cycle repairs both.
	svc, err := reconciliation.NewService(env.tasks, multipart, reconciliation.Config{
		Owner: "it-worker", StorageProvider: "minio", BatchSize: 100, Lease: time.Minute, Grace: 0,
		Retry:        retry.Policy{BaseDelay: time.Second, MaxDelay: time.Minute, MaxAttempts: 3},
		ScanPageSize: 100, ScanPages: 10, OrphanMinAge: time.Nanosecond, StuckAfter: time.Hour,
		// MinIO lists sessions only by exact key: use the database strategy.
		SessionScan: reconciliation.SessionScanDatabase, SessionLookback: time.Hour,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err != nil {
		t.Fatal(err)
	}
	stats, err := svc.RunOnce(ctx)
	if err != nil {
		t.Fatalf("reconciliation cycle: %v", err)
	}

	session, _ := env.uploads.GetByID(ctx, env.tenant, id)
	if session.Status != model.UploadStatusCompleted || storage.calls != 1 {
		t.Fatalf("expected COMPLETED with one provider completion, got %s after %d calls", session.Status, storage.calls)
	}
	if meta, err := provider.HeadObject(ctx, bucket, key); err != nil || meta.SizeBytes != int64(len(first)+len(second)) {
		t.Fatalf("the completed object must exist with its size: %+v, %v", meta, err)
	}

	aborted := false
	for _, f := range stats.Findings {
		aborted = aborted || (f.StorageUploadID == orphanID && f.Outcome == model.FindingDone)
	}
	if !aborted {
		t.Fatalf("the orphaned provider session must be aborted, findings %+v", stats.Findings)
	}
	if _, err := provider.ListParts(ctx, bucket, orphanKey, orphanID); !errors.Is(err, interfaces.ErrStorageUploadNotFound) {
		t.Fatalf("the orphan must be gone at the provider, got %v", err)
	}
	var recorded int
	if err := env.db.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM reconciliation_findings WHERE storage_upload_id = $1`, orphanID).Scan(&recorded); err != nil || recorded != 1 {
		t.Fatalf("the finding must be stored once, got %d, %v", recorded, err)
	}
	t.Cleanup(func() {
		_, _ = env.db.Pool.Exec(context.Background(), `DELETE FROM reconciliation_findings WHERE tenant_id = $1`, env.tenant)
	})
}

// failSave makes saving the provider upload ID fail, as a database outage
// right after the provider call would.
type failSave struct{ *postgres.UploadRepository }

func (failSave) SetStorageUploadID(context.Context, string, string, string, time.Time) error {
	return errors.New("database unavailable")
}

func TestLocalStackCleansUpInitiationWhoseIDWasNotSaved(t *testing.T) {
	env := openReconcileEnv(t)
	provider, bucket := openTestProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	multipart, err := uploadservice.NewMultipartService(failSave{env.uploads}, postgres.NewMultipartUploadRepository(env.db), provider,
		uploadservice.MultipartServiceConfig{Bucket: bucket, MaxSize: 1 << 30, SessionTTL: time.Hour, PartURLTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}

	_, err = multipart.Start(ctx, uploadservice.StartMultipartInput{
		TenantID: env.tenant, OwnerID: "owner", FileName: "f", ContentType: "application/octet-stream",
		SizeBytes: 10, ChecksumSHA256: strings.Repeat("a", 64), IdempotencyKey: "it-init",
	})
	if !errors.Is(err, uploadservice.ErrMultipartStartPending) || !strings.Contains(err.Error(), "provider session aborted") {
		t.Fatalf("expected a pending start with the provider session aborted, got %v", err)
	}

	session, err := env.uploads.GetByIdempotencyKey(ctx, env.tenant, "owner", "it-init")
	if err != nil {
		t.Fatal(err)
	}
	ids, err := provider.ListMultipartUploads(ctx, bucket, session.ObjectKey)
	if err != nil || len(ids) != 0 {
		t.Fatalf("no provider session may be left behind, got %v, %v", ids, err)
	}
}
