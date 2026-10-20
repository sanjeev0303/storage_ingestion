package processing

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/retry"
)

func TestIntakeProcessesAnUploadOnceAndIsSafeToRepeat(t *testing.T) {
	h := newHarness()
	h.goodUpload(t, "u1")
	svc := h.service(t, "worker-a")
	ctx := context.Background()

	note, err := svc.Intake(ctx, tenant, "u1")
	if err != nil || note != "asset promoted" {
		t.Fatalf("intake: %q %v", note, err)
	}
	if asset := h.onlyAsset(t); asset.Status != model.AssetStatusAvailable {
		t.Fatalf("asset is %s", asset.Status)
	}

	// The event delivered again, and the poll after it, find the work done.
	note, err = svc.Intake(ctx, tenant, "u1")
	if err != nil || !strings.HasPrefix(note, "processing already ended") {
		t.Fatalf("repeated intake: %q %v", note, err)
	}
	if stats := h.run(t, svc); stats.Enqueued != 0 || stats.Claimed != 0 {
		t.Fatalf("the poll must find nothing left: %+v", stats)
	}
	if n := len(h.memJobs.Jobs()); n != 1 {
		t.Fatalf("one upload, one job; got %d", n)
	}
}

func TestIntakeLeavesAJobHeldElsewhereToItsWorker(t *testing.T) {
	h := newHarness()
	h.goodUpload(t, "u1")
	svc := h.service(t, "worker-a")
	ctx := context.Background()
	if _, err := h.jobs.EnqueueDue(ctx, h.clock, 3); err != nil {
		t.Fatal(err)
	}
	if claimed, err := h.jobs.Claim(ctx, "worker-b", h.clock, time.Minute, 10); err != nil || len(claimed) != 1 {
		t.Fatalf("claim: %v", err)
	}

	note, err := svc.Intake(ctx, tenant, "u1")
	if err != nil || !strings.Contains(note, "another worker") {
		t.Fatalf("a job another worker holds is left alone: %q %v", note, err)
	}
	if job := h.onlyJob(t); job.LeaseOwner != "worker-b" || job.AttemptCount != 1 {
		t.Fatalf("the other worker's lease must stand: %+v", job)
	}
}

func TestIntakeRefusesWhatItCannotProcess(t *testing.T) {
	h := newHarness()
	svc := h.service(t, "worker-a")
	ctx := context.Background()

	if _, err := svc.Intake(ctx, tenant, "no-such-upload"); !retry.IsPermanent(err) {
		t.Fatalf("an unknown upload can never be processed, got %v", err)
	}

	session := model.UploadSession{
		ID: "u2", TenantID: tenant, OwnerID: "user-1", ObjectKey: "quarantine/" + tenant + "/u2", FileName: "f",
		ContentType: "application/pdf", ExpectedSize: 4, Status: model.UploadStatusFailed,
		CreatedAt: h.clock, UpdatedAt: h.clock, ExpiresAt: h.clock.Add(time.Hour),
	}
	if err := h.uploads.Create(ctx, &session); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Intake(ctx, tenant, "u2"); !errors.Is(err, model.ErrStaleEvent) {
		t.Fatalf("an upload that is no longer completed makes the event stale, got %v", err)
	}
	if n := len(h.memJobs.Jobs()); n != 0 {
		t.Fatalf("no job may be created, got %d", n)
	}
}
