package integration

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/sanjeev0303/object-storage-platform/internal/dto/response"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// Adoption jobs over HTTP, and the MHL and BagIt exports of a snapshot.
func TestHTTPAdoptionAndExports(t *testing.T) {
	s := openSecurityStack(t, nil)
	acme := s.team(t, "adopt-a")
	globex := s.team(t, "adopt-b")
	tok := s.signIn(t, acme.devUser.Email, acme.devPW).AccessToken
	other := s.signIn(t, globex.devUser.Email, globex.devPW).AccessToken

	// Objects are adopted from the organization's own connections only.
	s.expect(t, tok, "", http.MethodPost, "/api/v1/adoptions", `{"source":"MANIFEST","inline":"{\"key\":\"a\"}"}`, http.StatusBadRequest, nil)
	s.expect(t, tok, "", http.MethodPost, "/api/v1/adoptions", `{"source":"FTP","inline":"x"}`, http.StatusBadRequest, nil)
	s.expect(t, tok, "", http.MethodPost, "/api/v1/adoptions", `{"source":"STAC","source_ref":"http://169.254.169.254/latest"}`, http.StatusBadRequest, nil)

	var job model.AdoptionJob
	s.expect(t, tok, "", http.MethodPost, "/api/v1/adoptions",
		`{"source":"URL_LIST","inline":"https://example.org/a.csv tables/a.csv","require_digest":false}`, http.StatusCreated, &job)
	if job.State != model.AdoptionQueued || job.Mode != model.AdoptionDetach || job.RequireDigest {
		t.Fatalf("job: %+v", job)
	}
	var list struct {
		Adoptions []model.AdoptionJob `json:"adoptions"`
	}
	s.expect(t, tok, "", http.MethodGet, "/api/v1/adoptions", "", http.StatusOK, &list)
	if len(list.Adoptions) != 1 {
		t.Fatalf("list: %+v", list)
	}
	s.expect(t, other, "", http.MethodGet, "/api/v1/adoptions/"+job.ID, "", http.StatusNotFound, nil)
	res := s.call(t, tok, "", http.MethodGet, "/api/v1/adoptions/"+job.ID+"/results?format=csv", "")
	if res.status != http.StatusOK || !strings.HasPrefix(string(res.body), "line,object_key,url,path,state") {
		t.Fatalf("csv results: %d %q", res.status, string(res.body))
	}
	s.expect(t, tok, "", http.MethodPost, "/api/v1/adoptions/"+job.ID+"/cancel", "", http.StatusNoContent, nil)
	s.expect(t, tok, "", http.MethodGet, "/api/v1/adoptions/"+job.ID, "", http.StatusOK, &job)
	if job.State != model.AdoptionCancelled {
		t.Fatalf("after cancel: %s", job.State)
	}

	// Exports of a snapshot.
	objs := s.publishObjects(t, acme.developer, "ex", 2)
	var ds response.DatasetResponse
	s.expect(t, tok, "", http.MethodPost, "/api/v1/datasets", `{"name":"export-set"}`, http.StatusCreated, &ds)
	s.expect(t, tok, "", http.MethodPost, "/api/v1/datasets/"+ds.ID+"/members",
		fmt.Sprintf(`{"members":[{"object_id":%q},{"object_id":%q}]}`, objs[0].ObjectID, objs[1].ObjectID), http.StatusOK, nil)
	var v struct {
		ID string `json:"id"`
	}
	s.expect(t, tok, "", http.MethodPost, "/api/v1/datasets/"+ds.ID+"/snapshots", `{"idempotency_key":"ex-1"}`, http.StatusCreated, &v)

	bag := s.call(t, tok, "", http.MethodGet, "/api/v1/datasets/"+ds.ID+"/versions/"+v.ID+"/manifest-sha256.txt", "")
	if bag.status != http.StatusOK || strings.Count(string(bag.body), "  data/") != 2 {
		t.Fatalf("bag manifest: %d %q", bag.status, string(bag.body))
	}
	mhl := s.call(t, tok, "", http.MethodGet, "/api/v1/datasets/"+ds.ID+"/versions/"+v.ID+"/mhl", "")
	body := string(mhl.body)
	if mhl.status != http.StatusOK || !strings.Contains(body, `<hashlist version="2.0" xmlns="urn:ASC:MHL:v2.0">`) ||
		strings.Count(body, "<xxh64") != 2 {
		t.Fatalf("mhl: %d %s", mhl.status, body)
	}
	// The MD5 in the list is of the stored bytes.
	sum := md5.Sum(textData("ex-0", 40))
	if !strings.Contains(body, hex.EncodeToString(sum[:])) {
		t.Fatalf("mhl does not carry the stored bytes' MD5: %s", body)
	}
	s.expect(t, other, "", http.MethodGet, "/api/v1/datasets/"+ds.ID+"/versions/"+v.ID+"/mhl", "", http.StatusNotFound, nil)
}
