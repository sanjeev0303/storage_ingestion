package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/sanjeev0303/object-storage-platform/internal/dto/response"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	datasetservice "github.com/sanjeev0303/object-storage-platform/internal/service/dataset"
)

// Residency pinning and customer-managed keys over HTTP, with MinIO as the
// tenant's bucket (docs/residency.md).
func TestResidencyAndCustomerKeysEndToEnd(t *testing.T) {
	s := openBYOSStack(t)
	ctx := context.Background()
	tm := s.team(t, "resid")
	org := tm.org.ID
	admin := s.signIn(t, tm.adminUser.Email, tm.adminPW).AccessToken
	bucket := os.Getenv("TEST_S3_COMPATIBLE_BUCKET")
	if bucket == "" {
		bucket = "object-storage-it"
	}
	run := strings.TrimPrefix(s.run, "r")
	staging, ready := "rst"+run[len(run)-8:], "rpub"+run[len(run)-8:]
	var conn response.StorageConnectionResponse
	s.expect(t, admin, org, http.MethodPost, "/api/v1/storage-connections", fmt.Sprintf(`{"name":"MinIO residency","provider":"S3_COMPATIBLE",
		"auth_method":"ACCESS_KEY","bucket":%q,"endpoint":%q,"force_path_style":true,"staging_prefix":%q,"ready_prefix":%q,
		"credentials":{"access_key_id":%q,"secret_access_key":%q}}`, bucket, os.Getenv("TEST_S3_COMPATIBLE_ENDPOINT"), staging, ready,
		os.Getenv("TEST_S3_COMPATIBLE_ACCESS_KEY"), os.Getenv("TEST_S3_COMPATIBLE_SECRET_KEY")), http.StatusCreated, &conn)
	t.Cleanup(func() { s.removePrefix(t, org, conn.ID, staging+"/", ready+"/") })
	var tested response.StorageTestResponse
	s.expect(t, admin, org, http.MethodPost, "/api/v1/storage-connections/"+conn.ID+"/test", `{"write_probe":true}`, http.StatusOK, &tested)
	if !tested.OK || tested.Posture == nil {
		t.Fatalf("test: %+v", tested)
	}
	region, _ := tested.Posture.Check(model.CheckRegion)
	key, _ := tested.Posture.Check(model.CheckCustomerKey)
	if region.Status == "" || key.Status != model.PostureNotChecked {
		t.Fatalf("region %+v, key %+v", region, key)
	}

	var ds response.DatasetResponse
	s.expect(t, admin, org, http.MethodPost, "/api/v1/datasets", `{"name":"resid-scans"}`, http.StatusCreated, &ds)
	s.expect(t, admin, org, http.MethodPatch, "/api/v1/datasets/"+ds.ID, fmt.Sprintf(`{"revision":%d,"storage_connection_id":%q}`, ds.Revision, conn.ID),
		http.StatusOK, &ds)
	s.expect(t, admin, org, http.MethodPatch, "/api/v1/datasets/"+ds.ID, fmt.Sprintf(`{"revision":%d,"residency":["eu west"]}`, ds.Revision),
		http.StatusBadRequest, nil)

	// A location the provider never confirmed cannot show residency.
	res := s.expect(t, admin, org, http.MethodPatch, "/api/v1/datasets/"+ds.ID, fmt.Sprintf(`{"revision":%d,"residency":["ap-south-1"]}`, ds.Revision),
		http.StatusConflict, nil)
	if !strings.Contains(string(res.body), "placement_refused") {
		t.Fatalf("residency on an unconfirmed or foreign location: %s", res)
	}

	// The provider confirms the bucket is in Mumbai.
	report, _, err := s.connections.Posture(ctx, org, conn.ID)
	if err != nil {
		t.Fatal(err)
	}
	report.Location, report.LocationVerified = "ap-south-1", true
	if err := s.connections.PutPosture(ctx, org, report); err != nil {
		t.Fatal(err)
	}
	s.expect(t, admin, org, http.MethodPatch, "/api/v1/datasets/"+ds.ID, fmt.Sprintf(`{"revision":%d,"residency":["us-*"]}`, ds.Revision),
		http.StatusConflict, nil)
	s.expect(t, admin, org, http.MethodPatch, "/api/v1/datasets/"+ds.ID, fmt.Sprintf(`{"revision":%d,"residency":["AP-South-*"]}`, ds.Revision),
		http.StatusOK, &ds)
	if len(ds.Residency) != 1 || ds.Residency[0] != "ap-south-*" {
		t.Fatalf("residency: %+v", ds.Residency)
	}
	// No customer-managed key on the connection: a dataset that needs one
	// is refused.
	s.expect(t, admin, org, http.MethodPatch, "/api/v1/datasets/"+ds.ID, fmt.Sprintf(`{"revision":%d,"require_customer_key":true}`, ds.Revision),
		http.StatusConflict, nil)

	// A malformed key is refused outright; one MinIO cannot use fails the
	// write test and is not stored.
	s.expect(t, admin, org, http.MethodPatch, "/api/v1/storage-connections/"+conn.ID,
		fmt.Sprintf(`{"revision":%d,"encryption_key":"projects/not/a/s3/key"}`, tested.Connection.Revision), http.StatusBadRequest, nil)
	keyed := s.call(t, admin, org, http.MethodPatch, "/api/v1/storage-connections/"+conn.ID,
		fmt.Sprintf(`{"revision":%d,"encryption_key":"alias/aws/s3"}`, tested.Connection.Revision))
	switch keyed.status {
	case http.StatusUnprocessableEntity:
		var c response.StorageConnectionResponse
		s.expect(t, admin, org, http.MethodGet, "/api/v1/storage-connections/"+conn.ID, "", http.StatusOK, &c)
		if c.EncryptionKey != "" {
			t.Fatalf("a key that failed its test was stored: %+v", c)
		}
	case http.StatusOK:
		t.Log("this MinIO has a KMS: the key passed its write test")
	default:
		t.Fatalf("encryption key: %s", keyed)
	}

	// An upload into the dataset goes to its connection, inside its rules.
	data := []byte("resident in Mumbai\n")
	req := uploadOf("resident", data)
	req.Dataset, req.Path = "resid-scans", "scans/resident.txt"
	var upload response.CreateUploadResponse
	body, _ := json.Marshal(req)
	s.expect(t, admin, org, http.MethodPost, "/api/v1/uploads", string(body), http.StatusCreated, &upload)
	if upload.StorageConnectionID != conn.ID {
		t.Fatalf("bound to %s", upload.StorageConnectionID)
	}
	put(t, upload.UploadURL, map[string]string{"Content-Type": "text/plain"}, data)
	s.expect(t, admin, org, http.MethodPost, "/api/v1/uploads/"+upload.UploadID+"/complete", "", http.StatusOK, nil)
	if _, err := s.ie.proc.Intake(ctx, org, upload.UploadID); err != nil {
		t.Fatal(err)
	}
	asset, err := s.ie.assets.GetByUpload(ctx, org, upload.UploadID)
	if err != nil || asset.Status != model.AssetStatusAvailable {
		t.Fatalf("published: %+v %v", asset, err)
	}
	checks := s.auditEvents(t, org, "placement.check")
	denied := 0
	for _, e := range checks {
		if e.Outcome == model.AuditDenied {
			denied++
		}
	}
	if len(checks) < 4 || denied < 3 {
		t.Fatalf("placement checks in the audit log: %d, %d denied", len(checks), denied)
	}

	// A snapshot shows where its bytes are.
	s.expect(t, admin, org, http.MethodPost, "/api/v1/datasets/"+ds.ID+"/members", fmt.Sprintf(`{"members":[{"object_id":%q}]}`, asset.ObjectID),
		http.StatusOK, nil)
	var version struct {
		ID string `json:"id"`
	}
	s.expect(t, admin, org, http.MethodPost, "/api/v1/datasets/"+ds.ID+"/snapshots", `{"idempotency_key":"resid-1"}`, http.StatusCreated, &version)
	var where struct {
		Allowed   []string `json:"allowed"`
		Inside    bool     `json:"inside"`
		Locations []struct {
			ConnectionID     string `json:"connection_id"`
			Location         string `json:"location"`
			LocationVerified bool   `json:"location_verified"`
			Members          int64  `json:"members"`
			Allowed          bool   `json:"allowed"`
		} `json:"locations"`
	}
	s.expect(t, admin, org, http.MethodGet, "/api/v1/datasets/"+ds.ID+"/versions/"+version.ID+"/residency", "", http.StatusOK, &where)
	if !where.Inside || len(where.Locations) != 1 || where.Locations[0].Location != "ap-south-1" || !where.Locations[0].LocationVerified ||
		where.Locations[0].Members != 1 || where.Locations[0].ConnectionID != conn.ID {
		t.Fatalf("residency report: %+v", where)
	}

	// The readiness rule judges members against the dataset's locations,
	// or its own.
	inside := s.profileOf(t, tm.admin, "resid-inside", 0, model.Rule{ID: "where", Version: 1, Type: "residency", Severity: model.SeverityError, Enabled: true})
	_, _, view := s.validated(t, tm.admin, ds.ID, datasetservice.ValidationInput{VersionID: version.ID, ProfileID: inside.ID,
		IdempotencyKey: "resid-in-" + run}, false)
	if view.Status != "READY" {
		t.Fatalf("inside the dataset's locations: %s", view.Status)
	}
	outside := s.profileOf(t, tm.admin, "resid-eu", 0, model.Rule{ID: "where", Version: 1, Type: "residency", Severity: model.SeverityError,
		Enabled: true, Config: json.RawMessage(`{"locations":["eu-*"]}`)})
	_, report2, view := s.validated(t, tm.admin, ds.ID, datasetservice.ValidationInput{VersionID: version.ID, ProfileID: outside.ID,
		IdempotencyKey: "resid-out-" + run}, false)
	if view.Status == "READY" {
		t.Fatalf("outside eu-*: %s", view.Status)
	}
	found := false
	for _, f := range s.allFindings(t, tm.admin, report2.ID, "") {
		found = found || (f.Code == "RESIDENCY_OUTSIDE" && strings.Contains(f.Observed, "ap-south-1"))
	}
	if !found {
		t.Fatal("no RESIDENCY_OUTSIDE finding")
	}
}
