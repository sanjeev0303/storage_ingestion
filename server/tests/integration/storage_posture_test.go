package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/dto/response"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// Posture checks, the multipart self-test, staged credentials, the
// placement guard for PHI, out-of-band changes and tombstones, against
// MinIO as the tenant's bucket (docs/storage-posture.md).
func TestStoragePostureAndHealthEndToEnd(t *testing.T) {
	s := openBYOSStack(t)
	ctx := context.Background()
	tm := s.team(t, "posture")
	org := tm.org.ID
	admin := s.signIn(t, tm.adminUser.Email, tm.adminPW).AccessToken
	owner := s.signIn(t, tm.ownerUser.Email, tm.ownerPW).AccessToken
	dev := s.signIn(t, tm.devUser.Email, tm.devPW).AccessToken
	bucket := os.Getenv("TEST_S3_COMPATIBLE_BUCKET")
	if bucket == "" {
		bucket = "object-storage-it"
	}
	accessKey, secretKey := os.Getenv("TEST_S3_COMPATIBLE_ACCESS_KEY"), os.Getenv("TEST_S3_COMPATIBLE_SECRET_KEY")
	run := strings.TrimPrefix(s.run, "r")
	connect := func(name, staging, ready string) response.StorageConnectionResponse {
		var c response.StorageConnectionResponse
		s.expect(t, admin, org, http.MethodPost, "/api/v1/storage-connections", fmt.Sprintf(`{"name":%q,"provider":"S3_COMPATIBLE",
			"auth_method":"ACCESS_KEY","bucket":%q,"endpoint":%q,"force_path_style":true,"staging_prefix":%q,"ready_prefix":%q,
			"credentials":{"access_key_id":%q,"secret_access_key":%q}}`, name, bucket, os.Getenv("TEST_S3_COMPATIBLE_ENDPOINT"),
			staging, ready, accessKey, secretKey), http.StatusCreated, &c)
		t.Cleanup(func() { s.removePrefix(t, org, c.ID, staging+"/", ready+"/") })
		return c
	}
	staging, ready := "pst"+run[len(run)-8:], "ppub"+run[len(run)-8:]
	conn := connect("MinIO posture", staging, ready)

	// The test that activates a connection writes through a presigned
	// multipart part too, and checks the bucket's posture.
	var tested response.StorageTestResponse
	s.expect(t, admin, org, http.MethodPost, "/api/v1/storage-connections/"+conn.ID+"/test", `{"write_probe":true}`, http.StatusOK, &tested)
	if !tested.OK || tested.Posture == nil {
		t.Fatalf("test with posture: %+v", tested)
	}
	statuses := map[string]string{}
	for _, c := range tested.Posture.Checks {
		if c.Status != model.PosturePass && c.Status != model.PostureWarn && c.Status != model.PostureFail && c.Status != model.PostureNotChecked {
			t.Fatalf("check %s: status %q", c.Name, c.Status)
		}
		statuses[c.Name] = c.Status
	}
	// MinIO over plain HTTP refuses customer-provided keys.
	if statuses[model.CheckSelfTest] != model.PosturePass || statuses[model.CheckSSECDenied] != model.PosturePass ||
		statuses[model.CheckGuessableName] != model.PosturePass || statuses[model.CheckLifecycleDrift] == "" {
		t.Fatalf("posture: %+v", tested.Posture.Checks)
	}
	var stored model.PostureReport
	other := s.team(t, "posture-other")
	stranger := s.signIn(t, other.adminUser.Email, other.adminPW).AccessToken
	s.expect(t, stranger, other.org.ID, http.MethodGet, "/api/v1/storage-connections/"+conn.ID+"/posture", "", http.StatusNotFound, nil)
	s.expect(t, dev, org, http.MethodPost, "/api/v1/storage-connections/"+conn.ID+"/posture", "", http.StatusForbidden, nil)
	s.expect(t, admin, org, http.MethodGet, "/api/v1/storage-connections/"+conn.ID+"/posture", "", http.StatusOK, &stored)
	if stored.CheckedAt.Sub(tested.Posture.CheckedAt).Abs() > time.Millisecond || stored.Worst != tested.Posture.Worst {
		t.Fatalf("stored posture: %+v", stored)
	}
	s.expect(t, admin, org, http.MethodPost, "/api/v1/storage-connections/"+conn.ID+"/posture", `{"probe":false}`, http.StatusOK, &stored)
	if l, _ := stored.Check(model.CheckLifecycleDrift); l.Status == model.PostureWarn {
		t.Fatalf("the lifecycle rules did not change: %+v", l)
	}

	// Explicit checksum mode; an owner can only be pinned on AWS and GCS.
	var updated response.StorageConnectionResponse
	s.expect(t, admin, org, http.MethodPatch, "/api/v1/storage-connections/"+conn.ID,
		fmt.Sprintf(`{"revision":%d,"checksum_mode":"when_required"}`, tested.Connection.Revision), http.StatusOK, &updated)
	if updated.ChecksumMode != model.ChecksumWhenRequired {
		t.Fatalf("checksum mode: %+v", updated)
	}
	s.expect(t, admin, org, http.MethodPatch, "/api/v1/storage-connections/"+conn.ID,
		fmt.Sprintf(`{"revision":%d,"expected_owner":"123456789012"}`, updated.Revision), http.StatusBadRequest, nil)

	// Two credentials: stage, promote with a pre-flight, roll back.
	creds := fmt.Sprintf(`{"credentials":{"access_key_id":%q,"secret_access_key":%q}}`, accessKey, secretKey)
	s.expect(t, admin, org, http.MethodPost, "/api/v1/storage-connections/"+conn.ID+"/credentials/promote", "", http.StatusConflict, nil)
	s.expect(t, admin, org, http.MethodPost, "/api/v1/storage-connections/"+conn.ID+"/credentials/stage",
		fmt.Sprintf(`{"credentials":{"access_key_id":%q,"secret_access_key":"wrong-secret-0000"}}`, accessKey), http.StatusUnprocessableEntity, nil)
	var staged response.StorageTestResponse
	s.expect(t, admin, org, http.MethodPost, "/api/v1/storage-connections/"+conn.ID+"/credentials/stage", creds, http.StatusOK, &staged)
	if staged.Connection == nil || staged.Connection.StagedCredential == nil || staged.Connection.CredentialRotation != 1 {
		t.Fatalf("staged: %+v", staged.Connection)
	}
	var promoted response.StorageTestResponse
	s.expect(t, admin, org, http.MethodPost, "/api/v1/storage-connections/"+conn.ID+"/credentials/promote", "", http.StatusOK, &promoted)
	if c := promoted.Connection; c == nil || c.CredentialRotation != 2 || c.StagedCredential != nil || c.PreviousCredentialUntil == nil {
		t.Fatalf("promoted: %+v", promoted.Connection)
	}
	var rolled response.StorageConnectionResponse
	s.expect(t, admin, org, http.MethodPost, "/api/v1/storage-connections/"+conn.ID+"/credentials/rollback", "", http.StatusOK, &rolled)
	if rolled.CredentialRotation != 1 || rolled.PreviousCredentialUntil != nil {
		t.Fatalf("rolled back: %+v", rolled)
	}
	s.expect(t, admin, org, http.MethodPost, "/api/v1/storage-connections/"+conn.ID+"/credentials/rollback", "", http.StatusConflict, nil)
	// The rolled-back credentials still work.
	s.expect(t, admin, org, http.MethodPost, "/api/v1/storage-connections/"+conn.ID+"/test", "", http.StatusOK, &tested)
	if !tested.OK {
		t.Fatalf("after rollback: %+v", tested)
	}
	s.expect(t, admin, org, http.MethodPost, "/api/v1/storage-connections/"+conn.ID+"/credentials/stage", creds, http.StatusOK, nil)
	s.expect(t, admin, org, http.MethodDelete, "/api/v1/storage-connections/"+conn.ID+"/credentials/staged", "", http.StatusOK, &rolled)
	if rolled.StagedCredential != nil {
		t.Fatalf("discarded: %+v", rolled)
	}

	// An upload, published on the connection, carries its SHA-256.
	s.expect(t, admin, org, http.MethodPost, "/api/v1/storage-connections/"+conn.ID+"/set-default", "", http.StatusOK, nil)
	data := []byte("posture and drift, end to end\n")
	req := uploadOf("drift-file", data)
	req.Path = "drift/file.txt"
	var upload response.CreateUploadResponse
	body, _ := json.Marshal(req)
	s.expect(t, dev, org, http.MethodPost, "/api/v1/uploads", string(body), http.StatusCreated, &upload)
	put(t, upload.UploadURL, map[string]string{"Content-Type": "text/plain"}, data)
	s.expect(t, dev, org, http.MethodPost, "/api/v1/uploads/"+upload.UploadID+"/complete", "", http.StatusOK, nil)
	if _, err := s.ie.proc.Intake(ctx, org, upload.UploadID); err != nil {
		t.Fatal(err)
	}
	asset, err := s.ie.assets.GetByUpload(ctx, org, upload.UploadID)
	if err != nil || asset.Status != model.AssetStatusAvailable {
		t.Fatalf("published: %+v %v", asset, err)
	}
	sum := sha256.Sum256(data)
	content := s.expect(t, dev, org, http.MethodGet, "/api/v1/assets/"+asset.ID+"/content", "", http.StatusOK, nil)
	if got := content.header.Get("Repr-Digest"); got != "sha-256=:"+base64.StdEncoding.EncodeToString(sum[:])+":" {
		t.Fatalf("Repr-Digest: %q", got)
	}

	// PHI: refused on a connection the posture checks do not find safe.
	var ds response.DatasetResponse
	s.expect(t, admin, org, http.MethodPost, "/api/v1/datasets", `{"name":"phi-posture"}`, http.StatusCreated, &ds)
	want := http.StatusConflict
	if stored.PHISafe {
		want = http.StatusOK
	}
	s.expect(t, dev, org, http.MethodPatch, "/api/v1/datasets/"+ds.ID, fmt.Sprintf(`{"revision":%d,"data_class":"PHI"}`, ds.Revision),
		http.StatusForbidden, nil)
	res := s.expect(t, admin, org, http.MethodPatch, "/api/v1/datasets/"+ds.ID, fmt.Sprintf(`{"revision":%d,"data_class":"phi"}`, ds.Revision), want, nil)
	if want == http.StatusConflict && !strings.Contains(string(res.body), "placement_refused") {
		t.Fatalf("PHI refused: %s", res)
	}
	if want == http.StatusConflict {
		// Marked on the platform's own storage it is allowed; an upload into
		// it bound for the unsafe connection is not.
		s.expect(t, admin, org, http.MethodPatch, "/api/v1/datasets/"+ds.ID, fmt.Sprintf(`{"revision":%d,"data_class":"PHI","storage_connection_id":"platform"}`,
			ds.Revision), http.StatusOK, &ds)
		phi := uploadOf("phi-file", data)
		phi.Dataset, phi.StorageConnectionID = "phi-posture", conn.ID
		body, _ := json.Marshal(phi)
		res := s.expect(t, admin, org, http.MethodPost, "/api/v1/uploads", string(body), http.StatusConflict, nil)
		if !strings.Contains(string(res.body), "placement_refused") {
			t.Fatalf("PHI upload: %s", res)
		}
	}

	// A snapshot pins the published version.
	var set response.DatasetResponse
	s.expect(t, admin, org, http.MethodPost, "/api/v1/datasets", `{"name":"drift-set"}`, http.StatusCreated, &set)
	s.expect(t, admin, org, http.MethodPost, "/api/v1/datasets/"+set.ID+"/members", fmt.Sprintf(`{"members":[{"object_id":%q}]}`, asset.ObjectID),
		http.StatusOK, nil)
	var version struct {
		ID string `json:"id"`
	}
	s.expect(t, admin, org, http.MethodPost, "/api/v1/datasets/"+set.ID+"/snapshots", `{"idempotency_key":"drift-1"}`, http.StatusCreated, &version)
	var integrity struct {
		Degraded bool `json:"degraded"`
		Objects  []struct {
			Path, Kind string
		} `json:"objects"`
	}
	s.expect(t, dev, org, http.MethodGet, "/api/v1/datasets/"+set.ID+"/versions/"+version.ID+"/integrity", "", http.StatusOK, &integrity)
	if integrity.Degraded {
		t.Fatalf("degraded before anything changed: %+v", integrity)
	}

	// Someone with the bucket's keys replaces the object and drops another
	// next to it. The drift scan finds both.
	loc, err := s.registry.Locate(ctx, org, conn.ID)
	if err != nil {
		t.Fatal(err)
	}
	evil := []byte("replaced outside the platform, and longer\n")
	if _, err := loc.Storage.WriteObject(ctx, loc.ReadyBucket, asset.ObjectKey, bytes.NewReader(evil), int64(len(evil)), "text/plain"); err != nil {
		t.Fatal(err)
	}
	stray := ready + "/" + org + "/dropped-by-hand.bin"
	if _, err := loc.Storage.WriteObject(ctx, loc.ReadyBucket, stray, bytes.NewReader(evil), int64(len(evil)), "application/octet-stream"); err != nil {
		t.Fatal(err)
	}
	stats, err := s.health.ScanDrift(ctx, time.Nanosecond, 100, 1000)
	if err != nil || stats.Found < 2 {
		t.Fatalf("drift scan: %+v %v", stats, err)
	}
	var drift struct {
		Findings []model.DriftFinding `json:"findings"`
	}
	s.expect(t, admin, org, http.MethodGet, "/api/v1/storage-connections/"+conn.ID+"/drift", "", http.StatusOK, &drift)
	kinds := map[string]model.DriftFinding{}
	for _, f := range drift.Findings {
		kinds[f.Kind] = f
	}
	if kinds[model.DriftOverwritten].AssetID != asset.ID || kinds[model.DriftUnknown].ObjectKey != stray {
		t.Fatalf("findings: %+v", drift.Findings)
	}
	s.expect(t, dev, org, http.MethodGet, "/api/v1/datasets/"+set.ID+"/versions/"+version.ID+"/integrity", "", http.StatusOK, &integrity)
	if !integrity.Degraded || len(integrity.Objects) != 1 || integrity.Objects[0].Path != "drift/file.txt" {
		t.Fatalf("degraded snapshot: %+v", integrity)
	}
	var alerts struct {
		Alerts []model.SecurityAlert `json:"alerts"`
	}
	s.expect(t, owner, org, http.MethodGet, "/api/v1/security/alerts", "", http.StatusOK, &alerts)
	found := false
	for _, a := range alerts.Alerts {
		found = found || a.Kind == model.AlertOutOfBand
	}
	if !found {
		t.Fatalf("no out-of-band alert: %+v", alerts.Alerts)
	}
	// A second scan does not repeat open findings; resolving closes one.
	if again, _ := s.health.ScanDrift(ctx, time.Nanosecond, 100, 1000); again.Found != 0 {
		t.Fatalf("repeated findings: %+v", again)
	}
	s.expect(t, admin, org, http.MethodPost, "/api/v1/storage-drift/"+kinds[model.DriftUnknown].ID+"/resolve", "", http.StatusOK, nil)
	s.expect(t, admin, org, http.MethodPost, "/api/v1/storage-drift/"+kinds[model.DriftUnknown].ID+"/resolve", "", http.StatusNotFound, nil)

	// Removing an unused connection leaves a tombstone in its prefix.
	ready2 := "ptomb" + run[len(run)-8:]
	spare := connect("MinIO spare", "ptst"+run[len(run)-8:], ready2)
	s.expect(t, admin, org, http.MethodPost, "/api/v1/storage-connections/"+spare.ID+"/test", `{"write_probe":true}`, http.StatusOK, nil)
	s.expect(t, admin, org, http.MethodPost, "/api/v1/storage-connections/"+spare.ID+"/disable", "", http.StatusOK, nil)
	s.expect(t, admin, org, http.MethodDelete, "/api/v1/storage-connections/"+spare.ID, "", http.StatusNoContent, nil)
	tomb := ready2 + "/" + org + "/.osp-tombstone.json"
	if _, err := loc.Storage.HeadObject(ctx, loc.ReadyBucket, tomb); err != nil {
		t.Fatalf("tombstone: %v", err)
	}
	_ = loc.Storage.DeleteObject(ctx, loc.ReadyBucket, tomb)
}
