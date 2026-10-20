package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/dto/request"
	"github.com/sanjeev0303/object-storage-platform/internal/dto/response"
)

// api calls the ingestion API as one signed-in member of one
// organization.
type api struct {
	base   string
	org    string
	token  string
	client *http.Client
	stats  *requestStats
}

// signIn gets an access token for email. The run must finish within the
// token's lifetime (AUTH_ACCESS_TOKEN_TTL, 15m by default); raise it on
// the local API for longer runs.
func (a *api) signIn(ctx context.Context, email, password string) error {
	var tokens response.TokensResponse
	in := request.LoginRequest{Email: email, Password: password}
	if _, err := a.call(ctx, "login", http.MethodPost, "/api/v1/auth/login", in, &tokens); err != nil {
		return err
	}
	a.token = tokens.AccessToken
	return nil
}

// requestStats counts calls, failures and latencies by kind.
type requestStats struct {
	mu        sync.Mutex
	calls     map[string]int
	failures  map[string]int
	latencies map[string][]time.Duration
	samples   []string
}

func newRequestStats() *requestStats {
	return &requestStats{calls: map[string]int{}, failures: map[string]int{}, latencies: map[string][]time.Duration{}}
}

func (s *requestStats) record(kind string, took time.Duration, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls[kind]++
	s.latencies[kind] = append(s.latencies[kind], took)
	if err != nil {
		s.failures[kind]++
		if len(s.samples) < 10 {
			s.samples = append(s.samples, err.Error())
		}
	}
}

// percentile of kind's latencies, p between 0 and 1.
func (s *requestStats) percentile(kind string, p float64) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := slices.Clone(s.latencies[kind])
	if len(l) == 0 {
		return 0
	}
	slices.Sort(l)
	return l[int(float64(len(l)-1)*p)]
}

// call sends one API request. A non-2xx answer is an error carrying the
// response's first bytes. It returns the status.
func (a *api) call(ctx context.Context, kind, method, path string, in, out any) (int, error) {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return 0, err
		}
		body = bytes.NewReader(raw)
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, a.base+path, body)
	if err != nil {
		return 0, err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return a.send(kind, req, out)
}

func (a *api) send(kind string, req *http.Request, out any) (int, error) {
	if a.token != "" {
		req.Header.Set("Authorization", "Bearer "+a.token)
	}
	if a.org != "" {
		req.Header.Set("X-Organization-ID", a.org)
	}
	started := time.Now()
	status, err := a.roundTrip(req, out)
	a.stats.record(kind, time.Since(started), err)
	return status, err
}

func (a *api) roundTrip(req *http.Request, out any) (int, error) {
	resp, err := a.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return resp.StatusCode, fmt.Errorf("%s %s: HTTP %d: %s", req.Method, req.URL.Path, resp.StatusCode, bytes.TrimSpace(msg))
	}
	if out != nil && resp.StatusCode != http.StatusNoContent {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return resp.StatusCode, fmt.Errorf("%s %s: decode: %w", req.Method, req.URL.Path, err)
		}
	}
	return resp.StatusCode, nil
}

func jobPath(jobID string) string { return "/api/v1/ingestion/jobs/" + jobID }

func entryPath(jobID string, position int64) string {
	return fmt.Sprintf("%s/entries/%d", jobPath(jobID), position)
}

func (a *api) createDataset(ctx context.Context, name string) (response.DatasetResponse, error) {
	var out response.DatasetResponse
	_, err := a.call(ctx, "create_dataset", http.MethodPost, "/api/v1/datasets", request.CreateDatasetRequest{Name: name}, &out)
	return out, err
}

func (a *api) createJob(ctx context.Context, datasetID, key string) (response.IngestionJobResponse, error) {
	var out response.IngestionJobResponse
	_, err := a.call(ctx, "create_job", http.MethodPost, "/api/v1/ingestion/jobs",
		request.CreateIngestionJobRequest{DatasetID: datasetID, IdempotencyKey: key}, &out)
	return out, err
}

// putManifest sends the manifest in the request body.
func (a *api) putManifest(ctx context.Context, jobID string, body io.Reader, size int64, sum string) (response.ManifestReportResponse, error) {
	var out response.ManifestReportResponse
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, a.base+jobPath(jobID)+"/manifest", body)
	if err != nil {
		return out, err
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/x-ndjson")
	req.Header.Set("X-Manifest-SHA256", sum)
	_, err = a.send("submit_manifest", req, &out)
	return out, err
}

func (a *api) stageManifest(ctx context.Context, jobID string, size int64) (response.StagedManifestResponse, error) {
	var out response.StagedManifestResponse
	_, err := a.call(ctx, "stage_manifest", http.MethodPost, jobPath(jobID)+"/manifest/staged-url", request.StageManifestRequest{SizeBytes: size}, &out)
	return out, err
}

func (a *api) submitStaged(ctx context.Context, jobID, sum string) error {
	_, err := a.call(ctx, "submit_staged", http.MethodPost, jobPath(jobID)+"/manifest/staged", request.SubmitStagedManifestRequest{SHA256: sum}, nil)
	return err
}

func (a *api) finalize(ctx context.Context, jobID string) (response.IngestionJobResponse, error) {
	var out response.IngestionJobResponse
	_, err := a.call(ctx, "finalize", http.MethodPost, jobPath(jobID)+"/finalize", nil, &out)
	return out, err
}

func (a *api) job(ctx context.Context, jobID string) (response.IngestionJobResponse, error) {
	var out response.IngestionJobResponse
	_, err := a.call(ctx, "get_job", http.MethodGet, jobPath(jobID), nil, &out)
	return out, err
}

func (a *api) claim(ctx context.Context, jobID string, max int) (response.ClaimResponse, error) {
	var out response.ClaimResponse
	_, err := a.call(ctx, "claim", http.MethodPost, jobPath(jobID)+"/claims", request.ClaimEntriesRequest{Max: max}, &out)
	return out, err
}

func (a *api) partURLs(ctx context.Context, jobID string, position int64, numbers []int32) ([]response.PresignedPartURL, error) {
	var out response.PresignPartsResponse
	_, err := a.call(ctx, "part_urls", http.MethodPost, entryPath(jobID, position)+"/parts/urls", request.PresignPartsRequest{PartNumbers: numbers}, &out)
	return out.Parts, err
}

func (a *api) confirmParts(ctx context.Context, jobID string, position int64, parts []request.RecordPartRequest) error {
	_, err := a.call(ctx, "confirm_parts", http.MethodPost, entryPath(jobID, position)+"/parts/confirm", request.RecordPartsRequest{Parts: parts}, nil)
	return err
}

// complete reports an entry's upload done. It returns the HTTP status:
// 202 means the completion is still being confirmed.
func (a *api) complete(ctx context.Context, jobID string, position int64, parts []request.CompletedPartRequest) (int, error) {
	var in any
	if len(parts) > 0 {
		in = request.CompleteMultipartUploadRequest{Parts: parts}
	}
	return a.call(ctx, "complete", http.MethodPost, entryPath(jobID, position)+"/complete", in, nil)
}

func (a *api) cancel(ctx context.Context, jobID string) error {
	_, err := a.call(ctx, "cancel", http.MethodPost, jobPath(jobID)+"/cancel", nil, nil)
	return err
}

// put uploads size bytes from body to a presigned storage URL and returns
// the ETag. It talks to the object store, not to the API.
func (a *api) put(ctx context.Context, url, contentType string, body io.Reader, size int64) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, body)
	if err != nil {
		return "", err
	}
	req.ContentLength = size
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	started := time.Now()
	resp, err := a.client.Do(req)
	if err == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			err = fmt.Errorf("storage PUT: HTTP %d", resp.StatusCode)
		}
	}
	a.stats.record("storage_put", time.Since(started), err)
	if err != nil {
		return "", err
	}
	return resp.Header.Get("ETag"), nil
}
