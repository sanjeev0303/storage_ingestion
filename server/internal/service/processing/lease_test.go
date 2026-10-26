package processing

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// countingJobs counts lease renewals.
type countingJobs struct {
	interfaces.ProcessingJobRepository
	renewals atomic.Int64
}

func (c *countingJobs) Renew(ctx context.Context, jobID, owner string, now time.Time, lease time.Duration) error {
	c.renewals.Add(1)
	return c.ProcessingJobRepository.Renew(ctx, jobID, owner, now, lease)
}

// leaseHarness has a short lease, renewals counted, and a service clock
// that moves with real time, so an unrenewed lease really expires.
func leaseHarness(t *testing.T) (*harness, *Service, *countingJobs) {
	t.Helper()
	h := newHarness()
	h.goodUpload(t, "up-1")
	h.lease = 60 * time.Millisecond // renewed every 20ms
	counter := &countingJobs{ProcessingJobRepository: h.jobs}
	h.jobs = counter
	svc := h.service(t, "worker-a")
	start, base := time.Now(), h.clock
	svc.now = func() time.Time { return base.Add(time.Since(start)) }
	svc.cfg.ScanTimeout = 10 * time.Second
	return h, svc, counter
}

func TestALongScanKeepsItsJobByRenewingTheLease(t *testing.T) {
	h, svc, counter := leaseHarness(t)
	h.scanner.during = func(context.Context) error {
		time.Sleep(300 * time.Millisecond) // five leases
		return nil
	}

	stats := h.run(t, svc)
	if stats.Succeeded != 1 || stats.LeaseLost != 0 || h.onlyAsset(t).Status != model.AssetStatusAvailable {
		t.Fatalf("a scan longer than the lease must still finish: %+v", stats)
	}
	if counter.renewals.Load() < 5 {
		t.Fatalf("the lease was renewed %d times during a 300ms scan", counter.renewals.Load())
	}
}

// Jobs claimed together keep their leases while they wait for a free slot,
// not only once they run: four jobs, two at a time, each scan longer than
// the lease. Without renewal in the queue the last two expire, are lost
// and use up an attempt.
func TestQueuedJobsKeepTheirLeasesWhileTheyWait(t *testing.T) {
	h, svc, _ := leaseHarness(t)
	for _, id := range []string{"up-2", "up-3", "up-4"} {
		h.goodUpload(t, id)
	}
	h.scanner.during = func(context.Context) error {
		time.Sleep(100 * time.Millisecond)
		return nil
	}

	stats := h.run(t, svc)
	if stats.Claimed != 4 || stats.Succeeded != 4 || stats.LeaseLost != 0 {
		t.Fatalf("every queued job keeps its lease: %+v", stats)
	}
	for _, job := range h.memJobs.Jobs() {
		if job.AttemptCount != 1 {
			t.Fatalf("a job spent %d attempts", job.AttemptCount)
		}
	}
}

// Another worker reclaims the job mid-scan (as after a pause longer than
// the lease): the first worker's renewal fails, its work stops, and it
// writes and publishes nothing.
func TestALostLeaseStopsTheJobAndPublishesNothing(t *testing.T) {
	h, svc, _ := leaseHarness(t)
	var reclaimed model.ProcessingJob
	h.scanner.during = func(ctx context.Context) error {
		job := h.memJobs.Jobs()[0]
		past := svc.now().Add(-time.Millisecond)
		job.LeaseExpiresAt = &past
		h.memJobs.Put(job)
		reclaimed, _, _ = h.memJobs.ClaimJob(context.Background(), job.ID, "worker-b", svc.now(), time.Hour)
		<-ctx.Done() // the scan stops when worker-a's lease is gone
		return ctx.Err()
	}

	stats := h.run(t, svc)
	job := h.onlyJob(t)
	if stats.LeaseLost != 1 || stats.Succeeded+stats.Retried+stats.Failed+stats.Exhausted != 0 {
		t.Fatalf("the worker that lost the job must record nothing: %+v", stats)
	}
	if job.LeaseOwner != reclaimed.LeaseOwner || job.LeaseOwner != "worker-b#2" || job.ScanResult != "" {
		t.Fatalf("the new owner's claim must stand untouched: %+v", job)
	}
	if h.onlyAsset(t).Status == model.AssetStatusAvailable {
		t.Fatal("a worker that lost the job published it")
	}
}

// The same worker claiming a job again does not revive its earlier claim:
// each claim is its own lease owner.
func TestAnEarlierClaimOfTheSameWorkerCannotWrite(t *testing.T) {
	h := newHarness()
	h.goodUpload(t, "up-1")
	ctx := context.Background()
	if _, err := h.jobs.EnqueueDue(ctx, h.clock, 3); err != nil {
		t.Fatal(err)
	}
	first, err := h.jobs.Claim(ctx, "worker-a", h.clock, h.lease, 10)
	if err != nil || len(first) != 1 {
		t.Fatalf("claim: %v %d", err, len(first))
	}
	h.clock = h.clock.Add(h.lease + time.Second)
	second, err := h.jobs.Claim(ctx, "worker-a", h.clock, h.lease, 10)
	if err != nil || len(second) != 1 || second[0].LeaseOwner == first[0].LeaseOwner {
		t.Fatalf("reclaim: %v %+v", err, second)
	}
	for _, write := range []error{
		h.jobs.Renew(ctx, first[0].ID, first[0].LeaseOwner, h.clock, h.lease),
		h.jobs.SaveProgress(ctx, first[0], first[0].LeaseOwner, h.clock),
		h.jobs.Finish(ctx, first[0].ID, first[0].LeaseOwner, h.clock, model.ProcessingSucceeded, ""),
	} {
		if !errors.Is(write, model.ErrLeaseLost) {
			t.Fatalf("a write under the earlier claim: %v", write)
		}
	}
	if err := h.jobs.Renew(ctx, second[0].ID, second[0].LeaseOwner, h.clock, h.lease); err != nil {
		t.Fatalf("the current claim renews: %v", err)
	}
}

// Renewal ends with the job: nothing renews a finished job's lease.
func TestRenewalStopsWhenTheJobEnds(t *testing.T) {
	h, svc, counter := leaseHarness(t)
	h.scanner.during = func(context.Context) error {
		time.Sleep(100 * time.Millisecond)
		return nil
	}
	if stats := h.run(t, svc); stats.Succeeded != 1 {
		t.Fatalf("%+v", stats)
	}
	after := counter.renewals.Load()
	time.Sleep(150 * time.Millisecond) // several renewal intervals
	if counter.renewals.Load() != after {
		t.Fatal("the lease was renewed after the job ended")
	}
}

// A job is not kept forever: at MaxJobDuration it stops, gives its lease
// back and waits for its retry.
func TestAStuckJobStopsAtTheMaximumDuration(t *testing.T) {
	h, svc, _ := leaseHarness(t)
	svc.cfg.MaxJobDuration = 200 * time.Millisecond
	h.scanner.during = func(ctx context.Context) error {
		<-ctx.Done() // never finishes on its own
		return ctx.Err()
	}

	started := time.Now()
	stats := h.run(t, svc)
	job := h.onlyJob(t)
	if stats.Retried != 1 || job.LeaseOwner != "" || job.State != model.ProcessingPending {
		t.Fatalf("a stuck job must be retried with its lease released: %+v %+v", stats, job)
	}
	if took := time.Since(started); took > 2*time.Second {
		t.Fatalf("a 200ms cap took %s", took)
	}
}
