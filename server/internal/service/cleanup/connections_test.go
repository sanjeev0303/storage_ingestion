package cleanup

import (
	"context"
	"testing"
	"time"

	storagememory "github.com/sanjeev0303/object-storage-platform/internal/infrastructure/storage/memory"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/repository/memory"
)

type oneConnection struct {
	storage *storagememory.ObjectStorage
}

func (o oneConnection) Locate(_ context.Context, tenantID, connectionID string) (interfaces.StorageLocation, error) {
	if tenantID != "t1" || connectionID != "sc_c" {
		return interfaces.StorageLocation{}, model.ErrConnectionNotFound
	}
	return interfaces.StorageLocation{
		StoragePlace: interfaces.StoragePlace{ConnectionID: "sc_c", StagingBucket: "cust", StagingPrefix: "staging",
			ReadyBucket: "cust", ReadyPrefix: "published"},
		Storage: o.storage,
	}, nil
}

func TestCleanupSweepsEachConnectionThroughItself(t *testing.T) {
	e := newEnv(t)
	customer := storagememory.NewObjectStorage()
	customer.Now = func() time.Time { return e.clock }
	connections := memory.NewStorageConnectionRepository()
	connections.Put(model.StorageConnection{ID: "sc_c", TenantID: "t1", Name: "c", Provider: model.ProviderS3Compatible,
		Status: model.ConnectionDisabled, StagingMode: model.StagingInPlace, StagingPrefix: "staging", ReadyPrefix: "published"},
		interfaces.SealedCredential{})

	// On the tenant's bucket: a deleted asset's copy (proven orphan), an
	// object no record knows (maybe the customer's own), and a live asset.
	deleted := e.asset("gone", model.AssetStatusDeleted, "published/t1/gone")
	e.assets.Update(deleted.ID, func(a *model.Asset) { a.StorageConnectionID, a.StagingConnectionID = "sc_c", "sc_c" })
	customer.PutObject("published/t1/gone", []byte("data"))
	customer.PutObject("published/t1/unknown", []byte("customer data"))
	live := e.asset("live", model.AssetStatusAvailable, "published/t1/live")
	e.assets.Update(live.ID, func(a *model.Asset) { a.StorageConnectionID, a.StagingConnectionID = "sc_c", "sc_c" })
	customer.PutObject("published/t1/live", []byte("data"))

	// On the platform's bucket, the same key as the live tenant asset: the
	// asset is bound elsewhere, so it neither owns nor disowns this one.
	e.storage.PutObject("assets/t1/live", []byte("data"))

	svc := e.service(func(c *Config) { c.DeleteUnrecorded = true })
	svc.deps.Router, svc.deps.Connections = oneConnection{customer}, connections

	e.clock = e.clock.Add(2 * time.Hour)
	cycle(t, svc)
	e.clock = e.clock.Add(25 * time.Hour)
	stats := cycle(t, svc)

	if _, ok := customer.Object("published/t1/gone"); ok {
		t.Fatal("a deleted asset's copy on the tenant's bucket is removed through that connection")
	}
	if _, ok := customer.Object("published/t1/unknown"); !ok {
		t.Fatal("an object no record knows on a tenant's bucket is reported, never deleted")
	}
	if _, ok := customer.Object("published/t1/live"); !ok {
		t.Fatal("a live asset's object is kept")
	}
	if !e.present("assets/t1/live") {
		t.Fatal("the platform's object under the same key is not the tenant asset's to decide")
	}
	if stats.Kept == 0 {
		t.Fatalf("the unrecorded tenant object is counted as kept: %+v", stats)
	}

	// Scans and candidates are recorded per connection.
	scans, _ := e.inventory.RecentScans(context.Background(), 10)
	byConnection := map[string]int{}
	for _, s := range scans {
		byConnection[s.ConnectionID]++
	}
	if byConnection["sc_c"] == 0 || byConnection[model.PlatformConnectionID] == 0 {
		t.Fatalf("each connection has its own scans: %v", byConnection)
	}
}

func TestMissingCheckUsesTheAssetsBinding(t *testing.T) {
	e := newEnv(t)
	customer := storagememory.NewObjectStorage()
	live := e.asset("live", model.AssetStatusAvailable, "published/t1/live")
	e.assets.Update(live.ID, func(a *model.Asset) { a.StorageConnectionID, a.StagingConnectionID = "sc_c", "sc_c" })
	// The platform has an object at that key; the tenant's bucket does not.
	e.storage.PutObject("published/t1/live", []byte("data"))

	svc := e.service()
	svc.deps.Router = oneConnection{customer}
	var stats CycleStats
	if err := svc.CheckMissing(context.Background(), &stats); err != nil {
		t.Fatal(err)
	}
	if stats.Missing != 1 {
		t.Fatalf("the object is missing where the asset is bound, whatever the platform holds: %+v", stats)
	}
}
