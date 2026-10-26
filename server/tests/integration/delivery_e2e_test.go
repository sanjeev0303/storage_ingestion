package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/server"
	"github.com/sanjeev0303/object-storage-platform/internal/server/middleware"
	"github.com/sanjeev0303/object-storage-platform/internal/service/delivery"
)

// Phase 5.5 against the local stack. Needs every migration (through 000015).

func TestProviderConditionalRangeReads(t *testing.T) {
	storage, bucket := openTestProvider(t)
	ctx := context.Background()
	key := fmt.Sprintf("assets/integration-test/conditional-%d", time.Now().UnixNano())
	t.Cleanup(func() { _ = storage.DeleteObject(context.Background(), bucket, key) })
	putObject(t, storage, bucket, key, []byte("0123456789abcdefghij"))

	meta, err := storage.HeadObject(ctx, bucket, key)
	if err != nil || meta.ETag == "" {
		t.Fatalf("head: %+v %v", meta, err)
	}
	read := func(first, last int64, cond interfaces.ReadCondition) (string, error) {
		body, err := storage.OpenRange(ctx, bucket, key, first, last, cond)
		if err != nil {
			return "", err
		}
		defer body.Close()
		b, err := io.ReadAll(body)
		return string(b), err
	}

	if got, err := read(5, 9, interfaces.ReadCondition{IfMatch: meta.ETag}); err != nil || got != "56789" {
		t.Fatalf("conditional range: %q %v", got, err)
	}
	if _, err := read(0, -1, interfaces.ReadCondition{IfMatch: `"0000000000000000000000000000000a"`}); !errors.Is(err, interfaces.ErrPreconditionFailed) {
		t.Fatalf("a wrong If-Match must be ErrPreconditionFailed, got %v", err)
	}
	if _, err := read(100, 200, interfaces.ReadCondition{}); !errors.Is(err, interfaces.ErrInvalidRange) {
		t.Fatalf("a range past the end must be ErrInvalidRange, got %v", err)
	}
	if _, err := storage.HeadObject(ctx, bucket, key+"-missing"); !errors.Is(err, interfaces.ErrObjectNotFound) {
		t.Fatalf("missing HEAD: %v", err)
	}
	if _, err := read(0, -1, interfaces.ReadCondition{}); err != nil {
		t.Fatal(err)
	}

	// Overwrite: the old validator no longer opens anything.
	putObject(t, storage, bucket, key, []byte("ABCDEFGHIJKLMNOPQRST"))
	if _, err := read(10, 19, interfaces.ReadCondition{IfMatch: meta.ETag}); !errors.Is(err, interfaces.ErrPreconditionFailed) {
		t.Fatalf("a replaced object must not be read under the old validator, got %v", err)
	}
}

func TestPostgresDeliveryRepositories(t *testing.T) {
	env := openProcessingEnv(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	env.session(t, "repo", model.UploadStatusVerified)
	asset := model.Asset{
		ID: env.tenant + "-asset", TenantID: env.tenant, OwnerID: "owner", UploadID: env.tenant + "-repo",
		ObjectKey: "quarantine/" + env.tenant + "/repo", FileName: "f.pdf", ContentType: "application/pdf",
		SizeBytes: 10, Status: model.AssetStatusQuarantined, CreatedAt: now, UpdatedAt: now,
	}
	if err := env.assets.Create(ctx, &asset); err != nil {
		t.Fatal(err)
	}

	if err := env.assets.RecordStorageVersion(ctx, env.tenant, asset.ID, `"e"`, "", now); !errors.Is(err, model.ErrInvalidAssetState) {
		t.Fatalf("validators are recorded only while PROCESSING, got %v", err)
	}
	if err := env.assets.TransitionStatus(ctx, env.tenant, asset.ID, []model.AssetStatus{model.AssetStatusQuarantined}, model.AssetStatusProcessing, "", "", now); err != nil {
		t.Fatal(err)
	}
	if err := env.assets.RecordStorageVersion(ctx, env.tenant, asset.ID, `"etag-1"`, "v1", now); err != nil {
		t.Fatal(err)
	}
	if err := env.assets.RecordStorageVersion(ctx, "other-tenant", asset.ID, `"x"`, "", now); !errors.Is(err, model.ErrAssetNotFound) {
		t.Fatalf("cross-tenant write must look missing, got %v", err)
	}
	if err := env.assets.TransitionStatus(ctx, env.tenant, asset.ID, []model.AssetStatus{model.AssetStatusProcessing}, model.AssetStatusAvailable, "", "assets/"+env.tenant+"/"+asset.ID, now); err != nil {
		t.Fatal(err)
	}
	got, err := env.assets.GetByID(ctx, env.tenant, asset.ID)
	if err != nil || got.StorageETag != `"etag-1"` || got.StorageVersionID != "v1" || got.PublishedAt == nil || !got.PublishedAt.Equal(now) {
		t.Fatalf("published version not stored: %+v %v", got, err)
	}

	tokens := postgres.NewDownloadTokenRepository(env.db)
	token := model.DownloadToken{
		ID: env.tenant + "-tok", TokenHash: "hash-" + env.tenant, TenantID: env.tenant, AssetID: asset.ID,
		PrincipalID: "owner", Filename: "f.pdf", CreatedAt: now, ExpiresAt: now.Add(time.Minute),
	}
	if err := tokens.Create(ctx, token); err != nil {
		t.Fatal(err)
	}
	if loaded, err := tokens.GetByHash(ctx, token.TokenHash); err != nil || loaded.AssetID != asset.ID || loaded.RevokedAt != nil {
		t.Fatalf("get token: %+v %v", loaded, err)
	}
	if _, err := tokens.GetByHash(ctx, "no-such-hash"); !errors.Is(err, model.ErrDownloadTokenNotFound) {
		t.Fatalf("unknown hash: %v", err)
	}
	if n, err := tokens.RevokeForAsset(ctx, "other-tenant", asset.ID, now); err != nil || n != 0 {
		t.Fatalf("another tenant must revoke nothing: %d %v", n, err)
	}
	if n, err := tokens.RevokeForAsset(ctx, env.tenant, asset.ID, now); err != nil || n != 1 {
		t.Fatalf("revoke: %d %v", n, err)
	}
	if loaded, _ := tokens.GetByHash(ctx, token.TokenHash); loaded.RevokedAt == nil {
		t.Fatal("token not revoked")
	}

	audit := postgres.NewDeliveryAuditRepository(env.db)
	if err := audit.Record(ctx, model.DeliveryAuditEvent{
		ID: env.tenant + "-ev", TenantID: env.tenant, PrincipalID: "owner", AssetID: asset.ID,
		Operation: model.DeliveryOpToken, Outcome: model.DeliveryFailed, Reason: "stalled",
		RequestID: "req-1", ByteRange: "0-9/10", BytesSent: 4, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = env.db.Pool.Exec(context.Background(), `DELETE FROM delivery_audit_events WHERE tenant_id = $1`, env.tenant)
	})
	var requestID, byteRange string
	var sent int64
	if err := env.db.Pool.QueryRow(ctx, `SELECT request_id, byte_range, bytes_sent FROM delivery_audit_events WHERE id = $1`,
		env.tenant+"-ev").Scan(&requestID, &byteRange, &sent); err != nil || requestID != "req-1" || byteRange != "0-9/10" || sent != 4 {
		t.Fatalf("audit columns: %q %q %d %v", requestID, byteRange, sent, err)
	}
}

// deliveryClient talks to the real delivery API, served over HTTP from
// Postgres and MinIO, as the development header identity.
type deliveryClient struct {
	t      *testing.T
	server *httptest.Server
	tenant string
}

func (c deliveryClient) do(method, path string, headers map[string]string, body string) (*http.Response, []byte) {
	c.t.Helper()
	req, err := http.NewRequest(method, c.server.URL+path, strings.NewReader(body))
	if err != nil {
		c.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+c.tenant)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		c.t.Fatal(err)
	}
	return resp, data
}

func TestLocalStackResumableDownloadPinnedToVersion(t *testing.T) {
	stack := openContentStack(t)
	first := append([]byte("%PDF-1.7\n"), bytes.Repeat([]byte("d"), 5*1024*1024)...)
	second := bytes.Repeat([]byte("tail"), 4096)
	whole := append(append([]byte{}, first...), second...)
	id := stack.upload(t, "big.pdf", "application/pdf", first, second)
	runCycle(t, stack.service(t, serviceOptions{}))

	asset, _ := stack.assetFor(t, id)
	if asset.Status != model.AssetStatusAvailable || asset.StorageETag == "" || asset.PublishedAt == nil {
		t.Fatalf("promotion must record the published version: %+v", asset)
	}

	svc, err := delivery.NewService(stack.env.assets, stack.provider, postgres.NewDeliveryAuditRepository(stack.env.db),
		postgres.NewDownloadTokenRepository(stack.env.db), delivery.Config{
			Bucket: stack.bucket, Prefix: promotionPrefix, DefaultTTL: time.Minute, MaxTTL: 5 * time.Minute,
		}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	// The bearer token is the tenant: a developer of it, for this test only.
	resolver := func(r *http.Request) (model.Principal, error) {
		return member(middleware.BearerToken(r), "owner", model.RoleDeveloper), nil
	}
	api := httptest.NewServer(server.NewRouter(server.Routes{
		Resolver: resolver, Delivery: svc, ProxyEnabled: true,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}))
	defer api.Close()
	t.Cleanup(func() {
		_, _ = stack.env.db.Pool.Exec(context.Background(), `DELETE FROM delivery_audit_events WHERE tenant_id = $1`, stack.env.tenant)
	})
	client := deliveryClient{t: t, server: api, tenant: stack.env.tenant}
	content := "/api/v1/assets/" + asset.ID + "/content"

	// Metadata, then a download interrupted after 1 MiB and resumed.
	resp, body := client.do(http.MethodGet, "/api/v1/assets/"+asset.ID, nil, "")
	var info struct {
		Deliverable bool   `json:"deliverable"`
		ETag        string `json:"etag"`
		SizeBytes   int64  `json:"size_bytes"`
	}
	if err := json.Unmarshal(body, &info); err != nil || !info.Deliverable || info.SizeBytes != int64(len(whole)) {
		t.Fatalf("metadata: %d %s", resp.StatusCode, body)
	}

	head, _ := client.do(http.MethodHead, content, nil, "")
	if head.StatusCode != http.StatusOK || head.ContentLength != int64(len(whole)) || head.Header.Get("ETag") != info.ETag {
		t.Fatalf("HEAD: %d %d %q", head.StatusCode, head.ContentLength, head.Header.Get("ETag"))
	}

	part1, data1 := client.do(http.MethodGet, content, map[string]string{"Range": "bytes=0-1048575"}, "")
	if part1.StatusCode != http.StatusPartialContent || len(data1) != 1<<20 {
		t.Fatalf("first part: %d %d", part1.StatusCode, len(data1))
	}
	part2, data2 := client.do(http.MethodGet, content, map[string]string{
		"Range": fmt.Sprintf("bytes=%d-", len(data1)), "If-Range": part1.Header.Get("ETag"),
	}, "")
	wantRange := fmt.Sprintf("bytes %d-%d/%d", len(data1), len(whole)-1, len(whole))
	if part2.StatusCode != http.StatusPartialContent || part2.Header.Get("Content-Range") != wantRange {
		t.Fatalf("resume: %d %q", part2.StatusCode, part2.Header.Get("Content-Range"))
	}
	joined := sha256.Sum256(append(data1, data2...))
	original := sha256.Sum256(whole)
	if hex.EncodeToString(joined[:]) != hex.EncodeToString(original[:]) || `"`+hex.EncodeToString(original[:])+`"` != info.ETag {
		t.Fatal("the resumed download must equal the uploaded bytes, and the ETag their SHA-256")
	}

	// Another tenant sees nothing.
	foreign := deliveryClient{t: t, server: api, tenant: "someone-else"}
	if resp, _ := foreign.do(http.MethodGet, content, nil, ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-tenant download: %d", resp.StatusCode)
	}

	// A presigned URL downloads straight from MinIO, ranges included.
	resp, body = client.do(http.MethodPost, "/api/v1/assets/"+asset.ID+"/download-url", nil, `{"expires_in_seconds":60}`)
	var link struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(body, &link); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("presign: %d", resp.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodGet, link.URL, nil)
	req.Header.Set("Range", "bytes=0-8")
	direct, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	directBody, _ := io.ReadAll(direct.Body)
	direct.Body.Close()
	if direct.StatusCode != http.StatusPartialContent || string(directBody) != "%PDF-1.7\n" {
		t.Fatalf("presigned range: %d %q", direct.StatusCode, directBody)
	}

	// A token link works without credentials until it is revoked.
	resp, body = client.do(http.MethodPost, "/api/v1/assets/"+asset.ID+"/download-url", nil, `{"mode":"token"}`)
	if err := json.Unmarshal(body, &link); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("token link: %d %s", resp.StatusCode, body)
	}
	tokenGet := func() int {
		resp, err := http.Get(api.URL + link.URL + "?filename=resumed.pdf")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := tokenGet(); code != http.StatusOK {
		t.Fatalf("token download: %d", code)
	}
	if resp, _ := client.do(http.MethodDelete, "/api/v1/assets/"+asset.ID+"/download-links", nil, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("revoke: %d", resp.StatusCode)
	}
	if code := tokenGet(); code != http.StatusForbidden {
		t.Fatalf("revoked token link: %d", code)
	}

	// The stored object is replaced out of band (same size). Neither a new
	// download nor a resume may serve its bytes under this asset.
	putObject(t, stack.provider, stack.bucket, asset.ObjectKey, bytes.Repeat([]byte("X"), len(whole)))
	if resp, _ := client.do(http.MethodGet, content, map[string]string{"X-Request-ID": "it-mismatch"}, ""); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("replaced object: %d", resp.StatusCode)
	}
	resumed, _ := client.do(http.MethodGet, content, map[string]string{"Range": "bytes=1048576-", "If-Range": info.ETag}, "")
	if resumed.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("resume over a replaced object: %d", resumed.StatusCode)
	}
	var reason string
	if err := stack.env.db.Pool.QueryRow(context.Background(),
		`SELECT reason FROM delivery_audit_events WHERE request_id = 'it-mismatch' AND outcome = 'FAILED'`).Scan(&reason); err != nil || reason != "version_mismatch" {
		t.Fatalf("the mismatch must be audited: %q %v", reason, err)
	}
}
