package reconciliation

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	storagememory "github.com/sanjeev0303/object-storage-platform/internal/infrastructure/storage/memory"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/metrics"
	"github.com/sanjeev0303/object-storage-platform/internal/repository/memory"
	uploadservice "github.com/sanjeev0303/object-storage-platform/internal/service/upload"
)

// scanFixture wires the real multipart service to in-memory storage, with
// one COMPLETING session stuck for hours and one orphaned provider session.
type scanFixture struct {
	uploads *memory.UploadRepository
	parts   *memory.MultipartUploadRepository
	tasks   *memory.ReconciliationRepository
	storage *storagememory.ObjectStorage
	svc     *Service
	clock   time.Time
}

func newScanFixture(t *testing.T, dryRun bool, registry *metrics.Registry) *scanFixture {
	t.Helper()
	f := &scanFixture{
		uploads: memory.NewUploadRepository(),
		parts:   memory.NewMultipartUploadRepository(),
		storage: storagememory.NewObjectStorage(),
		clock:   time.Date(2026, 10, 16, 10, 0, 0, 0, time.UTC),
	}
	f.tasks = memory.NewReconciliationRepository(f.uploads)
	f.tasks.Parts = f.parts
	// Everything at the provider was created two days ago.
	f.storage.Now = func() time.Time { return f.clock.Add(-48 * time.Hour) }

	multipart, err := uploadservice.NewMultipartService(f.uploads, f.parts, f.storage, uploadservice.MultipartServiceConfig{
		Bucket: "b", MaxSize: 1 << 30, SessionTTL: time.Hour, PartURLTTL: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}

	stuck := model.UploadSession{
		ID: "stuck-1", TenantID: "tenant-1", OwnerID: "user-1", ObjectKey: "quarantine/tenant-1/stuck-1",
		FileName: "f", ContentType: "c", ExpectedSize: 10, Status: model.UploadStatusCompleting, StorageUploadID: "x",
		CreatedAt: f.clock.Add(-3 * time.Hour), UpdatedAt: f.clock.Add(-3 * time.Hour), ExpiresAt: f.clock.Add(time.Hour),
	}
	if err := f.uploads.Create(context.Background(), &stuck); err != nil {
		t.Fatal(err)
	}
	f.storage.AddUpload("quarantine/tenant-1/no-such-upload")

	f.svc, err = NewService(f.tasks, multipart, Config{
		Owner: "worker-a", StorageProvider: "s3", BatchSize: 10, Lease: time.Minute, Grace: time.Minute, Retry: policy,
		DryRun: dryRun, ScanPageSize: 10, ScanPages: 2, OrphanMinAge: 24 * time.Hour, StuckAfter: time.Hour,
		DeleteUnrecorded: true,
	}, quietLog, registry)
	if err != nil {
		t.Fatal(err)
	}
	f.svc.now = func() time.Time { return f.clock }
	return f
}

func TestDryRunReportsAndChangesNothing(t *testing.T) {
	f := newScanFixture(t, true, nil)

	stats, err := f.svc.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(stats.Due) != 1 || stats.Due[0].Operation != model.ReconcileComplete {
		t.Fatalf("the dry run must report the due COMPLETE, got %+v", stats.Due)
	}
	if len(stats.Findings) != 1 || stats.Findings[0].Outcome != model.FindingPlanned {
		t.Fatalf("the dry run must report the planned abort, got %+v", stats.Findings)
	}
	if len(f.tasks.Tasks()) != 0 || len(f.tasks.Findings()) != 0 {
		t.Fatal("a dry run must not write tasks or findings")
	}
	if f.storage.UploadCount() != 1 || f.storage.AbortCalls != 0 {
		t.Fatal("a dry run must not touch provider sessions")
	}
}

func TestRealRunActsAndRecords(t *testing.T) {
	f := newScanFixture(t, false, nil)

	if _, err := f.svc.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	findings := f.tasks.Findings()
	if len(findings) != 1 || findings[0].Kind != model.FindingOrphanSession || findings[0].Outcome != model.FindingDone {
		t.Fatalf("expected the orphan abort recorded, got %+v", findings)
	}
	if f.storage.UploadCount() != 0 {
		t.Fatal("the orphaned provider session must be aborted")
	}
	tasks := f.tasks.Tasks()
	if len(tasks) != 1 || tasks[0].Operation != model.ReconcileComplete {
		t.Fatalf("expected one COMPLETE task, got %+v", tasks)
	}
	// Neither object nor provider session exists: the task records FAILED.
	if tasks[0].State != model.ReconcileSucceeded || tasks[0].Outcome != string(model.UploadStatusFailed) {
		t.Fatalf("the task must record the status it left behind, got %+v", tasks[0])
	}

	// Running again finds nothing new and repeats nothing.
	aborts := f.storage.AbortCalls
	stats, err := f.svc.RunOnce(context.Background())
	if err != nil || len(stats.Findings) != 0 || f.storage.AbortCalls != aborts || len(f.tasks.Tasks()) != 1 {
		t.Fatalf("a repeated cycle must have no side effects: %+v, %v", stats, err)
	}
}

func TestStuckUploadsAreReportedAndMeasured(t *testing.T) {
	registry := metrics.New()
	f := newScanFixture(t, true, registry)

	stats, err := f.svc.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Stuck[model.UploadStatusCompleting] != 1 {
		t.Fatalf("expected one stuck COMPLETING upload, got %v", stats.Stuck)
	}
	snapshot := registry.Snapshot()
	if snapshot["uploads_stuck_COMPLETING"] != 1 || snapshot["discrepancy_ORPHAN_MULTIPART_SESSION_PLANNED"] != 1 {
		t.Fatalf("metrics not recorded: %v", snapshot)
	}
}

// failingTasks simulates a lost PostgreSQL connection.
type failingTasks struct {
	*memory.ReconciliationRepository
}

func (failingTasks) EnqueueDue(context.Context, time.Time, time.Duration, string, int) (int, error) {
	return 0, errors.New("dial tcp 127.0.0.1:5435: connection refused")
}

func TestDatabaseOutageIsReportedNotHidden(t *testing.T) {
	f := newScanFixture(t, false, nil)
	f.svc.tasks = failingTasks{f.tasks}

	_, err := f.svc.RunOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "enqueue due work") {
		t.Fatalf("a database outage must surface, got %v", err)
	}
	if len(f.tasks.Tasks()) != 0 {
		t.Fatal("no task may be created during the outage")
	}
}

func TestDatabaseSessionScanAbortsLeftoversOfFinishedSessions(t *testing.T) {
	f := newScanFixture(t, false, nil)
	f.svc.cfg.SessionScan = SessionScanDatabase
	f.svc.cfg.SessionLookback = 7 * 24 * time.Hour

	// An aborted session whose provider session survived (the abort's
	// database write raced a retry, or a provider hiccup).
	aborted := model.UploadSession{
		ID: "aborted-1", TenantID: "tenant-1", OwnerID: "user-1", ObjectKey: "quarantine/tenant-1/aborted-1",
		FileName: "f", ContentType: "c", ExpectedSize: 10, Status: model.UploadStatusAborted,
		CreatedAt: f.clock.Add(-30 * time.Hour), UpdatedAt: f.clock.Add(-30 * time.Hour), ExpiresAt: f.clock,
	}
	if err := f.uploads.Create(context.Background(), &aborted); err != nil {
		t.Fatal(err)
	}
	leftover := f.storage.AddUpload(aborted.ObjectKey)

	stats, err := f.svc.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, finding := range stats.Findings {
		found = found || (finding.StorageUploadID == leftover && finding.Outcome == model.FindingDone)
	}
	if !found {
		t.Fatalf("the leftover session must be aborted, findings %+v", stats.Findings)
	}
	// The database strategy cannot see a provider session with no record at
	// all; that is the documented MinIO limitation.
	if f.storage.UploadCount() != 1 {
		t.Fatalf("only the unrecorded orphan should remain, %d sessions left", f.storage.UploadCount())
	}
}

func TestReportedFindingsAreLoggedNotStored(t *testing.T) {
	f := newScanFixture(t, false, nil)
	f.svc.cfg.DeleteUnrecorded = false

	stats, err := f.svc.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(stats.Findings) != 1 || stats.Findings[0].Outcome != model.FindingReported {
		t.Fatalf("expected one reported finding, got %+v", stats.Findings)
	}
	if len(f.tasks.Findings()) != 0 || f.storage.UploadCount() != 1 {
		t.Fatal("a reported finding must neither be stored nor acted on")
	}
}
