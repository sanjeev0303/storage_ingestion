package reconciliation

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	storagememory "github.com/sanjeev0303/object-storage-platform/internal/infrastructure/storage/memory"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/repository/memory"
	uploadservice "github.com/sanjeev0303/object-storage-platform/internal/service/upload"
)

// Tenant connections, each with its own bucket (a memory store), stage
// uploads under stage/{tenant}/{upload}.
type router struct {
	places map[string]interfaces.StorageProvider
	down   map[string]error
}

func (r *router) Locate(_ context.Context, _, connectionID string) (interfaces.StorageLocation, error) {
	if err := r.down[connectionID]; err != nil {
		return interfaces.StorageLocation{}, err
	}
	return interfaces.StorageLocation{
		StoragePlace: interfaces.StoragePlace{ConnectionID: connectionID, StagingBucket: "bucket-" + connectionID, StagingPrefix: "stage"},
		Storage:      r.places[connectionID],
	}, nil
}

// failingUploads fails session lookups while down is set, as a database
// outage would.
type failingUploads struct {
	*memory.UploadRepository
	down bool
}

func (f *failingUploads) GetByID(ctx context.Context, tenantID, uploadID string) (*model.UploadSession, error) {
	if f.down {
		return nil, errors.New("database unavailable")
	}
	return f.UploadRepository.GetByID(ctx, tenantID, uploadID)
}

// failingList fails every object listing, with a signed URL in the error.
type failingList struct {
	*storagememory.ObjectStorage
}

func (failingList) ListObjectsPage(context.Context, string, string, string, int) (interfaces.ObjectPage, error) {
	return interfaces.ObjectPage{}, errors.New("GET https://b.example/x?X-Amz-Signature=abc: 503 slow down")
}

// listCursors records the cursors object listings start from.
type listCursors struct {
	*storagememory.ObjectStorage
	cursors []string
}

func (l *listCursors) ListObjectsPage(ctx context.Context, bucket, prefix, cursor string, limit int) (interfaces.ObjectPage, error) {
	l.cursors = append(l.cursors, cursor)
	return l.ObjectStorage.ListObjectsPage(ctx, bucket, prefix, cursor, limit)
}

type connEnv struct {
	t           *testing.T
	uploads     *failingUploads
	tasks       *memory.ReconciliationRepository
	inventory   *memory.InventoryRepository
	connections *memory.StorageConnectionRepository
	router      *router
	platform    *storagememory.ObjectStorage
	multipart   *uploadservice.MultipartService
	clock       time.Time
}

func newConnEnv(t *testing.T) *connEnv {
	t.Helper()
	e := &connEnv{
		t:           t,
		uploads:     &failingUploads{UploadRepository: memory.NewUploadRepository()},
		inventory:   memory.NewInventoryRepository(),
		connections: memory.NewStorageConnectionRepository(),
		router:      &router{places: map[string]interfaces.StorageProvider{}, down: map[string]error{}},
		platform:    storagememory.NewObjectStorage(),
		clock:       time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC),
	}
	e.tasks = memory.NewReconciliationRepository(e.uploads.UploadRepository)
	e.tasks.Parts = memory.NewMultipartUploadRepository()
	e.platform.Now = e.past
	var err error
	e.multipart, err = uploadservice.NewMultipartService(e.uploads, e.tasks.Parts, e.platform, uploadservice.MultipartServiceConfig{
		Bucket: "platform", MaxSize: 1 << 30, SessionTTL: time.Hour, PartURLTTL: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// past is when everything at the providers was written: older than the
// orphan minimum age.
func (e *connEnv) past() time.Time { return e.clock.Add(-48 * time.Hour) }

// connection adds a tenant connection with its own bucket.
func (e *connEnv) connection(id, tenant string, status model.ConnectionStatus, mode model.StagingMode) *storagememory.ObjectStorage {
	e.connections.Put(model.StorageConnection{ID: id, TenantID: tenant, Name: id, Provider: model.ProviderS3Compatible,
		Status: status, Bucket: "bucket-" + id, StagingPrefix: "stage", ReadyPrefix: "ready", StagingMode: mode}, interfaces.SealedCredential{})
	storage := storagememory.NewObjectStorage()
	storage.Now = e.past
	e.router.places[id] = storage
	return storage
}

func (e *connEnv) session(tenant, id, connectionID string, status model.UploadStatus) {
	e.t.Helper()
	at := e.past()
	err := e.uploads.Create(context.Background(), &model.UploadSession{
		ID: id, TenantID: tenant, OwnerID: "user", ObjectKey: "stage/" + tenant + "/" + id, FileName: "f", ContentType: "c",
		ExpectedSize: 1, Status: status, StorageConnectionID: connectionID, StagingConnectionID: connectionID,
		CreatedAt: at, UpdatedAt: at, ExpiresAt: at.Add(time.Hour),
	})
	if err != nil {
		e.t.Fatal(err)
	}
}

func (e *connEnv) service(pageSize, pages int) *Service {
	e.t.Helper()
	svc, err := NewService(e.tasks, e.multipart, Config{
		Owner: "worker-a", StorageProvider: "s3", BatchSize: 10, Lease: time.Minute, Grace: time.Minute, Retry: policy,
		ScanPageSize: pageSize, ScanPages: pages, OrphanMinAge: time.Hour, StuckAfter: time.Hour, DeleteUnrecorded: true,
	}, quietLog, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	svc.now = func() time.Time { return e.clock }
	return svc.Route(e.connections, e.router, e.inventory)
}

// scans are the inventory's scans of one kind on one connection.
func (e *connEnv) scans(kind, connectionID string) []model.InventoryScan {
	e.t.Helper()
	all, err := e.inventory.RecentScans(context.Background(), 100)
	if err != nil {
		e.t.Fatal(err)
	}
	var out []model.InventoryScan
	for _, s := range all {
		if s.Kind == kind && s.ConnectionID == connectionID {
			out = append(out, s)
		}
	}
	return out
}

func (e *connEnv) onlyScan(kind, connectionID string) model.InventoryScan {
	e.t.Helper()
	scans := e.scans(kind, connectionID)
	if len(scans) != 1 {
		e.t.Fatalf("%d %s scans of %s", len(scans), kind, connectionID)
	}
	return scans[0]
}

func hasUpload(s *storagememory.ObjectStorage, key, id string) bool {
	ids, _ := s.ListMultipartUploads(context.Background(), "", key)
	return slices.Contains(ids, id)
}

func findingsOn(findings []model.ReconciliationFinding, connectionID, outcome string) []string {
	var keys []string
	for _, f := range findings {
		if f.ConnectionID == connectionID && f.Outcome == outcome {
			keys = append(keys, f.Kind+" "+f.ObjectKey)
		}
	}
	slices.Sort(keys)
	return keys
}

func TestScansCoverEveryConnectionThatHoldsUploads(t *testing.T) {
	e := newConnEnv(t)
	a := e.connection("sc_a", "t-a", model.ConnectionActive, model.StagingInPlace)
	b := e.connection("sc_b", "t-b", model.ConnectionActive, model.StagingInPlace)
	c := e.connection("sc_c", "t-c", model.ConnectionDisabled, model.StagingInPlace)
	pending := e.connection("sc_p", "t-p", model.ConnectionPending, model.StagingInPlace)
	viaPlatform := e.connection("sc_v", "t-v", model.ConnectionActive, model.StagingPlatform)

	// A (versioned): an aborted upload whose provider session and object
	// were left behind; an upload no record names; a key naming no upload.
	a.Versioning = true
	e.session("t-a", "aborted-a", "sc_a", model.UploadStatusAborted)
	leftSession := a.AddUpload("stage/t-a/aborted-a")
	a.PutObject("stage/t-a/aborted-a", []byte("v1"))
	a.PutObject("stage/t-a/aborted-a", []byte("v2"))
	unknown := a.AddUpload("stage/t-a/unknown")
	a.PutObject("stage/t-a/unknown", []byte("the customer's?"))
	a.PutObject("stage/t-a/nested/key", []byte("not an upload key"))
	// B: an expired upload's object, and one under B's prefix whose upload
	// stages on A: not B's to touch.
	e.session("t-b", "expired", "sc_b", model.UploadStatusExpired)
	b.PutObject("stage/t-b/expired", []byte("x"))
	e.session("t-b", "elsewhere", "sc_a", model.UploadStatusAborted)
	b.PutObject("stage/t-b/elsewhere", []byte("x"))
	// C is disabled: it takes no new uploads but is still reconciled.
	e.session("t-c", "aborted-c", "sc_c", model.UploadStatusAborted)
	c.PutObject("stage/t-c/aborted-c", []byte("x"))
	// Neither a pending connection nor one staging on the platform is listed.
	pending.PutObject("stage/t-p/x", []byte("x"))
	viaPlatform.PutObject("stage/t-v/x", []byte("x"))
	// The platform behaves as before: an unrecorded session is aborted.
	platformOrphan := e.platform.AddUpload("quarantine/t-x/no-such-upload")

	stats, err := e.service(100, 2).RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if hasUpload(a, "stage/t-a/aborted-a", leftSession) || len(a.Versions("stage/t-a/aborted-a")) != 0 {
		t.Fatalf("A's aborted upload must lose its session and every version of its object: %v", a.Versions("stage/t-a/aborted-a"))
	}
	if !hasUpload(a, "stage/t-a/unknown", unknown) {
		t.Fatal("an unrecorded session on a tenant's bucket is never aborted")
	}
	for _, key := range []string{"stage/t-a/unknown", "stage/t-a/nested/key"} {
		if _, ok := a.Object(key); !ok {
			t.Fatalf("%s on a tenant's bucket was removed", key)
		}
	}
	if _, ok := b.Object("stage/t-b/expired"); ok {
		t.Fatal("B's expired upload object must be deleted")
	}
	if _, ok := b.Object("stage/t-b/elsewhere"); !ok {
		t.Fatal("an object of an upload bound to another connection was deleted")
	}
	if _, ok := c.Object("stage/t-c/aborted-c"); ok {
		t.Fatal("a disabled connection is still reconciled")
	}
	if pending.ListCalls != 0 || viaPlatform.ListCalls != 0 {
		t.Fatal("a pending connection and one staging on the platform are not listed")
	}
	if hasUpload(e.platform, "quarantine/t-x/no-such-upload", platformOrphan) {
		t.Fatal("the platform's own unrecorded session is aborted as before")
	}

	if got := findingsOn(stats.Findings, "sc_a", model.FindingDone); !slices.Equal(got, []string{
		"ORPHAN_MULTIPART_SESSION stage/t-a/aborted-a", "ORPHAN_OBJECT stage/t-a/aborted-a"}) {
		t.Fatalf("A done: %v", got)
	}
	if got := findingsOn(stats.Findings, "sc_a", model.FindingReported); !slices.Equal(got, []string{
		"ORPHAN_MULTIPART_SESSION stage/t-a/unknown", "ORPHAN_OBJECT stage/t-a/unknown", "UNVERIFIABLE_ITEM stage/t-a/nested/key"}) {
		t.Fatalf("A reported: %v", got)
	}
	if !slices.ContainsFunc(e.tasks.Findings(), func(f model.ReconciliationFinding) bool { return f.ConnectionID == "sc_c" }) {
		t.Fatalf("stored findings name their connection: %+v", e.tasks.Findings())
	}
	for _, id := range []string{"sc_a", "sc_b", "sc_c", model.PlatformConnectionID} {
		for _, kind := range []string{model.ScanUploadSessions, model.ScanUploadObjects} {
			if s := e.onlyScan(kind, id); s.Status != model.ScanCompleted {
				t.Fatalf("%s scan of %s: %+v", kind, id, s)
			}
		}
	}

	// Another cycle repeats nothing.
	again, err := e.service(100, 2).RunOnce(context.Background())
	if err != nil || len(findingsOn(again.Findings, "sc_a", model.FindingDone)) != 0 {
		t.Fatalf("a repeated cycle acted again: %v %v", findingsOn(again.Findings, "sc_a", model.FindingDone), err)
	}
}

func TestAFailingConnectionNeitherStopsTheOthersNorFallsBack(t *testing.T) {
	e := newConnEnv(t)
	e.connection("sc_a", "t-a", model.ConnectionActive, model.StagingInPlace)
	b := e.connection("sc_b", "t-b", model.ConnectionActive, model.StagingInPlace)
	c := e.connection("sc_c", "t-c", model.ConnectionActive, model.StagingInPlace)
	e.router.down["sc_a"] = errors.New("the stored credentials could not be decrypted")
	e.router.places["sc_b"] = failingList{b}
	e.session("t-c", "aborted-c", "sc_c", model.UploadStatusAborted)
	c.PutObject("stage/t-c/aborted-c", []byte("x"))
	// What A's leftover would look like, were A's keys looked for on the
	// platform.
	e.session("t-a", "aborted-a", "sc_a", model.UploadStatusAborted)
	e.platform.PutObject("stage/t-a/aborted-a", []byte("platform bytes"))
	svc := e.service(100, 2)

	_, err := svc.RunOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "sc_a") || !strings.Contains(err.Error(), "sc_b") {
		t.Fatalf("both failures are reported: %v", err)
	}
	if _, ok := c.Object("stage/t-c/aborted-c"); ok {
		t.Fatal("a healthy connection is reconciled although others fail")
	}
	if _, ok := e.platform.Object("stage/t-a/aborted-a"); !ok {
		t.Fatal("an unreachable connection fell back to the platform")
	}
	scanA, scanB := e.onlyScan(model.ScanUploadObjects, "sc_a"), e.onlyScan(model.ScanUploadObjects, "sc_b")
	if scanA.Errors != 1 || !strings.Contains(scanA.LastError, "decrypted") || scanA.Status != model.ScanRunning {
		t.Fatalf("A's failure is kept: %+v", scanA)
	}
	if scanB.Errors != 1 || strings.Contains(scanB.LastError, "Signature=abc") || !strings.Contains(scanB.LastError, "[redacted]") {
		t.Fatalf("B's failure is kept, sanitized: %+v", scanB)
	}

	// Failing every cycle, a scan ends FAILED after five in a row.
	for range maxScanErrors - 1 {
		_, _ = svc.RunOnce(context.Background())
	}
	if s := e.onlyScan(model.ScanUploadObjects, "sc_a"); s.Status != model.ScanFailed || s.Errors != maxScanErrors {
		t.Fatalf("A's object scan after %d failures: %+v", maxScanErrors, s)
	}
	if _, err := svc.RunOnce(context.Background()); err == nil || len(e.scans(model.ScanUploadObjects, "sc_a")) != 2 {
		t.Fatal("the next cycle starts a new scan of A")
	}
}

func TestAFailedLookupDeletesNothingAndAFailedDeleteIsRetried(t *testing.T) {
	e := newConnEnv(t)
	a := e.connection("sc_a", "t-a", model.ConnectionActive, model.StagingInPlace)
	e.session("t-a", "aborted-a", "sc_a", model.UploadStatusAborted)
	a.PutObject("stage/t-a/aborted-a", []byte("x"))
	svc := e.service(100, 1)

	e.uploads.down = true
	if _, err := svc.RunOnce(context.Background()); err == nil {
		t.Fatal("a failed lookup is an error")
	}
	if _, ok := a.Object("stage/t-a/aborted-a"); !ok || a.DeleteCalls != 0 {
		t.Fatal("nothing is deleted on a lookup that failed")
	}
	e.uploads.down = false

	a.DeleteErr = errors.New("provider unavailable") // the next delete fails
	stats, err := svc.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := findingsOn(stats.Findings, "sc_a", model.FindingFailed); len(got) != 1 {
		t.Fatalf("the failed delete is recorded: %v", got)
	}
	if _, err := svc.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.Object("stage/t-a/aborted-a"); ok {
		t.Fatal("the next scan retries the delete")
	}
}

func TestAScanResumesFromItsSavedCursorAfterARestart(t *testing.T) {
	e := newConnEnv(t)
	a := e.connection("sc_a", "t-a", model.ConnectionActive, model.StagingInPlace)
	for i := range 5 {
		id := fmt.Sprintf("owned-%d", i)
		e.session("t-a", id, "sc_a", model.UploadStatusCompleted)
		a.PutObject("stage/t-a/"+id, []byte("x"))
	}
	listed := &listCursors{ObjectStorage: a}
	e.router.places["sc_a"] = listed

	if _, err := e.service(2, 1).RunOnce(context.Background()); err != nil { // one page of two per cycle
		t.Fatal(err)
	}
	saved := e.onlyScan(model.ScanUploadObjects, "sc_a")
	if saved.Status != model.ScanRunning || saved.Cursor == "" || saved.Pages != 1 {
		t.Fatalf("the scan's place is saved: %+v", saved)
	}

	// A new process (the same worker) picks up where the first stopped.
	restarted := e.service(2, 1)
	listed.cursors = nil
	if _, err := restarted.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(listed.cursors) == 0 || listed.cursors[0] != saved.Cursor {
		t.Fatalf("the restarted scan began at %q, want the saved %q", listed.cursors, saved.Cursor)
	}
	if _, err := restarted.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s := e.onlyScan(model.ScanUploadObjects, "sc_a"); s.Status != model.ScanCompleted || s.Pages != 3 {
		t.Fatalf("five objects in pages of two: %+v", s)
	}
}
