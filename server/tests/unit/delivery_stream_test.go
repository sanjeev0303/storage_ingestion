package unit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	httpapi "github.com/sanjeev0303/object-storage-platform/internal/handler/http"
	storagememory "github.com/sanjeev0303/object-storage-platform/internal/infrastructure/storage/memory"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/metrics"
	"github.com/sanjeev0303/object-storage-platform/internal/repository/memory"
	"github.com/sanjeev0303/object-storage-platform/internal/server"
	"github.com/sanjeev0303/object-storage-platform/internal/service/delivery"
)

// streamAPI is the delivery API over in-memory storage, with its audit
// trail, metrics and log output exposed for assertions.
type streamAPI struct {
	handler  http.Handler
	storage  *storagememory.ObjectStorage
	audit    *memory.DeliveryAuditRepository
	registry *metrics.Registry
	logs     *syncBuffer
	etag     string
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type streamOptions struct {
	storage     interfaces.ObjectDelivery
	contentType string
	size        int64
	stall       time.Duration
	maxStreams  int
}

func newStreamAPI(t *testing.T, opts streamOptions) streamAPI {
	t.Helper()
	assets := memory.NewAssetRepository()
	memStorage := storagememory.NewObjectStorage()
	memStorage.PutObject("assets/tenant-1/a1", deliveredBytes)

	var storage interfaces.ObjectDelivery = memStorage
	if opts.storage != nil {
		storage = opts.storage
	}
	if opts.contentType == "" {
		opts.contentType = "application/pdf"
	}
	if opts.size == 0 {
		opts.size = int64(len(deliveredBytes))
	}
	sum := sha256.Sum256(deliveredBytes)
	published := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	asset := model.Asset{
		ID: "a1", TenantID: "tenant-1", OwnerID: "user-1", UploadID: "up-a1", ObjectKey: "assets/tenant-1/a1",
		FileName: "report.pdf", ContentType: opts.contentType, SizeBytes: opts.size,
		ChecksumSHA256: hex.EncodeToString(sum[:]), Status: model.AssetStatusAvailable,
		PublishedAt: &published, CreatedAt: published, UpdatedAt: published,
	}
	if opts.storage == nil {
		asset.StorageETag = memStorage.ETag("assets/tenant-1/a1")
	}
	if err := assets.Create(context.Background(), &asset); err != nil {
		t.Fatal(err)
	}

	logs := &syncBuffer{}
	log := slog.New(slog.NewJSONHandler(logs, nil))
	audit := memory.NewDeliveryAuditRepository()
	registry := metrics.New()
	svc, err := delivery.NewService(assets, storage, audit, memory.NewDownloadTokenRepository(), delivery.Config{
		Bucket: "private-bucket", Prefix: "assets", DefaultTTL: 5 * time.Minute, MaxTTL: 15 * time.Minute, Metrics: registry,
	}, log)
	if err != nil {
		t.Fatal(err)
	}
	stall := opts.stall
	if stall == 0 {
		stall = 5 * time.Second
	}
	return streamAPI{
		handler: server.NewRouter(server.Routes{
			Resolver: testIdentity, Delivery: svc, ProxyEnabled: true, Log: log,
			Stream: httpapi.StreamConfig{StallTimeout: stall, MaxStreams: opts.maxStreams, Metrics: registry},
		}),
		storage: memStorage, audit: audit, registry: registry, logs: logs,
		etag: `"` + asset.ChecksumSHA256 + `"`,
	}
}

func (a streamAPI) request(method, path string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("Authorization", "Bearer tenant-1/user-1")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	a.handler.ServeHTTP(rec, req)
	return rec
}

func TestHeadAnswersWithoutReadingTheObject(t *testing.T) {
	api := newStreamAPI(t, streamOptions{})
	rec := api.request(http.MethodHead, "/api/v1/assets/a1/content", map[string]string{"Range": "bytes=5-9"})

	if rec.Code != http.StatusPartialContent || rec.Header().Get("Content-Length") != "5" ||
		rec.Header().Get("Content-Range") != "bytes 5-9/20" || rec.Body.Len() != 0 {
		t.Fatalf("HEAD: %d %v %q", rec.Code, rec.Header(), rec.Body)
	}
	if api.storage.RangeCalls != 0 {
		t.Fatal("HEAD must not stream the object")
	}
}

func TestValidatorAndConditionalHeaders(t *testing.T) {
	api := newStreamAPI(t, streamOptions{})
	full := api.request(http.MethodGet, "/api/v1/assets/a1/content", nil)
	if full.Header().Get("ETag") != api.etag || full.Header().Get("Last-Modified") != "Fri, 16 Oct 2026 12:00:00 GMT" {
		t.Fatalf("validators: %v", full.Header())
	}

	if rec := api.request(http.MethodGet, "/api/v1/assets/a1/content", map[string]string{"If-None-Match": api.etag}); rec.Code != http.StatusNotModified || rec.Body.Len() != 0 {
		t.Fatalf("If-None-Match: %d", rec.Code)
	}
	if rec := api.request(http.MethodGet, "/api/v1/assets/a1/content", map[string]string{"If-Match": `"stale"`}); rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("If-Match: %d", rec.Code)
	}

	// Resuming with the right validator continues; with a stale one the
	// whole object comes back, so bytes from two versions are never joined.
	resumed := api.request(http.MethodGet, "/api/v1/assets/a1/content", map[string]string{"Range": "bytes=10-", "If-Range": api.etag})
	if resumed.Code != http.StatusPartialContent || resumed.Body.String() != "abcdefghij" {
		t.Fatalf("resume: %d %q", resumed.Code, resumed.Body)
	}
	stale := api.request(http.MethodGet, "/api/v1/assets/a1/content", map[string]string{"Range": "bytes=10-", "If-Range": `"stale"`})
	if stale.Code != http.StatusOK || stale.Body.String() != string(deliveredBytes) {
		t.Fatalf("stale resume: %d %q", stale.Code, stale.Body)
	}
}

func TestUnsafeTypesAreServedAsDownloads(t *testing.T) {
	api := newStreamAPI(t, streamOptions{contentType: "text/html; charset=utf-8"})
	rec := api.request(http.MethodGet, "/api/v1/assets/a1/content?filename=x%0d%0aSet-Cookie:%20a=1.html", nil)

	if rec.Header().Get("Content-Type") != "application/octet-stream" || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("HTML must not be served as HTML: %v", rec.Header())
	}
	disposition := rec.Header().Get("Content-Disposition")
	if !strings.HasPrefix(disposition, "attachment;") || strings.ContainsAny(disposition, "\r\n") || rec.Header().Get("Set-Cookie") != "" {
		t.Fatalf("unsafe disposition %q", disposition)
	}
	if rec.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatalf("private content must not be cached: %q", rec.Header().Get("Cache-Control"))
	}
}

func TestRequestIDsAreEchoedAndSanitized(t *testing.T) {
	api := newStreamAPI(t, streamOptions{})
	rec := api.request(http.MethodGet, "/api/v1/assets/a1/content", map[string]string{"X-Request-ID": "client-42"})
	if rec.Header().Get("X-Request-ID") != "client-42" {
		t.Fatalf("request ID not echoed: %q", rec.Header().Get("X-Request-ID"))
	}
	if event := api.audit.Events()[0]; event.RequestID != "client-42" {
		t.Fatalf("request ID not audited: %+v", event)
	}

	rec = api.request(http.MethodGet, "/api/v1/assets/a1/content", map[string]string{"X-Request-ID": "bad id\r\nX: y"})
	if got := rec.Header().Get("X-Request-ID"); got == "" || strings.ContainsAny(got, " \r\n:") {
		t.Fatalf("an unsafe request ID must be replaced, got %q", got)
	}
}

func TestTokenLinkFlowOverHTTP(t *testing.T) {
	api := newStreamAPI(t, streamOptions{})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/assets/a1/download-url", strings.NewReader(`{"mode":"token","expires_in_seconds":60}`))
	req.Header.Set("Authorization", "Bearer tenant-1/user-1")
	created := httptest.NewRecorder()
	api.handler.ServeHTTP(created, req)

	var body struct {
		URL       string `json:"url"`
		Delivery  string `json:"delivery"`
		Revocable bool   `json:"revocable"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &body); err != nil || body.Delivery != "token" || !body.Revocable {
		t.Fatalf("create token link: %d %q", created.Code, created.Body)
	}

	// The link needs no Authorization header: the token is the credential.
	get := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		api.handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, body.URL, nil))
		return rec
	}
	if rec := get(); rec.Code != http.StatusOK || rec.Body.String() != string(deliveredBytes) {
		t.Fatalf("token download: %d", rec.Code)
	}

	revoked := api.request(http.MethodDelete, "/api/v1/assets/a1/download-links", nil)
	if revoked.Code != http.StatusOK || !strings.Contains(revoked.Body.String(), `"revoked":1`) {
		t.Fatalf("revoke: %d %q", revoked.Code, revoked.Body)
	}
	if rec := get(); rec.Code != http.StatusForbidden {
		t.Fatalf("a revoked link must be refused, got %d", rec.Code)
	}

	unknown := httptest.NewRecorder()
	api.handler.ServeHTTP(unknown, httptest.NewRequest(http.MethodGet, "/api/v1/downloads/"+strings.Repeat("A", 43), nil))
	if unknown.Code != http.StatusForbidden {
		t.Fatalf("unknown token: %d", unknown.Code)
	}

	if strings.Contains(api.logs.String(), strings.TrimPrefix(body.URL, delivery.TokenPathPrefix)) {
		t.Fatal("the token must never be logged")
	}
}

func TestPresignedURLIsNeverLogged(t *testing.T) {
	api := newStreamAPI(t, streamOptions{})
	rec := api.request(http.MethodPost, "/api/v1/assets/a1/download-url", nil)
	var body struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.URL == "" {
		t.Fatalf("create URL: %d %q", rec.Code, rec.Body)
	}
	logs := api.logs.String()
	if strings.Contains(logs, body.URL) || strings.Contains(logs, "X-Amz-Signature") || !strings.Contains(logs, "download URL issued") {
		t.Fatalf("the URL leaked into the logs: %s", logs)
	}
	for _, event := range api.audit.Events() {
		if strings.Contains(event.Reason, "Signature") {
			t.Fatal("the URL leaked into the audit")
		}
	}
}

// blockingBody returns its first chunk, then blocks until its context ends,
// like a provider stream that stops sending.
type blockingBody struct {
	ctx  context.Context
	sent bool
}

func (b *blockingBody) Read(p []byte) (int, error) {
	if !b.sent {
		b.sent = true
		return copy(p, "0123"), nil
	}
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

func (b *blockingBody) Close() error { return nil }

func TestStalledUpstreamIsCutOffAndAudited(t *testing.T) {
	api := newStreamAPI(t, streamOptions{stall: 50 * time.Millisecond})
	api.storage.WrapRange = func(ctx context.Context, _ io.ReadCloser) io.ReadCloser { return &blockingBody{ctx: ctx} }

	done := make(chan *httptest.ResponseRecorder)
	go func() { done <- api.request(http.MethodGet, "/api/v1/assets/a1/content", nil) }()
	select {
	case rec := <-done:
		if rec.Code != http.StatusOK || rec.Body.String() != "0123" {
			t.Fatalf("expected the headers and the first chunk, got %d %q", rec.Code, rec.Body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a stalled download must not hang")
	}

	events := api.audit.Events()
	last := events[len(events)-1]
	if last.Outcome != model.DeliveryFailed || last.Reason != "stalled" || last.BytesSent != 4 {
		t.Fatalf("expected a stalled transfer audit, got %+v", last)
	}
	if api.registry.Snapshot()["downloads_interrupted"] != 1 || api.registry.Snapshot()["streams_in_flight"] != 0 {
		t.Fatalf("metrics %v", api.registry.Snapshot())
	}
}

// cancelOnWrite cancels the request after the first body write, as a
// client that disconnects mid-download.
type cancelOnWrite struct {
	*httptest.ResponseRecorder
	cancel context.CancelFunc
}

func (c cancelOnWrite) Write(p []byte) (int, error) {
	n, err := c.ResponseRecorder.Write(p)
	c.cancel()
	return n, err
}

func TestClientDisconnectEndsTheStream(t *testing.T) {
	api := newStreamAPI(t, streamOptions{})
	api.storage.WrapRange = func(ctx context.Context, _ io.ReadCloser) io.ReadCloser { return &blockingBody{ctx: ctx} }

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/api/v1/assets/a1/content", nil).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer tenant-1/user-1")
	rec := cancelOnWrite{httptest.NewRecorder(), cancel}

	finished := make(chan struct{})
	go func() {
		api.handler.ServeHTTP(rec, req)
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("the handler must return when the client leaves")
	}
	events := api.audit.Events()
	if last := events[len(events)-1]; last.Reason != "client_disconnected" {
		t.Fatalf("expected client_disconnected, got %+v", last)
	}
}

func TestConcurrentStreamsAreCapped(t *testing.T) {
	api := newStreamAPI(t, streamOptions{maxStreams: 1, stall: 2 * time.Second})
	release := make(chan struct{})
	api.storage.WrapRange = func(ctx context.Context, body io.ReadCloser) io.ReadCloser {
		<-release
		return body
	}

	first := make(chan int)
	go func() { first <- api.request(http.MethodGet, "/api/v1/assets/a1/content", nil).Code }()
	// Wait until the first download holds the only slot.
	deadline := time.Now().Add(2 * time.Second)
	for api.registry.Snapshot()["streams_in_flight"] != 1 && time.Now().Before(deadline) {
		runtime.Gosched()
	}

	busy := api.request(http.MethodGet, "/api/v1/assets/a1/content", nil)
	if busy.Code != http.StatusServiceUnavailable || busy.Header().Get("Retry-After") == "" {
		t.Fatalf("expected 503 with Retry-After, got %d", busy.Code)
	}
	close(release)
	if code := <-first; code != http.StatusOK {
		t.Fatalf("the first download must finish, got %d", code)
	}
}

// largeStorage serves a large object of zeros without holding it in memory.
type largeStorage struct{ size int64 }

func (l largeStorage) PresignGet(context.Context, string, string, time.Duration, interfaces.DownloadOptions) (string, error) {
	return "", errors.New("not used")
}

func (l largeStorage) HeadObject(context.Context, string, string) (interfaces.ObjectMetadata, error) {
	return interfaces.ObjectMetadata{SizeBytes: l.size, ETag: `"large"`}, nil
}

func (l largeStorage) OpenRange(_ context.Context, _, _ string, first, last int64, _ interfaces.ReadCondition) (io.ReadCloser, error) {
	if last < 0 {
		last = l.size - 1
	}
	return io.NopCloser(io.LimitReader(zeros{}, last-first+1)), nil
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// countingWriter is a ResponseWriter that only counts body bytes.
type countingWriter struct {
	header http.Header
	status int
	n      int64
}

func (c *countingWriter) Header() http.Header         { return c.header }
func (c *countingWriter) WriteHeader(status int)      { c.status = status }
func (c *countingWriter) Write(p []byte) (int, error) { c.n += int64(len(p)); return len(p), nil }

func TestLargeDownloadsUseBoundedMemory(t *testing.T) {
	const size = 256 << 20 // 256 MiB
	api := newStreamAPI(t, streamOptions{storage: largeStorage{size: size}, size: size})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/assets/a1/content", nil)
	req.Header.Set("Authorization", "Bearer tenant-1/user-1")
	w := &countingWriter{header: http.Header{}}

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	api.handler.ServeHTTP(w, req)
	runtime.ReadMemStats(&after)

	if w.status != http.StatusOK || w.n != size {
		t.Fatalf("streamed %d bytes with status %d", w.n, w.status)
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 8<<20 {
		t.Fatalf("serving %d MiB allocated %d MiB; memory must not grow with object size", size>>20, allocated>>20)
	}
}

func TestClientThatStopsReadingIsCutOff(t *testing.T) {
	const size = 512 << 20 // far more than the socket buffers hold
	api := newStreamAPI(t, streamOptions{storage: largeStorage{size: size}, size: size, stall: 200 * time.Millisecond})
	srv := httptest.NewServer(api.handler)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/assets/a1/content", nil)
	req.Header.Set("Authorization", "Bearer tenant-1/user-1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	// Read nothing: the server's writes block once the buffers are full.

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		events := api.audit.Events()
		if last := events[len(events)-1]; last.Outcome == model.DeliveryFailed {
			if last.Reason != "client_stalled" || last.BytesSent <= 0 || last.BytesSent >= size {
				t.Fatalf("unexpected failure audit %+v", last)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("a client that stops reading must be cut off after the stall timeout")
}
