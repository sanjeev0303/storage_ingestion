package upload

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	storagememory "github.com/sanjeev0303/object-storage-platform/internal/infrastructure/storage/memory"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/repository/memory"
)

const (
	testTenant = "tenant-1"
	testOwner  = "user-1"
)

var errTimeout = errors.New("request timeout")

// harness wires a MultipartService to in-memory repositories and provider.
type harness struct {
	svc     *MultipartService
	uploads *memory.UploadRepository
	parts   *memory.MultipartUploadRepository
	storage *storagememory.ObjectStorage
	clock   time.Time
	nextID  int
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	h := &harness{
		uploads: memory.NewUploadRepository(),
		parts:   memory.NewMultipartUploadRepository(),
		storage: storagememory.NewObjectStorage(),
		clock:   time.Date(2026, 10, 16, 10, 0, 0, 0, time.UTC),
	}
	svc, err := NewMultipartService(h.uploads, h.parts, h.storage, MultipartServiceConfig{
		Bucket:     "test-bucket",
		MaxSize:    1 << 30,
		SessionTTL: 30 * time.Minute,
		PartURLTTL: 15 * time.Minute,
	})
	if err != nil {
		t.Fatalf("new multipart service: %v", err)
	}
	svc.now = func() time.Time { return h.clock }
	h.storage.Now = func() time.Time { return h.clock }
	svc.newID = func() (string, error) {
		h.nextID++
		return fmt.Sprintf("upload-%d", h.nextID), nil
	}
	h.svc = svc
	return h
}

// twoParts is a 5 MiB part plus a short final part, the smallest valid
// multipart object.
func twoParts() ([]byte, []byte, string) {
	first := bytes.Repeat([]byte("a"), int(minNonFinalPartSize))
	second := []byte("final part")
	sum := sha256.Sum256(append(append([]byte{}, first...), second...))
	return first, second, hex.EncodeToString(sum[:])
}

func (h *harness) startInput(key string, size int64, checksum string) StartMultipartInput {
	return StartMultipartInput{
		TenantID:       testTenant,
		OwnerID:        testOwner,
		FileName:       "video.mp4",
		ContentType:    "video/mp4",
		SizeBytes:      size,
		ChecksumSHA256: checksum,
		IdempotencyKey: key,
	}
}

func (h *harness) session(t *testing.T, id string) model.UploadSession {
	t.Helper()
	s, err := h.uploads.GetByID(context.Background(), testTenant, id)
	if err != nil {
		t.Fatalf("get session %s: %v", id, err)
	}
	return *s
}

// uploadedSession starts an upload and stores both parts at the provider,
// recording them as a client would. It returns the session and part ETags.
func (h *harness) uploadedSession(t *testing.T, key string) (model.UploadSession, string, string) {
	t.Helper()
	ctx := context.Background()
	first, second, sum := twoParts()

	result, err := h.svc.Start(ctx, h.startInput(key, int64(len(first)+len(second)), sum))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	id := result.Session.ID
	etag1 := h.storage.PutPart(result.Session.StorageUploadID, 1, first)
	etag2 := h.storage.PutPart(result.Session.StorageUploadID, 2, second)

	err = h.svc.RecordParts(ctx, testTenant, testOwner, id, []RecordPartInput{
		{PartNumber: 1, ETag: etag1, SizeBytes: int64(len(first))},
		{PartNumber: 2, ETag: etag2, SizeBytes: int64(len(second))},
	})
	if err != nil {
		t.Fatalf("record parts: %v", err)
	}
	return h.session(t, id), etag1, etag2
}

func completeInput(etag1, etag2 string) CompleteMultipartInput {
	return CompleteMultipartInput{Parts: []CompletedPartInput{
		{PartNumber: 1, ETag: etag1},
		{PartNumber: 2, ETag: etag2},
	}}
}

// ─── Initiation ──────────────────────────────────────────────────────

func TestAmbiguousInitiationIsAdoptedNotRepeated(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	_, _, sum := twoParts()
	input := h.startInput("start-1", 10*1024*1024, sum)

	// The provider created the upload but the response was lost.
	h.storage.BeginLost = errTimeout
	_, err := h.svc.Start(ctx, input)
	if !errors.Is(err, ErrMultipartStartPending) {
		t.Fatalf("expected pending start, got %v", err)
	}

	// A client retry must not initiate a second upload.
	if _, err := h.svc.Start(ctx, input); !errors.Is(err, ErrMultipartStartPending) {
		t.Fatalf("expected retry to stay pending, got %v", err)
	}
	if h.storage.BeginCalls != 1 {
		t.Fatalf("BeginMultipart called %d times, want 1", h.storage.BeginCalls)
	}

	status, err := h.svc.ResolveInitiation(ctx, testTenant, "upload-1")
	if err != nil || status != model.UploadStatusPending {
		t.Fatalf("resolve initiation: status %s, err %v", status, err)
	}
	if h.session(t, "upload-1").StorageUploadID == "" {
		t.Fatal("expected the provider upload to be adopted")
	}

	reused, err := h.svc.Start(ctx, input)
	if err != nil || !reused.Reused {
		t.Fatalf("expected the retry to reuse the adopted session, got %+v, %v", reused, err)
	}
	if h.storage.BeginCalls != 1 {
		t.Fatalf("BeginMultipart called %d times after recovery, want 1", h.storage.BeginCalls)
	}
}

func TestInitiationThatNeverHappenedFailsTheSession(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	_, _, sum := twoParts()
	input := h.startInput("start-2", 10*1024*1024, sum)

	h.storage.BeginErr = errTimeout
	if _, err := h.svc.Start(ctx, input); !errors.Is(err, ErrMultipartStartPending) {
		t.Fatalf("expected pending start, got %v", err)
	}

	status, err := h.svc.ResolveInitiation(ctx, testTenant, "upload-1")
	if err != nil || status != model.UploadStatusFailed {
		t.Fatalf("resolve initiation: status %s, err %v", status, err)
	}
	if got := h.session(t, "upload-1").FailureReason; got != model.FailureInitiationFailed {
		t.Fatalf("failure reason %q", got)
	}
	if _, err := h.svc.Start(ctx, input); !errors.Is(err, ErrMultipartNotActive) {
		t.Fatalf("expected a failed initiation to stay failed, got %v", err)
	}
}

// failOnceSetStorageID makes saving the provider upload ID fail once, as
// a database outage right after the provider call would.
type failOnceSetStorageID struct {
	*memory.UploadRepository
	failed bool
}

func (r *failOnceSetStorageID) SetStorageUploadID(ctx context.Context, tenantID, uploadID, storageID string, at time.Time) error {
	if !r.failed {
		r.failed = true
		return errors.New("database unavailable")
	}
	return r.UploadRepository.SetStorageUploadID(ctx, tenantID, uploadID, storageID, at)
}

func TestProviderIDSaveFailureCleansUpTheProviderSession(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.svc.uploads = &failOnceSetStorageID{UploadRepository: h.uploads}
	_, _, sum := twoParts()

	if _, err := h.svc.Start(ctx, h.startInput("start-3", 10*1024*1024, sum)); !errors.Is(err, ErrMultipartStartPending) {
		t.Fatalf("expected pending start, got %v", err)
	}
	if h.storage.UploadCount() != 0 || h.storage.AbortCalls != 1 {
		t.Fatalf("the unusable provider session must be aborted: uploads %d, aborts %d", h.storage.UploadCount(), h.storage.AbortCalls)
	}

	// Reconciliation then finds nothing at the provider and closes the session.
	status, err := h.svc.ResolveInitiation(ctx, testTenant, "upload-1")
	if err != nil || status != model.UploadStatusFailed {
		t.Fatalf("resolve initiation: %s, %v", status, err)
	}
}

func TestProviderIDSaveAndCleanupFailureIsRecoveredLater(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.svc.uploads = &failOnceSetStorageID{UploadRepository: h.uploads}
	h.storage.AbortErr = errTimeout
	_, _, sum := twoParts()

	if _, err := h.svc.Start(ctx, h.startInput("start-5", 10*1024*1024, sum)); !errors.Is(err, ErrMultipartStartPending) {
		t.Fatalf("expected pending start, got %v", err)
	}
	if h.storage.UploadCount() != 1 {
		t.Fatal("a failed cleanup must leave the provider session for reconciliation")
	}

	// The PENDING session (tenant, key) is enough to find and adopt it.
	if _, err := h.svc.ResolveInitiation(ctx, testTenant, "upload-1"); err != nil {
		t.Fatalf("resolve initiation: %v", err)
	}
	if h.session(t, "upload-1").StorageUploadID != "provider-upload-1" {
		t.Fatal("expected the leftover provider session to be adopted")
	}
}

func TestDuplicateProviderUploadsAreAbortedAndSessionFailed(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	_, _, sum := twoParts()

	h.storage.BeginErr = errTimeout
	_, _ = h.svc.Start(ctx, h.startInput("start-4", 10*1024*1024, sum))
	key := h.session(t, "upload-1").ObjectKey
	h.storage.AddUpload(key)
	h.storage.AddUpload(key)

	status, err := h.svc.ResolveInitiation(ctx, testTenant, "upload-1")
	if err != nil || status != model.UploadStatusFailed {
		t.Fatalf("status %s, err %v", status, err)
	}
	if h.storage.UploadCount() != 0 {
		t.Fatalf("expected both provider uploads aborted, %d left", h.storage.UploadCount())
	}
}

// ─── Parts ───────────────────────────────────────────────────────────

func TestRecordPartsTrustsOnlyTheProvider(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	first, _, sum := twoParts()

	result, err := h.svc.Start(ctx, h.startInput("parts-1", 10*1024*1024, sum))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	id := result.Session.ID

	claim := []RecordPartInput{{PartNumber: 1, ETag: `"made-up"`, SizeBytes: int64(len(first))}}
	if err := h.svc.RecordParts(ctx, testTenant, testOwner, id, claim); !errors.Is(err, ErrInvalidPart) {
		t.Fatalf("a part the provider does not hold must be refused, got %v", err)
	}

	etag := h.storage.PutPart(result.Session.StorageUploadID, 1, first)
	wrongSize := []RecordPartInput{{PartNumber: 1, ETag: etag, SizeBytes: 1}}
	if err := h.svc.RecordParts(ctx, testTenant, testOwner, id, wrongSize); !errors.Is(err, ErrInvalidPart) {
		t.Fatalf("conflicting part metadata must be refused, got %v", err)
	}

	good := []RecordPartInput{{PartNumber: 1, ETag: etag, SizeBytes: int64(len(first))}}
	for i := 0; i < 2; i++ {
		if err := h.svc.RecordParts(ctx, testTenant, testOwner, id, good); err != nil {
			t.Fatalf("confirmation %d: %v", i+1, err)
		}
	}
	recorded, _ := h.parts.ListParts(ctx, testTenant, id)
	if len(recorded) != 1 || h.session(t, id).Status != model.UploadStatusUploading {
		t.Fatalf("expected one recorded part and UPLOADING, got %d parts, %s", len(recorded), h.session(t, id).Status)
	}
}

func TestCompleteRecoversPartsThatWereNeverRecorded(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	first, second, sum := twoParts()

	result, err := h.svc.Start(ctx, h.startInput("parts-2", int64(len(first)+len(second)), sum))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	id := result.Session.ID
	etag1 := h.storage.PutPart(result.Session.StorageUploadID, 1, first)
	etag2 := h.storage.PutPart(result.Session.StorageUploadID, 2, second)

	// Only part 1's confirmation reached Neon.
	if err := h.svc.RecordParts(ctx, testTenant, testOwner, id,
		[]RecordPartInput{{PartNumber: 1, ETag: etag1, SizeBytes: int64(len(first))}}); err != nil {
		t.Fatalf("record part 1: %v", err)
	}

	session, err := h.svc.Complete(ctx, testTenant, testOwner, id, completeInput(etag1, etag2))
	if err != nil || session.Status != model.UploadStatusCompleted {
		t.Fatalf("complete: status %s, err %v", session.Status, err)
	}
	recorded, _ := h.parts.ListParts(ctx, testTenant, id)
	if len(recorded) != 2 {
		t.Fatalf("expected the unrecorded part to be recovered, got %d parts", len(recorded))
	}
}

func TestCompleteRejectsAnETagTheProviderDoesNotHold(t *testing.T) {
	h := newHarness(t)
	session, etag1, _ := h.uploadedSession(t, "parts-3")

	_, err := h.svc.Complete(context.Background(), testTenant, testOwner, session.ID, completeInput(etag1, `"forged"`))
	if !errors.Is(err, ErrInvalidCompletion) {
		t.Fatalf("expected invalid completion, got %v", err)
	}
	if h.session(t, session.ID).Status != model.UploadStatusUploading {
		t.Fatal("a refused completion must not change the session")
	}
}

// ─── Completion ──────────────────────────────────────────────────────

func TestLostCompletionResponseIsVerifiedNotRepeated(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	session, etag1, etag2 := h.uploadedSession(t, "complete-1")

	h.storage.CompleteLost = errTimeout
	got, err := h.svc.Complete(ctx, testTenant, testOwner, session.ID, completeInput(etag1, etag2))
	if !errors.Is(err, ErrOperationPending) || got.Status != model.UploadStatusCompleting {
		t.Fatalf("expected pending COMPLETING, got %s, %v", got.Status, err)
	}

	// A client retry while pending does not complete again.
	if _, err := h.svc.Complete(ctx, testTenant, testOwner, session.ID, completeInput(etag1, etag2)); !errors.Is(err, ErrOperationPending) {
		t.Fatalf("expected the retry to stay pending, got %v", err)
	}

	status, err := h.svc.ResolveCompletion(ctx, testTenant, session.ID)
	if err != nil || status != model.UploadStatusCompleted {
		t.Fatalf("resolve completion: %s, %v", status, err)
	}
	if h.storage.CompleteCalls != 1 {
		t.Fatalf("CompleteMultipart called %d times, want 1", h.storage.CompleteCalls)
	}
}

func TestCompletionThatNeverReachedTheProviderIsRetriedOnce(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	session, etag1, etag2 := h.uploadedSession(t, "complete-2")

	h.storage.CompleteErr = errTimeout
	if _, err := h.svc.Complete(ctx, testTenant, testOwner, session.ID, completeInput(etag1, etag2)); !errors.Is(err, ErrOperationPending) {
		t.Fatalf("expected pending, got %v", err)
	}

	status, err := h.svc.ResolveCompletion(ctx, testTenant, session.ID)
	if err != nil || status != model.UploadStatusCompleted {
		t.Fatalf("resolve completion: %s, %v", status, err)
	}
	if h.storage.CompleteCalls != 2 {
		t.Fatalf("CompleteMultipart called %d times, want 2", h.storage.CompleteCalls)
	}

	// Repeating reconciliation on a completed session changes nothing.
	again, err := h.svc.ResolveCompletion(ctx, testTenant, session.ID)
	if err != nil || again != model.UploadStatusCompleted || h.storage.CompleteCalls != 2 {
		t.Fatalf("repeat reconciliation: %s, %v, calls %d", again, err, h.storage.CompleteCalls)
	}
}

func TestCompletedObjectOfTheWrongSizeFails(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	session, etag1, etag2 := h.uploadedSession(t, "complete-3")

	h.storage.SizeOverride = 7
	_, err := h.svc.Complete(ctx, testTenant, testOwner, session.ID, completeInput(etag1, etag2))
	if !errors.Is(err, ErrObjectSizeMismatch) {
		t.Fatalf("expected size mismatch, got %v", err)
	}
	got := h.session(t, session.ID)
	if got.Status != model.UploadStatusFailed || got.FailureReason != model.FailureObjectSizeMismatch {
		t.Fatalf("expected FAILED/object_size_mismatch, got %s/%s", got.Status, got.FailureReason)
	}
}

func TestVerificationFailureAfterProviderCompletionIsReconciled(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	session, etag1, etag2 := h.uploadedSession(t, "complete-4")

	// The provider completes, but checking the assembled object fails, so
	// the session cannot move past COMPLETING in this request.
	h.storage.HeadErr = errTimeout
	if _, err := h.svc.Complete(ctx, testTenant, testOwner, session.ID, completeInput(etag1, etag2)); !errors.Is(err, ErrOperationPending) {
		t.Fatalf("expected pending, got %v", err)
	}
	if _, ok := h.storage.Object(session.ObjectKey); !ok {
		t.Fatal("expected the provider to have assembled the object")
	}

	status, err := h.svc.ResolveCompletion(ctx, testTenant, session.ID)
	if err != nil || status != model.UploadStatusCompleted || h.storage.CompleteCalls != 1 {
		t.Fatalf("resolve: %s, %v, completes %d", status, err, h.storage.CompleteCalls)
	}
}

// failOnceTransitionTo makes the first move to one status fail, as a
// database outage right after a provider call would.
type failOnceTransitionTo struct {
	*memory.UploadRepository
	to     model.UploadStatus
	failed bool
}

func (r *failOnceTransitionTo) TransitionStatus(ctx context.Context, tenantID, uploadID string, from []model.UploadStatus, to model.UploadStatus, reason string, at time.Time, events ...model.OutboxEvent) error {
	if to == r.to && !r.failed {
		r.failed = true
		return errors.New("database unavailable")
	}
	return r.UploadRepository.TransitionStatus(ctx, tenantID, uploadID, from, to, reason, at, events...)
}

func TestDatabaseFailureAfterProviderCompletionIsReconciled(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	session, etag1, etag2 := h.uploadedSession(t, "complete-5")
	h.svc.uploads = &failOnceTransitionTo{UploadRepository: h.uploads, to: model.UploadStatusCompleted}

	if _, err := h.svc.Complete(ctx, testTenant, testOwner, session.ID, completeInput(etag1, etag2)); !errors.Is(err, ErrOperationPending) {
		t.Fatalf("expected pending, got %v", err)
	}
	if h.session(t, session.ID).Status != model.UploadStatusCompleting {
		t.Fatal("expected the session to stay COMPLETING")
	}

	status, err := h.svc.ResolveCompletion(ctx, testTenant, session.ID)
	if err != nil || status != model.UploadStatusCompleted || h.storage.CompleteCalls != 1 {
		t.Fatalf("resolve: %s, %v, completes %d", status, err, h.storage.CompleteCalls)
	}
}

func TestDatabaseFailureAfterProviderAbortIsReconciled(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	session, _, _ := h.uploadedSession(t, "abort-2")
	h.svc.uploads = &failOnceTransitionTo{UploadRepository: h.uploads, to: model.UploadStatusAborted}

	if _, err := h.svc.Abort(ctx, testTenant, testOwner, session.ID); !errors.Is(err, ErrOperationPending) {
		t.Fatalf("expected pending, got %v", err)
	}
	if h.storage.UploadCount() != 0 || h.session(t, session.ID).Status != model.UploadStatusAborting {
		t.Fatal("expected the provider upload gone and the session still ABORTING")
	}

	status, err := h.svc.ResolveAbort(ctx, testTenant, session.ID)
	if err != nil || status != model.UploadStatusAborted {
		t.Fatalf("resolve abort: %s, %v", status, err)
	}
	if h.session(t, session.ID).AbortedAt == nil {
		t.Fatal("the abort confirmation time must be recorded")
	}
}

// ─── Integrity ───────────────────────────────────────────────────────

func TestVerifiedChecksumIsTheOnlyWayToPromotion(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	session, etag1, etag2 := h.uploadedSession(t, "verify-1")

	completed, err := h.svc.Complete(ctx, testTenant, testOwner, session.ID, completeInput(etag1, etag2))
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if completed.CanPromote() {
		t.Fatal("a completed but unverified upload must not be promotable")
	}

	status, err := h.svc.VerifyIntegrity(ctx, testTenant, session.ID)
	if err != nil || status != model.UploadStatusVerified {
		t.Fatalf("verify: %s, %v", status, err)
	}
	if !h.session(t, session.ID).CanPromote() {
		t.Fatal("a verified upload must be promotable")
	}
}

func TestChecksumMismatchFailsAndBlocksPromotion(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	first, second, _ := twoParts()
	wrong := hex.EncodeToString(make([]byte, 32))

	result, err := h.svc.Start(ctx, h.startInput("verify-2", int64(len(first)+len(second)), wrong))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	id := result.Session.ID
	etag1 := h.storage.PutPart(result.Session.StorageUploadID, 1, first)
	etag2 := h.storage.PutPart(result.Session.StorageUploadID, 2, second)
	if err := h.svc.RecordParts(ctx, testTenant, testOwner, id, []RecordPartInput{
		{PartNumber: 1, ETag: etag1, SizeBytes: int64(len(first))},
		{PartNumber: 2, ETag: etag2, SizeBytes: int64(len(second))},
	}); err != nil {
		t.Fatalf("record: %v", err)
	}
	if _, err := h.svc.Complete(ctx, testTenant, testOwner, id, completeInput(etag1, etag2)); err != nil {
		t.Fatalf("complete: %v", err)
	}

	status, err := h.svc.VerifyIntegrity(ctx, testTenant, id)
	if err != nil || status != model.UploadStatusFailed {
		t.Fatalf("verify: %s, %v", status, err)
	}
	got := h.session(t, id)
	if got.FailureReason != model.FailureChecksumMismatch || got.CanPromote() {
		t.Fatalf("expected a durable checksum failure that blocks promotion, got %s/%s", got.Status, got.FailureReason)
	}
	if _, ok := h.storage.Object(got.ObjectKey); !ok {
		t.Fatal("the object must stay in quarantine for the retention policy")
	}
}

func TestVerificationStopsOnCancelledContext(t *testing.T) {
	h := newHarness(t)
	session, etag1, etag2 := h.uploadedSession(t, "verify-3")
	if _, err := h.svc.Complete(context.Background(), testTenant, testOwner, session.ID, completeInput(etag1, etag2)); err != nil {
		t.Fatalf("complete: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := h.svc.VerifyIntegrity(ctx, testTenant, session.ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
	if h.session(t, session.ID).Status != model.UploadStatusCompleted {
		t.Fatal("an interrupted verification must leave the session COMPLETED for a retry")
	}
}

// ─── Abort, expiry, concurrency ──────────────────────────────────────

func TestAbortIsRetryableAndToleratesAnAlreadyAbsentUpload(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	session, _, _ := h.uploadedSession(t, "abort-1")

	h.storage.AbortErr = errTimeout
	if _, err := h.svc.Abort(ctx, testTenant, testOwner, session.ID); !errors.Is(err, ErrOperationPending) {
		t.Fatalf("expected pending abort, got %v", err)
	}
	if h.session(t, session.ID).Status != model.UploadStatusAborting {
		t.Fatal("expected ABORTING after a failed provider abort")
	}

	// The provider already removed the upload (for example the timed-out
	// call did succeed). The repeated abort must still finish.
	if err := h.storage.AbortMultipart(ctx, "test-bucket", session.ObjectKey, session.StorageUploadID); err != nil {
		t.Fatalf("direct abort: %v", err)
	}
	got, err := h.svc.Abort(ctx, testTenant, testOwner, session.ID)
	if err != nil || got.Status != model.UploadStatusAborted || got.FailureReason != model.FailureAbortedByClient {
		t.Fatalf("expected ABORTED/aborted_by_client, got %s/%s, %v", got.Status, got.FailureReason, err)
	}
	if recorded, _ := h.parts.ListParts(ctx, testTenant, session.ID); len(recorded) != 0 {
		t.Fatal("aborted part metadata must be removed")
	}
}

func TestConcurrentCompleteAndAbortFinalizeOnce(t *testing.T) {
	for i := 0; i < 20; i++ {
		h := newHarness(t)
		session, etag1, etag2 := h.uploadedSession(t, fmt.Sprintf("race-%d", i))

		var wg sync.WaitGroup
		var completeErr, abortErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, completeErr = h.svc.Complete(context.Background(), testTenant, testOwner, session.ID, completeInput(etag1, etag2))
		}()
		go func() {
			defer wg.Done()
			_, abortErr = h.svc.Abort(context.Background(), testTenant, testOwner, session.ID)
		}()
		wg.Wait()

		final := h.session(t, session.ID).Status
		switch {
		case completeErr == nil && abortErr == nil:
			t.Fatalf("run %d: both complete and abort succeeded (final %s)", i, final)
		case completeErr == nil && final != model.UploadStatusCompleted:
			t.Fatalf("run %d: completion won but final status is %s", i, final)
		case abortErr == nil && final != model.UploadStatusAborted:
			t.Fatalf("run %d: abort won but final status is %s", i, final)
		}
	}
}

func TestExpiredUploadRefusesClientsAndIsCleanedUp(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	session, etag1, etag2 := h.uploadedSession(t, "expire-1")

	h.clock = session.ExpiresAt.Add(time.Second)

	if _, err := h.svc.PresignParts(ctx, testTenant, testOwner, session.ID, []int32{3}); !errors.Is(err, ErrMultipartNotActive) {
		t.Fatalf("expired session must not get part URLs, got %v", err)
	}
	if _, err := h.svc.Complete(ctx, testTenant, testOwner, session.ID, completeInput(etag1, etag2)); !errors.Is(err, ErrMultipartNotActive) {
		t.Fatalf("expired session must not complete, got %v", err)
	}

	status, err := h.svc.Expire(ctx, testTenant, session.ID)
	if err != nil || status != model.UploadStatusExpired {
		t.Fatalf("expire: %s, %v", status, err)
	}
	if h.storage.UploadCount() != 0 {
		t.Fatal("the provider upload of an expired session must be aborted")
	}
}

func TestRecoveryIsTenantScoped(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	session, _, _ := h.uploadedSession(t, "tenant-1")

	if _, err := h.svc.Expire(ctx, "tenant-2", session.ID); !errors.Is(err, ErrMultipartUploadNotFound) {
		t.Fatalf("another tenant must not see the session, got %v", err)
	}
	if _, err := h.svc.PresignParts(ctx, testTenant, "someone-else", session.ID, []int32{1}); !errors.Is(err, ErrMultipartUploadNotFound) {
		t.Fatalf("another owner must not see the session, got %v", err)
	}
}
