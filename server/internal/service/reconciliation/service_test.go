package reconciliation

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	storagememory "github.com/sanjeev0303/object-storage-platform/internal/infrastructure/storage/memory"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/retry"
	"github.com/sanjeev0303/object-storage-platform/internal/repository/memory"
	uploadservice "github.com/sanjeev0303/object-storage-platform/internal/service/upload"
)

var quietLog = slog.New(slog.NewTextHandler(io.Discard, nil))

var policy = retry.Policy{BaseDelay: time.Minute, MaxDelay: 10 * time.Minute, MaxAttempts: 3}

// scriptedResolver returns the next error from its script for every call.
type scriptedResolver struct {
	mu     sync.Mutex
	errs   []error
	calls  int
	during func() // runs inside the call, to interleave other workers
}

func (r *scriptedResolver) next() (model.UploadStatus, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	if r.during != nil {
		r.during()
	}
	if len(r.errs) == 0 {
		return model.UploadStatusCompleted, nil
	}
	err := r.errs[0]
	r.errs = r.errs[1:]
	return "", err
}

func (r *scriptedResolver) ResolveInitiation(context.Context, string, string) (model.UploadStatus, error) {
	return r.next()
}
func (r *scriptedResolver) ResolveCompletion(context.Context, string, string) (model.UploadStatus, error) {
	return r.next()
}
func (r *scriptedResolver) ResolveAbort(context.Context, string, string) (model.UploadStatus, error) {
	return r.next()
}
func (r *scriptedResolver) Expire(context.Context, string, string) (model.UploadStatus, error) {
	return r.next()
}
func (r *scriptedResolver) VerifyIntegrity(context.Context, string, string) (model.UploadStatus, error) {
	return r.next()
}

// The scans find nothing here; scan behaviour is tested with real services.
func (r *scriptedResolver) ScanOrphanSessions(context.Context, string, uploadservice.ScanOptions) ([]model.ReconciliationFinding, string, error) {
	return nil, "", nil
}
func (r *scriptedResolver) ScanOrphanObjects(context.Context, string, uploadservice.ScanOptions) ([]model.ReconciliationFinding, string, error) {
	return nil, "", nil
}
func (r *scriptedResolver) CleanStaleParts(context.Context, []model.UploadRef, bool) ([]model.ReconciliationFinding, error) {
	return nil, nil
}
func (r *scriptedResolver) AbortLeftoverSessions(context.Context, []model.UploadSession, bool) ([]model.ReconciliationFinding, error) {
	return nil, nil
}

type fixture struct {
	uploads *memory.UploadRepository
	tasks   *memory.ReconciliationRepository
	clock   time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{
		uploads: memory.NewUploadRepository(),
		clock:   time.Date(2026, 10, 16, 10, 0, 0, 0, time.UTC),
	}
	f.tasks = memory.NewReconciliationRepository(f.uploads)

	// One session stuck COMPLETING, as after a lost completion response.
	session := model.UploadSession{
		ID: "upload-1", TenantID: "tenant-1", OwnerID: "user-1",
		ObjectKey: "quarantine/tenant-1/upload-1", FileName: "f.bin", ContentType: "application/octet-stream",
		ExpectedSize: 10, Status: model.UploadStatusCompleting, StorageUploadID: "provider-upload-1",
		CreatedAt: f.clock.Add(-time.Hour), UpdatedAt: f.clock.Add(-time.Hour), ExpiresAt: f.clock.Add(time.Hour),
	}
	if err := f.uploads.Create(context.Background(), &session); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	return f
}

func (f *fixture) service(t *testing.T, owner string, resolver Resolver) *Service {
	t.Helper()
	svc, err := NewService(f.tasks, resolver, Config{
		Owner: owner, StorageProvider: "s3", BatchSize: 10,
		Lease: 5 * time.Minute, Grace: time.Minute, Retry: policy,
		ScanPageSize: 10, ScanPages: 3, OrphanMinAge: time.Hour, StuckAfter: time.Hour,
	}, quietLog, nil)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	svc.now = func() time.Time { return f.clock }
	return svc
}

func (f *fixture) onlyTask(t *testing.T) model.ReconciliationTask {
	t.Helper()
	tasks := f.tasks.Tasks()
	if len(tasks) != 1 {
		t.Fatalf("expected one task, got %d", len(tasks))
	}
	return tasks[0]
}

func TestSuccessFinishesTheTask(t *testing.T) {
	f := newFixture(t)
	svc := f.service(t, "worker-a", &scriptedResolver{})

	stats, err := svc.RunOnce(context.Background())
	if err != nil || stats.Enqueued != 1 || stats.Succeeded != 1 {
		t.Fatalf("stats %+v, err %v", stats, err)
	}
	task := f.onlyTask(t)
	if task.State != model.ReconcileSucceeded || task.Operation != model.ReconcileComplete || task.LeaseOwner != "" {
		t.Fatalf("unexpected task %+v", task)
	}
}

func TestTransientErrorIsRescheduledWithBackoff(t *testing.T) {
	f := newFixture(t)
	svc := f.service(t, "worker-a", &scriptedResolver{errs: []error{errors.New("timeout")}})

	stats, _ := svc.RunOnce(context.Background())
	if stats.Retried != 1 {
		t.Fatalf("stats %+v", stats)
	}
	task := f.onlyTask(t)
	wait := task.NextAttemptAt.Sub(f.clock)
	if task.State != model.ReconcilePending || wait < policy.BaseDelay/2 || wait > policy.BaseDelay {
		t.Fatalf("expected PENDING with backoff in [30s, 60s], got %s after %s", task.State, wait)
	}
	if task.LastError != "timeout" || task.LeaseOwner != "" {
		t.Fatalf("expected the error recorded and the lease released, got %+v", task)
	}

	// Not due yet: nothing is claimed.
	stats, _ = svc.RunOnce(context.Background())
	if stats.Claimed != 0 {
		t.Fatalf("claimed a task before its retry time: %+v", stats)
	}
}

func TestPermanentErrorIsExhaustedImmediately(t *testing.T) {
	f := newFixture(t)
	svc := f.service(t, "worker-a", &scriptedResolver{errs: []error{retry.Permanent(errors.New("AccessDenied"))}})

	stats, _ := svc.RunOnce(context.Background())
	if stats.Exhausted != 1 || f.onlyTask(t).State != model.ReconcileExhausted {
		t.Fatalf("stats %+v, task %+v", stats, f.onlyTask(t))
	}
}

func TestRetryBudgetRunsOutVisibly(t *testing.T) {
	f := newFixture(t)
	resolver := &scriptedResolver{errs: []error{errors.New("e1"), errors.New("e2"), errors.New("e3"), errors.New("e4")}}
	svc := f.service(t, "worker-a", resolver)

	for i := 0; i < policy.MaxAttempts; i++ {
		if _, err := svc.RunOnce(context.Background()); err != nil {
			t.Fatalf("cycle %d: %v", i, err)
		}
		f.clock = f.clock.Add(policy.MaxDelay + time.Second)
	}

	task := f.onlyTask(t)
	if task.State != model.ReconcileExhausted || task.AttemptCount != policy.MaxAttempts || task.LastError != "e3" {
		t.Fatalf("expected EXHAUSTED after %d attempts, got %+v", policy.MaxAttempts, task)
	}

	// An exhausted task stays visible and is never re-created or re-run.
	stats, _ := svc.RunOnce(context.Background())
	if stats.Enqueued != 0 || stats.Claimed != 0 || resolver.calls != policy.MaxAttempts {
		t.Fatalf("exhausted work ran again: %+v, calls %d", stats, resolver.calls)
	}
}

func TestExpiredLeaseIsReclaimedAndTheStaleWorkerCannotWrite(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// Worker A claims, then crashes without writing an outcome.
	if _, err := f.tasks.EnqueueDue(ctx, f.clock, time.Minute, "s3", policy.MaxAttempts); err != nil {
		t.Fatal(err)
	}
	claimed, err := f.tasks.Claim(ctx, "worker-a", f.clock, 5*time.Minute, 10)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: %v, %d", err, len(claimed))
	}

	// Within the lease, nobody else can take it.
	if again, _ := f.tasks.Claim(ctx, "worker-b", f.clock.Add(time.Minute), 5*time.Minute, 10); len(again) != 0 {
		t.Fatal("a leased task was claimed twice")
	}

	// After the lease, worker B recovers it.
	f.clock = f.clock.Add(6 * time.Minute)
	stats, err := f.service(t, "worker-b", &scriptedResolver{}).RunOnce(ctx)
	if err != nil || stats.Succeeded != 1 {
		t.Fatalf("worker B: %+v, %v", stats, err)
	}

	// Worker A wakes up: its stale claim must not overwrite the outcome.
	err = f.tasks.Finish(ctx, claimed[0].ID, "worker-a", f.clock, model.ReconcileExhausted, "", "stale")
	if !errors.Is(err, model.ErrLeaseLost) || f.onlyTask(t).State != model.ReconcileSucceeded {
		t.Fatalf("stale write: %v, task %+v", err, f.onlyTask(t))
	}
}

func TestLeaseLostDuringWorkIsCountedNotWritten(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// While worker A works, its lease runs out and worker B takes over.
	resolver := &scriptedResolver{}
	resolver.during = func() {
		f.clock = f.clock.Add(6 * time.Minute)
		if got, _ := f.tasks.Claim(ctx, "worker-b", f.clock, 5*time.Minute, 10); len(got) != 1 {
			t.Error("worker B should have reclaimed the expired task")
		}
	}

	stats, err := f.service(t, "worker-a", resolver).RunOnce(ctx)
	if err != nil || stats.LeaseLost != 1 || stats.Succeeded != 0 {
		t.Fatalf("stats %+v, err %v", stats, err)
	}
	if task := f.onlyTask(t); task.State != model.ReconcilePending || task.LeaseOwner != "worker-b" {
		t.Fatalf("worker B's claim must survive, got %+v", task)
	}
}

func TestConcurrentWorkersNeverShareATask(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for i := 2; i <= 30; i++ {
		s := model.UploadSession{
			ID: "upload-" + string(rune('A'+i)), TenantID: "tenant-1", OwnerID: "user-1",
			ObjectKey: "k", FileName: "f", ContentType: "c", ExpectedSize: 1, Status: model.UploadStatusCompleted,
			CreatedAt: f.clock, UpdatedAt: f.clock, ExpiresAt: f.clock.Add(time.Hour),
		}
		_ = f.uploads.Create(ctx, &s)
	}
	if _, err := f.tasks.EnqueueDue(ctx, f.clock, 0, "s3", 3); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	seen := map[string]string{}
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(owner string) {
			defer wg.Done()
			got, _ := f.tasks.Claim(ctx, owner, f.clock, time.Minute, 5)
			mu.Lock()
			defer mu.Unlock()
			for _, task := range got {
				if prev, dup := seen[task.ID]; dup {
					t.Errorf("task %s claimed by %s and %s", task.ID, prev, owner)
				}
				seen[task.ID] = owner
			}
		}(string(rune('a' + w)))
	}
	wg.Wait()
}

func TestShutdownWritesNoOutcome(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	resolver := &scriptedResolver{during: cancel}

	stats, _ := f.service(t, "worker-a", resolver).RunOnce(ctx)
	task := f.onlyTask(t)
	if stats.Succeeded+stats.Retried+stats.Exhausted != 0 || task.LeaseOwner != "worker-a" {
		t.Fatalf("an interrupted task must keep its lease to expire, got %+v / %+v", stats, task)
	}
}

// End to end: a lost completion response is recovered by the worker, with
// the real multipart service and an in-memory provider. Verification is the
// processing worker's job, so reconciliation leaves the session COMPLETED.
func TestWorkerRecoversALostCompletion(t *testing.T) {
	ctx := context.Background()
	uploads := memory.NewUploadRepository()
	parts := memory.NewMultipartUploadRepository()
	storage := storagememory.NewObjectStorage()
	multipart, err := uploadservice.NewMultipartService(uploads, parts, storage, uploadservice.MultipartServiceConfig{
		Bucket: "test-bucket", MaxSize: 1 << 30, SessionTTL: time.Hour, PartURLTTL: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}

	first := bytes.Repeat([]byte("x"), 5*1024*1024)
	second := []byte("tail")
	sum := sha256.Sum256(append(append([]byte{}, first...), second...))

	started, err := multipart.Start(ctx, uploadservice.StartMultipartInput{
		TenantID: "tenant-1", OwnerID: "user-1", FileName: "f.bin", ContentType: "application/octet-stream",
		SizeBytes: int64(len(first) + len(second)), ChecksumSHA256: hex.EncodeToString(sum[:]), IdempotencyKey: "e2e",
	})
	if err != nil {
		t.Fatal(err)
	}
	id := started.Session.ID
	etag1 := storage.PutPart(started.Session.StorageUploadID, 1, first)
	etag2 := storage.PutPart(started.Session.StorageUploadID, 2, second)
	if err := multipart.RecordParts(ctx, "tenant-1", "user-1", id, []uploadservice.RecordPartInput{
		{PartNumber: 1, ETag: etag1, SizeBytes: int64(len(first))},
		{PartNumber: 2, ETag: etag2, SizeBytes: int64(len(second))},
	}); err != nil {
		t.Fatal(err)
	}

	storage.CompleteLost = errors.New("timeout")
	_, err = multipart.Complete(ctx, "tenant-1", "user-1", id, uploadservice.CompleteMultipartInput{
		Parts: []uploadservice.CompletedPartInput{{PartNumber: 1, ETag: etag1}, {PartNumber: 2, ETag: etag2}},
	})
	if !errors.Is(err, uploadservice.ErrOperationPending) {
		t.Fatalf("expected pending completion, got %v", err)
	}

	tasks := memory.NewReconciliationRepository(uploads)
	svc, err := NewService(tasks, multipart, Config{
		Owner: "worker-a", StorageProvider: "s3", BatchSize: 10, Lease: time.Minute, Grace: 0, Retry: policy,
		ScanPageSize: 10, ScanPages: 3, OrphanMinAge: time.Hour, StuckAfter: time.Hour,
	}, quietLog, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Cycle 1 completes (COMPLETING -> COMPLETED); cycle 2 finds nothing.
	for i := 0; i < 2; i++ {
		if _, err := svc.RunOnce(ctx); err != nil {
			t.Fatalf("cycle %d: %v", i, err)
		}
	}

	session, _ := uploads.GetByID(ctx, "tenant-1", id)
	if session.Status != model.UploadStatusCompleted || session.CanPromote() {
		t.Fatalf("expected COMPLETED and not promotable, got %s (%s)", session.Status, session.FailureReason)
	}
	if len(tasks.Tasks()) != 1 {
		t.Fatalf("expected only the COMPLETE task, got %d tasks", len(tasks.Tasks()))
	}
	if storage.CompleteCalls != 1 {
		t.Fatalf("CompleteMultipart called %d times, want 1", storage.CompleteCalls)
	}
}
