package upload

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// ─── Completion: idempotency, concurrency, part list ─────────────────

func TestRepeatedCompletionIsIdempotent(t *testing.T) {
	h := newHarness(t)
	session, etag1, etag2 := h.uploadedSession(t, "repeat-1")

	for i := 0; i < 3; i++ {
		got, err := h.svc.Complete(context.Background(), testTenant, testOwner, session.ID, completeInput(etag1, etag2))
		if err != nil || got.Status != model.UploadStatusCompleted {
			t.Fatalf("attempt %d: %s, %v", i+1, got.Status, err)
		}
	}
	if h.storage.CompleteCalls != 1 {
		t.Fatalf("the provider must complete once, got %d calls", h.storage.CompleteCalls)
	}
	if h.session(t, session.ID).CompletedAt == nil {
		t.Fatal("the completion time must be recorded")
	}
}

func TestConcurrentCompletionCallsTheProviderOnce(t *testing.T) {
	h := newHarness(t)
	session, etag1, etag2 := h.uploadedSession(t, "concurrent-1")

	var wg sync.WaitGroup
	errs := make([]error, 10)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = h.svc.Complete(context.Background(), testTenant, testOwner, session.ID, completeInput(etag1, etag2))
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		// Losers answer as retries of the winner: done, or still pending.
		if err != nil && !errors.Is(err, ErrOperationPending) {
			t.Fatalf("request %d: %v", i, err)
		}
	}
	if h.storage.CompleteCalls != 1 || h.session(t, session.ID).Status != model.UploadStatusCompleted {
		t.Fatalf("expected one provider completion and COMPLETED, got %d calls, %s", h.storage.CompleteCalls, h.session(t, session.ID).Status)
	}
}

func TestCompletionPartListProblemsAreNamed(t *testing.T) {
	cases := []struct {
		name  string
		parts func(e1, e2 string) []CompletedPartInput
		want  string
	}{
		{"duplicate", func(e1, e2 string) []CompletedPartInput {
			return []CompletedPartInput{{1, e1}, {1, e1}}
		}, "duplicate part number 1"},
		{"out of order", func(e1, e2 string) []CompletedPartInput {
			return []CompletedPartInput{{2, e2}, {1, e1}}
		}, "ascending order"},
		{"missing ETag", func(e1, e2 string) []CompletedPartInput {
			return []CompletedPartInput{{1, ""}, {2, e2}}
		}, "ETag of part 1"},
		{"mismatched ETag", func(e1, e2 string) []CompletedPartInput {
			return []CompletedPartInput{{1, `"0000"`}, {2, e2}}
		}, "ETag of part 1"},
		{"part never uploaded", func(e1, e2 string) []CompletedPartInput {
			return []CompletedPartInput{{1, e1}, {2, e2}, {3, e2}}
		}, "part 3 was not uploaded"},
		{"uploaded part left out", func(e1, e2 string) []CompletedPartInput {
			return []CompletedPartInput{{1, e1}}
		}, "uploaded part 2 is missing"},
		{"out of range", func(e1, e2 string) []CompletedPartInput {
			return []CompletedPartInput{{0, e1}}
		}, "out of range"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			session, etag1, etag2 := h.uploadedSession(t, "list-"+c.name)

			_, err := h.svc.Complete(context.Background(), testTenant, testOwner, session.ID,
				CompleteMultipartInput{Parts: c.parts(etag1, etag2)})
			if !errors.Is(err, ErrInvalidCompletion) || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want an error mentioning %q", err, c.want)
			}
			if h.storage.CompleteCalls != 0 || h.session(t, session.ID).Status != model.UploadStatusUploading {
				t.Fatal("a refused list must not reach the provider or change the session")
			}
		})
	}
}

func TestSmallNonFinalPartIsRefused(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	small, last := []byte("tiny"), []byte("end")
	result, err := h.svc.Start(ctx, h.startInput("small-1", int64(len(small)+len(last)), sumOf(small, last)))
	if err != nil {
		t.Fatal(err)
	}
	e1 := h.storage.PutPart(result.Session.StorageUploadID, 1, small)
	e2 := h.storage.PutPart(result.Session.StorageUploadID, 2, last)
	if err := h.svc.RecordParts(ctx, testTenant, testOwner, result.Session.ID, []RecordPartInput{
		{PartNumber: 1, ETag: e1, SizeBytes: int64(len(small))}, {PartNumber: 2, ETag: e2, SizeBytes: int64(len(last))},
	}); err != nil {
		t.Fatal(err)
	}

	_, err = h.svc.Complete(ctx, testTenant, testOwner, result.Session.ID, completeInput(e1, e2))
	if !errors.Is(err, ErrInvalidCompletion) || !strings.Contains(err.Error(), "smaller than 5 MiB") {
		t.Fatalf("expected the 5 MiB rule, got %v", err)
	}
}

func TestConfiguredSizeLimitAndProviderCeiling(t *testing.T) {
	h := newHarness(t)
	big := h.startInput("big-1", (1<<30)+1, strings.Repeat("a", 64))
	if _, err := h.svc.Start(context.Background(), big); !errors.Is(err, ErrUploadTooLarge) {
		t.Fatalf("the configured limit must apply, got %v", err)
	}
	if _, err := NewMultipartService(h.uploads, h.parts, h.storage, MultipartServiceConfig{
		Bucket: "b", MaxSize: 6 << 40, SessionTTL: time.Hour, PartURLTTL: time.Minute,
	}); err == nil {
		t.Fatal("a limit above the 5 TiB S3 object ceiling must be refused")
	}
}

// ─── Missing provider state ──────────────────────────────────────────

func TestActiveRecordWithMissingProviderSession(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	session, etag1, etag2 := h.uploadedSession(t, "gone-1")
	// The provider lost the session (lifecycle rule, manual cleanup).
	if err := h.storage.AbortMultipart(ctx, "test-bucket", session.ObjectKey, session.StorageUploadID); err != nil {
		t.Fatal(err)
	}

	if _, err := h.svc.Complete(ctx, testTenant, testOwner, session.ID, completeInput(etag1, etag2)); !errors.Is(err, ErrMultipartNotActive) {
		t.Fatalf("completion must see the session is gone, got %v", err)
	}

	h.clock = session.ExpiresAt.Add(time.Second)
	status, err := h.svc.Expire(ctx, testTenant, session.ID)
	if err != nil || status != model.UploadStatusExpired {
		t.Fatalf("expiry must finish even with the provider session gone: %s, %v", status, err)
	}
}

func TestCompletingRecordWithNeitherObjectNorSessionFails(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	session, etag1, etag2 := h.uploadedSession(t, "gone-2")
	h.storage.CompleteErr = errTimeout
	if _, err := h.svc.Complete(ctx, testTenant, testOwner, session.ID, completeInput(etag1, etag2)); !errors.Is(err, ErrOperationPending) {
		t.Fatal(err)
	}
	if err := h.storage.AbortMultipart(ctx, "test-bucket", session.ObjectKey, session.StorageUploadID); err != nil {
		t.Fatal(err)
	}

	status, err := h.svc.ResolveCompletion(ctx, testTenant, session.ID)
	if err != nil || status != model.UploadStatusFailed || h.session(t, session.ID).FailureReason != model.FailureUploadMissing {
		t.Fatalf("expected FAILED/provider_upload_missing, got %s, %v", status, err)
	}
}

func TestThrottledProviderLeavesStateForRetry(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	session, etag1, etag2 := h.uploadedSession(t, "throttle-1")
	h.storage.CompleteLost = errTimeout
	if _, err := h.svc.Complete(ctx, testTenant, testOwner, session.ID, completeInput(etag1, etag2)); !errors.Is(err, ErrOperationPending) {
		t.Fatal(err)
	}

	h.storage.HeadErr = errors.New("head object: provider error SlowDown")
	if _, err := h.svc.ResolveCompletion(ctx, testTenant, session.ID); err == nil {
		t.Fatal("a throttled check must report an error so the task is retried")
	}
	if h.session(t, session.ID).Status != model.UploadStatusCompleting {
		t.Fatal("a throttled check must not change the session")
	}

	if status, err := h.svc.ResolveCompletion(ctx, testTenant, session.ID); err != nil || status != model.UploadStatusCompleted {
		t.Fatalf("the retry must complete: %s, %v", status, err)
	}
}

// ─── Discrepancy scans ───────────────────────────────────────────────

func TestOrphanProviderSessionsAreAbortedOnlyWhenSafe(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	live, _, _ := h.uploadedSession(t, "live-1")

	orphan := h.storage.AddUpload("quarantine/tenant-1/no-such-upload")
	duplicate := h.storage.AddUpload(live.ObjectKey) // a second session for a live upload
	foreign := h.storage.AddUpload("quarantine/not-an-upload-key/x/y")

	h.clock = h.clock.Add(48 * time.Hour)
	young := h.storage.AddUpload("quarantine/tenant-1/just-started")

	opts := ScanOptions{PageSize: 100, MinAge: 24 * time.Hour, DeleteUnrecorded: true}
	findings, cursor, err := h.svc.ScanOrphanSessions(ctx, "", opts)
	if err != nil || cursor != "" {
		t.Fatalf("scan: %v, cursor %q", err, cursor)
	}

	aborted := map[string]bool{}
	for _, f := range findings {
		if f.Kind != model.FindingOrphanSession || f.Outcome != model.FindingDone {
			t.Fatalf("unexpected finding %+v", f)
		}
		aborted[f.StorageUploadID] = true
	}
	if !aborted[orphan] || !aborted[duplicate] || len(aborted) != 2 {
		t.Fatalf("expected exactly the orphan and the duplicate aborted, got %v", aborted)
	}

	remaining := listUploadIDs(t, h)
	for _, id := range []string{live.StorageUploadID, foreign, young} {
		if !remaining[id] {
			t.Fatalf("%s must be left alone (live, not ours, or too young)", id)
		}
	}
}

func TestScanDryRunChangesNothing(t *testing.T) {
	h := newHarness(t)
	h.storage.AddUpload("quarantine/tenant-1/ghost")
	h.storage.PutObject("quarantine/tenant-1/ghost-object", []byte("x"))
	h.clock = h.clock.Add(48 * time.Hour)
	opts := ScanOptions{PageSize: 100, MinAge: time.Hour, DryRun: true, DeleteUnrecorded: true}

	sessions, _, err := h.svc.ScanOrphanSessions(context.Background(), "", opts)
	if err != nil || len(sessions) != 1 || sessions[0].Outcome != model.FindingPlanned {
		t.Fatalf("expected one planned abort, got %+v, %v", sessions, err)
	}
	objects, _, err := h.svc.ScanOrphanObjects(context.Background(), "", opts)
	if err != nil || len(objects) != 1 || objects[0].Outcome != model.FindingPlanned {
		t.Fatalf("expected one planned delete, got %+v, %v", objects, err)
	}
	if h.storage.UploadCount() != 1 || h.storage.AbortCalls != 0 {
		t.Fatal("a dry run must not abort anything")
	}
	if _, ok := h.storage.Object("quarantine/tenant-1/ghost-object"); !ok {
		t.Fatal("a dry run must not delete anything")
	}
}

func TestScanPaginatesAndRepeatsWithoutSideEffects(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < 25; i++ {
		h.storage.AddUpload(fmt.Sprintf("quarantine/tenant-1/orphan-%02d", i))
	}
	h.clock = h.clock.Add(48 * time.Hour)
	opts := ScanOptions{PageSize: 10, MinAge: time.Hour, DeleteUnrecorded: true}

	total, pages, cursor := 0, 0, ""
	for {
		findings, next, err := h.svc.ScanOrphanSessions(context.Background(), cursor, opts)
		if err != nil {
			t.Fatal(err)
		}
		total += len(findings)
		pages++
		if next == "" {
			break
		}
		cursor = next
	}
	if total != 25 || pages != 3 || h.storage.UploadCount() != 0 {
		t.Fatalf("expected 25 aborts over 3 pages, got %d over %d (%d left)", total, pages, h.storage.UploadCount())
	}

	aborts := h.storage.AbortCalls
	again, _, err := h.svc.ScanOrphanSessions(context.Background(), "", opts)
	if err != nil || len(again) != 0 || h.storage.AbortCalls != aborts {
		t.Fatalf("a repeated scan must find and abort nothing more: %d findings", len(again))
	}
}

func TestOrphanObjectsRespectOwnershipAndRetention(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	aborted, _, _ := h.uploadedSession(t, "obj-aborted")
	if _, err := h.svc.Abort(ctx, testTenant, testOwner, aborted.ID); err != nil {
		t.Fatal(err)
	}
	h.storage.PutObject(aborted.ObjectKey, []byte("leftover"))

	failed, _, _ := h.uploadedSession(t, "obj-failed")
	if err := h.uploads.TransitionStatus(ctx, testTenant, failed.ID, []model.UploadStatus{model.UploadStatusUploading},
		model.UploadStatusFailed, model.FailureChecksumMismatch, h.clock); err != nil {
		t.Fatal(err)
	}
	h.storage.PutObject(failed.ObjectKey, []byte("evidence"))
	h.storage.PutObject("quarantine/tenant-1/unknown-upload", []byte("orphan"))
	h.clock = h.clock.Add(48 * time.Hour)

	findings, _, err := h.svc.ScanOrphanObjects(ctx, "", ScanOptions{PageSize: 100, MinAge: time.Hour, DeleteUnrecorded: true})
	if err != nil || len(findings) != 2 {
		t.Fatalf("expected 2 orphan objects, got %+v, %v", findings, err)
	}
	if _, ok := h.storage.Object(aborted.ObjectKey); ok {
		t.Fatal("an aborted session's object must be removed")
	}
	if _, ok := h.storage.Object("quarantine/tenant-1/unknown-upload"); ok {
		t.Fatal("an object without a session must be removed")
	}
	if _, ok := h.storage.Object(failed.ObjectKey); !ok {
		t.Fatal("a FAILED session's object is kept as evidence")
	}
}

func TestUnrecordedItemsAreOnlyReportedByDefault(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	// Another environment sharing the bucket may own these.
	h.storage.AddUpload("quarantine/tenant-1/someone-elses-upload")
	h.storage.PutObject("quarantine/tenant-1/someone-elses-object", []byte("theirs"))

	aborted, _, _ := h.uploadedSession(t, "ours-aborted")
	if _, err := h.svc.Abort(ctx, testTenant, testOwner, aborted.ID); err != nil {
		t.Fatal(err)
	}
	h.storage.PutObject(aborted.ObjectKey, []byte("leftover"))
	h.clock = h.clock.Add(48 * time.Hour)

	opts := ScanOptions{PageSize: 100, MinAge: time.Hour} // DeleteUnrecorded off
	sessions, _, err := h.svc.ScanOrphanSessions(ctx, "", opts)
	if err != nil || len(sessions) != 1 || sessions[0].Outcome != model.FindingReported {
		t.Fatalf("an unrecorded session must only be reported, got %+v, %v", sessions, err)
	}
	if _, _, err := h.svc.ScanOrphanObjects(ctx, "", opts); err != nil {
		t.Fatal(err)
	}

	if h.storage.UploadCount() != 1 {
		t.Fatal("an unrecorded provider session must be left in place")
	}
	if _, ok := h.storage.Object("quarantine/tenant-1/someone-elses-object"); !ok {
		t.Fatal("an unrecorded object must be left in place")
	}
	// Ownership proven by our own ABORTED session: still cleaned up.
	if _, ok := h.storage.Object(aborted.ObjectKey); ok {
		t.Fatal("an object of our own aborted session must still be removed")
	}
}

func TestStalePartsOfFinishedSessionsAreRemoved(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	session, _, _ := h.uploadedSession(t, "stale-1")
	if err := h.uploads.TransitionStatus(ctx, testTenant, session.ID, []model.UploadStatus{model.UploadStatusUploading},
		model.UploadStatusFailed, model.FailureUploadMissing, h.clock); err != nil {
		t.Fatal(err)
	}
	live, _, _ := h.uploadedSession(t, "stale-2")

	findings, err := h.svc.CleanStaleParts(ctx, []model.UploadRef{
		{TenantID: testTenant, UploadID: session.ID},
		{TenantID: testTenant, UploadID: live.ID}, // listed earlier, but now live: left alone
	}, false)
	if err != nil || len(findings) != 1 || findings[0].Outcome != model.FindingDone {
		t.Fatalf("expected one cleanup, got %+v, %v", findings, err)
	}
	if parts, _ := h.parts.ListParts(ctx, testTenant, session.ID); len(parts) != 0 {
		t.Fatal("stale parts must be removed")
	}
	if parts, _ := h.parts.ListParts(ctx, testTenant, live.ID); len(parts) != 2 {
		t.Fatal("a live session's parts must be kept")
	}
}

func listUploadIDs(t *testing.T, h *harness) map[string]bool {
	t.Helper()
	page, err := h.storage.ListMultipartUploadsPage(context.Background(), "test-bucket", "", "", 1000)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, u := range page.Uploads {
		out[u.StorageUploadID] = true
	}
	return out
}

func sumOf(parts ...[]byte) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write(p)
	}
	return hex.EncodeToString(h.Sum(nil))
}
