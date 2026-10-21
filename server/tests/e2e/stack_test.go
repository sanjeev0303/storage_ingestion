// Package e2e drives the real binaries over HTTP: the API and the
// processing, dataset and cleanup workers, built from this module and
// started against PostgreSQL, MinIO and ClamAV. Nothing is called in
// process, so every step goes through the same code a deployment runs.
//
// It runs only when E2E_DATABASE_URL names a disposable database migrated
// through the latest migration; it never reads DATABASE_URL. The platform
// bucket and the tenants' buckets live on the S3_* endpoint (MinIO), and
// CLAMAV_ADDRESS must reach ClamAV. E2E_BYOS_ACCESS_KEY and
// E2E_BYOS_SECRET_KEY, when set, are a separate MinIO account for the
// tenants' connections, so their secret can be searched for in logs,
// responses and the database. See docs/release-candidate.md.
package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	s3storage "github.com/sanjeev0303/object-storage-platform/internal/infrastructure/storage/s3"
)

// The services run for the whole test; identity-admin is run on demand.
// The upload worker only serves bulk ingestion, which this flow does not use.
var (
	services = []string{"api", "processing-worker", "dataset-worker", "cleanup-worker"}
	binaries = []string{"api", "processing-worker", "dataset-worker", "cleanup-worker", "identity-admin"}
)

type stack struct {
	t        *testing.T
	dir      string
	env      []string
	api      string
	dbURL    string
	s3       *awss3.Client
	s3cfg    s3storage.Config
	platform string
	// metrics is where the processing worker serves /debug/vars.
	metrics string
	// The tenants' connections use these keys (E2E_BYOS_*, else the S3 ones).
	byosKey, byosSecret string
	// secrets must never show up in output, responses or the database.
	secrets []string

	mu    sync.Mutex
	procs map[string]*process
}

type process struct {
	cmd  *exec.Cmd
	log  *bytes.Buffer
	done chan struct{}
}

// lockedBuffer collects a process's output while the test reads it.
type lockedBuffer struct {
	mu  *sync.Mutex
	buf *bytes.Buffer
}

func (b lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func startStack(t *testing.T) *stack {
	t.Helper()
	dbURL := os.Getenv("E2E_DATABASE_URL")
	if dbURL == "" {
		t.Skip("E2E_DATABASE_URL is not set")
	}
	clamav := os.Getenv("CLAMAV_ADDRESS")
	if clamav == "" {
		t.Skip("CLAMAV_ADDRESS is not set")
	}
	s3cfg, err := s3storage.LoadConfig()
	if err != nil {
		t.Skipf("S3 settings: %v", err)
	}
	if s3cfg.Endpoint == "" || s3cfg.AccessKeyID == "" {
		t.Skip("S3_ENDPOINT and the S3 keys must point at a local MinIO")
	}

	client, err := s3storage.NewClient(context.Background(), s3cfg)
	if err != nil {
		t.Fatal(err)
	}
	s := &stack{
		t: t, dir: t.TempDir(), dbURL: dbURL, s3: client, s3cfg: s3cfg, procs: map[string]*process{},
		platform: envOr("E2E_S3_BUCKET", "object-storage-e2e"),
	}
	s.bucket(s.platform)
	s.build()

	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	credentialKey := base64.StdEncoding.EncodeToString(key)
	s.byosKey = envOr("E2E_BYOS_ACCESS_KEY", s3cfg.AccessKeyID)
	s.byosSecret = envOr("E2E_BYOS_SECRET_KEY", s3cfg.SecretAccessKey)
	// A secret equal to its access key (MinIO's default root account) is
	// not a usable marker: presigned URLs carry the access key by design.
	s.secrets = []string{credentialKey}
	if s3cfg.SecretAccessKey != s3cfg.AccessKeyID {
		s.secrets = append(s.secrets, s3cfg.SecretAccessKey)
	}
	if s.byosSecret != s.byosKey {
		s.secrets = append(s.secrets, s.byosSecret)
	}
	port := freePort(t)
	s.api = "http://127.0.0.1:" + port
	s.metrics = "127.0.0.1:" + freePort(t)

	// The workers poll every second and back off for seconds, not minutes,
	// so the test does not wait on production intervals. The queue stays
	// off and the pollers do the work. Deletions have no grace period.
	s.env = append(os.Environ(),
		"APP_ENV=development",
		"DATABASE_URL="+dbURL,
		"HTTP_PORT="+port,
		"S3_BUCKET="+s.platform,
		"EVENT_QUEUE=",
		"SCANNER=clamav",
		"CLAMAV_ADDRESS="+clamav,
		"BYOS_ENABLED=true",
		"STORAGE_CREDENTIAL_KEYS=1:"+credentialKey,
		"STORAGE_ALLOW_PRIVATE_ENDPOINTS=true",
		"STORAGE_CONNECTION_CACHE_TTL=1s",
		"AUTH_ACCESS_TOKEN_TTL=1h",
		"RATE_LIMIT_CREDENTIALS=100/1m",
		"PROCESSING_POLL_INTERVAL=1s",
		"PROCESSING_BACKOFF_BASE=1s",
		"PROCESSING_BACKOFF_MAX=5s",
		"DATASET_POLL_INTERVAL=1s",
		"LIFECYCLE_POLL_INTERVAL=1s",
		"LIFECYCLE_RECHECK_INTERVAL=5s",
		"LIFECYCLE_BACKOFF_BASE=1s",
		"LIFECYCLE_BACKOFF_MAX=5s",
		"RETENTION_DELETION_GRACE_FLOOR=0s",
		"RETENTION_DEFAULT_DELETION_GRACE=0s",
	)
	t.Cleanup(s.stopAll)
	for _, name := range services {
		s.start(name)
	}
	s.waitHealthy()
	return s
}

// build compiles every binary once into the test's directory.
func (s *stack) build() {
	s.t.Helper()
	args := []string{"build", "-o", s.dir + string(filepath.Separator)}
	for _, name := range binaries {
		args = append(args, "./cmd/"+name)
	}
	cmd := exec.Command("go", args...)
	cmd.Dir = moduleRoot(s.t)
	if out, err := cmd.CombinedOutput(); err != nil {
		s.t.Fatalf("go build: %v\n%s", err, out)
	}
}

func (s *stack) binary(name string) string {
	path := filepath.Join(s.dir, name)
	if runtime.GOOS == "windows" {
		path += ".exe"
	}
	return path
}

func (s *stack) start(name string) {
	s.t.Helper()
	cmd := exec.Command(s.binary(name))
	cmd.Dir = s.dir
	cmd.Env = s.env
	if name == "processing-worker" {
		cmd.Env = append(cmd.Env, "METRICS_ADDR="+s.metrics)
	}
	log := &bytes.Buffer{}
	out := lockedBuffer{mu: &s.mu, buf: log}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		s.t.Fatalf("start %s: %v", name, err)
	}
	p := &process{cmd: cmd, log: log, done: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		close(p.done)
	}()
	s.mu.Lock()
	if old := s.procs[name]; old != nil {
		// Keep what the previous run of this process wrote.
		log.WriteString(old.log.String())
	}
	s.procs[name] = p
	s.mu.Unlock()
}

// kill ends a process without letting it shut down cleanly, as a crash or
// an evicted container would.
func (s *stack) kill(name string) {
	s.t.Helper()
	s.mu.Lock()
	p := s.procs[name]
	s.mu.Unlock()
	if p == nil {
		return
	}
	_ = p.cmd.Process.Kill()
	<-p.done
}

func (s *stack) stopAll() {
	for name, p := range s.procs {
		_ = p.cmd.Process.Kill()
		<-p.done
		if s.t.Failed() {
			s.t.Logf("---- %s output ----\n%s", name, tail(s.logs(name), 6000))
		}
	}
}

func (s *stack) logs(name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.procs[name]; p != nil {
		return p.log.String()
	}
	return ""
}

func (s *stack) waitHealthy() {
	s.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(s.api + "/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		select {
		case <-s.procs["api"].done:
			s.t.Fatalf("the API exited:\n%s", s.logs("api"))
		case <-time.After(200 * time.Millisecond):
		}
	}
	s.t.Fatalf("the API did not become healthy:\n%s", s.logs("api"))
}

// admin runs identity-admin and decodes the JSON it prints.
func (s *stack) admin(args ...string) (map[string]any, error) {
	cmd := exec.Command(s.binary("identity-admin"), args...)
	cmd.Dir = s.dir
	cmd.Env = s.env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("identity-admin %s: %v: %s", args[0], err, stderr.String())
	}
	var out map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		return nil, fmt.Errorf("identity-admin %s printed %q", args[0], stdout.String())
	}
	return out, nil
}

// bucket creates a bucket on MinIO unless it is already there.
func (s *stack) bucket(name string) {
	s.t.Helper()
	_, err := s.s3.CreateBucket(context.Background(), &awss3.CreateBucketInput{Bucket: aws.String(name)})
	var owned *s3types.BucketAlreadyOwnedByYou
	var exists *s3types.BucketAlreadyExists
	if err != nil && !errors.As(err, &owned) && !errors.As(err, &exists) {
		s.t.Fatalf("create bucket %s: %v", name, err)
	}
}

// objectExists reports whether key is in bucket, read straight from MinIO.
func (s *stack) objectExists(bucket, key string) bool {
	s.t.Helper()
	_, err := s.s3.HeadObject(context.Background(), &awss3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err == nil {
		return true
	}
	var missing *s3types.NotFound
	if errors.As(err, &missing) {
		return false
	}
	s.t.Fatalf("head %s/%s: %v", bucket, key, err)
	return false
}

// keysIn lists every key under prefix in bucket.
func (s *stack) keysIn(bucket, prefix string) []string {
	s.t.Helper()
	var keys []string
	p := awss3.NewListObjectsV2Paginator(s.s3, &awss3.ListObjectsV2Input{Bucket: aws.String(bucket), Prefix: aws.String(prefix)})
	for p.HasMorePages() {
		page, err := p.NextPage(context.Background())
		if err != nil {
			s.t.Fatalf("list %s: %v", bucket, err)
		}
		for _, o := range page.Contents {
			keys = append(keys, aws.ToString(o.Key))
		}
	}
	return keys
}

// leakedInOutput returns the first secret found in any process's output.
func (s *stack) leakedInOutput() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.procs {
		if secret := firstSecret(s.secrets, p.log.String()); secret != "" {
			return secret
		}
	}
	return ""
}

func firstSecret(secrets []string, text string) string {
	for _, secret := range secrets {
		if secret != "" && strings.Contains(text, secret) {
			return secret
		}
	}
	return ""
}

// client is one signed-in account, working in one organization.
type client struct {
	s     *stack
	token string
	org   string
}

type response struct {
	status int
	header http.Header
	body   []byte
}

func (r response) json(t *testing.T) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(r.body, &out); err != nil {
		t.Fatalf("HTTP %d: not JSON: %s", r.status, r.body)
	}
	return out
}

// do sends body as JSON (nil sends none). header is name, value pairs.
func (c *client) do(method, path string, body any, header ...string) response {
	c.s.t.Helper()
	if body == nil {
		return c.send(method, path, "", nil, header...)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		c.s.t.Fatal(err)
	}
	return c.send(method, path, "application/json", raw, header...)
}

func (c *client) send(method, path, contentType string, body []byte, header ...string) response {
	c.s.t.Helper()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, c.s.api+path, reader)
	if err != nil {
		c.s.t.Fatal(err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if c.org != "" {
		req.Header.Set("X-Organization-ID", c.org)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.s.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		c.s.t.Fatalf("%s %s: read body: %v", method, path, err)
	}
	if secret := firstSecret(c.s.secrets, string(raw)); secret != "" {
		c.s.t.Fatalf("%s %s answered with a secret in its body", method, path)
	}
	return response{status: resp.StatusCode, header: resp.Header, body: raw}
}

// must sends the request and fails the test unless the status is want.
func (c *client) must(want int, method, path string, body any, header ...string) map[string]any {
	c.s.t.Helper()
	r := c.do(method, path, body, header...)
	if r.status != want {
		c.s.t.Fatalf("%s %s: HTTP %d, want %d: %s", method, path, r.status, want, r.body)
	}
	if len(r.body) == 0 {
		return nil
	}
	return r.json(c.s.t)
}

// put sends bytes to a presigned URL and returns the ETag.
func put(t *testing.T, url string, data []byte, header map[string]string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, url, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT: HTTP %d: %s", resp.StatusCode, body)
	}
	return resp.Header.Get("ETag")
}

// eventually polls check until it returns true or the deadline passes.
// GETs are rate limited per organization, so it polls four times a second.
func eventually(t *testing.T, timeout time.Duration, what string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !check() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}

func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	return port
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
