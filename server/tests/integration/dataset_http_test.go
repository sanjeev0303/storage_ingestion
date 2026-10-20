package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sanjeev0303/object-storage-platform/internal/dto/response"
	datasetservice "github.com/sanjeev0303/object-storage-platform/internal/service/dataset"
)

// Phase 10 over HTTP: the dataset routes through the real router, with
// real sign-in, roles, rate limits and audit.

func TestHTTPDatasetWorkflow(t *testing.T) {
	s := openSecurityStack(t, nil)
	acme := s.team(t, "ds-web")
	tok := s.signIn(t, acme.devUser.Email, acme.devPW).AccessToken
	adminTok := s.signIn(t, acme.adminUser.Email, acme.adminPW).AccessToken
	auditorTok := s.signIn(t, acme.auditorUser.Email, acme.auditorPW).AccessToken
	objs := s.publishObjects(t, acme.developer, "web", 4)
	worker := s.newDatasetService(t, nil)

	// Create: the Phase 7 request still works, and takes the new fields.
	var ds response.DatasetResponse
	r := s.expect(t, tok, "", http.MethodPost, "/api/v1/datasets",
		`{"name":"web-set","description":"pets","tags":["vision"],"metadata":{"team":"ml"},"completion_policy":"ALL"}`, http.StatusCreated, &ds)
	if ds.LifecycleStatus != "DRAFT" || ds.Status != "OPEN" || ds.Revision != 1 || r.header.Get("ETag") != `"1"` || ds.Metadata["team"] != "ml" {
		t.Fatalf("created: %+v %s", ds, r.header.Get("ETag"))
	}
	s.expect(t, tok, "", http.MethodPost, "/api/v1/datasets", `{"name":"web-set","description":"pets","tags":["vision"],"metadata":{"team":"ml"}}`, http.StatusOK, nil)
	base := "/api/v1/datasets/" + ds.ID

	// Members, in bulk, each judged on its own.
	var added response.AddMembersResponse
	s.expect(t, tok, "", http.MethodPost, base+"/members", fmt.Sprintf(`{"members":[
		{"object_id":%q,"label":"cat","split":"train"},{"object_id":%q,"label":"dog","split":"test"},{"object_id":"nope"}]}`,
		objs[0].ObjectID, objs[1].ObjectID), http.StatusOK, &added)
	if added.Added != 2 || added.Rejected != 1 || added.Results[2].Code != "object_not_found" || added.Dataset.MemberCount != 2 {
		t.Fatalf("members: %+v", added)
	}

	// An inline manifest import, run by the worker.
	manifest := fmt.Sprintf("{\"object_id\":%q,\"label\":\"cat\",\"split\":\"train\"}\n{\"object_id\":%q,\"label\":\"dog\",\"split\":\"validation\"}\n{\"object_id\":\"bad\"\n",
		objs[2].ObjectID, objs[3].ObjectID)
	ndjson := map[string]string{"Content-Type": "application/x-ndjson", "Idempotency-Key": "web-import-1"}
	if r := s.call(t, tok, "", http.MethodPost, base+"/manifests", manifest, map[string]string{"Content-Type": "application/x-ndjson"}); r.status != http.StatusBadRequest {
		t.Fatalf("no idempotency key: %s", r)
	}
	if r := s.call(t, tok, "", http.MethodPost, base+"/manifests", manifest, map[string]string{"Content-Type": "text/csv"}); r.status != http.StatusUnsupportedMediaType {
		t.Fatalf("csv: %s", r)
	}
	r = s.call(t, tok, "", http.MethodPost, base+"/manifests", manifest, ndjson)
	var imp response.ManifestImportResponse
	if r.status != http.StatusAccepted || json.Unmarshal(r.body, &imp) != nil || imp.State != "QUEUED" {
		t.Fatalf("import: %s", r)
	}
	drain(t, worker, "worker-web")
	s.expect(t, tok, "", http.MethodGet, base+"/manifests/"+imp.ID, "", http.StatusOK, &imp)
	if imp.State != "COMPLETED" || imp.Progress.MembersAdded != 2 || imp.Progress.LinesRejected != 1 {
		t.Fatalf("finished import: %+v", imp)
	}
	var importErrs response.ImportErrorPageResponse
	s.expect(t, auditorTok, "", http.MethodGet, base+"/manifests/"+imp.ID+"/errors", "", http.StatusOK, &importErrs)
	if len(importErrs.Errors) != 1 || importErrs.Errors[0].Line != 3 || importErrs.Errors[0].Code != "invalid_json" {
		t.Fatalf("import errors: %+v", importErrs)
	}
	// A staged import hands out an upload URL, and waits for the upload.
	var staged response.ManifestImportResponse
	s.expect(t, tok, "", http.MethodPost, base+"/manifests", `{"idempotency_key":"web-staged","size_bytes":120}`, http.StatusCreated, &staged)
	if staged.State != "AWAITING_UPLOAD" || staged.UploadURL == "" {
		t.Fatalf("staged: %+v", staged)
	}
	if r := s.call(t, tok, "", http.MethodPost, base+"/manifests/"+staged.ID+"/submit", ""); r.status != http.StatusConflict || !strings.Contains(string(r.body), "manifest_not_uploaded") {
		t.Fatalf("submit before upload: %s", r)
	}

	// Snapshot, idempotently.
	var v response.DatasetVersionResponse
	key := map[string]string{"Idempotency-Key": "web-snap-1"}
	if r := s.call(t, tok, "", http.MethodPost, base+"/snapshots", "", key); r.status != http.StatusCreated || json.Unmarshal(r.body, &v) != nil {
		t.Fatalf("snapshot: %s", r)
	}
	if r := s.call(t, tok, "", http.MethodPost, base+"/snapshots", "", key); r.status != http.StatusOK || !strings.Contains(string(r.body), v.ID) {
		t.Fatalf("snapshot replay: %s", r)
	}
	if v.Version != 1 || v.MemberCount != 4 || len(v.ManifestSHA256) != 64 {
		t.Fatalf("version: %+v", v)
	}
	var versions response.DatasetVersionPageResponse
	s.expect(t, auditorTok, "", http.MethodGet, base+"/versions", "", http.StatusOK, &versions)
	var vm response.VersionMemberPageResponse
	s.expect(t, tok, "", http.MethodGet, base+"/versions/"+v.ID+"/members?limit=3", "", http.StatusOK, &vm)
	if len(versions.Versions) != 1 || len(vm.Members) != 3 || vm.NextAfter != 3 {
		t.Fatalf("versions %+v, members %+v", versions, vm)
	}

	// Validation: accepted, run by the worker, reported, with pages of
	// findings.
	var job response.ValidationJobResponse
	r = s.expect(t, tok, "", http.MethodPost, base+"/validations", `{"profile_id":"builtin:image","mode":"METADATA"}`, http.StatusAccepted, &job)
	if r.header.Get("Location") != "/api/v1/validation-jobs/"+job.ID || job.State != "QUEUED" || job.Progress.Total != 4 {
		t.Fatalf("validation: %+v", job)
	}
	drain(t, worker, "worker-web")
	s.expect(t, auditorTok, "", http.MethodGet, "/api/v1/validation-jobs/"+job.ID, "", http.StatusOK, &job)
	if job.ReportID == "" || job.State != "FAILED" || job.Progress.Processed != 4 {
		t.Fatalf("finished validation: %+v", job)
	}
	var report response.ValidationReportResponse
	r = s.expect(t, tok, "", http.MethodGet, "/api/v1/validation-reports/"+job.ReportID, "", http.StatusOK, &report)
	if report.Status != "FAILED" || report.SchemaVersion != 1 || len(report.Report) == 0 || report.Summary == "" {
		t.Fatalf("report: %+v", report)
	}
	if body := string(r.body); strings.Contains(body, "published/") || strings.Contains(body, s.ie.bucket) || strings.Contains(body, "quarantine/") {
		t.Fatalf("storage keys in a report: %s", body)
	}
	var findings response.FindingPageResponse
	s.expect(t, tok, "", http.MethodGet, "/api/v1/validation-reports/"+job.ReportID+"/findings?severity=ERROR&limit=2", "", http.StatusOK, &findings)
	if len(findings.Findings) != 2 || findings.NextCursor == "" || findings.Findings[0].Code != "MEDIA_TYPE_NOT_ALLOWED" {
		t.Fatalf("findings: %+v", findings)
	}
	var more response.FindingPageResponse
	s.expect(t, tok, "", http.MethodGet, "/api/v1/validation-reports/"+job.ReportID+"/findings?severity=ERROR&limit=2&cursor="+findings.NextCursor, "", http.StatusOK, &more)
	if len(more.Findings) != 2 || more.Findings[0].ID == findings.Findings[0].ID {
		t.Fatalf("second page: %+v", more)
	}

	// A readiness assessment, and its report among the dataset's.
	s.expect(t, tok, "", http.MethodPost, base+"/readiness-assessments", `{"profile_id":"builtin:general"}`, http.StatusAccepted, &job)
	drain(t, worker, "worker-web")
	var readiness response.ValidationReportPageResponse
	s.expect(t, auditorTok, "", http.MethodGet, base+"/readiness-reports", "", http.StatusOK, &readiness)
	if len(readiness.Reports) != 1 || !readiness.Reports[0].Readiness || readiness.Reports[0].Report != nil {
		t.Fatalf("readiness reports: %+v", readiness)
	}
	var all response.ValidationJobPageResponse
	s.expect(t, tok, "", http.MethodGet, base+"/validations?limit=1", "", http.StatusOK, &all)
	if len(all.Jobs) != 1 || all.NextCursor == "" {
		t.Fatalf("validations page: %+v", all)
	}

	// Lineage of the dataset and of its version.
	var lineage response.LineageResponse
	s.expect(t, auditorTok, "", http.MethodGet, base+"/lineage?depth=2", "", http.StatusOK, &lineage)
	if lineage.Direction != "upstream" || len(lineage.Edges) < 3 {
		t.Fatalf("dataset lineage: %+v", lineage)
	}
	s.expect(t, tok, "", http.MethodGet, "/api/v1/dataset-versions/"+v.ID+"/lineage?direction=downstream", "", http.StatusOK, &lineage)
	if len(lineage.Edges) != 1 || lineage.Edges[0].Relation != "CONTAINS" || lineage.Edges[0].From.ID != ds.ID {
		t.Fatalf("version lineage: %+v", lineage)
	}

	// Updates need the revision: none is 428, a stale one 412.
	if r := s.call(t, tok, "", http.MethodPatch, base, `{"description":"cats and dogs"}`); r.status != http.StatusPreconditionRequired {
		t.Fatalf("no revision: %s", r)
	}
	if r := s.call(t, tok, "", http.MethodPatch, base, `{"description":"cats and dogs"}`, map[string]string{"If-Match": `"1"`}); r.status != http.StatusPreconditionFailed {
		t.Fatalf("stale revision: %s", r)
	}
	current := s.expect(t, tok, "", http.MethodGet, base, "", http.StatusOK, &ds)
	r = s.call(t, tok, "", http.MethodPatch, base, `{"description":"cats and dogs","metadata":{"team":null,"stage":"gold"}}`,
		map[string]string{"If-Match": current.header.Get("ETag")})
	var patched response.DatasetResponse
	if r.status != http.StatusOK || json.Unmarshal(r.body, &patched) != nil || patched.Description != "cats and dogs" ||
		patched.Metadata["stage"] != "gold" || patched.Metadata["team"] != "" || r.header.Get("ETag") == current.header.Get("ETag") {
		t.Fatalf("update: %s", r)
	}

	// Profiles: listed for readers, written by administrators.
	var profiles response.ValidationProfileListResponse
	s.expect(t, auditorTok, "", http.MethodGet, "/api/v1/validation-profiles", "", http.StatusOK, &profiles)
	if len(profiles.Profiles) != 6 || len(profiles.RuleTypes) < 20 {
		t.Fatalf("profiles: %d, rule types %d", len(profiles.Profiles), len(profiles.RuleTypes))
	}
	profile := `{"name":"web-profile","rules":[{"id":"label","version":1,"type":"required_label","severity":"error"}],"thresholds":{"max_errors":0}}`
	if r := s.call(t, tok, "", http.MethodPost, "/api/v1/validation-profiles", profile); r.status != http.StatusForbidden {
		t.Fatalf("developer writes a profile: %s", r)
	}
	var created response.ValidationProfileResponse
	s.expect(t, adminTok, "", http.MethodPost, "/api/v1/validation-profiles", profile, http.StatusCreated, &created)
	s.expect(t, tok, "", http.MethodGet, "/api/v1/validation-profiles/"+created.ID, "", http.StatusOK, &created)
	if created.Version != 1 || created.BuiltIn || !strings.Contains(string(created.Definition), `"enabled":true`) {
		t.Fatalf("profile: %+v %s", created, created.Definition)
	}

	// Roles: auditors read but never change.
	for _, c := range []struct{ method, path, body string }{
		{http.MethodPost, base + "/members", fmt.Sprintf(`{"members":[{"object_id":%q}]}`, objs[0].ObjectID)},
		{http.MethodPost, base + "/snapshots", ""},
		{http.MethodPost, base + "/validations", `{}`},
		{http.MethodPost, base + "/readiness-assessments", `{"profile_id":"builtin:general"}`},
		{http.MethodPost, base + "/archive", ""},
		{http.MethodPost, "/api/v1/datasets", `{"name":"auditor-set"}`},
	} {
		if r := s.call(t, auditorTok, "", c.method, c.path, c.body); r.status != http.StatusForbidden {
			t.Fatalf("auditor %s %s: %s", c.method, c.path, r)
		}
	}

	// Bad input is a 400, never a 500.
	for _, path := range []string{
		"/api/v1/datasets?limit=abc", "/api/v1/datasets?limit=100000", "/api/v1/datasets?cursor=garbage", "/api/v1/datasets?status=SHINY",
		base + "/members?cursor=garbage", base + "/versions?before=-1", base + "/lineage?depth=99", base + "/lineage?direction=sideways",
		"/api/v1/validation-reports/" + job.ReportID + "/findings?severity=FATAL", base + "/validations?cursor=garbage",
	} {
		if r := s.call(t, tok, "", http.MethodGet, path, ""); r.status != http.StatusBadRequest {
			t.Fatalf("%s: %s", path, r)
		}
	}
	for _, c := range []struct{ path, body string }{
		{base + "/validations", `{"mode":"WILD"}`},
		{base + "/validations", `{"mode":"METADATA","sample_size":5}`},
		{base + "/members", `{"members":[]}`},
		{base + "/members", `{"on_duplicate":"MERGE","members":[{"object_id":"x"}]}`},
		{base + "/members", `{not json`},
		{"/api/v1/datasets", `{"name":"Bad Name"}`},
	} {
		if r := s.call(t, tok, "", http.MethodPost, c.path, c.body); r.status != http.StatusBadRequest {
			t.Fatalf("POST %s %s: %s", c.path, c.body, r)
		}
	}

	// Archiving takes datasets.manage; an archived dataset refuses changes.
	if r := s.call(t, tok, "", http.MethodPost, base+"/archive", ""); r.status != http.StatusForbidden {
		t.Fatalf("developer archive: %s", r)
	}
	s.expect(t, adminTok, "", http.MethodPost, base+"/archive", "", http.StatusOK, &ds)
	if ds.LifecycleStatus != "ARCHIVED" {
		t.Fatalf("archived: %+v", ds)
	}
	if r := s.call(t, tok, "", http.MethodPost, base+"/members", fmt.Sprintf(`{"members":[{"object_id":%q}]}`, objs[0].ObjectID)); r.status != http.StatusConflict ||
		!strings.Contains(string(r.body), "dataset_archived") {
		t.Fatalf("members of an archived dataset: %s", r)
	}

	// Every change is in the organization's audit trail.
	for _, action := range []string{"dataset.create", "dataset.members_add", "dataset.manifest_import", "dataset.snapshot",
		"dataset.validate", "dataset.readiness", "dataset.update", "dataset.archive", "validation_profile.create"} {
		if successes(s.auditEvents(t, acme.org.ID, action)) == 0 {
			t.Errorf("no audit event for %s", action)
		}
	}
	denied := 0
	for _, e := range s.auditEvents(t, acme.org.ID, "dataset.archive") {
		if e.Outcome != "success" {
			denied++
		}
	}
	if denied == 0 {
		t.Fatal("refused archives are audited")
	}
}

func TestHTTPDatasetTenantIsolation(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	acme := s.team(t, "ds-iso-a")
	globex := s.team(t, "ds-iso-b")
	dev := acme.developer
	objs := s.publishObjects(t, dev, "iso", 2)
	d := s.newDataset(t, dev, "private")
	s.addMembers(t, dev, d.ID, memberOf(objs[0], "a", "train"), memberOf(objs[1], "b", "test"))
	imp, _, err := s.datasets.ImportInline(ctx, dev, d.ID, datasetservice.ImportInput{IdempotencyKey: "iso-1"}, strings.NewReader(fmt.Sprintf(`{"object_id":%q}`, objs[0].ObjectID)))
	if err != nil {
		t.Fatal(err)
	}
	v, _, err := s.datasets.Snapshot(ctx, dev, d.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	job, report, _ := s.validated(t, dev, d.ID, datasetservice.ValidationInput{}, false)
	members, _ := s.datasets.Members(ctx, dev, d.ID, "", 1)
	globexTok := s.signIn(t, globex.devUser.Email, globex.devPW).AccessToken

	routes := []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/datasets/%s", ""},
		{http.MethodPatch, "/api/v1/datasets/%s", `{"revision":1,"description":"stolen"}`},
		{http.MethodPost, "/api/v1/datasets/%s/archive", ""},
		{http.MethodGet, "/api/v1/datasets/%s/members", ""},
		{http.MethodPost, "/api/v1/datasets/%s/members", fmt.Sprintf(`{"members":[{"object_id":%q}]}`, objs[0].ObjectID)},
		{http.MethodDelete, "/api/v1/datasets/%s/members/" + members.Items[0].ID, ""},
		{http.MethodGet, "/api/v1/datasets/%s/manifests/" + imp.ID, ""},
		{http.MethodGet, "/api/v1/datasets/%s/manifests/" + imp.ID + "/errors", ""},
		{http.MethodPost, "/api/v1/datasets/%s/manifests/" + imp.ID + "/cancel", ""},
		{http.MethodPost, "/api/v1/datasets/%s/snapshots", ""},
		{http.MethodGet, "/api/v1/datasets/%s/versions", ""},
		{http.MethodGet, "/api/v1/datasets/%s/versions/" + v.ID, ""},
		{http.MethodGet, "/api/v1/datasets/%s/versions/" + v.ID + "/members", ""},
		{http.MethodPost, "/api/v1/datasets/%s/validations", `{}`},
		{http.MethodGet, "/api/v1/datasets/%s/validations", ""},
		{http.MethodPost, "/api/v1/datasets/%s/readiness-assessments", `{"profile_id":"builtin:general"}`},
		{http.MethodGet, "/api/v1/datasets/%s/readiness-reports", ""},
		{http.MethodGet, "/api/v1/datasets/%s/lineage", ""},
	}
	for _, rt := range routes {
		theirs := s.call(t, globexTok, "", rt.method, fmt.Sprintf(rt.path, d.ID), rt.body)
		missing := s.call(t, globexTok, "", rt.method, fmt.Sprintf(rt.path, "no-such-dataset"), rt.body)
		// Archiving needs a permission a developer lacks: refused before the
		// dataset is looked up, the same for any ID.
		want := http.StatusNotFound
		if strings.HasSuffix(rt.path, "/archive") {
			want = http.StatusForbidden
		}
		if theirs.status != want || string(theirs.body) != string(missing.body) {
			t.Fatalf("%s %s across tenants: %s, missing: %s", rt.method, rt.path, theirs, missing)
		}
	}
	for _, path := range []string{"/api/v1/validation-jobs/" + job.ID, "/api/v1/validation-reports/" + report.ID,
		"/api/v1/validation-reports/" + report.ID + "/findings", "/api/v1/dataset-versions/" + v.ID + "/lineage"} {
		if r := s.call(t, globexTok, "", http.MethodGet, path, ""); r.status != http.StatusNotFound {
			t.Fatalf("%s across tenants: %s", path, r)
		}
	}
	if r := s.call(t, globexTok, "", http.MethodPost, "/api/v1/validation-jobs/"+job.ID+"/cancel", ""); r.status != http.StatusNotFound {
		t.Fatalf("cancel across tenants: %s", r)
	}
	var page response.DatasetPageResponse
	s.expect(t, globexTok, "", http.MethodGet, "/api/v1/datasets", "", http.StatusOK, &page)
	if len(page.Datasets) != 0 {
		t.Fatalf("globex lists acme's datasets: %+v", page.Datasets)
	}
	// Nothing of acme's changed.
	if got, _ := s.datasets.Get(ctx, dev, d.ID); got.Description != "" || got.MemberCount != 2 || got.Lifecycle == "ARCHIVED" {
		t.Fatalf("acme's dataset after globex's attempts: %+v", got)
	}
}

func TestHTTPDatasetRateLimits(t *testing.T) {
	s := openSecurityStack(t, map[string]string{"RATE_LIMIT_JOB_CREATE": "3/1m"})
	acme := s.team(t, "ds-rate")
	tok := s.signIn(t, acme.devUser.Email, acme.devPW).AccessToken
	obj := s.publishAt(t, acme.developer, "rate/a.txt", textData("rate", 30))
	var ds response.DatasetResponse
	s.expect(t, tok, "", http.MethodPost, "/api/v1/datasets", `{"name":"rated"}`, http.StatusCreated, &ds)
	s.expect(t, tok, "", http.MethodPost, "/api/v1/datasets/"+ds.ID+"/members", fmt.Sprintf(`{"members":[{"object_id":%q}]}`, obj.ObjectID), http.StatusOK, nil)
	s.expect(t, tok, "", http.MethodPost, "/api/v1/datasets/"+ds.ID+"/snapshots", "", http.StatusCreated, nil)
	s.expect(t, tok, "", http.MethodPost, "/api/v1/datasets/"+ds.ID+"/snapshots", "", http.StatusCreated, nil)
	r := s.call(t, tok, "", http.MethodPost, "/api/v1/datasets/"+ds.ID+"/snapshots", "")
	if r.status != http.StatusTooManyRequests || r.header.Get("Retry-After") == "" {
		t.Fatalf("fourth job: %s", r)
	}
	// Listings count against their own limit.
	s.expect(t, tok, "", http.MethodGet, "/api/v1/datasets/"+ds.ID+"/versions", "", http.StatusOK, nil)
}

// TestDatasetFixtureWorkflow runs the documented sample (testdata/datasets)
// through the API, so the example in docs/datasets.md keeps working.
func TestDatasetFixtureWorkflow(t *testing.T) {
	s := openSecurityStack(t, nil)
	acme := s.team(t, "ds-fixture")
	tok := s.signIn(t, acme.devUser.Email, acme.devPW).AccessToken
	ownerTok := s.signIn(t, acme.ownerUser.Email, acme.ownerPW).AccessToken
	fixture := filepath.Join("..", "..", "testdata", "datasets")
	files, err := filepath.Glob(filepath.Join(fixture, "pets", "*.txt"))
	if err != nil || len(files) != 6 {
		t.Fatalf("fixture files: %v %v", files, err)
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		s.publishAt(t, acme.developer, "pets/"+filepath.Base(f), data)
	}
	manifest, err := os.ReadFile(filepath.Join(fixture, "manifest.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	profile, err := os.ReadFile(filepath.Join(fixture, "profile.json"))
	if err != nil {
		t.Fatal(err)
	}
	worker := s.newDatasetService(t, nil)

	var ds response.DatasetResponse
	s.expect(t, tok, "", http.MethodPost, "/api/v1/datasets", `{"name":"pets"}`, http.StatusCreated, &ds)
	base := "/api/v1/datasets/" + ds.ID
	var imp response.ManifestImportResponse
	r := s.call(t, tok, "", http.MethodPost, base+"/manifests", string(manifest),
		map[string]string{"Content-Type": "application/x-ndjson", "Idempotency-Key": "pets-1"})
	if r.status != http.StatusAccepted || json.Unmarshal(r.body, &imp) != nil {
		t.Fatalf("import: %s", r)
	}
	drain(t, worker, "worker-fixture")
	s.expect(t, tok, "", http.MethodGet, base+"/manifests/"+imp.ID, "", http.StatusOK, &imp)
	if imp.State != "COMPLETED" || imp.Progress.MembersAdded != 6 || imp.Progress.LinesRejected != 2 {
		t.Fatalf("fixture import: %+v", imp)
	}
	var prof response.ValidationProfileResponse
	s.expect(t, ownerTok, "", http.MethodPost, "/api/v1/validation-profiles", string(profile), http.StatusCreated, &prof)
	s.expect(t, tok, "", http.MethodPost, base+"/snapshots", "", http.StatusCreated, nil)
	var job response.ValidationJobResponse
	s.expect(t, tok, "", http.MethodPost, base+"/validations", fmt.Sprintf(`{"profile_id":%q,"mode":"FULL"}`, prof.ID), http.StatusAccepted, &job)
	drain(t, worker, "worker-fixture")
	s.expect(t, tok, "", http.MethodGet, "/api/v1/validation-jobs/"+job.ID, "", http.StatusOK, &job)
	var findings response.FindingPageResponse
	s.expect(t, tok, "", http.MethodGet, "/api/v1/validation-reports/"+job.ReportID+"/findings?limit=50", "", http.StatusOK, &findings)
	codes := map[string]int{}
	for _, f := range findings.Findings {
		codes[f.Code]++
	}
	// cat-1 (train) and its copy (test) are the same verified bytes: leakage
	// fails the version; the copy also has no licence.
	if job.State != "FAILED" || codes["SPLIT_LEAKAGE"] != 1 || codes["DUPLICATE_CONTENT"] != 1 ||
		codes["MISSING_GOVERNANCE_METADATA"] != 1 || len(codes) != 3 || job.Progress.BytesRead == 0 {
		t.Fatalf("fixture validation: %s %v %+v", job.State, codes, job.Progress)
	}
}
