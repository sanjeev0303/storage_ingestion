package delivery

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	storagememory "github.com/sanjeev0303/object-storage-platform/internal/infrastructure/storage/memory"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// tenantRouter resolves one tenant connection.
type tenantRouter struct {
	storage *storagememory.ObjectStorage
	presign bool
	down    bool
}

func (r *tenantRouter) Locate(_ context.Context, tenantID, connectionID string) (interfaces.StorageLocation, error) {
	if r.down {
		return interfaces.StorageLocation{}, errors.New("unreachable")
	}
	if tenantID != "tenant-1" || connectionID != "sc_customer" {
		return interfaces.StorageLocation{}, model.ErrConnectionNotFound
	}
	return interfaces.StorageLocation{
		StoragePlace: interfaces.StoragePlace{ConnectionID: connectionID, ReadyBucket: "customer-bucket", ReadyPrefix: "published"},
		Storage:      r.storage, Capabilities: model.ProviderCapabilities{PresignedDownload: r.presign, RangeReads: true},
	}, nil
}

// tenantAsset seeds an AVAILABLE asset published on the tenant connection.
func (e *env) tenantAsset(t *testing.T, r *tenantRouter, id string) {
	t.Helper()
	key := "published/tenant-1/" + id
	r.storage.PutObject(key, content)
	at := baseTime
	asset := model.Asset{
		ID: id, TenantID: "tenant-1", OwnerID: "user-1", UploadID: "up-" + id, ObjectKey: key,
		FileName: "report.pdf", ContentType: "application/pdf", SizeBytes: int64(len(content)),
		ChecksumSHA256: contentSum, StorageETag: r.storage.ETag(key), PublishedAt: &at,
		Status: model.AssetStatusAvailable, CreatedAt: baseTime, UpdatedAt: baseTime,
		StorageConnectionID: "sc_customer", StagingConnectionID: "sc_customer",
	}
	if err := e.assets.Create(context.Background(), &asset); err != nil {
		t.Fatal(err)
	}
}

func TestTenantAssetIsServedThroughItsOwnConnection(t *testing.T) {
	signer := &recordingSigner{}
	e := newEnv(t, &CDNConfig{BaseURL: "https://cdn.example.test", Signer: signer})
	r := &tenantRouter{storage: storagememory.NewObjectStorage(), presign: true}
	e.svc.Route(r)
	e.tenantAsset(t, r, "a1")
	e.asset(t, "a2", model.AssetStatusAvailable) // a platform asset beside it
	ctx := context.Background()

	got, err := e.svc.CreateDownloadURL(ctx, owner, "a1", URLRequest{})
	if err != nil {
		t.Fatal(err)
	}
	// The CDN fronts the platform's bucket only; the tenant's asset is
	// signed by its own provider.
	if got.Via != ModeStorage || !strings.Contains(got.URL, "customer-bucket/published/tenant-1/a1") || len(signer.signed) != 0 {
		t.Fatalf("signed by the tenant's storage, not the CDN: %+v", got)
	}
	if len(e.storage.Presigns) != 0 {
		t.Fatal("the platform's storage must not sign a tenant's asset")
	}
	if platform, err := e.svc.CreateDownloadURL(ctx, owner, "a2", URLRequest{}); err != nil || platform.Via != "cdn" {
		t.Fatalf("platform assets still go through the CDN: %+v %v", platform, err)
	}

	d, err := e.svc.Open(ctx, owner, "a1", DownloadRequest{Range: "bytes=2-5"})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(d.Body)
	d.Body.Close()
	if string(body) != "2345" || !d.Partial {
		t.Fatalf("range reads come from the tenant's connection: %q", body)
	}
}

func TestConnectionThatCannotSignServesATokenLink(t *testing.T) {
	e := newEnv(t, nil)
	r := &tenantRouter{storage: storagememory.NewObjectStorage(), presign: false}
	e.svc.Route(r)
	e.tenantAsset(t, r, "a1")

	got, err := e.svc.CreateDownloadURL(context.Background(), owner, "a1", URLRequest{Mode: ModeStorage})
	if err != nil {
		t.Fatal(err)
	}
	if got.Via != ModeToken || !got.Revocable || len(r.storage.Presigns) != 0 {
		t.Fatalf("a SAS-token connection streams through the API: %+v", got)
	}
}

func TestUnreachableConnectionFailsWithoutFallingBack(t *testing.T) {
	e := newEnv(t, nil)
	r := &tenantRouter{storage: storagememory.NewObjectStorage(), presign: true, down: true}
	e.svc.Route(r)
	e.tenantAsset(t, r, "a1")
	// The same key on the platform's bucket must never be served instead.
	e.storage.PutObject("published/tenant-1/a1", []byte("not this one"))

	if _, err := e.svc.CreateDownloadURL(context.Background(), owner, "a1", URLRequest{}); !errors.Is(err, ErrStorageUnavailable) {
		t.Fatalf("an outage answers unavailable: %v", err)
	}
	if _, err := e.svc.Open(context.Background(), owner, "a1", DownloadRequest{}); !errors.Is(err, ErrStorageUnavailable) {
		t.Fatalf("streams too: %v", err)
	}
	if len(e.storage.Presigns) != 0 {
		t.Fatal("no fallback to the platform's storage")
	}
}

func TestTenantAssetWithoutRoutingIsNotServed(t *testing.T) {
	e := newEnv(t, nil)
	r := &tenantRouter{storage: storagememory.NewObjectStorage(), presign: true}
	e.tenantAsset(t, r, "a1")
	if _, err := e.svc.CreateDownloadURL(context.Background(), owner, "a1", URLRequest{}); !errors.Is(err, ErrStorageUnavailable) {
		t.Fatalf("without BYOS a tenant asset is unavailable, never served from the default: %v", err)
	}
}
