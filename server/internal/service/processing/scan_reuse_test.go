package processing

import (
	"context"
	"strings"
	"testing"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// jobFor returns the job of the asset made from one upload.
func (h *harness) jobFor(t *testing.T, uploadID string) model.ProcessingJob {
	t.Helper()
	for _, job := range h.memJobs.Jobs() {
		if job.UploadID == uploadID {
			return job
		}
	}
	t.Fatalf("no job for upload %s", uploadID)
	return model.ProcessingJob{}
}

func TestSameBytesAreNotScannedTwiceAtOneSignatureVersion(t *testing.T) {
	h := newHarness()
	h.scanner.version = "ClamAV 1.4.3/27800"
	svc := h.service(t, "worker-a")

	h.goodUpload(t, "up-1")
	h.run(t, svc)
	first := h.jobFor(t, "up-1")
	if first.ScannerVersion != "ClamAV 1.4.3/27800" || first.ScanReusedFrom != "" {
		t.Fatalf("a fresh clean scan records its version: %+v", first)
	}

	h.goodUpload(t, "up-2")
	h.run(t, svc)
	second := h.jobFor(t, "up-2")
	if h.scanner.calls != 1 {
		t.Fatalf("the same bytes were scanned %d times", h.scanner.calls)
	}
	if second.State != model.ProcessingSucceeded || second.ScanResult != model.ScanClean ||
		second.ScanReusedFrom != first.AssetID || second.ScannerVersion != "ClamAV 1.4.3/27800" {
		t.Fatalf("the second upload reuses the first verdict: %+v", second)
	}
	asset, err := h.assets.GetByID(context.Background(), tenant, second.AssetID)
	if err != nil || asset.Status != model.AssetStatusAvailable {
		t.Fatalf("the reused asset is published: %+v %v", asset, err)
	}
	if got := h.eventNames(t, second.AssetID); got != "verified,scan_reused,promoted" {
		t.Fatalf("the reuse is on record: %q", got)
	}

	// Another tenant never sees this tenant's verdicts.
	if _, ok, _ := h.memJobs.FindCleanScan(context.Background(), "tenant-2", "x", asset.ChecksumSHA256, asset.SizeBytes, "fake", "ClamAV 1.4.3/27800"); ok {
		t.Fatal("verdicts are reused within a tenant only")
	}
}

func TestNewSignaturesScanTheSameBytesAgain(t *testing.T) {
	h := newHarness()
	h.scanner.version = "ClamAV 1.4.3/27800"
	svc := h.service(t, "worker-a")
	h.goodUpload(t, "up-1")
	h.run(t, svc)

	h.scanner.version = "ClamAV 1.4.3/27801"
	h.goodUpload(t, "up-2")
	h.run(t, svc)
	second := h.jobFor(t, "up-2")
	if h.scanner.calls != 2 || second.ScanReusedFrom != "" || second.ScannerVersion != "ClamAV 1.4.3/27801" {
		t.Fatalf("new signatures mean a new scan: calls %d, job %+v", h.scanner.calls, second)
	}
}

func TestOtherBytesAreAlwaysScanned(t *testing.T) {
	h := newHarness()
	h.scanner.version = "ClamAV 1.4.3/27800"
	svc := h.service(t, "worker-a")
	h.goodUpload(t, "up-1")
	h.run(t, svc)

	other := []byte("%PDF-1.7\n" + strings.Repeat("z", 600))
	h.completedUpload(t, "up-2", other, sum(other), int64(len(other)), "application/pdf")
	h.run(t, svc)
	if h.scanner.calls != 2 || h.jobFor(t, "up-2").ScanReusedFrom != "" {
		t.Fatalf("different bytes are scanned: calls %d", h.scanner.calls)
	}
}

// A scan that saw other bytes than the declared ones is evidence about
// nothing: it is never reused, even though the scanner said CLEAN.
func TestAVerdictAboutSwappedBytesIsNeverReused(t *testing.T) {
	h := newHarness()
	h.scanner.version = "ClamAV 1.4.3/27800"
	svc := h.service(t, "worker-a")

	h.goodUpload(t, "up-1")
	key := "quarantine/" + tenant + "/up-1"
	h.scanner.onPing = func() { h.storage.PutObject(key, []byte("%PDF-1.7\n"+strings.Repeat("y", 600))) }
	h.run(t, svc)
	if job := h.jobFor(t, "up-1"); job.ScannerVersion != "" {
		t.Fatalf("a rejected scan records no reusable version: %+v", job)
	}

	h.scanner.onPing = nil
	h.goodUpload(t, "up-2")
	h.run(t, svc)
	if h.scanner.calls != 2 || h.jobFor(t, "up-2").ScanReusedFrom != "" {
		t.Fatalf("the honest upload is scanned itself: calls %d", h.scanner.calls)
	}
}

// Signatures reloaded while a scan ran: the verdict may come from either
// set, so it is kept but not offered for reuse.
func TestAVersionChangeDuringTheScanIsNotRecorded(t *testing.T) {
	h := newHarness()
	h.scanner.version = "ClamAV 1.4.3/27800"
	h.scanner.during = func(context.Context) error { h.scanner.version = "ClamAV 1.4.3/27801"; return nil }
	h.goodUpload(t, "up-1")
	h.run(t, h.service(t, "worker-a"))
	if job := h.jobFor(t, "up-1"); job.ScanResult != model.ScanClean || job.ScannerVersion != "" {
		t.Fatalf("a scan that spanned a reload is clean but not reusable: %+v", job)
	}
}
