package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/sanjeev0303/object-storage-platform/internal/dto/request"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/service/catalog"
	uploadservice "github.com/sanjeev0303/object-storage-platform/internal/service/upload"
)

// Phase 9: the catalog against PostgreSQL, through the services.

var uploadSeq atomic.Int64

// catalogUpload is a text upload of data to path; change adjusts it.
func catalogUpload(path string, data []byte, change ...func(*request.CreateUploadRequest)) request.CreateUploadRequest {
	sum := sha256.Sum256(data)
	req := request.CreateUploadRequest{
		FileName: model.BaseName(path), ContentType: "text/plain", SizeBytes: int64(len(data)),
		ChecksumSHA256: hex.EncodeToString(sum[:]), IdempotencyKey: fmt.Sprintf("cat-%d", uploadSeq.Add(1)), Path: path,
	}
	for _, c := range change {
		c(&req)
	}
	return req
}

// startUpload starts an upload as p and stores its bytes.
func (s *securityStack) startUpload(t *testing.T, p model.Principal, req request.CreateUploadRequest, data []byte) *model.UploadSession {
	t.Helper()
	result, err := s.ie.single.CreateFor(context.Background(), p, req)
	if err != nil {
		t.Fatalf("upload to %s: %v", req.Path, err)
	}
	s.ie.storage.PutObject(result.Upload.ObjectKey, data)
	return result.Upload
}

// finishUpload completes and processes an upload, returning its asset.
func (s *securityStack) finishUpload(t *testing.T, p model.Principal, upload *model.UploadSession) model.Asset {
	t.Helper()
	ctx := context.Background()
	if _, err := s.ie.single.CompleteFor(ctx, p, upload.ID); err != nil {
		t.Fatalf("complete %s: %v", upload.ID, err)
	}
	if _, err := s.ie.proc.Intake(ctx, p.TenantID, upload.ID); err != nil {
		t.Fatalf("process %s: %v", upload.ID, err)
	}
	asset, err := s.ie.assets.GetByUpload(ctx, p.TenantID, upload.ID)
	if err != nil {
		t.Fatal(err)
	}
	return *asset
}

// publishAt uploads and publishes data at path.
func (s *securityStack) publishAt(t *testing.T, p model.Principal, path string, data []byte, change ...func(*request.CreateUploadRequest)) model.Asset {
	t.Helper()
	return s.finishUpload(t, p, s.startUpload(t, p, catalogUpload(path, data, change...), data))
}

func (s *securityStack) object(t *testing.T, p model.Principal, dataset, path string) model.CatalogObject {
	t.Helper()
	o, err := s.catalog.Lookup(context.Background(), p, dataset, path)
	if err != nil {
		t.Fatalf("lookup %s: %v", path, err)
	}
	return o
}

func withMetadata(m map[string]string) func(*request.CreateUploadRequest) {
	return func(r *request.CreateUploadRequest) { r.Metadata = m }
}

func TestCatalogObjectIsCreatedAndPublished(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	acme := s.team(t, "cat-new")
	dev := acme.developer
	data := textData("q1", 300)

	upload := s.startUpload(t, dev, catalogUpload("Reports/2026/q1.txt", data, withMetadata(map[string]string{"owner": "finance"})), data)
	if upload.ObjectID == "" {
		t.Fatal("an upload claims its object when it starts")
	}
	// Claimed, not published: found by path, absent from the listing.
	pending := s.object(t, dev, "", "reports/2026/Q1.TXT")
	if pending.ID != upload.ObjectID || pending.Published() || pending.Path != "Reports/2026/q1.txt" {
		t.Fatalf("pending object: %+v", pending)
	}
	if listed, _ := s.catalog.List(ctx, dev, catalog.ListRequest{AnyDataset: true}); len(listed.Objects) != 0 {
		t.Fatalf("an unpublished object is not listed: %+v", listed.Objects)
	}
	if all, _ := s.catalog.List(ctx, dev, catalog.ListRequest{AnyDataset: true, IncludeUnpublished: true}); len(all.Objects) != 1 {
		t.Fatalf("include=unpublished lists it: %+v", all.Objects)
	}

	asset := s.finishUpload(t, dev, upload)
	o, err := s.catalog.Get(ctx, dev, upload.ObjectID)
	if err != nil {
		t.Fatal(err)
	}
	if asset.ObjectID != o.ID || asset.Version != 1 || o.CurrentAssetID != asset.ID || o.CurrentVersion != 1 ||
		o.SizeBytes != 300 || o.ContentType != "text/plain" || o.ChecksumSHA256 != asset.ChecksumSHA256 ||
		o.Metadata["owner"] != "finance" || o.PublishedAt == nil || o.Versions != 1 {
		t.Fatalf("published object: %+v, asset %+v", o, asset)
	}
	if len(s.auditEvents(t, acme.org.ID, "catalog.object_created")) != 1 || len(s.auditEvents(t, acme.org.ID, "catalog.version_published")) != 1 {
		t.Fatal("creation and publication are audited")
	}
}

func TestDuplicatePathsAndCreateOnly(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	acme := s.team(t, "cat-dup")
	dev := acme.developer
	createOnly := func(r *request.CreateUploadRequest) { r.CreateOnly = true }

	first := s.publishAt(t, dev, "a.txt", textData("a1", 50))
	// The path is taken: a create-only upload is refused, an ordinary one
	// is the next version and nothing is overwritten.
	if _, err := s.ie.single.CreateFor(ctx, dev, catalogUpload("a.txt", textData("a2", 50), createOnly)); !errors.Is(err, model.ErrPathExists) {
		t.Fatalf("create-only on an existing path: %v", err)
	}
	second := s.publishAt(t, dev, "A.txt", textData("a2", 60))
	if second.ObjectID != first.ObjectID || second.Version != 2 {
		t.Fatalf("second upload: %+v", second)
	}
	if v1, err := s.catalog.Version(ctx, dev, first.ObjectID, 1); err != nil || v1.Status != model.AssetStatusAvailable || v1.ID != first.ID {
		t.Fatalf("the first version stays: %+v %v", v1, err)
	}

	// A create-only upload in flight reserves its path.
	s.startUpload(t, dev, catalogUpload("b.txt", textData("b", 10), createOnly), textData("b", 10))
	if _, err := s.ie.single.CreateFor(ctx, dev, catalogUpload("b.txt", textData("b2", 10), createOnly)); !errors.Is(err, model.ErrPathBusy) {
		t.Fatalf("second create-only while one is in flight: %v", err)
	}
	if _, err := s.ie.single.CreateFor(ctx, dev, catalogUpload("b.txt", textData("b3", 10))); !errors.Is(err, model.ErrPathBusy) {
		t.Fatalf("an upload while a create-only one is in flight: %v", err)
	}

	// Another namespace is another object.
	other := s.publishAt(t, dev, "a.txt", textData("a3", 50), func(r *request.CreateUploadRequest) { r.Dataset = "reports" })
	if other.ObjectID == first.ObjectID || other.Version != 1 {
		t.Fatalf("same path in a dataset: %+v", other)
	}
}

func TestPathNormalizationAndTraversal(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	acme := s.team(t, "cat-norm")
	dev := acme.developer

	composed := s.publishAt(t, dev, "Café/Menu.txt", textData("m1", 40))    // é as one code point
	decomposed := s.publishAt(t, dev, "café/menu.TXT", textData("m2", 40)) // e and a combining accent
	if composed.ObjectID != decomposed.ObjectID || decomposed.Version != 2 {
		t.Fatalf("composed and decomposed spellings are one path: %s vs %s", composed.ObjectID, decomposed.ObjectID)
	}
	o := s.object(t, dev, "", "CAFÉ//./menu.txt")
	if o.ID != composed.ObjectID || o.Path != "Café/Menu.txt" {
		t.Fatalf("lookup: %+v", o)
	}

	for _, bad := range []string{"../etc/passwd", "a/../../b.txt", "/abs.txt", `a\b.txt`, "_system/x.txt", "dir/", "invoice\u202etxt.exe"} {
		if _, err := s.ie.single.CreateFor(ctx, dev, catalogUpload(bad, textData("x", 10))); !errors.Is(err, uploadservice.ErrInvalidRequest) {
			t.Fatalf("upload to %q: %v", bad, err)
		}
		if _, err := s.catalog.Lookup(ctx, dev, "", bad); !errors.Is(err, catalog.ErrInvalidRequest) {
			t.Fatalf("lookup of %q: %v", bad, err)
		}
	}
	// A file name that is no valid path is refused when it is the path.
	if _, err := s.ie.single.CreateFor(ctx, dev, catalogUpload("", textData("x", 10), func(r *request.CreateUploadRequest) { r.FileName = "what?.txt" })); !errors.Is(err, uploadservice.ErrInvalidRequest) {
		t.Fatalf("an invalid file name as the path: %v", err)
	}
}

func TestConcurrentCreationOfOnePath(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	acme := s.team(t, "cat-race")
	dev := acme.developer

	// Ten uploads to a new path at once: one object, ten uploads of it.
	objects := make([]string, 10)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range objects {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			res, err := s.ie.single.CreateFor(ctx, dev, catalogUpload("shared/new.txt", textData(fmt.Sprint(i), 20)))
			if err != nil {
				t.Errorf("upload %d: %v", i, err)
				return
			}
			objects[i] = res.Upload.ObjectID
		}()
	}
	close(start)
	wg.Wait()
	for _, id := range objects {
		if id == "" || id != objects[0] {
			t.Fatalf("one path, one object: %v", objects)
		}
	}

	// Ten create-only uploads to another new path: exactly one wins.
	results := make([]error, 10)
	start = make(chan struct{})
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, results[i] = s.ie.single.CreateFor(ctx, dev, catalogUpload("shared/exclusive.txt", textData(fmt.Sprint(i), 20),
				func(r *request.CreateUploadRequest) { r.CreateOnly = true }))
		}()
	}
	close(start)
	wg.Wait()
	won := 0
	for _, err := range results {
		switch {
		case err == nil:
			won++
		case errors.Is(err, model.ErrPathBusy), errors.Is(err, model.ErrPathExists):
		default:
			t.Fatalf("unexpected: %v", err)
		}
	}
	if won != 1 {
		t.Fatalf("exactly one create-only upload wins, %d did", won)
	}
	var objectsAtPath int
	if err := s.db.Pool.QueryRow(ctx, `SELECT count(*) FROM catalog_objects WHERE tenant_id = $1 AND path_key LIKE 'shared/%'`,
		acme.org.ID).Scan(&objectsAtPath); err != nil || objectsAtPath != 2 {
		t.Fatalf("objects: %d %v", objectsAtPath, err)
	}
}

func TestConcurrentVersionsLeaveOneCurrent(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	acme := s.team(t, "cat-ver")
	dev := acme.developer

	// Eight uploads of one path, processed at the same time.
	uploads := make([]*model.UploadSession, 8)
	for i := range uploads {
		data := textData(fmt.Sprintf("v%d", i), 100+i)
		uploads[i] = s.startUpload(t, dev, catalogUpload("hot.txt", data), data)
		if _, err := s.ie.single.CompleteFor(ctx, dev, uploads[i].ID); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	for _, u := range uploads {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.ie.proc.Intake(ctx, acme.org.ID, u.ID); err != nil {
				t.Errorf("process: %v", err)
			}
		}()
	}
	wg.Wait()

	o := s.object(t, dev, "", "hot.txt")
	page, err := s.catalog.Versions(ctx, dev, o.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var numbers []int
	for _, v := range page.Versions {
		numbers = append(numbers, v.Version)
	}
	sort.Ints(numbers)
	if fmt.Sprint(numbers) != "[1 2 3 4 5 6 7 8]" {
		t.Fatalf("version numbers: %v", numbers)
	}
	if o.CurrentVersion != 8 || o.CurrentAssetID != page.Versions[0].ID || page.Versions[0].Version != 8 {
		t.Fatalf("the newest version is current: %+v", o)
	}

	// Deleting the current version, promoting an old one and deleting
	// another, all at once, leave exactly one valid current version.
	ops := []func() error{
		func() error { _, err := s.catalog.DeleteVersion(ctx, dev, o.ID, 8); return err },
		func() error { _, err := s.catalog.DeleteVersion(ctx, dev, o.ID, 5); return err },
		func() error {
			fresh, err := s.catalog.Get(ctx, dev, o.ID)
			if err != nil {
				return err
			}
			_, err = s.catalog.Promote(ctx, dev, o.ID, 3, fresh.Revision)
			if errors.Is(err, model.ErrRevisionConflict) {
				return nil // a concurrent change came first: legitimate
			}
			return err
		},
	}
	for _, op := range ops {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := op(); err != nil {
				t.Errorf("concurrent change: %v", err)
			}
		}()
	}
	wg.Wait()
	final := s.object(t, dev, "", "hot.txt")
	current, err := s.catalog.Version(ctx, dev, o.ID, 0)
	if err != nil || current.Status != model.AssetStatusAvailable || current.ID != final.CurrentAssetID ||
		current.Version == 8 || current.Version == 5 {
		t.Fatalf("current after concurrent changes: %+v %v", current, err)
	}
	checks, err := s.catalogDB.CheckObjects(ctx, acme.org.ID, model.CatalogPosition{}, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range checks {
		if c.CurrentStale || c.MissingCurrent || c.Unnumbered || (c.CurrentStatus != "" && c.CurrentStatus != "AVAILABLE") {
			t.Fatalf("inconsistent object after concurrent changes: %+v", c)
		}
	}
}

func TestRejectedOrHeldUploadNeverBecomesCurrent(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	acme := s.team(t, "cat-quar")
	dev := acme.developer

	good := s.publishAt(t, dev, "doc.txt", textData("good", 80))
	// Malware at the same path: rejected, no version number, not current.
	infected := infectedFile("doc.txt").data
	rejected := s.finishUpload(t, dev, s.startUpload(t, dev, catalogUpload("doc.txt", infected), infected))
	if rejected.Status != model.AssetStatusRejected || rejected.Version != 0 {
		t.Fatalf("infected upload: %+v", rejected)
	}
	// A held upload (the scanner is down) neither.
	s.ie.scannerDown.Store(true)
	held := s.finishUpload(t, dev, s.startUpload(t, dev, catalogUpload("doc.txt", textData("held", 80)), textData("held", 80)))
	s.ie.scannerDown.Store(false)
	if held.Status == model.AssetStatusAvailable || held.Version != 0 {
		t.Fatalf("held upload: %+v", held)
	}
	o := s.object(t, dev, "", "doc.txt")
	if o.CurrentAssetID != good.ID || o.CurrentVersion != 1 {
		t.Fatalf("the published version stays current: %+v", o)
	}
	if page, _ := s.catalog.Versions(ctx, dev, o.ID, 0, 10); len(page.Versions) != 1 {
		t.Fatalf("only published versions are in the history: %d", len(page.Versions))
	}
}

func TestVersionMetadataAndHistory(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	acme := s.team(t, "cat-hist")
	dev := acme.developer

	var assets []model.Asset
	for i := 1; i <= 5; i++ {
		assets = append(assets, s.publishAt(t, dev, "history.txt", textData(fmt.Sprint(i), 100*i),
			withMetadata(map[string]string{"rev": fmt.Sprint(i)})))
	}
	o := s.object(t, dev, "", "history.txt")
	if o.Metadata["rev"] != "5" || o.CurrentVersion != 5 {
		t.Fatalf("the object takes the newest upload's metadata: %+v", o)
	}
	v3, err := s.catalog.Version(ctx, dev, o.ID, 3)
	if err != nil || v3.ID != assets[2].ID || v3.Metadata["rev"] != "3" || v3.SizeBytes != 300 || v3.ChecksumSHA256 == "" {
		t.Fatalf("version 3: %+v %v", v3, err)
	}
	current, err := s.catalog.Version(ctx, dev, o.ID, 0)
	if err != nil || current.Version != 5 {
		t.Fatalf("current: %+v %v", current, err)
	}
	if _, err := s.catalog.Version(ctx, dev, o.ID, 9); !errors.Is(err, model.ErrVersionNotFound) {
		t.Fatalf("missing version: %v", err)
	}

	// Newest first, two at a time.
	var seen []int
	before := 0
	for {
		page, err := s.catalog.Versions(ctx, dev, o.ID, before, 2)
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range page.Versions {
			seen = append(seen, v.Version)
		}
		if page.NextBefore == 0 {
			break
		}
		before = page.NextBefore
	}
	if fmt.Sprint(seen) != "[5 4 3 2 1]" {
		t.Fatalf("history: %v", seen)
	}
}

func TestMetadataUpdatesAreValidatedAndConditional(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	acme := s.team(t, "cat-meta")
	dev := acme.developer
	s.publishAt(t, dev, "tagged.txt", textData("t", 50), withMetadata(map[string]string{"camera": "a7"}))
	o := s.object(t, dev, "", "tagged.txt")
	str := func(v string) *string { return &v }

	for name, patch := range map[string]map[string]*string{
		"reserved name":   {"size_bytes": str("1")},
		"reserved prefix": {"system.owner": str("x")},
		"bad key":         {"with space": str("x")},
		"control char":    {"note": str("a\x00b")},
		"too long":        {"note": str(string(make([]byte, 2000)))},
	} {
		if _, err := s.catalog.UpdateMetadata(ctx, dev, o.ID, o.Revision, patch); !errors.Is(err, catalog.ErrInvalidRequest) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if _, err := s.catalog.UpdateMetadata(ctx, dev, o.ID, 0, map[string]*string{"a": str("b")}); !errors.Is(err, catalog.ErrRevisionRequired) {
		t.Fatalf("no revision: %v", err)
	}
	if _, err := s.catalog.UpdateMetadata(ctx, acme.auditor, o.ID, o.Revision, map[string]*string{"a": str("b")}); !errors.Is(err, model.ErrForbidden) {
		t.Fatalf("an auditor changes metadata: %v", err)
	}

	// Two writers with the same revision: one wins, the other is told.
	results := make([]error, 2)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, results[i] = s.catalog.UpdateMetadata(ctx, dev, o.ID, o.Revision, map[string]*string{"writer": str(fmt.Sprint(i)), "camera": nil})
		}()
	}
	wg.Wait()
	wins, conflicts := 0, 0
	for _, err := range results {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, model.ErrRevisionConflict):
			conflicts++
		default:
			t.Fatal(err)
		}
	}
	after := s.object(t, dev, "", "tagged.txt")
	if wins != 1 || conflicts != 1 || after.Revision != o.Revision+1 || after.Metadata["camera"] != "" || after.Metadata["writer"] == "" {
		t.Fatalf("concurrent updates: %v, %+v", results, after)
	}
	// System metadata is not user metadata: the object's size and checksum
	// never changed.
	if after.SizeBytes != o.SizeBytes || after.ChecksumSHA256 != o.ChecksumSHA256 || after.CurrentAssetID != o.CurrentAssetID {
		t.Fatal("a metadata update must not touch system metadata")
	}

	// Rename: a metadata change; conditional; refused onto a taken path.
	s.publishAt(t, dev, "taken.txt", textData("x", 10))
	if _, err := s.catalog.Rename(ctx, dev, o.ID, after.Revision, "TAKEN.txt"); !errors.Is(err, model.ErrPathExists) {
		t.Fatalf("rename onto a taken path: %v", err)
	}
	if _, err := s.catalog.Rename(ctx, dev, o.ID, o.Revision, "renamed.txt"); !errors.Is(err, model.ErrRevisionConflict) {
		t.Fatalf("rename with a stale revision: %v", err)
	}
	renamed, err := s.catalog.Rename(ctx, dev, o.ID, after.Revision, "folder/renamed.txt")
	if err != nil || renamed.Path != "folder/renamed.txt" || renamed.CurrentAssetID != o.CurrentAssetID {
		t.Fatalf("rename: %+v %v", renamed, err)
	}
	if _, err := s.catalog.Lookup(ctx, dev, "", "tagged.txt"); !errors.Is(err, model.ErrObjectNotFound) {
		t.Fatalf("the old path is free: %v", err)
	}
	if v, _ := s.catalog.Version(ctx, dev, o.ID, 1); v.ObjectKey == "" || v.ID != o.CurrentAssetID {
		t.Fatal("versions and their storage keys stay")
	}
	if successes(s.auditEvents(t, acme.org.ID, "catalog.rename")) != 1 || successes(s.auditEvents(t, acme.org.ID, "catalog.metadata_update")) != 1 {
		t.Fatal("changes are audited")
	}
	if denied := s.auditEvents(t, acme.org.ID, "catalog.metadata_update"); len(denied) != 2 || denied[0].ActorID != acme.auditorUser.ID {
		t.Fatalf("the auditor's refused attempt is audited too: %+v", denied)
	}
}

func successes(events []model.AuditEvent) int {
	n := 0
	for _, e := range events {
		if e.Outcome == model.AuditSuccess {
			n++
		}
	}
	return n
}
