package asset

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	storagememory "github.com/sanjeev0303/object-storage-platform/internal/infrastructure/storage/memory"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/retry"
	"github.com/sanjeev0303/object-storage-platform/internal/repository/memory"
	"github.com/sanjeev0303/object-storage-platform/internal/service/delivery"
)

var quietLog = slog.New(slog.NewTextHandler(io.Discard, nil))

var owner = member("tenant-1", "user-1", model.RoleDeveloper)

// member is an organization member with the built-in role roleID.
func member(tenantID, userID, roleID string) model.Principal {
	role, _ := model.BuiltInRole(roleID)
	return model.Principal{TenantID: tenantID, UserID: userID, RoleID: role.ID, Permissions: role.Permissions}
}

const grace = 24 * time.Hour

type env struct {
	t          *testing.T
	assets     *memory.AssetRepository
	uploads    *memory.UploadRepository
	jobs       *memory.ProcessingJobRepository
	quarantine *memory.QuarantineRepository
	policies   *memory.RetentionPolicyRepository
	holds      *memory.LegalHoldRepository
	deletions  *memory.DeletionRepository
	events     *memory.LifecycleEventRepository
	cursors    *memory.CursorRepository
	tokens     *memory.DownloadTokenRepository
	storage    *storagememory.ObjectStorage
	clock      time.Time
	expire     bool
	// deletionRepo and storageAPI, when set, wrap the memory ones for
	// failure injection.
	deletionRepo interfaces.DeletionRepository
	storageAPI   interfaces.LifecycleStorage
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{
		t:          t,
		assets:     memory.NewAssetRepository(),
		uploads:    memory.NewUploadRepository(),
		quarantine: memory.NewQuarantineRepository(),
		policies:   memory.NewRetentionPolicyRepository(),
		holds:      memory.NewLegalHoldRepository(),
		events:     memory.NewLifecycleEventRepository(),
		cursors:    memory.NewCursorRepository(),
		tokens:     memory.NewDownloadTokenRepository(),
		storage:    storagememory.NewObjectStorage(),
		clock:      time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC),
		expire:     true,
	}
	e.jobs = memory.NewProcessingJobRepository(e.uploads, e.assets)
	e.deletions = memory.NewDeletionRepository(e.assets, e.tokens, e.uploads, e.jobs, e.quarantine, e.holds, e.events)
	return e
}

func (e *env) service(workerID string, change ...func(*Config)) *Service {
	e.t.Helper()
	deletions := interfaces.DeletionRepository(e.deletions)
	if e.deletionRepo != nil {
		deletions = e.deletionRepo
	}
	storage := interfaces.LifecycleStorage(e.storage)
	if e.storageAPI != nil {
		storage = e.storageAPI
	}
	cfg := Config{
		Owner: workerID, QuarantineBucket: "bucket", DeliveryBucket: "bucket", DeliveryPrefix: "assets",
		Platform: PlatformRetention{
			DefaultDeletionGrace: grace, DefaultQuarantineRetention: 30 * day, QuarantineRetentionFloor: 30 * day,
		},
		BatchSize: 10, Concurrency: 2, Lease: 5 * time.Minute,
		Retry:           retry.Policy{BaseDelay: time.Minute, MaxDelay: 10 * time.Minute, MaxAttempts: 3},
		RecheckInterval: time.Hour, ExpireAssets: e.expire, MaxDeletionsPerRun: 100,
	}
	for _, c := range change {
		c(&cfg)
	}
	svc, err := NewService(Dependencies{
		Assets: e.assets, Uploads: e.uploads, Jobs: e.jobs, Quarantine: e.quarantine, Policies: e.policies,
		Holds: e.holds, Deletions: deletions, Events: e.events, Cursors: e.cursors, Storage: storage,
	}, cfg, quietLog)
	if err != nil {
		e.t.Fatal(err)
	}
	svc.now = func() time.Time { return e.clock }
	return svc
}

// published seeds an AVAILABLE asset with its delivery copy and, as a
// promotion crash would leave it, its quarantine source.
func (e *env) published(id string, created time.Time) model.Asset {
	e.t.Helper()
	asset := model.Asset{
		ID: id, TenantID: "tenant-1", OwnerID: "user-1", UploadID: "up-" + id, ObjectKey: "assets/tenant-1/" + id,
		FileName: id + ".pdf", ContentType: "application/pdf", SizeBytes: 4, ChecksumSHA256: strings.Repeat("a", 64),
		Status: model.AssetStatusAvailable, CreatedAt: created, UpdatedAt: created,
	}
	if err := e.assets.Create(context.Background(), &asset); err != nil {
		e.t.Fatal(err)
	}
	e.storage.PutObject("assets/tenant-1/"+id, []byte("data"))
	e.storage.PutObject("quarantine/tenant-1/up-"+id, []byte("data"))
	return asset
}

func (e *env) status(id string) model.AssetStatus {
	e.t.Helper()
	asset, err := e.assets.GetByID(context.Background(), "tenant-1", id)
	if err != nil {
		e.t.Fatal(err)
	}
	return asset.Status
}

func (e *env) objects(id string) int {
	n := 0
	for _, key := range []string{"assets/tenant-1/" + id, "quarantine/tenant-1/up-" + id} {
		if _, ok := e.storage.Object(key); ok {
			n++
		}
	}
	return n
}

func (e *env) op(id string) model.DeletionOperation {
	e.t.Helper()
	ops, err := e.deletions.ListForAsset(context.Background(), "tenant-1", id)
	if err != nil || len(ops) == 0 {
		e.t.Fatalf("no deletion for %s: %v", id, err)
	}
	return ops[0]
}

func (e *env) run(svc *Service) CycleStats {
	e.t.Helper()
	var stats CycleStats
	if err := svc.ProcessDeletions(context.Background(), &stats); err != nil {
		e.t.Fatalf("process deletions: %v", err)
	}
	return stats
}

func TestSoftDeleteBlocksDeliveryAndPurgesAfterGrace(t *testing.T) {
	e := newEnv(t)
	e.published("a1", e.clock)
	svc := e.service("worker-a")
	deliverer, err := delivery.NewService(e.assets, e.storage, memory.NewDeliveryAuditRepository(), e.tokens, delivery.Config{
		Bucket: "bucket", Prefix: "assets", DefaultTTL: time.Minute, MaxTTL: time.Minute,
	}, quietLog)
	if err != nil {
		t.Fatal(err)
	}

	result, err := svc.RequestDeletion(context.Background(), owner, "a1")
	if err != nil || !result.Created || result.Operation.State != model.DeletionScheduled ||
		!result.Operation.PurgeAfter.Equal(e.clock.Add(grace)) {
		t.Fatalf("request: %+v %v", result, err)
	}
	if _, err := deliverer.Open(context.Background(), owner, "a1", delivery.DownloadRequest{}); !errors.Is(err, delivery.ErrNotAvailable) {
		t.Fatalf("a soft-deleted asset must not be delivered, got %v", err)
	}

	again, err := svc.RequestDeletion(context.Background(), owner, "a1")
	if err != nil || again.Created || again.Operation.ID != result.Operation.ID {
		t.Fatalf("a repeated request must return the same deletion: %+v %v", again, err)
	}

	if stats := e.run(svc); stats.Claimed != 0 || e.objects("a1") != 2 {
		t.Fatalf("nothing may be removed during the grace period: %+v", stats)
	}

	e.clock = e.clock.Add(grace)
	if stats := e.run(svc); stats.Completed != 1 {
		t.Fatalf("expected the deletion to complete: %+v", stats)
	}
	if e.objects("a1") != 0 || e.status("a1") != model.AssetStatusDeleted || e.op("a1").State != model.DeletionCompleted {
		t.Fatalf("objects %d, asset %s, op %s", e.objects("a1"), e.status("a1"), e.op("a1").State)
	}
	want := []string{"deletion_requested", "deletion_started", "storage_deleted", "deletion_completed"}
	if got := e.events.Names(); !slices.Equal(got, want) {
		t.Fatalf("audit trail %v, want %v", got, want)
	}
	if _, err := svc.RequestDeletion(context.Background(), owner, "a1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a deleted asset must look missing, got %v", err)
	}

	// Operators still see the tombstone and how it came to be.
	inspection, err := svc.Inspect(context.Background(), "tenant-1", "a1")
	if err != nil || inspection.Asset.Status != model.AssetStatusDeleted || len(inspection.Operations) != 1 ||
		inspection.Operations[0].State != model.DeletionCompleted || len(inspection.Events) != 4 {
		t.Fatalf("operator inspection of a tombstone: %+v %v", inspection, err)
	}
	if _, err := svc.OwnerInspect(context.Background(), owner, "a1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the owner's view hides the tombstone, got %v", err)
	}
}

func TestRestoreDuringGraceOnly(t *testing.T) {
	e := newEnv(t)
	e.published("a1", e.clock)
	svc := e.service("worker-a")

	if _, err := svc.RequestDeletion(context.Background(), owner, "a1"); err != nil {
		t.Fatal(err)
	}
	op, err := svc.Restore(context.Background(), owner, "a1")
	if err != nil || op.State != model.DeletionCancelled || e.status("a1") != model.AssetStatusAvailable {
		t.Fatalf("restore: %+v %v, asset %s", op, err, e.status("a1"))
	}
	e.clock = e.clock.Add(2 * grace)
	if stats := e.run(svc); stats.Claimed != 0 || e.objects("a1") != 2 {
		t.Fatalf("a restored asset must keep its objects: %+v", stats)
	}
	if _, err := svc.Restore(context.Background(), owner, "a1"); !errors.Is(err, model.ErrNotRestorable) {
		t.Fatalf("nothing left to restore, got %v", err)
	}

	// Once removal has started, restoring would bring back a partial asset.
	if _, err := svc.RequestDeletion(context.Background(), owner, "a1"); err != nil {
		t.Fatal(err)
	}
	e.clock = e.clock.Add(grace)
	e.storage.DeleteErr = errors.New("connection reset")
	if stats := e.run(svc); stats.Retried != 1 || e.op("a1").State != model.DeletionRetryWait {
		t.Fatalf("expected a retry after the failed delete: %+v %s", stats, e.op("a1").State)
	}
	if _, err := svc.Restore(context.Background(), owner, "a1"); !errors.Is(err, model.ErrNotRestorable) {
		t.Fatalf("a started deletion must not be restorable, got %v", err)
	}
}

func TestLegalHoldAppliedAfterSchedulingProtects(t *testing.T) {
	for _, hold := range []model.LegalHold{
		{TenantID: "tenant-1", Scope: model.ScopeAsset, ScopeKey: "a1", Reason: "case 42", PlacedBy: "legal"},
		{TenantID: "tenant-1", Scope: model.ScopeDataset, ScopeKey: "reports", Reason: "audit", PlacedBy: "legal"},
		{TenantID: "tenant-1", Scope: model.ScopeTenant, Reason: "litigation", PlacedBy: "legal"},
	} {
		t.Run(hold.Scope, func(t *testing.T) {
			e := newEnv(t)
			asset := e.published("a1", e.clock)
			e.assets.Update(asset.ID, func(a *model.Asset) { a.Dataset = "reports" })
			svc := e.service("worker-a")
			if _, err := svc.RequestDeletion(context.Background(), owner, "a1"); err != nil {
				t.Fatal(err)
			}

			placed, err := svc.PlaceHold(context.Background(), hold)
			if err != nil {
				t.Fatal(err)
			}
			e.clock = e.clock.Add(grace)
			if stats := e.run(svc); stats.Protected != 1 || e.objects("a1") != 2 {
				t.Fatalf("a hold placed after scheduling must protect: %+v", stats)
			}
			op := e.op("a1")
			if op.State != model.DeletionProtected || !strings.Contains(op.ProtectedReason, "legal hold "+placed.ID) {
				t.Fatalf("op %+v", op)
			}
			if !op.NextAttemptAt.Equal(e.clock.Add(time.Hour)) {
				t.Fatalf("a held deletion is checked again after the recheck interval, got %v", op.NextAttemptAt)
			}

			if err := svc.ReleaseHold(context.Background(), "tenant-1", placed.ID, "legal"); err != nil {
				t.Fatal(err)
			}
			e.clock = e.clock.Add(time.Hour)
			if stats := e.run(svc); stats.Completed != 1 || e.objects("a1") != 0 {
				t.Fatalf("after release the deletion must complete: %+v", stats)
			}
		})
	}
}

// holdAtStart places a hold in the moment between the eligibility check
// and the start, which only the atomic start can catch.
type holdAtStart struct {
	*memory.DeletionRepository
	holds *memory.LegalHoldRepository
}

func (h holdAtStart) Start(ctx context.Context, id, owner string, now time.Time) (bool, error) {
	_, _ = h.holds.Place(ctx, model.LegalHold{ID: "late", TenantID: "tenant-1", Scope: model.ScopeTenant, Reason: "late", PlacedBy: "legal", PlacedAt: now})
	return h.DeletionRepository.Start(ctx, id, owner, now)
}

func TestHoldPlacedAtTheLastMomentStillProtects(t *testing.T) {
	e := newEnv(t)
	e.published("a1", e.clock)
	e.deletionRepo = holdAtStart{e.deletions, e.holds}
	svc := e.service("worker-a")
	if _, err := svc.RequestDeletion(context.Background(), owner, "a1"); err != nil {
		t.Fatal(err)
	}
	e.clock = e.clock.Add(grace)
	if stats := e.run(svc); stats.Protected != 1 || e.objects("a1") != 2 || e.op("a1").State != model.DeletionProtected {
		t.Fatalf("expected protection, got %+v, op %s", stats, e.op("a1").State)
	}
}

func TestRetentionExtendedAfterSchedulingIsHonoured(t *testing.T) {
	e := newEnv(t)
	created := e.clock
	e.published("a1", created)
	svc := e.service("worker-a")
	if _, err := svc.RequestDeletion(context.Background(), owner, "a1"); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.SetPolicy(context.Background(), model.RetentionPolicy{
		TenantID: "tenant-1", Scope: model.ScopeAsset, ScopeKey: "a1", MinRetention: duration(30 * day),
		CreatedBy: "compliance", Reason: "retention extended",
	}); err != nil {
		t.Fatal(err)
	}

	e.clock = e.clock.Add(grace)
	if stats := e.run(svc); stats.Protected != 1 || e.objects("a1") != 2 {
		t.Fatalf("an extended retention must protect: %+v", stats)
	}
	if reason := e.op("a1").ProtectedReason; !strings.Contains(reason, "retention until") || !strings.Contains(reason, "ASSET v1") {
		t.Fatalf("reason %q", reason)
	}

	e.clock = created.Add(30 * day)
	if stats := e.run(svc); stats.Completed != 1 {
		t.Fatalf("after retention the deletion must complete: %+v", stats)
	}
}

// cancelAfterDelete deletes, then cancels the worker's context, as a
// worker crashing right after the provider call would.
type cancelAfterDelete struct {
	*storagememory.ObjectStorage
	cancel context.CancelFunc
}

func (c cancelAfterDelete) DeleteObject(ctx context.Context, bucket, key string) error {
	err := c.ObjectStorage.DeleteObject(ctx, bucket, key)
	c.cancel()
	return err
}

func TestCrashAfterStorageDeletionResumes(t *testing.T) {
	e := newEnv(t)
	e.published("a1", e.clock)
	svc := e.service("worker-a")
	if _, err := svc.RequestDeletion(context.Background(), owner, "a1"); err != nil {
		t.Fatal(err)
	}
	e.clock = e.clock.Add(grace)

	ctx, cancel := context.WithCancel(context.Background())
	e.storageAPI = cancelAfterDelete{e.storage, cancel}
	var stats CycleStats
	_ = e.service("worker-a").ProcessDeletions(ctx, &stats)
	op := e.op("a1")
	if op.State != model.DeletionInProgress || op.LeaseOwner != "worker-a" || e.status("a1") != model.AssetStatusSoftDeleted {
		t.Fatalf("a crashed step must leave the deletion in progress under its lease: %+v", op)
	}

	// Another worker takes over once the lease expires and finds the first
	// object already gone.
	e.storageAPI = nil
	e.clock = e.clock.Add(5*time.Minute + time.Second)
	calls := e.storage.DeleteCalls
	if stats := e.run(e.service("worker-b")); stats.Completed != 1 {
		t.Fatalf("expected recovery to complete: %+v", stats)
	}
	if e.objects("a1") != 0 || e.status("a1") != model.AssetStatusDeleted {
		t.Fatal("recovery must finish the deletion")
	}
	// What is left, and the gone object's key once more, which is a no-op
	// here and clears versions a crashed step left on a versioned bucket.
	if extra := e.storage.DeleteCalls - calls; extra != 2 {
		t.Fatalf("recovery must delete what is left and clear the other key, made %d calls", extra)
	}
}

// failComplete fails the metadata step once, as a database outage right
// after storage deletion was confirmed.
type failComplete struct {
	*memory.DeletionRepository
	failed bool
}

func (f *failComplete) Complete(ctx context.Context, id, owner string, now time.Time, event model.LifecycleEvent) error {
	if !f.failed {
		f.failed = true
		return errors.New("database unavailable")
	}
	return f.DeletionRepository.Complete(ctx, id, owner, now, event)
}

func TestMetadataFailureAfterStorageDeletionIsRetried(t *testing.T) {
	e := newEnv(t)
	e.published("a1", e.clock)
	e.deletionRepo = &failComplete{DeletionRepository: e.deletions}
	svc := e.service("worker-a")
	if _, err := svc.RequestDeletion(context.Background(), owner, "a1"); err != nil {
		t.Fatal(err)
	}
	e.clock = e.clock.Add(grace)

	if stats := e.run(svc); stats.Retried != 1 {
		t.Fatalf("expected a retry: %+v", stats)
	}
	op := e.op("a1")
	if op.State != model.DeletionStorageDeleted || op.StorageDeletedAt == nil || e.status("a1") != model.AssetStatusSoftDeleted {
		t.Fatalf("storage deletion must stay recorded while the metadata step waits: %+v", op)
	}

	e.clock = e.clock.Add(time.Hour)
	calls := e.storage.DeleteCalls
	if stats := e.run(svc); stats.Completed != 1 || e.storage.DeleteCalls != calls {
		t.Fatalf("the retry must only finish the metadata: %+v, %d new deletes", stats, e.storage.DeleteCalls-calls)
	}
	if e.status("a1") != model.AssetStatusDeleted {
		t.Fatal("asset must be tombstoned")
	}
}

// failingDeletes fails every delete, transiently or permanently.
type failingDeletes struct {
	*storagememory.ObjectStorage
	err error
}

func (f failingDeletes) DeleteObject(context.Context, string, string) error { return f.err }

func TestStorageFailuresRetryThenFailThenRequeue(t *testing.T) {
	e := newEnv(t)
	e.published("a1", e.clock)
	e.storageAPI = failingDeletes{e.storage, errors.New("throttled")}
	svc := e.service("worker-a")
	if _, err := svc.RequestDeletion(context.Background(), owner, "a1"); err != nil {
		t.Fatal(err)
	}
	e.clock = e.clock.Add(grace)

	for attempt := 1; attempt <= 3; attempt++ {
		stats := e.run(svc)
		op := e.op("a1")
		if attempt < 3 && (stats.Retried != 1 || op.State != model.DeletionRetryWait || op.AttemptCount != attempt) {
			t.Fatalf("attempt %d: %+v %+v", attempt, stats, op)
		}
		if attempt == 3 && (stats.Failed != 1 || op.State != model.DeletionFailed) {
			t.Fatalf("attempts exhausted: %+v %+v", stats, op)
		}
		e.clock = e.clock.Add(time.Hour)
	}
	if !slices.Contains(e.events.Names(), "deletion_failed") || e.status("a1") != model.AssetStatusSoftDeleted {
		t.Fatal("a failed deletion must be audited and keep the asset hidden")
	}

	e.storageAPI = nil
	svc = e.service("worker-a")
	op, err := svc.Requeue(context.Background(), "tenant-1", e.op("a1").ID, "ops")
	if err != nil || op.State != model.DeletionRetryWait || op.AttemptCount != 0 {
		t.Fatalf("requeue: %+v %v", op, err)
	}
	if stats := e.run(svc); stats.Completed != 1 {
		t.Fatalf("expected completion after requeue: %+v", stats)
	}

	// A permanent error fails at once.
	e2 := newEnv(t)
	e2.published("a2", e2.clock)
	e2.storageAPI = failingDeletes{e2.storage, retry.Permanent(errors.New("access denied"))}
	svc2 := e2.service("worker-a")
	if _, err := svc2.RequestDeletion(context.Background(), owner, "a2"); err != nil {
		t.Fatal(err)
	}
	e2.clock = e2.clock.Add(grace)
	if stats := e2.run(svc2); stats.Failed != 1 || e2.op("a2").AttemptCount != 1 {
		t.Fatalf("a permanent error must fail at once: %+v", stats)
	}
}

func TestConcurrentWorkersDeleteEachAssetOnce(t *testing.T) {
	e := newEnv(t)
	const n = 6
	for i := 0; i < n; i++ {
		e.published(fmt.Sprintf("a%d", i), e.clock)
	}
	svc := e.service("worker-a")
	for i := 0; i < n; i++ {
		if _, err := svc.RequestDeletion(context.Background(), owner, fmt.Sprintf("a%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	e.clock = e.clock.Add(grace)

	var wg sync.WaitGroup
	var mu sync.Mutex
	completed := 0
	for w := 0; w < 3; w++ {
		worker := e.service(fmt.Sprintf("worker-%d", w))
		wg.Add(1)
		go func() {
			defer wg.Done()
			var stats CycleStats
			_ = worker.ProcessDeletions(context.Background(), &stats)
			mu.Lock()
			completed += stats.Completed
			mu.Unlock()
		}()
	}
	wg.Wait()

	if completed != n {
		t.Fatalf("each deletion must complete exactly once, got %d", completed)
	}
	if calls := e.storage.DeleteCalls; calls != 2*n {
		t.Fatalf("each object must be deleted once, got %d calls", calls)
	}
}

func TestSharedReferencesDeferPhysicalDeletion(t *testing.T) {
	e := newEnv(t)
	e.published("a1", e.clock)
	// A second record pointing at a1's delivery object.
	other := model.Asset{
		ID: "a2", TenantID: "tenant-1", OwnerID: "user-1", UploadID: "up-a2", ObjectKey: "assets/tenant-1/a1",
		FileName: "copy.pdf", ContentType: "application/pdf", SizeBytes: 4, Status: model.AssetStatusAvailable,
		CreatedAt: e.clock, UpdatedAt: e.clock,
	}
	if err := e.assets.Create(context.Background(), &other); err != nil {
		t.Fatal(err)
	}
	svc := e.service("worker-a")
	if _, err := svc.RequestDeletion(context.Background(), owner, "a1"); err != nil {
		t.Fatal(err)
	}
	e.clock = e.clock.Add(grace)

	if stats := e.run(svc); stats.Protected != 1 || e.objects("a1") != 2 {
		t.Fatalf("a shared object must not be deleted: %+v", stats)
	}
	if reason := e.op("a1").ProtectedReason; !strings.Contains(reason, "asset a2 is AVAILABLE") {
		t.Fatalf("reason %q", reason)
	}

	e.assets.Update("a2", func(a *model.Asset) { a.Status = model.AssetStatusDeleted })
	e.clock = e.clock.Add(time.Hour)
	if stats := e.run(svc); stats.Completed != 1 {
		t.Fatalf("once nothing else refers to it the deletion completes: %+v", stats)
	}
}

func TestActiveProcessingBlocksDeletion(t *testing.T) {
	e := newEnv(t)
	session := model.UploadSession{
		ID: "up-1", TenantID: "tenant-1", OwnerID: "user-1", ObjectKey: "quarantine/tenant-1/up-1", FileName: "f.pdf",
		ContentType: "application/pdf", ExpectedSize: 4, Status: model.UploadStatusCompleted,
		CreatedAt: e.clock, UpdatedAt: e.clock, ExpiresAt: e.clock.Add(time.Hour),
	}
	if err := e.uploads.Create(context.Background(), &session); err != nil {
		t.Fatal(err)
	}
	if _, err := e.jobs.EnqueueDue(context.Background(), e.clock, 3); err != nil {
		t.Fatal(err)
	}
	asset, err := e.assets.GetByUpload(context.Background(), "tenant-1", "up-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.service("worker-a").RequestDeletion(context.Background(), owner, asset.ID); !errors.Is(err, model.ErrDeletionBlocked) {
		t.Fatalf("an asset in active processing must not be deleted, got %v", err)
	}
}

func TestQuarantineEvidenceIsKeptUntilItsRetention(t *testing.T) {
	e := newEnv(t)
	asset := e.published("a1", e.clock)
	e.assets.Update("a1", func(a *model.Asset) {
		a.Status, a.RejectionReason, a.ObjectKey = model.AssetStatusRejected, "malware_detected", "quarantine/tenant-1/up-a1"
	})
	e.storage.DeleteObject(context.Background(), "bucket", "assets/tenant-1/a1")
	retainUntil := e.clock.Add(30 * day)
	if err := e.quarantine.Reject(context.Background(), model.QuarantineRecord{
		TenantID: "tenant-1", AssetID: asset.ID, UploadID: asset.UploadID, ObjectKey: "quarantine/tenant-1/up-a1",
		Category: model.QuarantineMalwareDetected, Reason: "malware_detected", RetainUntil: &retainUntil, CreatedAt: e.clock,
	}); err != nil {
		t.Fatal(err)
	}

	svc := e.service("worker-a")
	if _, err := svc.RequestDeletion(context.Background(), owner, "a1"); err != nil {
		t.Fatal(err)
	}
	e.clock = e.clock.Add(grace)
	if stats := e.run(svc); stats.Protected != 1 || !strings.Contains(e.op("a1").ProtectedReason, "quarantine evidence") {
		t.Fatalf("quarantine evidence must be kept: %+v %q", stats, e.op("a1").ProtectedReason)
	}

	e.clock = retainUntil
	if stats := e.run(svc); stats.Completed != 1 || e.objects("a1") != 0 {
		t.Fatalf("after quarantine retention the deletion completes: %+v", stats)
	}
}

func TestRetentionEvaluation(t *testing.T) {
	e := newEnv(t)
	now := e.clock
	e.published("old", now.Add(-11*day))
	e.published("boundary", now.Add(-10*day))
	e.published("young", now.Add(-10*day+time.Second))
	e.published("held", now.Add(-20*day))
	svc := e.service("worker-a")
	if _, err := svc.SetPolicy(context.Background(), model.RetentionPolicy{
		TenantID: "tenant-1", Scope: model.ScopeTenant, MaxAge: duration(10 * day), CreatedBy: "ops", Reason: "keep ten days",
		EffectiveFrom: now.Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.PlaceHold(context.Background(), model.LegalHold{
		TenantID: "tenant-1", Scope: model.ScopeAsset, ScopeKey: "held", Reason: "case", PlacedBy: "legal",
	}); err != nil {
		t.Fatal(err)
	}

	// Report-only: nothing is scheduled.
	e.expire = false
	var report CycleStats
	if err := e.service("worker-a").EvaluateRetention(context.Background(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Expired != 2 || report.HeldFromExpiry != 1 || len(e.deletions.Ops()) != 0 {
		t.Fatalf("report-only evaluation: %+v, %d ops", report, len(e.deletions.Ops()))
	}

	// A dry run writes nothing either, not even its cursor.
	var dry CycleStats
	if err := e.service("worker-a", func(c *Config) { c.DryRun = true; c.ExpireAssets = true }).EvaluateRetention(context.Background(), &dry); err != nil {
		t.Fatal(err)
	}
	if dry.Expired != 2 || len(e.deletions.Ops()) != 0 {
		t.Fatalf("dry run: %+v", dry)
	}

	e.expire = true
	var stats CycleStats
	if err := e.service("worker-a").EvaluateRetention(context.Background(), &stats); err != nil {
		t.Fatal(err)
	}
	if stats.Expired != 2 || e.status("old") != model.AssetStatusSoftDeleted || e.status("boundary") != model.AssetStatusSoftDeleted ||
		e.status("young") != model.AssetStatusAvailable || e.status("held") != model.AssetStatusAvailable {
		t.Fatalf("expiry: %+v old=%s boundary=%s young=%s held=%s", stats, e.status("old"), e.status("boundary"), e.status("young"), e.status("held"))
	}
	if op := e.op("old"); op.Reason != model.DeletionReasonExpired || op.RequestedBy != "policy" || !strings.Contains(op.PolicyDetail, "TENANT v1") {
		t.Fatalf("expiration op %+v", op)
	}
}

func TestRetentionEvaluationResumesFromItsCursor(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < 5; i++ {
		e.published(fmt.Sprintf("a%d", i), e.clock)
	}
	svc := e.service("worker-a", func(c *Config) { c.BatchSize = 2 })
	var seen []int
	for cycle := 0; cycle < 4; cycle++ {
		var stats CycleStats
		if err := svc.EvaluateRetention(context.Background(), &stats); err != nil {
			t.Fatal(err)
		}
		seen = append(seen, stats.Evaluated)
	}
	// 2, 2, 1 (end: start over), then 2 again.
	if !slices.Equal(seen, []int{2, 2, 1, 2}) {
		t.Fatalf("evaluated per cycle %v", seen)
	}
}

func TestDryRunDeletionChangesNothing(t *testing.T) {
	e := newEnv(t)
	e.published("a1", e.clock)
	if _, err := e.service("worker-a").RequestDeletion(context.Background(), owner, "a1"); err != nil {
		t.Fatal(err)
	}
	e.clock = e.clock.Add(grace)
	before := e.op("a1")

	var stats CycleStats
	if err := e.service("worker-a", func(c *Config) { c.DryRun = true }).ProcessDeletions(context.Background(), &stats); err != nil {
		t.Fatal(err)
	}
	after := e.op("a1")
	if len(stats.DeletionReport) != 1 || !strings.Contains(stats.DeletionReport[0], "would delete") {
		t.Fatalf("report %v", stats.DeletionReport)
	}
	if after.State != before.State || after.LeaseOwner != "" || e.objects("a1") != 2 || e.storage.DeleteCalls != 0 {
		t.Fatalf("a dry run must change nothing: %+v", after)
	}
}

func TestCrossTenantRequestsSeeNothing(t *testing.T) {
	e := newEnv(t)
	e.published("a1", e.clock)
	svc := e.service("worker-a")
	intruder := member("tenant-2", "user-1", model.RoleDeveloper)

	if _, err := svc.RequestDeletion(context.Background(), intruder, "a1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another tenant must not delete, got %v", err)
	}
	if _, err := svc.RequestDeletion(context.Background(), member("tenant-1", "user-2", model.RoleAuditor), "a1"); !errors.Is(err, model.ErrForbidden) {
		t.Fatalf("a member without assets.delete must not delete, got %v", err)
	}
	if _, err := svc.OperatorDelete(context.Background(), "tenant-2", "a1", "ops", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an operator scoped to another tenant must not delete, got %v", err)
	}
	if _, err := svc.Restore(context.Background(), intruder, "a1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another tenant must not restore, got %v", err)
	}

	// A worker scoped to tenant-2 never claims tenant-1's work.
	if _, err := svc.RequestDeletion(context.Background(), owner, "a1"); err != nil {
		t.Fatal(err)
	}
	e.clock = e.clock.Add(grace)
	if stats := e.run(e.service("worker-b", func(c *Config) { c.TenantID = "tenant-2" })); stats.Claimed != 0 || e.objects("a1") != 2 {
		t.Fatalf("a tenant-scoped worker claimed another tenant's deletion: %+v", stats)
	}
}

// In a versioned bucket no version of the asset's keys survives: the
// recorded one goes by its ID, the rest (a second copy, a source an
// earlier attempt only hid behind a delete marker) with the key. A key
// that only shares a prefix is left alone.
func TestVersionedBucketsKeepNoVersionOfADeletedAsset(t *testing.T) {
	e := newEnv(t)
	e.storage.Versioning = true
	e.published("a1", e.clock)
	recorded := e.storage.Versions("assets/tenant-1/a1")[0]
	e.assets.Update("a1", func(a *model.Asset) { a.StorageVersionID = recorded })
	e.storage.PutObject("assets/tenant-1/a1", []byte("a second copy"))
	e.storage.HideObject("quarantine/tenant-1/up-a1")
	e.storage.PutObject("quarantine/tenant-1/up-a10", []byte("another upload"))

	svc := e.service("worker-a")
	if _, err := svc.RequestDeletion(context.Background(), owner, "a1"); err != nil {
		t.Fatal(err)
	}
	e.clock = e.clock.Add(grace)
	if stats := e.run(svc); stats.Completed != 1 {
		t.Fatalf("%+v", stats)
	}
	for _, key := range []string{"assets/tenant-1/a1", "quarantine/tenant-1/up-a1"} {
		if left := e.storage.Versions(key); len(left) != 0 {
			t.Fatalf("%s keeps versions %v", key, left)
		}
	}
	if !slices.Contains(e.storage.DeletedVersions, recorded) {
		t.Fatalf("the recorded version is deleted: %v", e.storage.DeletedVersions)
	}
	if len(e.storage.Versions("quarantine/tenant-1/up-a10")) != 1 {
		t.Fatal("another upload's key was touched")
	}
}

func TestVersionedObjectsAreDeletedByVersion(t *testing.T) {
	e := newEnv(t)
	e.published("a1", e.clock)
	e.assets.Update("a1", func(a *model.Asset) { a.StorageVersionID = "v-123" })
	e.storage.SetVersion("assets/tenant-1/a1", "v-123")
	svc := e.service("worker-a")
	if _, err := svc.RequestDeletion(context.Background(), owner, "a1"); err != nil {
		t.Fatal(err)
	}
	e.clock = e.clock.Add(grace)
	if stats := e.run(svc); stats.Completed != 1 {
		t.Fatalf("%+v", stats)
	}
	if !slices.Equal(e.storage.DeletedVersions, []string{"v-123"}) || e.objects("a1") != 0 {
		t.Fatalf("the recorded version must be deleted exactly, got %v", e.storage.DeletedVersions)
	}
}
