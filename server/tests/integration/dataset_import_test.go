package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/catalogbench"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/config"
	datasetservice "github.com/sanjeev0303/object-storage-platform/internal/service/dataset"
)

// Phase 10 manifest imports against PostgreSQL and in-memory storage.

// drain runs a dataset worker's sweeps until nothing is left to do, waiting
// out short retry backoffs.
func drain(t *testing.T, worker *datasetservice.Service, owner string) {
	t.Helper()
	ctx := context.Background()
	idle := 0
	for range 500 {
		imports, err := worker.RunDueImports(ctx, owner, 2)
		if err != nil {
			t.Fatal(err)
		}
		validations, err := worker.RunDueValidations(ctx, owner, 2)
		if err != nil {
			t.Fatal(err)
		}
		if imports+validations > 0 {
			idle = 0
			continue
		}
		// Retries are due a few milliseconds later; three idle rounds in a
		// row mean nothing is left.
		if idle++; idle == 3 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("dataset work still due after 500 rounds")
}

func (s *securityStack) importOf(t *testing.T, p model.Principal, datasetID, importID string) model.ManifestImport {
	t.Helper()
	imp, err := s.datasets.Import(context.Background(), p, datasetID, importID)
	if err != nil {
		t.Fatal(err)
	}
	return imp
}

func TestInlineManifestImport(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	acme := s.team(t, "ds-imp-a")
	globex := s.team(t, "ds-imp-b")
	dev := acme.developer
	objs := s.publishObjects(t, dev, "imp", 4)
	foreign := s.publishAt(t, globex.developer, "imp/item-00.txt", textData("theirs", 40))
	d := s.newDataset(t, dev, "imported")

	lines := []string{
		fmt.Sprintf(`{"object_id":%q,"label":"cat","split":"train","metadata":{"source":"camera-1"}}`, objs[0].ObjectID), // 1
		``, // 2: blank
		`{"path":"imp/item-01.txt","label":"dog","schema_version":1}`,                            // 3: by path
		fmt.Sprintf(`{"object_id":%q,"label":"again"}`, objs[0].ObjectID),                        // 4: duplicate
		fmt.Sprintf(`{"object_id":%q}`, foreign.ObjectID),                                        // 5: another tenant's
		fmt.Sprintf(`{"object_id":%q,"tenant_id":%q}`, objs[2].ObjectID, globex.org.ID),          // 6: tenant field
		`{"path":"../../etc/passwd"}`,                                                            // 7: unsafe path
		fmt.Sprintf(`{"object_id":%q`, objs[2].ObjectID),                                         // 8: malformed
		"{\"object_id\":\"\xff\xfe\"}",                                                           // 9: not UTF-8
		fmt.Sprintf(`{"object_id":%q,"version":7}`, objs[2].ObjectID),                            // 10: no such version
		fmt.Sprintf(`{"object_id":%q,"label":%q}`, objs[2].ObjectID, strings.Repeat("x", 70000)), // 11: too long
		fmt.Sprintf(`{"object_id":%q,"split":"test","version":1}`, objs[3].ObjectID),             // 12
	}
	body := []byte(strings.Join(lines, "\n") + "\n")
	in := datasetservice.ImportInput{IdempotencyKey: "import-1"}
	imp, created, err := s.datasets.ImportInline(ctx, dev, d.ID, in, bytes.NewReader(body))
	if err != nil || !created || imp.State != model.ImportQueued || imp.Source != model.ManifestSourceInline {
		t.Fatalf("import: %+v %v", imp, err)
	}
	sum := sha256.Sum256(body)
	if imp.ManifestSHA256 != hex.EncodeToString(sum[:]) || imp.ManifestBytes != int64(len(body)) {
		t.Fatalf("recorded manifest: %+v", imp)
	}
	// The same key and manifest is the same import; another manifest under
	// the key is refused.
	if again, created, err := s.datasets.ImportInline(ctx, dev, d.ID, in, bytes.NewReader(body)); err != nil || created || again.ID != imp.ID {
		t.Fatalf("replay: %+v %v", again, err)
	}
	if _, _, err := s.datasets.ImportInline(ctx, dev, d.ID, in, strings.NewReader(lines[0])); !errors.Is(err, model.ErrImportConflict) {
		t.Fatalf("other manifest under the key: %v", err)
	}

	drain(t, s.newDatasetService(t, nil), "worker-1")
	imp = s.importOf(t, dev, d.ID, imp.ID)
	if imp.State != model.ImportCompleted || imp.LinesRead != 12 || imp.MembersAdded != 3 || imp.LinesRejected != 8 || imp.CompletedAt == nil {
		t.Fatalf("finished import: %+v", imp)
	}
	errs, err := s.datasets.ImportErrors(ctx, dev, d.ID, imp.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	want := map[int64]string{
		4: model.LineDuplicateInFile, 5: model.LineObjectNotFound, 6: "tenant_field_not_allowed", 7: model.LineInvalidPath,
		8: model.LineInvalidJSON, 9: model.LineInvalidUTF8, 10: model.LineVersionNotFound, 11: model.LineTooLong,
	}
	if len(errs) != len(want) {
		t.Fatalf("errors: %+v", errs)
	}
	for _, e := range errs {
		if want[e.Line] != e.Code || e.Detail == "" {
			t.Errorf("line %d: %s (%s), want %s", e.Line, e.Code, e.Detail, want[e.Line])
		}
	}
	// Paged by line number.
	if page, _ := s.datasets.ImportErrors(ctx, dev, d.ID, imp.ID, 7, 2); len(page) != 2 || page[0].Line != 8 {
		t.Fatalf("page after line 7: %+v", page)
	}
	dataset, _ := s.datasets.Get(ctx, dev, d.ID)
	members, _ := s.datasets.Members(ctx, dev, d.ID, "", 10)
	if dataset.MemberCount != 3 || len(members.Items) != 3 || members.Items[0].Source != "import:"+imp.ID {
		t.Fatalf("draft after import: %d %+v", dataset.MemberCount, members.Items)
	}
	// The dataset was imported from it.
	lineage, err := s.datasets.DatasetLineage(ctx, dev, d.ID, datasetservice.LineageRequest{})
	if err != nil || len(lineage.Edges) != 1 || lineage.Edges[0].Relation != model.RelImportedFrom || lineage.Edges[0].To.RefID != imp.ID {
		t.Fatalf("lineage: %+v %v", lineage, err)
	}

	// The duplicate policy decides what a later import does to members.
	relabel := fmt.Sprintf(`{"object_id":%q,"label":"lion"}`, objs[0].ObjectID) + "\n" +
		fmt.Sprintf(`{"object_id":%q,"label":"wolf"}`, objs[1].ObjectID) + "\n"
	for _, c := range []struct {
		policy           model.DuplicatePolicy
		updated, skipped int64
	}{{model.DuplicateSkip, 0, 2}, {model.DuplicateReplace, 2, 0}} {
		imp, _, err := s.datasets.ImportInline(ctx, dev, d.ID, datasetservice.ImportInput{IdempotencyKey: "relabel-" + string(c.policy),
			DuplicatePolicy: c.policy}, strings.NewReader(relabel))
		if err != nil {
			t.Fatal(err)
		}
		drain(t, s.newDatasetService(t, nil), "worker-1")
		if imp = s.importOf(t, dev, d.ID, imp.ID); imp.MembersUpdated != c.updated || imp.MembersSkipped != c.skipped || imp.LinesRejected != 0 {
			t.Fatalf("%s: %+v", c.policy, imp)
		}
	}

	// Bad requests.
	for _, c := range []struct {
		in   datasetservice.ImportInput
		body string
		want error
	}{
		{datasetservice.ImportInput{}, lines[0], datasetservice.ErrInvalidRequest},
		{datasetservice.ImportInput{IdempotencyKey: "k", DuplicatePolicy: "MERGE"}, lines[0], datasetservice.ErrInvalidRequest},
		{datasetservice.ImportInput{IdempotencyKey: "k"}, "\n\n", datasetservice.ErrInvalidRequest},
		{datasetservice.ImportInput{IdempotencyKey: "k", SHA256: strings.Repeat("0", 64)}, lines[0], datasetservice.ErrManifestChecksum},
	} {
		if _, _, err := s.datasets.ImportInline(ctx, dev, d.ID, c.in, strings.NewReader(c.body)); !errors.Is(err, c.want) {
			t.Errorf("%+v: %v, want %v", c.in, err, c.want)
		}
	}
	small := s.newDatasetService(t, func(c *config.Datasets) { c.MaxInlineManifestBytes = 64 })
	if _, _, err := small.ImportInline(ctx, dev, d.ID, datasetservice.ImportInput{IdempotencyKey: "big"}, bytes.NewReader(body)); !errors.Is(err, datasetservice.ErrManifestTooLarge) {
		t.Fatalf("oversized inline manifest: %v", err)
	}
	// Other tenants cannot import into it, or see its imports.
	if _, _, err := s.datasets.ImportInline(ctx, globex.developer, d.ID, datasetservice.ImportInput{IdempotencyKey: "x"}, strings.NewReader(lines[0])); !errors.Is(err, model.ErrDatasetNotFound) {
		t.Fatalf("cross-tenant import: %v", err)
	}
	if _, err := s.datasets.Import(ctx, globex.developer, d.ID, imp.ID); !errors.Is(err, model.ErrImportNotFound) {
		t.Fatalf("cross-tenant read: %v", err)
	}
}

func TestStagedManifestImportResumesAfterACrash(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	acme := s.team(t, "ds-staged")
	dev := acme.developer
	const objects = 600
	if _, err := catalogbench.Populate(ctx, s.db.Pool, catalogbench.Spec{
		TenantID: acme.org.ID, Objects: objects, VersionsPerObject: 1, Datasets: 1, BatchSize: objects, Base: time.Now(),
	}, nil); err != nil {
		t.Fatal(err)
	}
	var manifest bytes.Buffer
	for i := range objects {
		if i%100 == 0 {
			// Some lines name their object by path.
			fmt.Fprintf(&manifest, "{\"path\":\"%s/%d/file-%09d.txt\",\"split\":\"train\"}\n",
				[]string{"photos", "docs", "videos", "logs"}[i%4], 2020+i%6, i)
			continue
		}
		fmt.Fprintf(&manifest, "{\"object_id\":\"obj-%s-%d\",\"label\":\"c%d\"}\n", acme.org.ID, i, i%7)
	}
	body := manifest.Bytes()
	sum := sha256.Sum256(body)
	d := s.newDataset(t, dev, "staged")

	imp, upload, created, err := s.datasets.StageImport(ctx, dev, d.ID, datasetservice.ImportInput{
		IdempotencyKey: "staged-1", SizeBytes: int64(len(body)), SHA256: hex.EncodeToString(sum[:]),
	})
	if err != nil || !created || imp.State != model.ImportAwaitingUpload || upload.URL == "" {
		t.Fatalf("stage: %+v %+v %v", imp, upload, err)
	}
	if _, err := s.datasets.SubmitImport(ctx, dev, d.ID, imp.ID); !errors.Is(err, datasetservice.ErrManifestNotStaged) {
		t.Fatalf("submit before upload: %v", err)
	}
	s.ie.storage.PutObject(imp.ManifestKey, body)
	if imp, err = s.datasets.SubmitImport(ctx, dev, d.ID, imp.ID); err != nil || imp.State != model.ImportQueued {
		t.Fatalf("submit: %+v %v", imp, err)
	}

	// A worker that lets go after every batch reads part of it...
	slow := s.newDatasetService(t, func(c *config.Datasets) { c.ImportBatch, c.ImportBudget = 100, time.Nanosecond })
	if n, err := slow.RunDueImports(ctx, "worker-slow", 1); err != nil || n != 1 {
		t.Fatalf("first pass: %d %v", n, err)
	}
	if imp = s.importOf(t, dev, d.ID, imp.ID); imp.LinesRead != 100 || imp.MembersAdded != 100 || imp.State != model.ImportRunning {
		t.Fatalf("after one batch: %+v", imp)
	}
	// ...then another claims it and dies holding it.
	if claimed, err := s.datasetDB.ClaimImports(ctx, "worker-dead", imp.ID, time.Now(), 50*time.Millisecond, 1); err != nil || len(claimed) != 1 {
		t.Fatalf("dead claim: %v %v", claimed, err)
	}
	time.Sleep(100 * time.Millisecond)
	// A storage failure costs one attempt; the import resumes from its
	// checkpoint once the lease and the backoff pass.
	s.ie.storage.RangeErr = errors.New("storage unavailable")
	drain(t, s.newDatasetService(t, func(c *config.Datasets) { c.ImportBatch = 100 }), "worker-live")

	imp = s.importOf(t, dev, d.ID, imp.ID)
	if imp.State != model.ImportCompleted || imp.LinesRead != objects || imp.MembersAdded != objects || imp.LinesRejected != 0 {
		t.Fatalf("resumed import: %+v", imp)
	}
	var rows int64
	if err := s.db.Pool.QueryRow(ctx, `SELECT count(*) FROM dataset_members WHERE dataset_id = $1`, d.ID).Scan(&rows); err != nil || rows != objects {
		t.Fatalf("member rows: %d %v", rows, err)
	}
	if got, _ := s.datasets.Get(ctx, dev, d.ID); got.MemberCount != objects {
		t.Fatalf("member count: %d", got.MemberCount)
	}
	// The whole draft snapshots, and its hash is the canonical one.
	v, _, err := s.datasets.Snapshot(ctx, dev, d.ID, "")
	if err != nil || v.MemberCount != objects {
		t.Fatalf("snapshot: %+v %v", v, err)
	}
	all, err := s.datasetDB.ListVersionMembers(ctx, acme.org.ID, v.ID, 0, objects)
	if err != nil || manifestHash(all) != v.ManifestSHA256 {
		t.Fatalf("hash of %d members: %v", len(all), err)
	}

	// A declared checksum the upload does not match fails the import before
	// anything is added; so does a manifest replaced after submission.
	bad, _, _, err := s.datasets.StageImport(ctx, dev, d.ID, datasetservice.ImportInput{
		IdempotencyKey: "staged-bad", SizeBytes: int64(len(body)), SHA256: strings.Repeat("ab", 32), DuplicatePolicy: model.DuplicateReplace,
	})
	if err != nil {
		t.Fatal(err)
	}
	s.ie.storage.PutObject(bad.ManifestKey, body)
	if _, err := s.datasets.SubmitImport(ctx, dev, d.ID, bad.ID); err != nil {
		t.Fatal(err)
	}
	replaced, _, _, err := s.datasets.StageImport(ctx, dev, d.ID, datasetservice.ImportInput{
		IdempotencyKey: "staged-replaced", SizeBytes: int64(len(body)), DuplicatePolicy: model.DuplicateReplace,
	})
	if err != nil {
		t.Fatal(err)
	}
	s.ie.storage.PutObject(replaced.ManifestKey, body)
	if _, err := s.datasets.SubmitImport(ctx, dev, d.ID, replaced.ID); err != nil {
		t.Fatal(err)
	}
	other := bytes.Replace(body, []byte(`"c1"`), []byte(`"c9"`), 1)
	s.ie.storage.PutObject(replaced.ManifestKey, other)
	drain(t, s.newDatasetService(t, nil), "worker-live")
	for _, id := range []string{bad.ID, replaced.ID} {
		got := s.importOf(t, dev, d.ID, id)
		if got.State != model.ImportFailed || got.MembersAdded+got.MembersUpdated != 0 || got.LastError == "" {
			t.Fatalf("import %s: %+v", id, got)
		}
	}
}

func TestManifestImportCancellation(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	acme := s.team(t, "ds-cancel")
	dev := acme.developer
	objs := s.publishObjects(t, dev, "cancel", 2)
	d := s.newDataset(t, dev, "cancel")
	line := fmt.Sprintf(`{"object_id":%q}`, objs[0].ObjectID)

	// A queued import is cancelled at once.
	queued, _, err := s.datasets.ImportInline(ctx, dev, d.ID, datasetservice.ImportInput{IdempotencyKey: "c1"}, strings.NewReader(line))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.datasets.CancelImport(ctx, dev, d.ID, queued.ID); err != nil || got.State != model.ImportCancelled {
		t.Fatalf("cancel queued: %+v %v", got, err)
	}
	// One a worker holds is asked to stop; the worker ends it.
	held, _, err := s.datasets.ImportInline(ctx, dev, d.ID, datasetservice.ImportInput{IdempotencyKey: "c2"}, strings.NewReader(line))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.datasetDB.ClaimImports(ctx, "worker-busy", held.ID, time.Now(), time.Hour, 1); err != nil {
		t.Fatal(err)
	}
	got, err := s.datasets.CancelImport(ctx, dev, d.ID, held.ID)
	if err != nil || got.State != model.ImportRunning || !got.CancelRequested {
		t.Fatalf("cancel held: %+v %v", got, err)
	}
	if stop, err := s.datasetDB.ExtendImport(ctx, held.ID, "worker-busy", time.Now(), time.Hour); err != nil || !stop {
		t.Fatalf("the holder learns of the cancellation: %v %v", stop, err)
	}
	if _, err := s.db.Pool.Exec(ctx, `UPDATE dataset_manifest_imports SET lease_expires_at = now() - interval '1 second' WHERE id = $1`, held.ID); err != nil {
		t.Fatal(err)
	}
	drain(t, s.newDatasetService(t, nil), "worker-next")
	if got = s.importOf(t, dev, d.ID, held.ID); got.State != model.ImportCancelled || got.MembersAdded != 0 {
		t.Fatalf("held import: %+v", got)
	}
	if _, err := s.datasets.CancelImport(ctx, dev, d.ID, held.ID); !errors.Is(err, model.ErrImportState) {
		t.Fatalf("cancel a cancelled import: %v", err)
	}
	if _, err := s.datasets.CancelImport(ctx, acme.auditor, d.ID, held.ID); !errors.Is(err, model.ErrForbidden) {
		t.Fatalf("auditor cancel: %v", err)
	}
	// Archiving cancels what waits.
	waiting, _, err := s.datasets.ImportInline(ctx, dev, d.ID, datasetservice.ImportInput{IdempotencyKey: "c3"}, strings.NewReader(line))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.datasets.Archive(ctx, acme.admin, d.ID); err != nil {
		t.Fatal(err)
	}
	if got = s.importOf(t, dev, d.ID, waiting.ID); got.State != model.ImportCancelled {
		t.Fatalf("import of an archived dataset: %+v", got)
	}
}
