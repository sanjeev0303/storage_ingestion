package cleanup

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	storagememory "github.com/sanjeev0303/object-storage-platform/internal/infrastructure/storage/memory"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/repository/memory"
)

var quietLog = slog.New(slog.NewTextHandler(io.Discard, nil))

type env struct {
	t         *testing.T
	storage   *storagememory.ObjectStorage
	inventory *memory.InventoryRepository
	cursors   *memory.CursorRepository
	uploads   *memory.UploadRepository
	assets    *memory.AssetRepository
	jobs      *memory.ProcessingJobRepository
	holds     *memory.LegalHoldRepository
	events    *memory.LifecycleEventRepository
	clock     time.Time
}

func newEnv(t *testing.T) *env {
	e := &env{
		t: t, storage: storagememory.NewObjectStorage(), inventory: memory.NewInventoryRepository(),
		cursors: memory.NewCursorRepository(), uploads: memory.NewUploadRepository(), assets: memory.NewAssetRepository(),
		holds: memory.NewLegalHoldRepository(), events: memory.NewLifecycleEventRepository(),
		clock: time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC),
	}
	e.jobs = memory.NewProcessingJobRepository(e.uploads, e.assets)
	// Objects are stored "now" by default; tests move the clock on.
	e.storage.Now = func() time.Time { return e.clock }
	return e
}

func (e *env) service(change ...func(*Config)) *Service {
	e.t.Helper()
	cfg := Config{
		Owner: "worker-a", QuarantineBucket: "bucket", DeliveryBucket: "bucket", DeliveryPrefix: "assets",
		PageSize: 2, PagesPerRun: 10, Lease: 5 * time.Minute, MinAge: time.Hour, MinObservations: 2,
		SafetyInterval: 24 * time.Hour, FailedUploadRetention: 30 * 24 * time.Hour, DeleteOrphans: true,
		MaxDeletionsPerRun: 100, MaxAttempts: 3, MaxScanErrors: 2, CheckBatch: 10,
	}
	for _, c := range change {
		c(&cfg)
	}
	svc, err := NewService(Dependencies{
		Storage: e.storage, Inventory: e.inventory, Cursors: e.cursors, Uploads: e.uploads, Assets: e.assets,
		Jobs: e.jobs, Holds: e.holds, Events: e.events,
	}, cfg, quietLog)
	if err != nil {
		e.t.Fatal(err)
	}
	svc.now = func() time.Time { return e.clock }
	return svc
}

func (e *env) asset(id string, status model.AssetStatus, key string) model.Asset {
	e.t.Helper()
	a := model.Asset{
		ID: id, TenantID: "t1", OwnerID: "u1", UploadID: "up-" + id, ObjectKey: key, FileName: "f", ContentType: "application/pdf",
		SizeBytes: 4, Status: status, CreatedAt: e.clock, UpdatedAt: e.clock,
	}
	if status == model.AssetStatusRejected {
		a.RejectionReason = "malware_detected"
	}
	if err := e.assets.Create(context.Background(), &a); err != nil {
		e.t.Fatal(err)
	}
	return a
}

func (e *env) session(id string, status model.UploadStatus, updated time.Time) {
	e.t.Helper()
	s := model.UploadSession{
		ID: id, TenantID: "t1", OwnerID: "u1", ObjectKey: "quarantine/t1/" + id, FileName: "f", ContentType: "application/pdf",
		ExpectedSize: 4, Status: status, CreatedAt: updated, UpdatedAt: updated, ExpiresAt: updated.Add(time.Hour),
	}
	if err := e.uploads.Create(context.Background(), &s); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) present(key string) bool {
	_, ok := e.storage.Object(key)
	return ok
}

// seed lays out one object of every kind the scan must tell apart.
func (e *env) seed() {
	e.asset("owned", model.AssetStatusAvailable, "assets/t1/owned")
	e.storage.PutObject("assets/t1/owned", []byte("data"))

	e.asset("rejected", model.AssetStatusRejected, "quarantine/t1/up-rejected")
	e.storage.PutObject("assets/t1/rejected", []byte("data")) // dead promotion copy

	e.storage.PutObject("assets/t1/ghost", []byte("data")) // no asset record

	e.session("up-promoted", model.UploadStatusVerified, e.clock)
	promoted := e.asset("promoted", model.AssetStatusAvailable, "assets/t1/promoted")
	promoted.UploadID = "up-promoted"
	e.assets.Update("promoted", func(a *model.Asset) { a.UploadID = "up-promoted" })
	e.jobs.Put(model.ProcessingJob{ID: "job-promoted", TenantID: "t1", AssetID: "promoted", State: model.ProcessingSucceeded})
	e.storage.PutObject("assets/t1/promoted", []byte("data"))
	e.storage.PutObject("quarantine/t1/up-promoted", []byte("data")) // leftover source

	e.asset("busy", model.AssetStatusProcessing, "quarantine/t1/up-busy")
	e.jobs.Put(model.ProcessingJob{ID: "job-busy", TenantID: "t1", AssetID: "busy", State: model.ProcessingPending, PromotionKey: "assets/t1/busy"})
	e.storage.PutObject("assets/t1/busy", []byte("data")) // promotion in progress
}

func cycle(t *testing.T, svc *Service) CycleStats {
	t.Helper()
	var stats CycleStats
	if err := svc.Scan(context.Background(), &stats); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if err := svc.Sweep(context.Background(), &stats); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	return stats
}

func TestClassification(t *testing.T) {
	e := newEnv(t)
	e.seed()
	e.session("up-failed-old", model.UploadStatusFailed, e.clock.Add(-31*24*time.Hour))
	e.session("up-failed-new", model.UploadStatusFailed, e.clock.Add(-time.Hour))
	e.session("up-aborted", model.UploadStatusAborted, e.clock)
	e.session("up-uploading", model.UploadStatusUploading, e.clock)
	e.asset("deleted", model.AssetStatusDeleted, "assets/t1/deleted")
	e.asset("held", model.AssetStatusProcessing, "quarantine/t1/up-held")
	e.jobs.Put(model.ProcessingJob{ID: "job-held", TenantID: "t1", AssetID: "held", State: model.ProcessingExhausted})
	e.asset("soft", model.AssetStatusSoftDeleted, "assets/t1/soft")
	svc := e.service()
	areas, _ := svc.areas(context.Background())
	delivery, quarantine := areas[1], areas[0]
	old := e.clock.Add(-2 * time.Hour)

	cases := []struct {
		a        area
		key      string
		category string // "" when it must be skipped
		skip     string
	}{
		{delivery, "assets/t1/owned", "", "owned"},
		{delivery, "assets/t1/rejected", model.OrphanDeadPromotionCopy, ""},
		{delivery, "assets/t1/ghost", model.OrphanNoAsset, ""},
		{delivery, "assets/t1/busy", "", "promotion_in_progress"},
		{delivery, "assets/t1/held", "", "held_asset"},
		{delivery, "assets/t1/deleted", model.OrphanDeletedAssetObject, ""},
		{delivery, "assets/t1/soft", "", "deletion_workflow"},
		{delivery, "assets/t1/nested/key", "", "not_managed"},
		{quarantine, "quarantine/t1/up-promoted", model.OrphanLeftoverSource, ""},
		{quarantine, "quarantine/t1/up-nobody", model.OrphanNoUploadSession, ""},
		{quarantine, "quarantine/t1/up-failed-old", model.OrphanExpiredFailedUpload, ""},
		{quarantine, "quarantine/t1/up-failed-new", "", "evidence_retention"},
		{quarantine, "quarantine/t1/up-aborted", "", "reconciliation"},
		{quarantine, "quarantine/t1/up-uploading", "", "active_upload"},
	}
	for _, c := range cases {
		v, err := svc.classify(context.Background(), c.a, interfaces.StoredObject{ObjectKey: c.key, LastModified: old}, true)
		if err != nil {
			t.Fatalf("%s: %v", c.key, err)
		}
		if c.category != "" && (!v.orphan || v.category != c.category) {
			t.Errorf("%s: got %+v, want orphan %s", c.key, v, c.category)
		}
		if c.category == "" && (v.orphan || v.skip != c.skip) {
			t.Errorf("%s: got %+v, want skipped as %s", c.key, v, c.skip)
		}
	}

	young, _ := svc.classify(context.Background(), delivery, interfaces.StoredObject{ObjectKey: "assets/t1/ghost", LastModified: e.clock}, true)
	if young.orphan || young.skip != "young" {
		t.Fatalf("an object newer than the grace period must be skipped: %+v", young)
	}
}

func TestMarkAndSweepNeedsRepeatedObservations(t *testing.T) {
	e := newEnv(t)
	e.clock = e.clock.Add(-2 * time.Hour)
	e.seed()
	e.clock = e.clock.Add(2 * time.Hour)
	start := e.clock
	svc := e.service()

	first := cycle(t, svc)
	if first.Marked != 3 || first.Deleted != 0 || first.ScansCompleted != 2 {
		t.Fatalf("first cycle must only mark: %+v", first)
	}

	e.clock = start.Add(time.Hour)
	if second := cycle(t, svc); second.Deleted != 0 {
		t.Fatalf("seen twice but within the safety interval: nothing may be deleted: %+v", second)
	}

	e.clock = start.Add(25 * time.Hour)
	third := cycle(t, svc)
	if third.Deleted != 2 || !e.present("assets/t1/ghost") || e.present("assets/t1/rejected") || e.present("quarantine/t1/up-promoted") {
		t.Fatalf("expected the two proven orphans deleted and the unrecorded one kept: %+v", third)
	}
	if third.Kept != 1 {
		t.Fatalf("the unrecorded orphan must be reported, not deleted: %+v", third)
	}
	for _, key := range []string{"assets/t1/owned", "assets/t1/promoted", "assets/t1/busy"} {
		if !e.present(key) {
			t.Fatalf("an owned object was deleted: %s", key)
		}
	}

	// With the operator's explicit opt-in, the unrecorded orphan goes too.
	unrecorded := e.service(func(c *Config) { c.DeleteUnrecorded = true })
	var stats CycleStats
	if err := unrecorded.Sweep(context.Background(), &stats); err != nil {
		t.Fatal(err)
	}
	if stats.Deleted != 1 || e.present("assets/t1/ghost") {
		t.Fatalf("expected the unrecorded orphan deleted after opting in: %+v", stats)
	}
	if !slices.Contains(e.events.Names(), "orphan_deleted") {
		t.Fatal("orphan deletions must be audited")
	}
}

func TestReportModeNeverDeletes(t *testing.T) {
	e := newEnv(t)
	e.clock = e.clock.Add(-2 * time.Hour)
	e.seed()
	e.clock = e.clock.Add(2 * time.Hour)
	svc := e.service(func(c *Config) { c.DeleteOrphans = false; c.DeleteUnrecorded = true })
	start := e.clock
	for _, at := range []time.Duration{0, time.Hour, 25 * time.Hour} {
		e.clock = start.Add(at)
		cycle(t, svc)
	}
	for _, key := range []string{"assets/t1/rejected", "assets/t1/ghost", "quarantine/t1/up-promoted"} {
		if !e.present(key) {
			t.Fatalf("report mode deleted %s", key)
		}
	}
}

// failingList fails listings after the first page, as a provider outage
// mid-scan would.
type failingList struct {
	*storagememory.ObjectStorage
	fail bool
}

func (f *failingList) ListObjectsPage(ctx context.Context, bucket, prefix, cursor string, limit int) (interfaces.ObjectPage, error) {
	if f.fail && cursor != "" {
		return interfaces.ObjectPage{}, errors.New("provider unavailable")
	}
	return f.ObjectStorage.ListObjectsPage(ctx, bucket, prefix, cursor, limit)
}

func TestFailedScansNeverJustifyASweep(t *testing.T) {
	e := newEnv(t)
	e.clock = e.clock.Add(-2 * time.Hour)
	e.asset("r1", model.AssetStatusRejected, "quarantine/t1/up-r1")
	e.asset("r2", model.AssetStatusRejected, "quarantine/t1/up-r2")
	e.asset("r3", model.AssetStatusRejected, "quarantine/t1/up-r3")
	for _, key := range []string{"assets/t1/r1", "assets/t1/r2", "assets/t1/r3"} {
		e.storage.PutObject(key, []byte("data"))
	}
	e.clock = e.clock.Add(2 * time.Hour)
	start := e.clock

	flaky := &failingList{ObjectStorage: e.storage, fail: true}
	scanFailing := func() {
		svc, err := NewService(Dependencies{
			Storage: flaky, Inventory: e.inventory, Cursors: e.cursors, Uploads: e.uploads, Assets: e.assets,
			Jobs: e.jobs, Holds: e.holds, Events: e.events,
		}, Config{
			Owner: "worker-a", QuarantineBucket: "bucket", DeliveryBucket: "bucket", DeliveryPrefix: "assets", TenantID: "t1",
			PageSize: 2, PagesPerRun: 10, Lease: 5 * time.Minute, MinAge: time.Hour, MinObservations: 2,
			SafetyInterval: 24 * time.Hour, FailedUploadRetention: time.Hour, DeleteOrphans: true,
			MaxDeletionsPerRun: 100, MaxAttempts: 3, MaxScanErrors: 1, CheckBatch: 10,
		}, quietLog)
		if err != nil {
			t.Fatal(err)
		}
		svc.now = func() time.Time { return e.clock }
		var stats CycleStats
		_ = svc.Scan(context.Background(), &stats)
		_ = svc.Sweep(context.Background(), &stats)
		if stats.Deleted != 0 {
			t.Fatalf("a failed scan must never lead to a deletion: %+v", stats)
		}
	}

	// Three failing scans over 25 hours: r1 and r2 are seen each time (the
	// first page), but no scan ever completes.
	for _, at := range []time.Duration{0, time.Hour, 25 * time.Hour} {
		e.clock = start.Add(at)
		scanFailing()
	}
	if !e.present("assets/t1/r1") || !e.present("assets/t1/r2") {
		t.Fatal("objects seen only in failed scans must be kept")
	}
	scans, _ := e.inventory.RecentScans(context.Background(), 10)
	for _, s := range scans {
		if s.Prefix == "assets/t1/" && s.Status != model.ScanFailed {
			t.Fatalf("expected every delivery scan to fail, got %s", s.Status)
		}
	}

	// A completed scan confirms r1 and r2, seen repeatedly since the start:
	// they may go. r3 was never listed before (its page always failed), so
	// it waits for its own safety interval.
	healthy := e.service(func(c *Config) { c.TenantID = "t1" })
	e.clock = start.Add(26 * time.Hour)
	if stats := cycle(t, healthy); stats.Deleted != 2 || !e.present("assets/t1/r3") {
		t.Fatalf("expected r1 and r2 swept and r3 kept: %+v", stats)
	}
	e.clock = start.Add(27 * time.Hour)
	if stats := cycle(t, healthy); stats.Deleted != 0 {
		t.Fatalf("r3 is within its safety interval: %+v", stats)
	}
	e.clock = start.Add(51 * time.Hour)
	if stats := cycle(t, healthy); stats.Deleted != 1 || e.present("assets/t1/r3") {
		t.Fatalf("r3 is swept after its safety interval: %+v", stats)
	}
}

func TestScanResumesFromItsCheckpoint(t *testing.T) {
	e := newEnv(t)
	e.clock = e.clock.Add(-2 * time.Hour)
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		e.asset(id, model.AssetStatusAvailable, "assets/t1/"+id)
		e.storage.PutObject("assets/t1/"+id, []byte("data"))
	}
	e.clock = e.clock.Add(2 * time.Hour)

	onePage := func(owner string) *Service {
		return e.service(func(c *Config) { c.Owner, c.PagesPerRun, c.PageSize, c.TenantID = owner, 1, 2, "t1" })
	}
	var seen []int
	for i, owner := range []string{"worker-a", "worker-b", "worker-c"} {
		// Each "restart" is a new worker; the previous lease has expired.
		e.clock = e.clock.Add(time.Duration(i) * 6 * time.Minute)
		var stats CycleStats
		if err := onePage(owner).Scan(context.Background(), &stats); err != nil {
			t.Fatal(err)
		}
		seen = append(seen, stats.ObjectsSeen)
	}
	// Quarantine prefix is empty (one page, done); delivery has 5 objects
	// in pages of two: 2, 2, then 1 and the scan completes.
	if !slices.Equal(seen, []int{2, 2, 1}) {
		t.Fatalf("objects per run %v; the scan must resume, not restart", seen)
	}
	last, ok, _ := e.inventory.LastCompleted(context.Background(), model.ScanObjects, "", "bucket", "assets/t1/")
	if !ok || last.ObjectsSeen != 5 || last.Pages != 3 {
		t.Fatalf("the completed scan must cover every object once: %+v", last)
	}
}

func TestReplacedObjectsAreClearedAndHoldsKeepOrphans(t *testing.T) {
	e := newEnv(t)
	e.clock = e.clock.Add(-2 * time.Hour)
	e.asset("r1", model.AssetStatusRejected, "quarantine/t1/up-r1")
	e.asset("r2", model.AssetStatusRejected, "quarantine/t1/up-r2")
	e.storage.PutObject("assets/t1/r1", []byte("data"))
	e.storage.PutObject("assets/t1/r2", []byte("data"))
	e.clock = e.clock.Add(2 * time.Hour)
	start := e.clock
	svc := e.service()
	cycle(t, svc)
	e.clock = start.Add(time.Hour)
	cycle(t, svc)

	// r1 is overwritten with new content; r2's asset is put under a hold.
	e.storage.PutObject("assets/t1/r1", []byte("other content"))
	if _, err := e.holds.Place(context.Background(), model.LegalHold{ID: "h1", TenantID: "t1", Scope: model.ScopeAsset, ScopeKey: "r2", Reason: "case", PlacedBy: "legal"}); err != nil {
		t.Fatal(err)
	}
	e.clock = start.Add(25 * time.Hour)
	var stats CycleStats
	if err := svc.Sweep(context.Background(), &stats); err != nil {
		t.Fatal(err)
	}
	if stats.Deleted != 0 || stats.Cleared != 1 || stats.Kept != 1 || !e.present("assets/t1/r1") || !e.present("assets/t1/r2") {
		t.Fatalf("a replaced object is cleared and a held one kept: %+v", stats)
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	e := newEnv(t)
	e.clock = e.clock.Add(-2 * time.Hour)
	e.seed()
	e.asset("lost", model.AssetStatusAvailable, "assets/t1/lost") // no object
	e.clock = e.clock.Add(2 * time.Hour)

	stats, err := e.service(func(c *Config) { c.DryRun = true; c.DeleteUnrecorded = true }).RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	candidates, _ := e.inventory.ListCandidates(context.Background(), "", 100)
	scans, _ := e.inventory.RecentScans(context.Background(), 100)
	cursor, _ := e.cursors.Get(context.Background(), "missing:*")
	if len(candidates) != 0 || len(scans) != 0 || cursor != "" || len(e.events.Names()) != 0 || e.storage.DeleteCalls != 0 {
		t.Fatalf("a dry run wrote something: %d candidates, %d scans, cursor %q, events %v", len(candidates), len(scans), cursor, e.events.Names())
	}
	if stats.Marked != 3 || stats.Missing != 2 || len(stats.Report) == 0 {
		t.Fatalf("the dry run must still report: %+v", stats)
	}
}

func TestMissingObjectsAreReportedNeverDeleted(t *testing.T) {
	e := newEnv(t)
	e.asset("lost", model.AssetStatusAvailable, "assets/t1/lost")
	e.asset("busy", model.AssetStatusProcessing, "quarantine/t1/up-busy")
	e.jobs.Put(model.ProcessingJob{ID: "job-busy", TenantID: "t1", AssetID: "busy", State: model.ProcessingPending})
	e.asset("fine", model.AssetStatusAvailable, "assets/t1/fine")
	e.storage.PutObject("assets/t1/fine", []byte("data"))
	svc := e.service()

	var stats CycleStats
	if err := svc.CheckMissing(context.Background(), &stats); err != nil {
		t.Fatal(err)
	}
	if stats.MissingChecked != 3 || stats.Missing != 2 {
		t.Fatalf("%+v", stats)
	}
	report := strings.Join(stats.Report, "\n")
	if !strings.Contains(report, "inconsistent: an available asset") || !strings.Contains(report, "awaiting processing") {
		t.Fatalf("missing objects must be classified:\n%s", report)
	}
	if asset, _ := e.assets.GetByID(context.Background(), "t1", "lost"); asset.Status != model.AssetStatusAvailable {
		t.Fatal("metadata must never be changed for a missing object")
	}

	// The next passes find it again but do not record it again that day.
	for pass := 0; pass < 3; pass++ {
		e.clock = e.clock.Add(time.Hour)
		var again CycleStats
		if err := svc.CheckMissing(context.Background(), &again); err != nil || again.Missing != 2 {
			t.Fatalf("pass %d: %+v %v", pass, again, err)
		}
	}
	recorded := 0
	for _, name := range e.events.Names() {
		if name == "object_missing" {
			recorded++
		}
	}
	if recorded != 2 {
		t.Fatalf("each missing object is recorded once a day, got %d events", recorded)
	}

	// Storage trouble is no evidence of a missing object.
	e2 := newEnv(t)
	e2.asset("lost", model.AssetStatusAvailable, "assets/t1/lost")
	e2.storage.HeadErr = errors.New("timeout")
	var stats2 CycleStats
	if err := e2.service().CheckMissing(context.Background(), &stats2); err != nil {
		t.Fatal(err)
	}
	if stats2.Missing != 0 {
		t.Fatalf("a failed check must not count as missing: %+v", stats2)
	}
}

func TestTenantScopeOnlyTouchesThatTenant(t *testing.T) {
	e := newEnv(t)
	e.clock = e.clock.Add(-2 * time.Hour)
	e.storage.PutObject("assets/t1/ghost", []byte("data"))
	e.storage.PutObject("assets/t2/ghost", []byte("data"))
	e.clock = e.clock.Add(2 * time.Hour)

	svc := e.service(func(c *Config) { c.TenantID = "t2"; c.DeleteUnrecorded = true })
	start := e.clock
	for _, at := range []time.Duration{0, time.Hour, 25 * time.Hour} {
		e.clock = start.Add(at)
		cycle(t, svc)
	}
	if !e.present("assets/t1/ghost") || e.present("assets/t2/ghost") {
		t.Fatal("a tenant-scoped cleanup must touch only that tenant's objects")
	}
}
