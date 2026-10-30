package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/sanjeev0303/object-storage-platform/internal/dto/response"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// The copy engine end to end over HTTP, with two MinIO prefixes as two
// connections: replicas, escrow, read failover, scrub and repair, drills,
// restores, clean points, migration and the export bundle
// (docs/copy-engine.md).
func TestCopyEngineEndToEnd(t *testing.T) {
	s := openBYOSStack(t)
	ctx := context.Background()
	tm := s.team(t, "copies")
	org := tm.org.ID
	admin := s.signIn(t, tm.adminUser.Email, tm.adminPW).AccessToken
	owner := s.signIn(t, tm.ownerUser.Email, tm.ownerPW).AccessToken
	dev := s.signIn(t, tm.devUser.Email, tm.devPW).AccessToken
	bucket := os.Getenv("TEST_S3_COMPATIBLE_BUCKET")
	if bucket == "" {
		bucket = "object-storage-it"
	}
	run := strings.TrimPrefix(s.run, "r")
	run = run[len(run)-8:]
	connect := func(name, tag string) response.StorageConnectionResponse {
		staging, ready := "cst"+tag+run, "cpub"+tag+run
		var c response.StorageConnectionResponse
		s.expect(t, admin, org, http.MethodPost, "/api/v1/storage-connections", fmt.Sprintf(`{"name":%q,"provider":"S3_COMPATIBLE",
			"auth_method":"ACCESS_KEY","bucket":%q,"endpoint":%q,"force_path_style":true,"staging_prefix":%q,"ready_prefix":%q,
			"credentials":{"access_key_id":%q,"secret_access_key":%q}}`, name, bucket, os.Getenv("TEST_S3_COMPATIBLE_ENDPOINT"), staging, ready,
			os.Getenv("TEST_S3_COMPATIBLE_ACCESS_KEY"), os.Getenv("TEST_S3_COMPATIBLE_SECRET_KEY")), http.StatusCreated, &c)
		t.Cleanup(func() { s.removePrefix(t, org, c.ID, staging+"/", ready+"/") })
		s.expect(t, admin, org, http.MethodPost, "/api/v1/storage-connections/"+c.ID+"/test", `{"write_probe":true}`, http.StatusOK, nil)
		return c
	}
	primary, second := connect("Primary", "a"), connect("Second provider", "b")
	s.expect(t, admin, org, http.MethodPost, "/api/v1/storage-connections/"+primary.ID+"/set-default", "", http.StatusOK, nil)

	publish := func(path, text string) model.Asset {
		data := []byte(text)
		req := uploadOf(strings.ReplaceAll(path, "/", "-"), data)
		req.Path, req.IdempotencyKey = path, "cp-"+req.ChecksumSHA256[:16]
		var upload response.CreateUploadResponse
		body, _ := json.Marshal(req)
		s.expect(t, dev, org, http.MethodPost, "/api/v1/uploads", string(body), http.StatusCreated, &upload)
		put(t, upload.UploadURL, map[string]string{"Content-Type": "text/plain"}, data)
		s.expect(t, dev, org, http.MethodPost, "/api/v1/uploads/"+upload.UploadID+"/complete", "", http.StatusOK, nil)
		if _, err := s.ie.proc.Intake(ctx, org, upload.UploadID); err != nil {
			t.Fatal(err)
		}
		a, err := s.ie.assets.GetByUpload(ctx, org, upload.UploadID)
		if err != nil || a.Status != model.AssetStatusAvailable {
			t.Fatalf("publish %s: %+v %v", path, a, err)
		}
		return *a
	}
	runJobs := func() {
		for i := 0; i < 50; i++ {
			found, err := s.copies.RunOnce(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if !found {
				return
			}
		}
		t.Fatal("copy jobs never finished")
	}
	job := func(id string) model.CopyJob {
		var j model.CopyJob
		s.expect(t, admin, org, http.MethodGet, "/api/v1/copy-jobs/"+id, "", http.StatusOK, &j)
		return j
	}
	content := func(assetID string) string {
		res := s.expect(t, dev, org, http.MethodGet, "/api/v1/assets/"+assetID+"/content", "", http.StatusOK, nil)
		return string(res.body)
	}

	a1, a2, a3 := publish("scans/one.txt", "first scan\n"), publish("scans/two.txt", "second scan\n"), publish("scans/three.txt", "third scan\n")
	var ds response.DatasetResponse
	s.expect(t, admin, org, http.MethodPost, "/api/v1/datasets", `{"name":"protected"}`, http.StatusCreated, &ds)
	s.expect(t, admin, org, http.MethodPost, "/api/v1/datasets/"+ds.ID+"/members", fmt.Sprintf(`{"members":[{"object_id":%q},{"object_id":%q},{"object_id":%q}]}`,
		a1.ObjectID, a2.ObjectID, a3.ObjectID), http.StatusOK, nil)
	var v1 struct {
		ID string `json:"id"`
	}
	s.expect(t, admin, org, http.MethodPost, "/api/v1/datasets/"+ds.ID+"/snapshots", `{"idempotency_key":"cp-1"}`, http.StatusCreated, &v1)
	s.expect(t, admin, org, http.MethodGet, "/api/v1/datasets/"+ds.ID, "", http.StatusOK, &ds)

	// Protection: a replica at the second connection, escrow, scrubs. A
	// locked vault needs a bucket with Object Lock, which this one lacks.
	s.expect(t, admin, org, http.MethodPatch, "/api/v1/datasets/"+ds.ID, fmt.Sprintf(`{"revision":%d,"replica_connection_id":%q,"escrow":true,"scrub_days":1}`,
		ds.Revision, second.ID), http.StatusOK, &ds)
	res := s.expect(t, admin, org, http.MethodPatch, "/api/v1/datasets/"+ds.ID, fmt.Sprintf(`{"revision":%d,"vault_retention_days":30}`, ds.Revision),
		http.StatusConflict, nil)
	if !strings.Contains(string(res.body), "vault_requirements") {
		t.Fatalf("vault: %s", res)
	}

	// Escrow: no deletion before a verified copy exists.
	if _, err := s.lifecycle.RequestDeletion(ctx, tm.admin, a3.ID); !errors.Is(err, model.ErrEscrowRequired) {
		t.Fatalf("deletion before escrow: %v", err)
	}
	// The passes are organization-wide: other runs' leftovers may be in
	// them too, so this organization's own results are checked below.
	stats, err := s.copies.Replicate(ctx)
	if err != nil || stats.Copied < 3 {
		t.Fatalf("replicate: %+v %v", stats, err)
	}
	var rep struct {
		Replicated, Pending  int64
		RecoveryPointSeconds int64 `json:"recovery_point_seconds"`
		Objects              int64
	}
	s.expect(t, dev, org, http.MethodGet, "/api/v1/datasets/"+ds.ID+"/replication", "", http.StatusOK, &rep)
	if rep.Objects != 3 || rep.Replicated != 3 || rep.Pending != 0 || rep.RecoveryPointSeconds != 0 {
		t.Fatalf("replication: %+v", rep)
	}
	if _, err := s.lifecycle.RequestDeletion(ctx, tm.admin, a3.ID); err != nil {
		t.Fatalf("deletion after escrow: %v", err)
	}
	if err := s.copies.RunSchedules(ctx); err != nil {
		t.Fatal(err)
	}
	var alerts struct {
		Alerts []model.SecurityAlert `json:"alerts"`
	}
	s.expect(t, owner, org, http.MethodGet, "/api/v1/security/alerts", "", http.StatusOK, &alerts)
	kinds := map[string]bool{}
	for _, a := range alerts.Alerts {
		kinds[a.Kind] = true
	}
	if !kinds[model.AlertMassDeletion] {
		t.Fatalf("no mass deletion alert: %+v", alerts.Alerts)
	}

	// The primary loses an object: reads fail over to the verified replica,
	// and the next scrub writes it back.
	primaryLoc, err := s.registry.Locate(ctx, org, primary.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := primaryLoc.Storage.DeleteObject(ctx, primaryLoc.ReadyBucket, a1.ObjectKey); err != nil {
		t.Fatal(err)
	}
	if got := content(a1.ID); got != "first scan\n" {
		t.Fatalf("failover read: %q", got)
	}
	scrub, err := s.copies.Scrub(ctx)
	if err != nil || scrub.Damaged < 1 || scrub.Repaired < 1 {
		t.Fatalf("scrub: %+v %v", scrub, err)
	}
	if body, err := primaryLoc.Storage.OpenRange(ctx, primaryLoc.ReadyBucket, a1.ObjectKey, 0, -1, interfaces.ReadCondition{}); err == nil {
		got, _ := io.ReadAll(body)
		body.Close()
		if string(got) != "first scan\n" {
			t.Fatalf("repaired object: %q", got)
		}
	} else {
		t.Fatalf("the object was not written back: %v", err)
	}
	var scrubs struct {
		Scrubs []model.ScrubResult `json:"scrubs"`
	}
	s.expect(t, dev, org, http.MethodGet, "/api/v1/datasets/"+ds.ID+"/scrubs", "", http.StatusOK, &scrubs)
	if len(scrubs.Scrubs) != 1 || !scrubs.Scrubs[0].Repaired || scrubs.Scrubs[0].Path != "scans/one.txt" {
		t.Fatalf("scrub results: %+v", scrubs.Scrubs)
	}

	// A drill from the replica: three.txt was deleted, so two remain.
	var drill model.CopyJob
	s.expect(t, admin, org, http.MethodPost, "/api/v1/datasets/"+ds.ID+"/drills", `{"sample":10,"from_replica":true}`, http.StatusAccepted, &drill)
	runJobs()
	drill = job(drill.ID)
	var drillReport map[string]any
	_ = json.Unmarshal(drill.Report, &drillReport)
	if drill.State != model.CopyCompleted || drillReport["intact"] != float64(2) {
		t.Fatalf("drill: %s %s", drill.State, drill.Report)
	}

	// Restore the snapshot to the second connection.
	var restore model.CopyJob
	s.expect(t, admin, org, http.MethodPost, "/api/v1/datasets/"+ds.ID+"/restores",
		fmt.Sprintf(`{"version_id":%q,"target_connection_id":%q,"prefix":"clean"}`, v1.ID, second.ID), http.StatusAccepted, &restore)
	runJobs()
	if restore = job(restore.ID); restore.State != model.CopyCompleted || restore.DoneItems != 2 {
		t.Fatalf("restore: %+v", restore)
	}
	secondLoc, err := s.registry.Locate(ctx, org, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"scans/two.txt", "manifest-sha256.txt"} {
		if _, err := secondLoc.Storage.HeadObject(ctx, secondLoc.ReadyBucket, secondLoc.ReadyPrefix+"/"+org+"/.restores/clean/"+key); err != nil {
			t.Fatalf("restored %s: %v", key, err)
		}
	}

	// A clean point, and what changed since, with who uploaded it.
	s.expect(t, admin, org, http.MethodPut, "/api/v1/datasets/"+ds.ID+"/clean-point", fmt.Sprintf(`{"version_id":%q}`, v1.ID), http.StatusNoContent, nil)
	publish("scans/two.txt", "second scan, tampered\n")
	added := publish("scans/four.txt", "a new file\n")
	s.expect(t, admin, org, http.MethodPost, "/api/v1/datasets/"+ds.ID+"/members", fmt.Sprintf(`{"members":[{"object_id":%q}]}`, added.ObjectID),
		http.StatusOK, nil)
	var diff struct {
		Changes []struct {
			Path, Change string
			UploadedBy   string `json:"uploaded_by"`
		} `json:"changes"`
	}
	s.expect(t, dev, org, http.MethodGet, "/api/v1/datasets/"+ds.ID+"/clean-point/diff", "", http.StatusOK, &diff)
	changes := map[string]string{}
	for _, c := range diff.Changes {
		changes[c.Path] = c.Change
		if c.Change != "REMOVED" && c.UploadedBy != tm.devUser.ID {
			t.Fatalf("uploader: %+v", c)
		}
	}
	if changes["scans/two.txt"] != "CHANGED" || changes["scans/four.txt"] != "ADDED" || changes["scans/three.txt"] != "REMOVED" {
		t.Fatalf("clean diff: %+v", diff.Changes)
	}

	// Migration to the second connection: a dry run first, then the real
	// one, which switches only after every copy is verified.
	var dry struct {
		Plan struct {
			Objects int64   `json:"objects"`
			Bytes   int64   `json:"bytes"`
			Egress  float64 `json:"egress_estimate_usd"`
		} `json:"plan"`
	}
	s.expect(t, admin, org, http.MethodPost, "/api/v1/datasets/"+ds.ID+"/migrations", fmt.Sprintf(`{"target_connection_id":%q,"dry_run":true}`, second.ID),
		http.StatusOK, &dry)
	if dry.Plan.Objects < 3 || dry.Plan.Bytes == 0 {
		t.Fatalf("dry run: %+v", dry)
	}
	s.expect(t, dev, org, http.MethodPost, "/api/v1/datasets/"+ds.ID+"/migrations", fmt.Sprintf(`{"target_connection_id":%q}`, second.ID),
		http.StatusForbidden, nil)
	var migration struct {
		Job model.CopyJob `json:"job"`
	}
	s.expect(t, admin, org, http.MethodPost, "/api/v1/datasets/"+ds.ID+"/migrations", fmt.Sprintf(`{"target_connection_id":%q}`, second.ID),
		http.StatusAccepted, &migration)
	runJobs()
	moved := job(migration.Job.ID)
	var movedReport map[string]any
	_ = json.Unmarshal(moved.Report, &movedReport)
	if moved.State != model.CopyCompleted || movedReport["switched"] == float64(0) {
		t.Fatalf("migration: %s %s: %s", moved.State, moved.Error, moved.Report)
	}
	after, err := s.ie.assets.GetByID(ctx, org, a2.ID)
	if err != nil || after.StorageConnectionID != second.ID {
		t.Fatalf("switched asset: %+v %v", after, err)
	}
	if got := content(a2.ID); got != "second scan\n" {
		t.Fatalf("read after migration: %q", got)
	}
	s.expect(t, admin, org, http.MethodGet, "/api/v1/datasets/"+ds.ID, "", http.StatusOK, &ds)
	if ds.StorageConnectionID != second.ID {
		t.Fatalf("dataset connection: %s", ds.StorageConnectionID)
	}

	// The export-everything bundle.
	var export model.CopyJob
	s.expect(t, dev, org, http.MethodPost, "/api/v1/exports", fmt.Sprintf(`{"target_connection_id":%q}`, second.ID), http.StatusForbidden, nil)
	s.expect(t, owner, org, http.MethodPost, "/api/v1/exports", fmt.Sprintf(`{"target_connection_id":%q}`, second.ID), http.StatusAccepted, &export)
	runJobs()
	if export = job(export.ID); export.State != model.CopyCompleted {
		t.Fatalf("export: %s %s", export.State, export.Error)
	}
	page, err := secondLoc.Storage.ListObjectsPage(ctx, secondLoc.ReadyBucket, secondLoc.ReadyPrefix+"/"+org+"/.exports/", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, o := range page.Objects {
		names[o.ObjectKey[strings.LastIndex(o.ObjectKey, "/")+1:]] = true
	}
	for _, want := range []string{"SHA256SUMS", "audit.jsonl", "version-members.jsonl", "catalog.jsonl", "README.md"} {
		if !names[want] {
			t.Fatalf("bundle lacks %s: %v", want, names)
		}
	}
	var jobs struct {
		Jobs []model.CopyJob `json:"jobs"`
	}
	s.expect(t, dev, org, http.MethodGet, "/api/v1/copy-jobs?dataset_id="+ds.ID, "", http.StatusOK, &jobs)
	if len(jobs.Jobs) != 3 {
		t.Fatalf("jobs of the dataset: %d", len(jobs.Jobs))
	}
}
