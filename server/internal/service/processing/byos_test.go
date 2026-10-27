package processing

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	storagememory "github.com/sanjeev0303/object-storage-platform/internal/infrastructure/storage/memory"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/repository/memory"
	"github.com/sanjeev0303/object-storage-platform/internal/service/promotion"
	uploadservice "github.com/sanjeev0303/object-storage-platform/internal/service/upload"
)

// router resolves one tenant connection to an in-memory provider.
type router struct {
	customer *storagememory.ObjectStorage
	located  []string
	down     bool
}

func (r *router) Locate(_ context.Context, tenantID, connectionID string) (interfaces.StorageLocation, error) {
	r.located = append(r.located, connectionID)
	if r.down {
		return interfaces.StorageLocation{}, errors.New("provider unreachable")
	}
	if tenantID != tenant || connectionID != "sc_customer" {
		return interfaces.StorageLocation{}, model.ErrConnectionNotFound
	}
	return interfaces.StorageLocation{
		StoragePlace: interfaces.StoragePlace{ConnectionID: connectionID, StagingBucket: "customer", StagingPrefix: "staging",
			ReadyBucket: "customer", ReadyPrefix: "published"},
		Provider: model.ProviderS3Compatible, Storage: r.customer,
		Capabilities: model.ProviderCapabilities{PresignedUpload: true, Multipart: true, ServerSideCopy: true},
	}, nil
}

// byosService is the harness's processing service with promotion and
// verification routed through the router.
func (h *harness) byosService(t *testing.T, r *router) *Service {
	t.Helper()
	multipart, err := uploadservice.NewMultipartService(h.uploads, memory.NewMultipartUploadRepository(), h.storage,
		uploadservice.MultipartServiceConfig{Bucket: "bucket", MaxSize: 1 << 30, SessionTTL: time.Hour, PartURLTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	multipart.Route(nil, r)
	promoter, err := promotion.NewService(h.assets, h.storage, promotion.Config{SourceBucket: "bucket", DestinationBucket: "bucket", Prefix: "assets"})
	if err != nil {
		t.Fatal(err)
	}
	promoter.Route(r)
	svc := h.service(t, "worker-1")
	svc.deps.Verifier, svc.deps.Promoter, svc.deps.Router = multipart, promoter, r
	return svc
}

// boundUpload records a completed upload bound to connections, with its
// bytes stored where its staging binding says.
func (h *harness) boundUpload(t *testing.T, id, staging, final string, at *storagememory.ObjectStorage, key string) {
	t.Helper()
	at.PutObject(key, pdfBytes)
	session := model.UploadSession{
		ID: id, TenantID: tenant, OwnerID: "user-1", ObjectKey: key, FileName: "file.pdf", ContentType: "application/pdf",
		ExpectedSize: int64(len(pdfBytes)), ChecksumSHA256: sum(pdfBytes), Status: model.UploadStatusCompleted,
		StorageUploadID: "provider-" + id, CreatedAt: h.clock, UpdatedAt: h.clock, ExpiresAt: h.clock.Add(time.Hour),
		StorageConnectionID: final, StagingConnectionID: staging,
	}
	if err := h.uploads.Create(context.Background(), &session); err != nil {
		t.Fatal(err)
	}
}

func TestUploadOnATenantConnectionIsProcessedAndPromotedThere(t *testing.T) {
	h := newHarness()
	r := &router{customer: storagememory.NewObjectStorage()}
	svc := h.byosService(t, r)
	h.boundUpload(t, "up-1", "sc_customer", "sc_customer", r.customer, "staging/"+tenant+"/up-1")

	if _, err := svc.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	asset := h.onlyAsset(t)
	if asset.Status != model.AssetStatusAvailable || asset.ObjectKey != "published/"+tenant+"/"+asset.ID {
		t.Fatalf("promoted under the connection's ready prefix: %+v", asset)
	}
	if got, ok := r.customer.Object(asset.ObjectKey); !ok || !bytes.Equal(got, pdfBytes) {
		t.Fatal("the published bytes are on the tenant's connection")
	}
	if _, ok := r.customer.Object("staging/" + tenant + "/up-1"); ok {
		t.Fatal("the staged upload is removed after promotion")
	}
	if r.customer.CopyCalls != 1 || r.customer.WriteCalls != 0 {
		t.Fatalf("one connection: a copy inside the provider, copies %d writes %d", r.customer.CopyCalls, r.customer.WriteCalls)
	}
	if page, _ := h.storage.ListObjectsPage(context.Background(), "bucket", "", "", 100); len(page.Objects) != 0 {
		t.Fatalf("nothing touches the platform's bucket: %+v", page.Objects)
	}
}

func TestPlatformStagedUploadIsStreamedToTheTenantConnection(t *testing.T) {
	h := newHarness()
	r := &router{customer: storagememory.NewObjectStorage()}
	svc := h.byosService(t, r)
	// Staged on the platform, published on the tenant's connection: only
	// verified, scanned bytes ever reach the customer's bucket.
	h.boundUpload(t, "up-2", model.PlatformConnectionID, "sc_customer", h.storage, "quarantine/"+tenant+"/up-2")

	if _, err := svc.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	asset := h.onlyAsset(t)
	if asset.Status != model.AssetStatusAvailable || asset.StorageConnectionID != "sc_customer" {
		t.Fatalf("published on the tenant connection: %+v", asset)
	}
	if got, ok := r.customer.Object(asset.ObjectKey); !ok || !bytes.Equal(got, pdfBytes) {
		t.Fatal("the verified bytes were streamed to the tenant's bucket")
	}
	if r.customer.WriteCalls != 1 || r.customer.CopyCalls != 0 {
		t.Fatalf("across connections the bytes are streamed: writes %d copies %d", r.customer.WriteCalls, r.customer.CopyCalls)
	}
	if _, ok := h.storage.Object("quarantine/" + tenant + "/up-2"); ok {
		t.Fatal("the platform's staged copy is removed after promotion")
	}
}

func TestRejectedUploadNeverReachesTheTenantBucket(t *testing.T) {
	h := newHarness()
	r := &router{customer: storagememory.NewObjectStorage()}
	svc := h.byosService(t, r)
	h.storage.PutObject("quarantine/"+tenant+"/up-3", exeBytes)
	session := model.UploadSession{
		ID: "up-3", TenantID: tenant, OwnerID: "user-1", ObjectKey: "quarantine/" + tenant + "/up-3", FileName: "x.pdf",
		ContentType: "application/pdf", ExpectedSize: int64(len(exeBytes)), ChecksumSHA256: sum(exeBytes),
		Status: model.UploadStatusCompleted, StorageUploadID: "p", CreatedAt: h.clock, UpdatedAt: h.clock, ExpiresAt: h.clock.Add(time.Hour),
		StorageConnectionID: "sc_customer", StagingConnectionID: model.PlatformConnectionID,
	}
	if err := h.uploads.Create(context.Background(), &session); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if asset := h.onlyAsset(t); asset.Status != model.AssetStatusRejected {
		t.Fatalf("a disguised executable is rejected: %+v", asset)
	}
	if r.customer.WriteCalls != 0 {
		t.Fatal("unverified content must never be written to the tenant's bucket")
	}
}

func TestUnreachableConnectionRetriesWithoutSwitchingStorage(t *testing.T) {
	h := newHarness()
	r := &router{customer: storagememory.NewObjectStorage(), down: true}
	svc := h.byosService(t, r)
	h.boundUpload(t, "up-4", "sc_customer", "sc_customer", r.customer, "staging/"+tenant+"/up-4")

	stats, err := svc.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Retried != 1 || stats.Succeeded != 0 {
		t.Fatalf("an outage is retried later: %+v", stats)
	}
	if asset := h.onlyAsset(t); asset.Status != model.AssetStatusQuarantined {
		t.Fatalf("the asset waits: %+v", asset)
	}
	if page, _ := h.storage.ListObjectsPage(context.Background(), "bucket", "", "", 100); len(page.Objects) != 0 {
		t.Fatal("nothing falls back to the platform's bucket")
	}

	// The outage ends: the same job finishes on the same connection.
	r.down = false
	h.clock = h.clock.Add(time.Hour)
	if _, err := svc.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if asset := h.onlyAsset(t); asset.Status != model.AssetStatusAvailable {
		t.Fatalf("finished after the outage: %+v", asset)
	}
}

// One tenant's provider being down holds back only that tenant's upload: in
// the same cycle another tenant's upload, on the platform's storage, is
// processed as usual.
func TestOneTenantsOutageDoesNotHoldBackAnother(t *testing.T) {
	h := newHarness()
	r := &router{customer: storagememory.NewObjectStorage(), down: true}
	svc := h.byosService(t, r)
	h.boundUpload(t, "up-down", "sc_customer", "sc_customer", r.customer, "staging/"+tenant+"/up-down")

	const other = "tenant-2"
	key := "quarantine/" + other + "/up-other"
	h.storage.PutObject(key, pdfBytes)
	session := model.UploadSession{
		ID: "up-other", TenantID: other, OwnerID: "user-2", ObjectKey: key, FileName: "file.pdf", ContentType: "application/pdf",
		ExpectedSize: int64(len(pdfBytes)), ChecksumSHA256: sum(pdfBytes), Status: model.UploadStatusCompleted,
		StorageUploadID: "provider-up-other", CreatedAt: h.clock, UpdatedAt: h.clock, ExpiresAt: h.clock.Add(time.Hour),
	}
	if err := h.uploads.Create(context.Background(), &session); err != nil {
		t.Fatal(err)
	}

	stats, err := svc.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Retried != 1 || stats.Succeeded != 1 {
		t.Fatalf("the outage is retried and the other tenant's upload finishes: %+v", stats)
	}
	for _, job := range h.memJobs.Jobs() {
		asset, err := h.assets.GetByID(context.Background(), job.TenantID, job.AssetID)
		if err != nil {
			t.Fatal(err)
		}
		want := model.AssetStatusAvailable
		if job.TenantID == tenant {
			want = model.AssetStatusQuarantined
		}
		if asset.Status != want {
			t.Fatalf("%s's asset is %s, want %s", job.TenantID, asset.Status, want)
		}
	}
}
