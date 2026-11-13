package packing

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/config"
)

// rewriteQueue is a pack repository with packs to rewrite, honouring the
// limit it is asked for, as the database does.
type rewriteQueue struct {
	interfaces.PackRepository
	toRewrite []model.Pack
	building  map[string]int
	planned   []string // tenants of the rewrites planned
}

func (q *rewriteQueue) PacksToRewrite(_ context.Context, _ float64, _ time.Time, limit int) ([]model.Pack, error) {
	return q.toRewrite[:min(limit, len(q.toRewrite))], nil
}

func (q *rewriteQueue) BuildingPacks(_ context.Context, tenantID string) (int, error) {
	return q.building[tenantID], nil
}

func (q *rewriteQueue) Entries(_ context.Context, packID string) ([]model.PackEntry, error) {
	return []model.PackEntry{{PackID: packID, AssetID: packID + "-asset", SizeBytes: 10, State: model.PackEntryPublished}}, nil
}

func (q *rewriteQueue) CreatePack(_ context.Context, p model.Pack, _ []model.PackEntry, _ ...model.OutboxEvent) error {
	q.planned = append(q.planned, p.TenantID)
	q.building[p.TenantID]++
	return nil
}

type platformOnly struct{ interfaces.StorageRouter }

func (platformOnly) Locate(_ context.Context, _, connectionID string) (interfaces.StorageLocation, error) {
	return interfaces.StorageLocation{StoragePlace: interfaces.StoragePlace{ConnectionID: connectionID, ReadyBucket: "b", ReadyPrefix: "published"}}, nil
}

// A tenant whose pack slots are full, with the oldest packs to rewrite,
// must not hold back another tenant's rewrite.
func TestPlanRewritesLooksPastBusyTenants(t *testing.T) {
	pack := func(id, tenant string) model.Pack {
		return model.Pack{ID: id, TenantID: tenant, ConnectionID: model.PlatformConnectionID, State: model.PackPublished}
	}
	q := &rewriteQueue{
		toRewrite: []model.Pack{pack("a1", "busy"), pack("a2", "busy"), pack("b1", "idle")},
		building:  map[string]int{"busy": 1},
	}
	cfg := config.Packing{MinObjectBytes: 1, MaxObjectBytes: 1024, TargetPackBytes: 1 << 20, MaxPackBytes: 2 << 20,
		MaxEntries: 100, SourceConcurrency: 1, ActiveWriters: 1, TenantActivePacks: 1, TenantsPerCycle: 1,
		Lease: 3 * time.Second, MaxJobDuration: time.Minute, MaxAttempts: 1, RetryBase: time.Second, RetryMax: time.Second,
		RewriteDeadRatio: 0.5, MaxDeletionDelay: time.Hour, PollInterval: time.Second}
	s, err := NewService(Dependencies{Packs: q, Assets: struct{ AssetReader }{}, Router: platformOnly{},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, Config{Packing: cfg, Owner: "w"})
	if err != nil {
		t.Fatal(err)
	}
	var stats CycleStats
	if err := s.planRewrites(context.Background(), &stats); err != nil {
		t.Fatal(err)
	}
	if len(q.planned) != 1 || q.planned[0] != "idle" || stats.Rewrites != 1 {
		t.Fatalf("planned rewrites for %v; want one, for the idle tenant", q.planned)
	}
}
