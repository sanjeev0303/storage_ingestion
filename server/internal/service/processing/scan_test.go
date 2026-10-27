package processing

import (
	"bytes"
	"context"
	"io"
	"sync"
	"testing"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// markerScanner reports INFECTED for any stream containing marker, the way
// a byte signature matches, and records how many bytes each scan read.
type markerScanner struct {
	marker []byte
	mu     sync.Mutex
	scans  []int
}

func (m *markerScanner) Name() string               { return "marker" }
func (m *markerScanner) Ping(context.Context) error { return nil }
func (m *markerScanner) Scan(_ context.Context, content io.Reader) interfaces.ScanResult {
	data, err := io.ReadAll(content)
	if err != nil {
		return interfaces.ScanResult{Outcome: model.ScanError, Detail: "read failed"}
	}
	m.mu.Lock()
	m.scans = append(m.scans, len(data))
	m.mu.Unlock()
	if bytes.Contains(data, m.marker) {
		return interfaces.ScanResult{Outcome: model.ScanInfected, Signature: "Test.Marker"}
	}
	return interfaces.ScanResult{Outcome: model.ScanClean}
}

const mib = 1 << 20

// largeHarness processes objects up to 16 MiB in 2 MiB scan segments.
func largeHarness(t *testing.T, data []byte, contentType string) (*harness, *markerScanner, *Service) {
	t.Helper()
	h := newHarness()
	h.maxSize, h.segment = 16*mib, 2*mib
	h.completedUpload(t, "up-1", data, sum(data), int64(len(data)), contentType)
	svc := h.service(t, "worker-a")
	scanner := &markerScanner{marker: []byte("SIGNATURE-ACROSS-THE-BOUNDARY")}
	svc.deps.Scanner = scanner
	return h, scanner, svc
}

func TestALargeObjectIsScannedInOverlappingSegmentsAndPromoted(t *testing.T) {
	data := bytes.Repeat([]byte{0x01, 0x02, 0x03, 0x04}, 7*mib/4) // 7 MiB, binary
	h, scanner, svc := largeHarness(t, data, "application/octet-stream")

	if stats := h.run(t, svc); stats.Succeeded != 1 {
		t.Fatalf("stats %+v", stats)
	}
	if asset := h.onlyAsset(t); asset.Status != model.AssetStatusAvailable {
		t.Fatalf("asset is %s, want AVAILABLE", asset.Status)
	}
	// 7 MiB in 2 MiB segments is four scans; all but the first reach 1 MiB back.
	if len(scanner.scans) != 4 {
		t.Fatalf("%d scans, want 4", len(scanner.scans))
	}
	var scanned int
	for _, n := range scanner.scans {
		scanned += n
	}
	if want := 7*mib + 3*scanOverlap; scanned != want {
		t.Fatalf("scanned %d bytes, want %d (every byte, plus each overlap)", scanned, want)
	}
}

func TestASignatureAcrossASegmentBoundaryIsFound(t *testing.T) {
	data := bytes.Repeat([]byte{0x01, 0x02, 0x03, 0x04}, 7*mib/4)
	marker := []byte("SIGNATURE-ACROSS-THE-BOUNDARY")
	copy(data[4*mib-10:], marker) // half in the second segment, half in the third
	h, _, svc := largeHarness(t, data, "application/octet-stream")

	h.run(t, svc)
	asset, job := h.onlyAsset(t), h.onlyJob(t)
	if asset.Status != model.AssetStatusRejected || asset.RejectionReason != model.RejectMalwareDetected {
		t.Fatalf("asset is %s (%s), want REJECTED for malware", asset.Status, asset.RejectionReason)
	}
	if job.ScanResult != model.ScanInfected || job.ScanSignature != "Test.Marker" {
		t.Fatalf("job scan %s %q", job.ScanResult, job.ScanSignature)
	}
	if record := h.onlyRecord(t, asset.ID); record.Status != model.QuarantineRejected {
		t.Fatalf("quarantine record is %s, want REJECTED", record.Status)
	}
}

func TestAnArchiveTooLargeToScanWholeIsHeldNotPassed(t *testing.T) {
	data := append([]byte("PK\x03\x04"), bytes.Repeat([]byte{0x01, 0x02}, 3*mib)...)
	h, scanner, svc := largeHarness(t, data, "application/zip")

	if stats := h.run(t, svc); stats.Exhausted != 1 {
		t.Fatalf("stats %+v, want the job exhausted", stats)
	}
	asset, job := h.onlyAsset(t), h.onlyJob(t)
	if asset.Status == model.AssetStatusAvailable || job.ScanResult != model.ScanUnsupported || len(scanner.scans) != 0 {
		t.Fatalf("asset %s, scan %s after %d scans; a large archive must be held unscanned", asset.Status, job.ScanResult, len(scanner.scans))
	}
	if record := h.onlyRecord(t, asset.ID); record.Status != model.QuarantineHeld {
		t.Fatalf("quarantine record is %s, want HELD", record.Status)
	}
}

func TestTheJobBudgetGrowsWithTheObject(t *testing.T) {
	data := bytes.Repeat([]byte{0x01}, 3*mib)
	h, _, svc := largeHarness(t, data, "application/octet-stream")
	svc.cfg.MinThroughput = mib // 1 MiB/s: 3 MiB earns 3 more seconds
	h.run(t, svc)
	job := h.onlyJob(t)
	if got, want := svc.budget(context.Background(), job), svc.cfg.MaxJobDuration+3e9; got != want {
		t.Fatalf("budget %v, want %v", got, want)
	}
}

// While its slots are busy with slow scans, a worker leaves the other jobs
// unclaimed, so another worker (or the event consumer) runs them meanwhile
// instead of their waiting behind the slow ones.
func TestBusyWorkerDoesNotHoldJobsItCannotStart(t *testing.T) {
	h := newHarness()
	for _, id := range []string{"up-1", "up-2", "up-3"} {
		h.goodUpload(t, id)
	}
	release := make(chan struct{})
	started := make(chan struct{}, 3)
	var mu sync.Mutex
	slow := 0
	h.scanner.during = func(ctx context.Context) error {
		mu.Lock()
		first := slow < 2
		slow++
		mu.Unlock()
		if !first {
			return nil
		}
		started <- struct{}{}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	busy, other := h.service(t, "worker-a"), h.service(t, "worker-b")

	done := make(chan CycleStats)
	go func() { done <- h.run(t, busy) }()
	<-started
	<-started // both of worker-a's slots are now scanning

	stats := h.run(t, other)
	close(release)
	busyStats := <-done
	if stats.Claimed != 1 || stats.Succeeded != 1 {
		t.Fatalf("the other worker ran %+v; want the third job claimed and finished", stats)
	}
	if busyStats.Claimed != 2 || busyStats.Succeeded != 2 {
		t.Fatalf("the busy worker ran %+v; want only the two it could start", busyStats)
	}
}
