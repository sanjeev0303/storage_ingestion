package upload

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// startedWith starts an upload declaring startSum (empty: none) and stores
// both parts, as a client hashing while it uploads would.
func (h *harness) startedWith(t *testing.T, key, startSum string) (model.UploadSession, CompleteMultipartInput, string) {
	t.Helper()
	ctx := context.Background()
	first, second, sum := twoParts()
	result, err := h.svc.Start(ctx, h.startInput(key, int64(len(first)+len(second)), startSum))
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
		t.Fatalf("record parts: %v", err)
	}
	return h.session(t, id), completeInput(etag1, etag2), sum
}

func TestAChecksumGivenAtCompletionIsRecordedAndVerified(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	session, input, sum := h.startedWith(t, "late", "")
	if session.ChecksumSHA256 != "" {
		t.Fatalf("start recorded checksum %q, want none yet", session.ChecksumSHA256)
	}

	input.ChecksumSHA256 = strings.ToUpper(sum)
	done, err := h.svc.Complete(ctx, testTenant, testOwner, session.ID, input)
	if err != nil || done.Status != model.UploadStatusCompleted {
		t.Fatalf("complete: %v, %s", err, done.Status)
	}
	if got := h.session(t, session.ID).ChecksumSHA256; got != sum {
		t.Fatalf("recorded checksum %q, want %q (lowercase)", got, sum)
	}
	if status, err := h.svc.VerifyIntegrity(ctx, testTenant, session.ID); err != nil || status != model.UploadStatusVerified {
		t.Fatalf("verify: %v, %s; want VERIFIED", err, status)
	}
}

func TestAWrongChecksumGivenAtCompletionFailsVerification(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	session, input, _ := h.startedWith(t, "late-wrong", "")
	input.ChecksumSHA256 = strings.Repeat("0", 64)
	if _, err := h.svc.Complete(ctx, testTenant, testOwner, session.ID, input); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if status, _ := h.svc.VerifyIntegrity(ctx, testTenant, session.ID); status != model.UploadStatusFailed {
		t.Fatalf("verify: %s; want FAILED, never promotable", status)
	}
}

func TestCompletionNeedsAChecksumAndCannotContradictTheOneGivenAtStart(t *testing.T) {
	_, _, sum := twoParts()
	cases := []struct {
		name, atStart, atEnd string
		ok                   bool
	}{
		{"neither", "", "", false},
		{"only at start", sum, "", true},
		{"same at both", sum, sum, true},
		{"different at completion", sum, strings.Repeat("0", 64), false},
		{"not hex", "", strings.Repeat("z", 64), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			session, input, _ := h.startedWith(t, c.name, c.atStart)
			input.ChecksumSHA256 = c.atEnd
			_, err := h.svc.Complete(context.Background(), testTenant, testOwner, session.ID, input)
			if c.ok && err != nil {
				t.Fatalf("complete: %v", err)
			}
			if !c.ok {
				if !errors.Is(err, ErrInvalidRequest) {
					t.Fatalf("err = %v, want an invalid request", err)
				}
				if s := h.session(t, session.ID); s.Status != model.UploadStatusUploading {
					t.Fatalf("session is %s after a refused completion; want it still UPLOADING", s.Status)
				}
			}
		})
	}
}

func TestStartingAgainWithoutAChecksumIsTheSameUpload(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	session, input, sum := h.startedWith(t, "retry", "")
	input.ChecksumSHA256 = sum
	if _, err := h.svc.Complete(ctx, testTenant, testOwner, session.ID, input); err != nil {
		t.Fatal(err)
	}
	first, second, _ := twoParts()
	again, err := h.svc.Start(ctx, h.startInput("retry", int64(len(first)+len(second)), ""))
	if err != nil || again.Session.ID != session.ID {
		t.Fatalf("retried start: %v, %q; want the same upload %q", err, again.Session.ID, session.ID)
	}
}
