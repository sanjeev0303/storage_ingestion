package integration

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sanjeev0303/object-storage-platform/internal/dto/response"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/server"
)

// apiClient calls the ingestion API as one of the test's callers.
type apiClient struct {
	t    *testing.T
	base string
}

// call sends a request with token as its bearer token ("" sends none) and
// returns the status and body.
func (c apiClient) call(token, method, path string, headers map[string]string, body string) (int, []byte) {
	c.t.Helper()
	req, err := http.NewRequest(method, c.base+path, strings.NewReader(body))
	if err != nil {
		c.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" && headers["Content-Type"] == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		c.t.Fatal(err)
	}
	return resp.StatusCode, out
}

// json calls the API, expects status and decodes the answer into out.
func (c apiClient) json(token, method, path string, headers map[string]string, body string, status int, out any) {
	c.t.Helper()
	code, raw := c.call(token, method, path, headers, body)
	if code != status {
		c.t.Fatalf("%s %s: HTTP %d, want %d: %s", method, path, code, status, raw)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			c.t.Fatalf("%s %s: %v: %s", method, path, err, raw)
		}
	}
}

func TestIngestionHTTPAPI(t *testing.T) {
	// Authentication, tenant isolation, pagination and the client flow,
	// through the real router.
	e := openIngestEnv(t)
	other := e.newTenant(t)
	tokens := map[string]model.Principal{"token-a": e.p, "token-b": other}
	identity := func(r *http.Request) (model.Principal, error) {
		p, ok := tokens[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
		if !ok {
			return model.Principal{}, errors.New("unknown token")
		}
		return p, nil
	}
	api := httptest.NewServer(server.NewRouter(server.Routes{
		Resolver: identity, Uploads: e.single, Multipart: e.multi, Ingestion: e.svc, Log: lifecycleLog,
	}))
	t.Cleanup(api.Close)
	c := apiClient{t: t, base: api.URL}

	// Every route needs a known caller.
	if code, _ := c.call("", http.MethodGet, "/api/v1/ingestion/jobs", nil, ""); code != http.StatusUnauthorized {
		t.Fatalf("no token: HTTP %d", code)
	}
	if code, _ := c.call("forged", http.MethodPost, "/api/v1/datasets", nil, `{"name":"x"}`); code != http.StatusUnauthorized {
		t.Fatalf("unknown token: HTTP %d", code)
	}

	var ds response.DatasetResponse
	c.json("token-a", http.MethodPost, "/api/v1/datasets", nil, `{"name":"http-set","completion_policy":"ALL"}`, http.StatusCreated, &ds)
	if ds.Status != string(model.DatasetOpen) {
		t.Fatalf("new dataset: %+v", ds)
	}
	jobBody := fmt.Sprintf(`{"dataset_id":%q,"idempotency_key":"http-1"}`, ds.ID)
	var job, same response.IngestionJobResponse
	c.json("token-a", http.MethodPost, "/api/v1/ingestion/jobs", nil, jobBody, http.StatusCreated, &job)
	c.json("token-a", http.MethodPost, "/api/v1/ingestion/jobs", nil, jobBody, http.StatusOK, &same)
	if same.ID != job.ID || job.State != string(model.JobPendingValidation) {
		t.Fatalf("job: %+v, again: %s", job, same.ID)
	}
	jobPath := "/api/v1/ingestion/jobs/" + job.ID

	files := series("http", 250, 64)
	manifest := manifestOf(files...) + "not json\n"
	var report response.ManifestReportResponse
	c.json("token-a", http.MethodPut, jobPath+"/manifest",
		map[string]string{"Content-Type": "application/x-ndjson", "X-Manifest-SHA256": sha256Hex(manifest)},
		manifest, http.StatusOK, &report)
	if report.Accepted != 250 || report.Invalid != 1 || !report.Complete || len(report.Samples) != 1 {
		t.Fatalf("report: %+v", report)
	}
	c.json("token-a", http.MethodPost, jobPath+"/finalize", nil, "", http.StatusOK, &job)
	if job.State != string(model.JobReady) || job.Progress.Valid != 250 || job.Progress.Pending != 250 {
		t.Fatalf("finalized: %+v", job)
	}

	// Another tenant can neither see nor change any of it.
	for _, path := range []string{"/api/v1/datasets/" + ds.ID, jobPath, jobPath + "/entries", jobPath + "/failures", jobPath + "/entries/1"} {
		if code, _ := c.call("token-b", http.MethodGet, path, nil, ""); code != http.StatusNotFound {
			t.Fatalf("other tenant GET %s: HTTP %d", path, code)
		}
	}
	for path, body := range map[string]string{
		jobPath + "/claims": `{"max":1}`, jobPath + "/cancel": "", jobPath + "/retry": `{"all":true}`,
		jobPath + "/finalize": "", jobPath + "/entries/1/complete": "", jobPath + "/entries/1/heartbeat": "",
	} {
		if code, _ := c.call("token-b", http.MethodPost, path, nil, body); code != http.StatusNotFound {
			t.Fatalf("other tenant POST %s: HTTP %d", path, code)
		}
	}
	var theirs response.JobPageResponse
	c.json("token-b", http.MethodGet, "/api/v1/ingestion/jobs", nil, "", http.StatusOK, &theirs)
	if len(theirs.Jobs) != 0 {
		t.Fatalf("the other tenant lists %d jobs", len(theirs.Jobs))
	}

	// Entries page by position, with next_after pointing at the next page.
	var after int64
	seen := 0
	for pages := 0; ; pages++ {
		var page response.EntryPageResponse
		c.json("token-a", http.MethodGet, fmt.Sprintf("%s/entries?limit=100&after=%d", jobPath, after), nil, "", http.StatusOK, &page)
		for _, entry := range page.Entries {
			seen++
			if entry.Position != int64(seen) {
				t.Fatalf("position %d, want %d", entry.Position, seen)
			}
		}
		if page.NextAfter == nil {
			break
		}
		if pages > 10 {
			t.Fatal("paging does not end")
		}
		after = *page.NextAfter
	}
	if seen != 251 {
		t.Fatalf("paged through %d entries", seen)
	}
	var failures, pending response.EntryPageResponse
	c.json("token-a", http.MethodGet, jobPath+"/failures", nil, "", http.StatusOK, &failures)
	if len(failures.Entries) != 1 || failures.Entries[0].ErrorCode != model.EntryErrInvalidJSON || failures.NextAfter != nil {
		t.Fatalf("failures: %+v", failures)
	}
	c.json("token-a", http.MethodGet, jobPath+"/entries?state=pending&limit=1000", nil, "", http.StatusOK, &pending)
	if len(pending.Entries) != 250 {
		t.Fatalf("pending entries: %d", len(pending.Entries))
	}

	// The client flow: claim, upload, complete.
	var claim response.ClaimResponse
	c.json("token-a", http.MethodPost, jobPath+"/claims", nil, `{"max":2}`, http.StatusOK, &claim)
	if len(claim.Grants) != 2 || claim.Grants[0].UploadURL == "" || claim.Grants[0].Entry.UploadID == "" {
		t.Fatalf("claim: %+v", claim)
	}
	data := contents(files...)
	for _, g := range claim.Grants {
		// The memory provider's URLs are not real: store the bytes directly.
		e.storage.PutObject(e.session(t, g.Entry.UploadID).ObjectKey, data[g.Entry.Path])
		var entry response.IngestionEntryResponse
		c.json("token-a", http.MethodPost, fmt.Sprintf("%s/entries/%d/complete", jobPath, g.Entry.Position), nil, "", http.StatusOK, &entry)
		if entry.State != string(model.EntryUploaded) {
			t.Fatalf("completed entry: %+v", entry)
		}
	}

	// Bad requests are refused as such.
	if code, _ := c.call("token-a", http.MethodPost, "/api/v1/datasets", nil, `{"name":`); code != http.StatusBadRequest {
		t.Fatalf("broken JSON: HTTP %d", code)
	}
	if code, _ := c.call("token-a", http.MethodGet, jobPath+"/entries/0", nil, ""); code != http.StatusBadRequest {
		t.Fatalf("position 0: HTTP %d", code)
	}
	if code, _ := c.call("token-a", http.MethodGet, jobPath+"/entries/9999", nil, ""); code != http.StatusNotFound {
		t.Fatalf("unknown position: HTTP %d", code)
	}
	if code, _ := c.call("token-a", http.MethodPut, jobPath+"/manifest", nil, manifestOf(textFile("x.txt", 3))); code != http.StatusConflict {
		t.Fatalf("a second, different manifest: HTTP %d", code)
	}

	// Jobs page too.
	for i := 2; i <= 3; i++ {
		c.json("token-a", http.MethodPost, "/api/v1/ingestion/jobs", nil,
			fmt.Sprintf(`{"dataset_id":%q,"idempotency_key":"http-%d"}`, ds.ID, i), http.StatusCreated, nil)
	}
	var first, second response.JobPageResponse
	c.json("token-a", http.MethodGet, "/api/v1/ingestion/jobs?limit=2", nil, "", http.StatusOK, &first)
	if len(first.Jobs) != 2 || first.NextAfter == "" {
		t.Fatalf("first page of jobs: %+v", first)
	}
	c.json("token-a", http.MethodGet, "/api/v1/ingestion/jobs?limit=2&after="+first.NextAfter, nil, "", http.StatusOK, &second)
	if len(second.Jobs) != 1 || second.NextAfter != "" || second.Jobs[0].ID == first.Jobs[0].ID || second.Jobs[0].ID == first.Jobs[1].ID {
		t.Fatalf("second page of jobs: %+v", second)
	}

	// Cancelling stops new grants.
	c.json("token-a", http.MethodPost, jobPath+"/cancel", nil, "", http.StatusAccepted, &job)
	if job.State != string(model.JobCancelling) {
		t.Fatalf("cancel: %+v", job)
	}
	c.json("token-a", http.MethodPost, jobPath+"/claims", nil, `{"max":5}`, http.StatusOK, &claim)
	if len(claim.Grants) != 0 || claim.LimitedBy != "job_not_running" {
		t.Fatalf("claim after cancel: %+v", claim)
	}
}
