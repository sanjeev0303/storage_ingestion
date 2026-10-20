package unit

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	storagememory "github.com/sanjeev0303/object-storage-platform/internal/infrastructure/storage/memory"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/retry"
	"github.com/sanjeev0303/object-storage-platform/internal/repository/memory"
	"github.com/sanjeev0303/object-storage-platform/internal/server"
	assetservice "github.com/sanjeev0303/object-storage-platform/internal/service/asset"
	"github.com/sanjeev0303/object-storage-platform/internal/service/delivery"
)

type lifecycleAPI struct {
	handler   http.Handler
	lifecycle *assetservice.Service
	holds     *memory.LegalHoldRepository
}

func newLifecycleAPI(t *testing.T) lifecycleAPI {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	assets := memory.NewAssetRepository()
	uploads := memory.NewUploadRepository()
	jobs := memory.NewProcessingJobRepository(uploads, assets)
	tokens := memory.NewDownloadTokenRepository()
	quarantine := memory.NewQuarantineRepository()
	holds := memory.NewLegalHoldRepository()
	events := memory.NewLifecycleEventRepository()
	storage := storagememory.NewObjectStorage()

	storage.PutObject("assets/tenant-1/a1", deliveredBytes)
	now := time.Now().UTC()
	for id, status := range map[string]model.AssetStatus{"a1": model.AssetStatusAvailable, "p1": model.AssetStatusProcessing} {
		key := "assets/tenant-1/" + id
		if status != model.AssetStatusAvailable {
			key = "quarantine/tenant-1/up-" + id
		}
		asset := model.Asset{
			ID: id, TenantID: "tenant-1", OwnerID: "user-1", UploadID: "up-" + id, ObjectKey: key,
			FileName: "report.pdf", ContentType: "application/pdf", SizeBytes: int64(len(deliveredBytes)),
			Status: status, CreatedAt: now, UpdatedAt: now,
		}
		if err := assets.Create(context.Background(), &asset); err != nil {
			t.Fatal(err)
		}
	}
	jobs.Put(model.ProcessingJob{ID: "job-p1", TenantID: "tenant-1", AssetID: "p1", State: model.ProcessingPending})

	deliverer, err := delivery.NewService(assets, storage, memory.NewDeliveryAuditRepository(), tokens, delivery.Config{
		Bucket: "private-bucket", Prefix: "assets", DefaultTTL: time.Minute, MaxTTL: time.Minute,
	}, log)
	if err != nil {
		t.Fatal(err)
	}
	// No grace, so a deletion is due at once and the worker can be run here.
	lifecycle, err := assetservice.NewService(assetservice.Dependencies{
		Assets: assets, Uploads: uploads, Jobs: jobs, Quarantine: quarantine,
		Policies: memory.NewRetentionPolicyRepository(), Holds: holds,
		Deletions: memory.NewDeletionRepository(assets, tokens, uploads, jobs, quarantine, holds, events),
		Events:    events, Cursors: memory.NewCursorRepository(), Storage: storage,
	}, assetservice.Config{
		Owner: "worker", QuarantineBucket: "private-bucket", DeliveryBucket: "private-bucket", DeliveryPrefix: "assets",
		Platform:  assetservice.PlatformRetention{DefaultQuarantineRetention: time.Hour, QuarantineRetentionFloor: time.Hour},
		BatchSize: 10, Concurrency: 1, Lease: time.Minute, RecheckInterval: time.Hour, MaxDeletionsPerRun: 10,
		Retry: retry.Policy{BaseDelay: time.Second, MaxDelay: time.Minute, MaxAttempts: 3},
	}, log)
	if err != nil {
		t.Fatal(err)
	}
	return lifecycleAPI{
		handler: server.NewRouter(server.Routes{
			Resolver: testIdentity, Delivery: deliverer, ProxyEnabled: true, Lifecycle: lifecycle, Log: log,
		}),
		lifecycle: lifecycle,
		holds:     holds,
	}
}

func (a lifecycleAPI) do(method, path, as string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	if as != "" {
		req.Header.Set("Authorization", "Bearer "+as)
	}
	rec := httptest.NewRecorder()
	a.handler.ServeHTTP(rec, req)
	return rec
}

func TestDeleteRestoreOverHTTP(t *testing.T) {
	api := newLifecycleAPI(t)
	const me = "tenant-1/user-1"

	deleted := api.do(http.MethodDelete, "/api/v1/assets/a1", me)
	var body struct {
		Status   string `json:"status"`
		Created  bool   `json:"created"`
		Deletion struct {
			ID         string `json:"id"`
			State      string `json:"state"`
			Restorable bool   `json:"restorable"`
		} `json:"deletion"`
	}
	if err := json.Unmarshal(deleted.Body.Bytes(), &body); err != nil || deleted.Code != http.StatusAccepted ||
		body.Status != "SOFT_DELETED" || !body.Created || body.Deletion.State != "SCHEDULED" || !body.Deletion.Restorable {
		t.Fatalf("delete: %d %s", deleted.Code, deleted.Body)
	}
	if again := api.do(http.MethodDelete, "/api/v1/assets/a1", me); again.Code != http.StatusAccepted || !strings.Contains(again.Body.String(), body.Deletion.ID) ||
		!strings.Contains(again.Body.String(), `"created":false`) {
		t.Fatalf("a repeated delete must return the same deletion: %s", again.Body)
	}

	// Delivery stops at once; the owner still sees the asset and its state.
	if rec := api.do(http.MethodGet, "/api/v1/assets/a1/content", me); rec.Code != http.StatusConflict {
		t.Fatalf("a soft-deleted asset must not be delivered, got %d", rec.Code)
	}
	if rec := api.do(http.MethodGet, "/api/v1/assets/a1", me); !strings.Contains(rec.Body.String(), `"status":"SOFT_DELETED"`) ||
		!strings.Contains(rec.Body.String(), `"deliverable":false`) {
		t.Fatalf("metadata: %s", rec.Body)
	}

	restored := api.do(http.MethodPost, "/api/v1/assets/a1/restore", me)
	if restored.Code != http.StatusOK || !strings.Contains(restored.Body.String(), `"status":"AVAILABLE"`) {
		t.Fatalf("restore: %d %s", restored.Code, restored.Body)
	}
	if rec := api.do(http.MethodGet, "/api/v1/assets/a1/content", me); rec.Code != http.StatusOK || rec.Body.String() != string(deliveredBytes) {
		t.Fatalf("a restored asset must be delivered again, got %d", rec.Code)
	}
	if rec := api.do(http.MethodPost, "/api/v1/assets/a1/restore", me); rec.Code != http.StatusConflict {
		t.Fatalf("restoring twice must be refused, got %d", rec.Code)
	}
}

func TestLifecycleRoutesAreOwnerOnly(t *testing.T) {
	api := newLifecycleAPI(t)
	for _, c := range []struct{ method, path string }{
		{http.MethodDelete, "/api/v1/assets/a1"},
		{http.MethodPost, "/api/v1/assets/a1/restore"},
		{http.MethodGet, "/api/v1/assets/a1/lifecycle"},
	} {
		if rec := api.do(c.method, c.path, ""); rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s without credentials: %d", c.method, c.path, rec.Code)
		}
		if rec := api.do(c.method, c.path, "tenant-2/user-1"); rec.Code != http.StatusNotFound {
			t.Fatalf("%s %s from another tenant: %d", c.method, c.path, rec.Code)
		}
	}
	if rec := api.do(http.MethodDelete, "/api/v1/assets/p1", "tenant-1/user-1"); rec.Code != http.StatusConflict {
		t.Fatalf("an asset in active processing must not be deleted, got %d", rec.Code)
	}
}

func TestLifecycleViewHidesLegalHolds(t *testing.T) {
	api := newLifecycleAPI(t)
	if rec := api.do(http.MethodDelete, "/api/v1/assets/a1", "tenant-1/user-1"); rec.Code != http.StatusAccepted {
		t.Fatalf("delete: %d", rec.Code)
	}
	hold, err := api.lifecycle.PlaceHold(context.Background(), model.LegalHold{
		TenantID: "tenant-1", Scope: model.ScopeAsset, ScopeKey: "a1", Reason: "confidential case 77", PlacedBy: "legal",
	})
	if err != nil {
		t.Fatal(err)
	}
	var stats assetservice.CycleStats
	if err := api.lifecycle.ProcessDeletions(context.Background(), &stats); err != nil || stats.Protected != 1 {
		t.Fatalf("the hold must protect the deletion: %+v %v", stats, err)
	}

	rec := api.do(http.MethodGet, "/api/v1/assets/a1/lifecycle", "tenant-1/user-1")
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, `"state":"SCHEDULED"`) {
		t.Fatalf("lifecycle: %d %s", rec.Code, body)
	}
	if strings.Contains(body, hold.ID) || strings.Contains(body, "case 77") || strings.Contains(body, "PROTECTED") || strings.Contains(strings.ToLower(body), "hold") {
		t.Fatalf("an owner must not learn about a legal hold: %s", body)
	}
}
