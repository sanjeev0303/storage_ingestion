package integration

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/config"
	assetservice "github.com/sanjeev0303/object-storage-platform/internal/service/asset"
	"github.com/sanjeev0303/object-storage-platform/internal/service/packing"
)

// Backlog 3 to 7 of phase15-report.md §13: leases by the database's clock,
// tenants taken in turn, verification by the planned entry count, a
// deletion racing a rewrite, and unpacking.

// A worker's clock never decides a lease: one running an hour ahead cannot
// take a lease that is still held, and one running behind renews it from
// the database's now, not into the past.
func TestPackingLeasesFollowTheDatabaseClock(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	acme := s.team(t, "pack-clock")
	tenant := acme.org.ID
	s.smallAssets(t, acme.developer, "clock", 3)
	rig := s.packingRig(t)
	w := rig.worker(t, "w1", nil, []string{tenant}, nil)
	var planned packing.CycleStats
	if err := w.Plan(ctx, &planned); err != nil || planned.Planned != 1 {
		t.Fatalf("plan: %+v %v", planned, err)
	}
	p := rig.packs(t, tenant)[0]

	now := time.Now().UTC()
	claimed, ok, err := rig.repo.ClaimPack(ctx, tenant, p.ID, "w1", now, time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	if err := rig.repo.RenewPack(ctx, p.ID, claimed.LeaseOwner, now.Add(-time.Hour), time.Minute); err != nil {
		t.Fatalf("renew from a slow clock: %v", err)
	}
	if _, ok, err := rig.repo.ClaimPack(ctx, tenant, p.ID, "w2", now.Add(time.Hour), time.Minute); err != nil || ok {
		t.Fatalf("a clock an hour ahead took a lease still held (%v)", err)
	}
	if got, err := rig.repo.ClaimPacks(ctx, "w2", now.Add(time.Hour), time.Minute, 1000); err != nil || slices.ContainsFunc(got, func(q model.Pack) bool { return q.ID == p.ID }) {
		t.Fatalf("ClaimPacks with a fast clock took a held lease (%v)", err)
	}

	expireLease(t, s, p.ID)
	taken, ok, err := rig.repo.ClaimPack(ctx, tenant, p.ID, "w2", now.Add(-time.Hour), time.Minute)
	if err != nil || !ok {
		t.Fatalf("a clock an hour behind takes an expired lease of a due pack: %v %v", ok, err)
	}
	// Not left for later tests' workers to claim.
	if err := rig.repo.FailPack(ctx, p.ID, taken.LeaseOwner, time.Now().UTC(), "test over", nil); err != nil {
		t.Fatal(err)
	}
}

// retirePacks retires the tenants' published packs, so packs left with
// deleted entries are not rewritten by later tests' workers.
func retirePacks(t *testing.T, s *securityStack, tenantIDs ...string) {
	t.Helper()
	t.Cleanup(func() {
		_, err := s.db.Pool.Exec(context.Background(), `
			UPDATE packs SET state = 'RETIRING', retired_at = now(), updated_at = now()
			WHERE tenant_id = ANY($1) AND state = 'PUBLISHED'
		`, tenantIDs)
		if err != nil {
			t.Error(err)
		}
	})
}

// Every tenant's oldest repack request comes before any tenant's second,
// so one tenant with many requests cannot fill the page.
func TestPackingRepacksTakeTenantsInTurn(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	repo := s.packingRig(t).repo
	suffix := fmt.Sprint(time.Now().UnixNano())
	busy, other := "busy-"+suffix, "other-"+suffix
	base := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC) // older than anything else pending
	requests := []model.RepackRequest{
		{ID: busy + "-1", TenantID: busy, DatasetID: busy + "-d1", CreatedAt: base},
		{ID: busy + "-2", TenantID: busy, DatasetID: busy + "-d2", CreatedAt: base.Add(time.Minute)},
		{ID: busy + "-3", TenantID: busy, DatasetID: busy + "-d3", CreatedAt: base.Add(2 * time.Minute)},
		{ID: other + "-1", TenantID: other, DatasetID: other + "-d1", CreatedAt: base.Add(3 * time.Minute)},
	}
	for _, q := range requests {
		q.RequestedBy = "it"
		if err := repo.CreateRepack(ctx, q); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = repo.CancelRepack(context.Background(), q.TenantID, q.ID, time.Now().UTC()) })
	}
	got, err := repo.PendingRepacks(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != busy+"-1" || got[1].ID != other+"-1" {
		t.Fatalf("pending repacks %+v; want the busy tenant's oldest, then the other tenant's", got)
	}
}

// Packs to rewrite are listed tenant by tenant too: a tenant's second pack
// comes after every other tenant's first.
func TestPackingRewritesTakeTenantsInTurn(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	rig := s.packingRig(t)
	busy := s.team(t, "pack-turn-busy")
	other := s.team(t, "pack-turn-other")
	busyAssets, _ := s.smallAssets(t, busy.developer, "turn", 4)
	otherAssets, _ := s.smallAssets(t, other.developer, "turn", 2)
	retirePacks(t, s, busy.org.ID, other.org.ID)
	pairs := func(c *config.Packing) { c.MaxEntries = 2 }
	wb := rig.worker(t, "wb", nil, []string{busy.org.ID}, pairs)
	wo := rig.worker(t, "wo", nil, []string{other.org.ID}, pairs)
	for _, w := range []*packing.Service{wb, wb, wo} { // published in this order
		if stats := rig.run(t, w); stats.Published != 1 {
			t.Fatalf("one pack per cycle: %+v", stats)
		}
	}
	busyPacks, otherPacks := rig.packs(t, busy.org.ID), rig.packs(t, other.org.ID)
	if len(busyPacks) != 2 || len(otherPacks) != 1 {
		t.Fatalf("%d and %d packs", len(busyPacks), len(otherPacks))
	}
	for _, a := range []model.Asset{busyAssets[0], busyAssets[2], otherAssets[0]} {
		if _, err := rig.repo.ReleaseAsset(ctx, a.TenantID, a.ID, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}

	got, err := rig.repo.PacksToRewrite(ctx, 0.01, time.Now().UTC(), 1000)
	if err != nil {
		t.Fatal(err)
	}
	at := func(id string) int {
		return slices.IndexFunc(got, func(p model.Pack) bool { return p.ID == id })
	}
	first, second, others := at(busyPacks[0].ID), at(busyPacks[1].ID), at(otherPacks[0].ID)
	if first < 0 || second < 0 || others < 0 || !(first < others && others < second) {
		t.Fatalf("positions: busy %d and %d, other %d; want the other tenant's pack between the busy tenant's", first, second, others)
	}
}

// A pack planned under a higher PACKING_MAX_ENTRIES is still verified and
// published after the limit is lowered.
func TestPackingVerifiesWithThePlannedEntryCount(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	acme := s.team(t, "pack-entries")
	tenant := acme.org.ID
	_, data := s.smallAssets(t, acme.developer, "entries", 3)
	rig := s.packingRig(t)
	w := rig.worker(t, "w1", nil, []string{tenant}, nil)
	var planned packing.CycleStats
	if err := w.Plan(ctx, &planned); err != nil || planned.Planned != 1 {
		t.Fatalf("plan: %+v %v", planned, err)
	}
	lowered := rig.worker(t, "w2", nil, []string{tenant}, func(c *config.Packing) { c.MaxEntries = 2 })
	if stats := rig.run(t, lowered); stats.Published != 1 || stats.Retried != 0 || stats.Failed != 0 {
		t.Fatalf("the three-entry pack is published: %+v", stats)
	}
	for id, b := range data {
		s.mustDownload(t, acme.developer, id, b)
	}
}

// A deletion released while a rewrite of its pack is being published: the
// rewrite has moved the asset's entry out of the old pack but not yet
// committed. The release must still catch the entry in the new pack.
func TestPackingReleaseWaitsForARewriteBeingPublished(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	acme := s.team(t, "pack-release-race")
	tenant := acme.org.ID
	assets, _ := s.smallAssets(t, acme.developer, "rr", 3)
	retirePacks(t, s, tenant)
	rig := s.packingRig(t)
	tenants := []string{tenant}
	w := rig.worker(t, "w1", nil, tenants, nil)
	if stats := rig.run(t, w); stats.Published != 1 {
		t.Fatalf("packed: %+v", stats)
	}
	old := rig.packs(t, tenant)[0]
	gone, racer := assets[2], assets[0]
	if _, err := rig.repo.ReleaseAsset(ctx, tenant, gone.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	impatient := rig.worker(t, "w2", nil, tenants, func(c *config.Packing) { c.MaxDeletionDelay = time.Nanosecond })
	var st packing.CycleStats
	if err := impatient.Retire(ctx, &st); err != nil || st.Rewrites != 1 {
		t.Fatalf("rewrite planned: %+v %v", st, err)
	}
	all := rig.packs(t, tenant)
	rewrite := all[len(all)-1]

	// The rewrite, built as far as its publication.
	now := time.Now().UTC()
	claimed, ok, err := rig.repo.ClaimPack(ctx, tenant, rewrite.ID, "racer", now, time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	owner := claimed.LeaseOwner
	if err := rig.repo.AdvancePack(ctx, rewrite.ID, owner, model.PackPlanned, model.PackWriting, interfaces.PackChange{}, now); err != nil {
		t.Fatal(err)
	}
	change := interfaces.PackChange{SHA256: sha256Of([]byte("pack")), StorageETag: `"pack"`}
	if err := rig.repo.AdvancePack(ctx, rewrite.ID, owner, model.PackWriting, model.PackVerifying, change, now); err != nil {
		t.Fatal(err)
	}

	// Holding the old pack's row stops PublishPack at its last step, after
	// it has moved the entries, before it commits.
	hold, err := s.db.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = hold.Rollback(context.Background()) }()
	if _, err := hold.Exec(ctx, `SELECT 1 FROM packs WHERE id = $1 FOR UPDATE`, old.ID); err != nil {
		t.Fatal(err)
	}
	waitingForLocks := func(n int) {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for {
			var waiting int
			err := s.db.Pool.QueryRow(ctx, `
				SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'
			`).Scan(&waiting)
			if err != nil {
				t.Fatal(err)
			}
			if waiting >= n {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%d sessions waiting for a lock; want %d", waiting, n)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	published := make(chan error, 1)
	go func() {
		_, _, err := rig.repo.PublishPack(ctx, rewrite.ID, owner, sha256Of([]byte("manifest")), time.Now().UTC())
		published <- err
	}()
	waitingForLocks(1)
	released := make(chan error, 1)
	go func() {
		_, err := rig.repo.ReleaseAsset(ctx, tenant, racer.ID, time.Now().UTC())
		released <- err
	}()
	waitingForLocks(2)
	if err := hold.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-published; err != nil {
		t.Fatalf("publish: %v", err)
	}
	if err := <-released; err != nil {
		t.Fatalf("release: %v", err)
	}

	states := rig.stateOf(t, rewrite.ID)
	if states[racer.ID] != model.PackEntryDeleted || states[assets[1].ID] != model.PackEntryPublished {
		t.Fatalf("rewrite entries %v; the released asset's must be DELETED, the other's PUBLISHED", states)
	}
	if _, packed, err := rig.repo.PackedLocation(ctx, tenant, racer.ID); packed || err != nil {
		t.Fatalf("a released asset is not served from the rewrite (%v)", err)
	}
}

// Unpacking copies each asset still served from a pack back to its own
// object, checked against its SHA-256, points the asset at it and retires
// the pack. A deleted asset's bytes are not brought back; an asset whose
// own object was never retired keeps it. Afterwards every asset is read
// from its own object and nothing is packed.
func TestPackingUnpackRestoresEveryAssetsOwnObject(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	acme := s.team(t, "pack-unpack")
	dev := acme.developer
	tenant := acme.org.ID
	assets, data := s.smallAssets(t, dev, "unpack", 4)
	rig := s.packingRig(t)
	w := rig.worker(t, "w1", nil, []string{tenant}, func(c *config.Packing) { c.SourceRetireAfter = 0 })
	if stats := rig.run(t, w); stats.Published != 1 || stats.SourcesRetired != 4 {
		t.Fatalf("packed and retired: %+v", stats)
	}
	p := rig.packs(t, tenant)[0]
	gone, live := assets[3], assets[:3]
	if _, err := s.lifecycle.RequestDeletion(ctx, dev, gone.ID); err != nil {
		t.Fatal(err)
	}
	var cycle assetservice.CycleStats
	if err := s.lifecycle.ProcessDeletions(ctx, &cycle); err != nil || cycle.WaitingForPack != 1 {
		t.Fatalf("the deletion waits for the pack: %+v %v", cycle, err)
	}
	var liveBytes int64
	for _, a := range live {
		liveBytes += a.SizeBytes
	}

	dry, err := w.Unpack(ctx, tenant, "", true)
	if err != nil || dry != (packing.UnpackStats{Packs: 1, Entries: 3, Copied: 3, BytesCopied: liveBytes}) {
		t.Fatalf("dry run: %+v %v", dry, err)
	}
	for _, a := range assets {
		if _, ok := s.ie.storage.Object(a.ObjectKey); ok {
			t.Fatal("a dry run writes nothing")
		}
	}

	stats, err := w.Unpack(ctx, tenant, p.ID, false)
	if err != nil || stats != (packing.UnpackStats{Packs: 1, Entries: 3, Copied: 3, BytesCopied: liveBytes}) {
		t.Fatalf("unpack: %+v %v", stats, err)
	}
	for _, a := range live {
		stored, ok := s.ie.storage.Object(a.ObjectKey)
		if !ok || !bytes.Equal(stored, data[a.ID]) {
			t.Fatalf("asset %s: its own object holds %d bytes, want its %d", a.ID, len(stored), len(data[a.ID]))
		}
		if _, packed, err := rig.repo.PackedLocation(ctx, tenant, a.ID); packed || err != nil {
			t.Fatalf("asset %s is still served from a pack (%v)", a.ID, err)
		}
		s.mustDownload(t, dev, a.ID, data[a.ID])
	}
	if _, ok := s.ie.storage.Object(gone.ObjectKey); ok {
		t.Fatal("a deleted asset's bytes are not brought back")
	}
	if got, err := rig.repo.GetPack(ctx, tenant, p.ID); err != nil || got.State != model.PackRetiring {
		t.Fatalf("the unpacked pack retires: %s %v", got.State, err)
	}
	if again, err := w.Unpack(ctx, tenant, "", false); err != nil || again.Packs != 0 {
		t.Fatalf("nothing is left to unpack: %+v %v", again, err)
	}

	// Sources never retired: only the records change.
	kept := s.team(t, "pack-unpack-kept")
	keptAssets, keptData := s.smallAssets(t, kept.developer, "kept", 2)
	keeper := rig.worker(t, "w2", nil, []string{kept.org.ID}, nil)
	if stats := rig.run(t, keeper); stats.Published != 1 || stats.SourcesRetired != 0 {
		t.Fatalf("packed, sources kept: %+v", stats)
	}
	writes := s.ie.storage.WriteCalls
	stats, err = keeper.Unpack(ctx, kept.org.ID, "", false)
	if err != nil || stats != (packing.UnpackStats{Packs: 1, Entries: 2, Standalone: 2}) {
		t.Fatalf("unpack: %+v %v", stats, err)
	}
	if s.ie.storage.WriteCalls != writes {
		t.Fatal("nothing is copied when the asset's own object is still there")
	}
	for _, a := range keptAssets {
		s.mustDownload(t, kept.developer, a.ID, keptData[a.ID])
	}
}
