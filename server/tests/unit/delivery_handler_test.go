package unit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	storagememory "github.com/sanjeev0303/object-storage-platform/internal/infrastructure/storage/memory"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/repository/memory"
	"github.com/sanjeev0303/object-storage-platform/internal/server"
	"github.com/sanjeev0303/object-storage-platform/internal/server/middleware"
	"github.com/sanjeev0303/object-storage-platform/internal/service/delivery"
)

var deliveredBytes = []byte("0123456789abcdefghij")

// testIdentity authenticates "Bearer <tenant>/<user>" for these tests
// only, as a member with the built-in developer role.
func testIdentity(r *http.Request) (model.Principal, error) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	tenant, user, found := strings.Cut(token, "/")
	if !ok || !found {
		return model.Principal{}, errors.New("no credentials")
	}
	developer, _ := model.BuiltInRole(model.RoleDeveloper)
	return model.Principal{TenantID: tenant, UserID: user, RoleID: developer.ID, Permissions: developer.Permissions}, nil
}

type deliveryAPI struct {
	handler http.Handler
	storage *storagememory.ObjectStorage
}

func newDeliveryAPI(t *testing.T, identity middleware.Resolver, proxy bool) deliveryAPI {
	t.Helper()
	assets := memory.NewAssetRepository()
	storage := storagememory.NewObjectStorage()
	storage.PutObject("assets/tenant-1/a1", deliveredBytes)

	for id, status := range map[string]model.AssetStatus{"a1": model.AssetStatusAvailable, "p1": model.AssetStatusProcessing} {
		key := "assets/tenant-1/" + id
		if status != model.AssetStatusAvailable {
			key = "quarantine/tenant-1/up-" + id
		}
		asset := model.Asset{
			ID: id, TenantID: "tenant-1", OwnerID: "user-1", UploadID: "up-" + id, ObjectKey: key,
			FileName: "report.pdf", ContentType: "application/pdf", SizeBytes: int64(len(deliveredBytes)),
			Status: status, CreatedAt: time.Now(), UpdatedAt: time.Now(),
		}
		if err := assets.Create(context.Background(), &asset); err != nil {
			t.Fatal(err)
		}
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc, err := delivery.NewService(assets, storage, memory.NewDeliveryAuditRepository(), memory.NewDownloadTokenRepository(), delivery.Config{
		Bucket: "private-bucket", Prefix: "assets", DefaultTTL: 5 * time.Minute, MaxTTL: 15 * time.Minute,
	}, log)
	if err != nil {
		t.Fatal(err)
	}
	return deliveryAPI{
		handler: server.NewRouter(server.Routes{Resolver: identity, Delivery: svc, ProxyEnabled: proxy, Log: log}),
		storage: storage,
	}
}

func (a deliveryAPI) do(method, path, as string, body string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	if as != "" {
		req.Header.Set("Authorization", "Bearer "+as)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	a.handler.ServeHTTP(rec, req)
	return rec
}

func TestDeliveryRequiresAuthentication(t *testing.T) {
	for _, identity := range []middleware.Resolver{nil, testIdentity} {
		api := newDeliveryAPI(t, identity, true)
		for _, c := range []struct{ method, path string }{
			{http.MethodGet, "/api/v1/assets/a1"},
			{http.MethodPost, "/api/v1/assets/a1/download-url"},
			{http.MethodGet, "/api/v1/assets/a1/content"},
		} {
			if rec := api.do(c.method, c.path, "", "", nil); rec.Code != http.StatusUnauthorized {
				t.Fatalf("%s %s without credentials: %d", c.method, c.path, rec.Code)
			}
		}
		if len(api.storage.Presigns) != 0 {
			t.Fatal("no URL may be signed for an unauthenticated request")
		}
	}
}

func TestDeliveryHidesOtherTenantsAssets(t *testing.T) {
	api := newDeliveryAPI(t, testIdentity, true)

	foreign := api.do(http.MethodPost, "/api/v1/assets/a1/download-url", "tenant-2/user-1", "", nil)
	missing := api.do(http.MethodPost, "/api/v1/assets/nope/download-url", "tenant-1/user-1", "", nil)
	if foreign.Code != http.StatusNotFound || foreign.Body.String() != missing.Body.String() {
		t.Fatalf("another tenant's asset must look missing: %d %q vs %q", foreign.Code, foreign.Body, missing.Body)
	}
	if rec := api.do(http.MethodGet, "/api/v1/assets/p1/content", "tenant-1/user-1", "", nil); rec.Code != http.StatusConflict {
		t.Fatalf("own unavailable asset: %d", rec.Code)
	}
}

func TestDownloadURLResponse(t *testing.T) {
	api := newDeliveryAPI(t, testIdentity, true)

	rec := api.do(http.MethodPost, "/api/v1/assets/a1/download-url", "tenant-1/user-1",
		`{"expires_in_seconds": 86400, "filename": "../q3.pdf"}`, map[string]string{"Content-Type": "application/json"})
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status %d, cache %q", rec.Code, rec.Header().Get("Cache-Control"))
	}
	var body struct {
		URL       string    `json:"url"`
		ExpiresAt time.Time `json:"expires_at"`
		Delivery  string    `json:"delivery"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.URL == "" || body.Delivery != "storage" {
		t.Fatalf("body %q: %v", rec.Body, err)
	}
	if ttl := api.storage.Presigns[0].TTL; ttl != 15*time.Minute {
		t.Fatalf("a day was requested; the URL must be capped at 15m, got %s", ttl)
	}

	// Clients cannot pick a bucket or key.
	rec = api.do(http.MethodPost, "/api/v1/assets/a1/download-url", "tenant-1/user-1",
		`{"bucket": "other", "key": "assets/tenant-2/x"}`, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown fields must be refused, got %d", rec.Code)
	}
}

func TestProxiedDownloadHeadersAndRanges(t *testing.T) {
	api := newDeliveryAPI(t, testIdentity, true)
	get := func(rangeHeader string) *httptest.ResponseRecorder {
		h := map[string]string{}
		if rangeHeader != "" {
			h["Range"] = rangeHeader
		}
		return api.do(http.MethodGet, "/api/v1/assets/a1/content", "tenant-1/user-1", "", h)
	}

	full := get("")
	if full.Code != http.StatusOK || full.Body.String() != string(deliveredBytes) ||
		full.Header().Get("Content-Length") != "20" || full.Header().Get("Accept-Ranges") != "bytes" ||
		full.Header().Get("X-Content-Type-Options") != "nosniff" || full.Header().Get("Cache-Control") != "private, no-store" ||
		!strings.HasPrefix(full.Header().Get("Content-Disposition"), "attachment;") {
		t.Fatalf("full download: %d %v", full.Code, full.Header())
	}

	part := get("bytes=5-9")
	if part.Code != http.StatusPartialContent || part.Body.String() != "56789" ||
		part.Header().Get("Content-Range") != "bytes 5-9/20" || part.Header().Get("Content-Length") != "5" {
		t.Fatalf("range: %d %q %v", part.Code, part.Body, part.Header())
	}

	bad := get("bytes=40-")
	if bad.Code != http.StatusRequestedRangeNotSatisfiable || bad.Header().Get("Content-Range") != "bytes */20" {
		t.Fatalf("unsatisfiable: %d %v", bad.Code, bad.Header())
	}

	if multi := get("bytes=0-1,4-5"); multi.Code != http.StatusOK || multi.Body.Len() != 20 {
		t.Fatalf("multiple ranges must fall back to the whole object: %d", multi.Code)
	}
}

func TestStorageErrorsDoNotReachTheClient(t *testing.T) {
	api := newDeliveryAPI(t, testIdentity, true)
	api.storage.PresignErr = errors.New("presign download: provider error AccessDenied for bucket private-bucket")

	rec := api.do(http.MethodPost, "/api/v1/assets/a1/download-url", "tenant-1/user-1", "", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "AccessDenied") || strings.Contains(rec.Body.String(), "private-bucket") {
		t.Fatalf("provider details leaked: %q", rec.Body)
	}
}

func TestProxyEndpointCanBeDisabled(t *testing.T) {
	api := newDeliveryAPI(t, testIdentity, false)
	if rec := api.do(http.MethodGet, "/api/v1/assets/a1/content", "tenant-1/user-1", "", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("expected no proxy route, got %d", rec.Code)
	}
}
