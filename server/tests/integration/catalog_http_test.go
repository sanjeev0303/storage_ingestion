package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/sanjeev0303/object-storage-platform/internal/dto/response"
)

// Phase 9 over HTTP: the catalog routes through the real router.

func TestHTTPCatalogFlow(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	acme := s.team(t, "cat-web")
	tok := s.signIn(t, acme.devUser.Email, acme.devPW).AccessToken
	auditorTok := s.signIn(t, acme.auditorUser.Email, acme.auditorPW).AccessToken

	// An upload names its path and metadata.
	data := textData("report", 120)
	sum := sha256.Sum256(data)
	body := fmt.Sprintf(`{"file_name":"report.txt","content_type":"text/plain","size_bytes":%d,"checksum_sha256":%q,
		"idempotency_key":"web-1","path":"Reports/2026/report.txt","metadata":{"team":"finance"}}`, len(data), hex.EncodeToString(sum[:]))
	var created response.CreateUploadResponse
	s.expect(t, tok, "", http.MethodPost, "/api/v1/uploads", body, http.StatusCreated, &created)
	session, err := s.ie.uploads.GetByID(ctx, acme.org.ID, created.UploadID)
	if err != nil {
		t.Fatal(err)
	}
	s.ie.storage.PutObject(session.ObjectKey, data)
	s.expect(t, tok, "", http.MethodPost, "/api/v1/uploads/"+created.UploadID+"/complete", "{}", http.StatusOK, nil)
	if _, err := s.ie.proc.Intake(ctx, acme.org.ID, created.UploadID); err != nil {
		t.Fatal(err)
	}

	// Found by path in any case, listed, with its revision as the ETag.
	var obj response.CatalogObjectResponse
	r := s.expect(t, tok, "", http.MethodGet, "/api/v1/catalog/lookup?path=reports/2026/REPORT.TXT", "", http.StatusOK, &obj)
	if obj.CurrentVersion == nil || obj.CurrentVersion.Version != 1 || obj.Metadata["team"] != "finance" ||
		r.header.Get("ETag") != fmt.Sprintf("%q", fmt.Sprint(obj.Revision)) {
		t.Fatalf("object: %+v, ETag %s", obj, r.header.Get("ETag"))
	}
	var page response.CatalogPageResponse
	s.expect(t, auditorTok, "", http.MethodGet, "/api/v1/catalog/objects?prefix=reports/&metadata.team=finance&limit=1", "", http.StatusOK, &page)
	if len(page.Objects) != 1 || page.Objects[0].ID != obj.ID {
		t.Fatalf("listing: %+v", page)
	}

	// Metadata changes need the revision: none is 428, a stale one 412.
	objPath := "/api/v1/catalog/objects/" + obj.ID
	if r := s.call(t, tok, "", http.MethodPatch, objPath, `{"metadata":{"stage":"final"}}`); r.status != http.StatusPreconditionRequired {
		t.Fatalf("no revision: %s", r)
	}
	stale := map[string]string{"If-Match": `"1"`}
	if r := s.call(t, tok, "", http.MethodPatch, objPath, `{"metadata":{"stage":"final"}}`, stale); r.status != http.StatusPreconditionFailed {
		t.Fatalf("stale revision: %s", r)
	}
	if r := s.call(t, auditorTok, "", http.MethodPatch, objPath, `{"metadata":{"stage":"final"}}`); r.status != http.StatusForbidden {
		t.Fatalf("an auditor changes metadata: %s", r)
	}
	current := map[string]string{"If-Match": fmt.Sprintf("%q", fmt.Sprint(obj.Revision))}
	var patched response.CatalogObjectResponse
	s.call(t, tok, "", http.MethodPatch, objPath, `{"metadata":{"stage":"final","team":null}}`, current)
	s.expect(t, tok, "", http.MethodGet, objPath, "", http.StatusOK, &patched)
	if patched.Metadata["stage"] != "final" || patched.Metadata["team"] != "" || patched.Revision != obj.Revision+1 {
		t.Fatalf("patched: %+v", patched)
	}

	// Versions: the history and the current one, without storage keys.
	var versions response.VersionPageResponse
	r = s.expect(t, tok, "", http.MethodGet, objPath+"/versions", "", http.StatusOK, &versions)
	if len(versions.Versions) != 1 || !versions.Versions[0].Current || versions.Versions[0].ChecksumAlgorithm != "sha256" {
		t.Fatalf("versions: %+v", versions)
	}
	if strings.Contains(string(r.body), "quarantine/") || strings.Contains(string(r.body), "published/") || strings.Contains(string(r.body), s.ie.bucket) {
		t.Fatalf("storage keys or buckets leaked: %s", r.body)
	}
	var v response.ObjectVersionResponse
	s.expect(t, tok, "", http.MethodGet, objPath+"/versions/current", "", http.StatusOK, &v)
	if v.Version != 1 || v.AssetID != obj.CurrentVersion.AssetID || v.Metadata["team"] != "finance" {
		t.Fatalf("current version: %+v", v)
	}

	// Rename, then delete the only version: the object leaves the listing.
	s.expect(t, tok, "", http.MethodPost, objPath+"/rename", fmt.Sprintf(`{"path":"archive/report.txt","revision":%d}`, patched.Revision), http.StatusOK, &obj)
	if obj.Path != "archive/report.txt" {
		t.Fatalf("renamed: %+v", obj)
	}
	s.expect(t, tok, "", http.MethodDelete, objPath+"/versions/1", "", http.StatusAccepted, nil)
	s.expect(t, tok, "", http.MethodGet, "/api/v1/catalog/objects", "", http.StatusOK, &page)
	if len(page.Objects) != 0 {
		t.Fatalf("an object without a current version is not listed: %+v", page)
	}

	// Bad input is a 400, never a 500.
	for _, path := range []string{"/api/v1/catalog/objects?limit=abc", "/api/v1/catalog/objects?limit=100000",
		"/api/v1/catalog/objects?cursor=garbage", "/api/v1/catalog/objects?order=random", "/api/v1/catalog/lookup?path=../x",
		"/api/v1/catalog/objects?created_after=yesterday", objPath + "/versions/0"} {
		if r := s.call(t, tok, "", http.MethodGet, path, ""); r.status != http.StatusBadRequest {
			t.Fatalf("%s: %s", path, r)
		}
	}
	var caps response.StorageCapabilitiesResponse
	s.expect(t, tok, "", http.MethodGet, "/api/v1/catalog/capabilities", "", http.StatusOK, &caps)
	if !caps.LogicalVersioning || caps.ProviderVersioning {
		t.Fatalf("capabilities: %+v", caps)
	}
}

func TestHTTPCatalogTenantIsolation(t *testing.T) {
	s := openSecurityStack(t, nil)
	acme := s.team(t, "cat-iso-a")
	globex := s.team(t, "cat-iso-b")
	secret := s.publishAt(t, acme.developer, "private/plan.txt", textData("plan", 60))
	acmeTok := s.signIn(t, acme.devUser.Email, acme.devPW).AccessToken
	globexTok := s.signIn(t, globex.devUser.Email, globex.devPW).AccessToken
	revision := `{"revision":1}`

	routes := []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/catalog/objects/%s", ""},
		{http.MethodGet, "/api/v1/catalog/objects/%s/versions", ""},
		{http.MethodGet, "/api/v1/catalog/objects/%s/versions/1", ""},
		{http.MethodGet, "/api/v1/catalog/objects/%s/versions/current", ""},
		{http.MethodPatch, "/api/v1/catalog/objects/%s", `{"revision":1,"metadata":{"x":"y"}}`},
		{http.MethodPost, "/api/v1/catalog/objects/%s/rename", `{"revision":1,"path":"stolen.txt"}`},
		{http.MethodPut, "/api/v1/catalog/objects/%s/versioning", `{"revision":1,"versioning":"DISABLED"}`},
		{http.MethodPost, "/api/v1/catalog/objects/%s/versions/1/promote", revision},
		{http.MethodDelete, "/api/v1/catalog/objects/%s/versions/1", ""},
		{http.MethodDelete, "/api/v1/catalog/objects/%s", ""},
	}
	for _, rt := range routes {
		// The route serves its own tenant (reads only: the writes would
		// change the object)...
		if rt.method == http.MethodGet {
			if mine := s.call(t, acmeTok, "", rt.method, fmt.Sprintf(rt.path, secret.ObjectID), rt.body); mine.status != http.StatusOK {
				t.Fatalf("acme %s %s: %s", rt.method, rt.path, mine)
			}
		}
		// ...and answers another tenant exactly like a missing object.
		theirs := s.call(t, globexTok, "", rt.method, fmt.Sprintf(rt.path, secret.ObjectID), rt.body)
		missing := s.call(t, globexTok, "", rt.method, fmt.Sprintf(rt.path, "no-such-object"), rt.body)
		if theirs.status != http.StatusNotFound || string(theirs.body) != string(missing.body) {
			t.Fatalf("%s %s across tenants: %s, missing: %s", rt.method, rt.path, theirs, missing)
		}
	}
	// The other tenant's object is untouched.
	if o, err := s.catalog.Get(context.Background(), acme.developer, secret.ObjectID); err != nil ||
		o.Path != "private/plan.txt" || len(o.Metadata) != 0 || !o.Published() || o.Versioning != "ENABLED" {
		t.Fatalf("acme's object after globex's attempts: %+v %v", o, err)
	}

	// Paths are per tenant: the same path in globex's namespace is not
	// acme's object, and a listing shows only one's own.
	if r := s.call(t, globexTok, "", http.MethodGet, "/api/v1/catalog/lookup?path=private/plan.txt", ""); r.status != http.StatusNotFound {
		t.Fatalf("lookup across tenants: %s", r)
	}
	var page response.CatalogPageResponse
	s.expect(t, globexTok, "", http.MethodGet, "/api/v1/catalog/objects?include=unpublished", "", http.StatusOK, &page)
	if len(page.Objects) != 0 {
		t.Fatalf("globex lists acme's objects: %+v", page.Objects)
	}
	// Filtering by acme's asset ID finds nothing either.
	s.expect(t, globexTok, "", http.MethodGet, "/api/v1/catalog/objects?asset_id="+secret.ID, "", http.StatusOK, &page)
	if len(page.Objects) != 0 {
		t.Fatal("an asset filter must not cross tenants")
	}
	// A cursor is bound to its tenant.
	s.publishAt(t, acme.developer, "private/notes.txt", textData("notes", 60))
	var acmePage response.CatalogPageResponse
	s.expect(t, acmeTok, "", http.MethodGet, "/api/v1/catalog/objects?limit=1", "", http.StatusOK, &acmePage)
	if acmePage.NextCursor == "" {
		t.Fatal("expected a second page")
	}
	r := s.call(t, globexTok, "", http.MethodGet, "/api/v1/catalog/objects?limit=1&cursor="+acmePage.NextCursor, "")
	var problem struct{ Code string }
	_ = json.Unmarshal(r.body, &problem)
	if r.status != http.StatusBadRequest || problem.Code != "invalid_request" {
		t.Fatalf("another tenant's cursor: %s", r)
	}
}
