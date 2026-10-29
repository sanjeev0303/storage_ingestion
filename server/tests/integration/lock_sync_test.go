package integration

import (
	"context"
	"testing"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// The lock sync queue against PostgreSQL: a request names its scope's
// available assets in order, and is finished once they are all applied.
func TestLockSyncQueue(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	acme := s.team(t, "locksync")
	asset := s.availableAsset(t, acme.developer, "held.txt", []byte("bytes to keep"))
	queue := postgres.NewLockSyncRepository(s.db)
	now := time.Now().UTC().Truncate(time.Microsecond)

	if err := queue.Request(ctx, model.LockSyncRequest{ID: "ls-" + s.run, TenantID: acme.org.ID, Scope: model.ScopeAsset,
		ScopeKey: asset.ID, Reason: "hold h1", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	req, found, err := queue.Next(ctx, acme.org.ID)
	if err != nil || !found || req.ScopeKey != asset.ID {
		t.Fatalf("next: %+v %v %v", req, found, err)
	}
	assets, err := queue.Assets(ctx, req, 10)
	if err != nil || len(assets) != 1 || assets[0].ID != asset.ID {
		t.Fatalf("assets in scope: %+v %v", assets, err)
	}
	// Advancing past the last asset, then finishing.
	if err := queue.Advance(ctx, req.ID, asset.ID, false, now); err != nil {
		t.Fatal(err)
	}
	req, _, _ = queue.Next(ctx, acme.org.ID)
	if more, err := queue.Assets(ctx, req, 10); err != nil || len(more) != 0 {
		t.Fatalf("assets after the position: %+v %v", more, err)
	}
	if err := queue.Advance(ctx, req.ID, asset.ID, true, now); err != nil {
		t.Fatal(err)
	}
	if _, found, err := queue.Next(ctx, acme.org.ID); err != nil || found {
		t.Fatalf("a finished request is still next: %v %v", found, err)
	}
}
