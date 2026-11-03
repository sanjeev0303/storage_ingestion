package integration

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/dto/request"
	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/service/asset"
	"github.com/sanjeev0303/object-storage-platform/internal/service/chunkstore"
	"github.com/sanjeev0303/object-storage-platform/internal/service/delivery"
)

func inNamespace(name string) func(*request.CreateUploadRequest) {
	return func(r *request.CreateUploadRequest) { r.Dataset = name }
}

func rangeHeader(first, last int64) string {
	return fmt.Sprintf("bytes=%d-%d", first, last)
}

// lockedRouter is storage on a bucket with Object Lock on.
type lockedStorage struct {
	interfaces.StorageProvider
}

func (lockedStorage) ObjectLockEnabled(context.Context, string) (bool, error) { return true, nil }

func (lockedStorage) LockObjectVersion(context.Context, string, string, string, interfaces.VersionLock) error {
	return nil
}

func (lockedStorage) ObjectLockStatus(context.Context, string, string, string) (interfaces.VersionLock, error) {
	return interfaces.VersionLock{}, nil
}

type lockedRouter struct{ chunkstore.Router }

func (r lockedRouter) Locate(ctx context.Context, tenantID, connectionID string) (interfaces.StorageLocation, error) {
	loc, err := r.Router.Locate(ctx, tenantID, connectionID)
	loc.Storage = lockedStorage{loc.Storage}
	return loc, err
}

// What a bucket locks is the object itself, and a chunk could be kept there past
// any use of it, so nothing on such a bucket is converted. A retention policy
// keeps a dataset out too.
func TestChunkStorageLeavesLockedStorageAlone(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	rig := s.packingRig(t)
	acme := s.team(t, "chunklock")
	dev, owner := acme.developer, acme.owner
	data := randomBytes(31, 3<<20)
	d := s.newDataset(t, dev, "lock-set")
	a1 := s.publishAt(t, dev, "a.bin", data, inNamespace("lock-set"))
	a2 := s.publishAt(t, dev, "b.bin", append(append([]byte{}, data...), 'x'), inNamespace("lock-set"))
	s.addMembers(t, dev, d.ID, memberOf(a1, "", ""), memberOf(a2, "", ""))
	if _, _, err := s.datasets.Snapshot(ctx, dev, d.ID, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.datasets.SetChunkStorage(ctx, owner, d.ID, true); err != nil {
		t.Fatal(err)
	}
	for {
		found, err := s.datasets.ChunkOnce(ctx, "worker")
		if err != nil {
			t.Fatal(err)
		}
		if !found {
			break
		}
	}
	svc := chunkstore.New(chunkstore.Deps{Store: postgres.NewChunkStorageRepository(s.db), Packs: postgres.NewPackRepository(s.db), Router: lockedRouter{rig.router}, Log: s.log})
	st, err := svc.RunOnce(ctx)
	if err != nil || st.Converted != 0 || st.Failed != 2 {
		t.Fatalf("pass: %+v %v", st, err)
	}
	var reason string
	if err := s.db.Pool.QueryRow(ctx, `SELECT reason FROM chunked_assets WHERE asset_id = $1`, a1.ID).Scan(&reason); err != nil || reason == "" {
		t.Fatalf("reason: %q %v", reason, err)
	}
	s.mustDownload(t, dev, a1.ID, data)

	// A dataset under a retention policy, or a hold, is refused outright.
	other := s.newDataset(t, dev, "kept-set")
	if _, err := s.db.Pool.Exec(ctx, `INSERT INTO retention_policies (id, tenant_id, scope, scope_key, version, min_retention_seconds, reason, created_by, effective_from, created_at)
		VALUES ('p-kept', $1, 'DATASET', 'kept-set', 1, 86400, 'test', 'x', now(), now())`, dev.TenantID); err != nil {
		t.Fatal(err)
	}
	if err := s.datasets.SetChunkStorage(ctx, owner, other.ID, true); err == nil {
		t.Fatal("a dataset under a retention policy took chunk storage")
	}
}

func chunkService(s *securityStack, rig *packingRig, retire time.Duration) *chunkstore.Service {
	return chunkstore.New(chunkstore.Deps{Store: postgres.NewChunkStorageRepository(s.db), Packs: postgres.NewPackRepository(s.db), Router: rig.router, Log: s.log,
		SourceRetireAfter: retire, UnusedAfter: time.Nanosecond, RecheckAfter: time.Nanosecond})
}

// Bytes kept as chunks read exactly as they did as an object, by every reader;
// the original only goes after the chunks proved themselves; a file that shares
// nothing stays whole; deletion and rehydration leave nothing behind.
func TestChunkStorage(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	rig := s.packingRig(t)
	s.delivery.Route(rig.router)
	acme := s.team(t, "chunkstore")
	dev, owner := acme.developer, acme.owner
	repo := postgres.NewChunkStorageRepository(s.db)
	packs := postgres.NewPackRepository(s.db)

	index := func() {
		t.Helper()
		for {
			found, err := s.datasets.ChunkOnce(ctx, "worker")
			if err != nil {
				t.Fatal(err)
			}
			if !found {
				return
			}
		}
	}
	pass := func(svc *chunkstore.Service) chunkstore.Stats {
		t.Helper()
		stats, err := svc.RunOnce(ctx)
		if err != nil {
			t.Fatalf("chunk store: %v (%+v)", err, stats)
		}
		return stats
	}
	read := func(id string) []byte {
		t.Helper()
		got, err := s.download(dev, id, "")
		if err != nil {
			t.Fatalf("download %s: %v", id, err)
		}
		return got
	}
	exists := func(key string) bool {
		_, err := s.ie.storage.HeadObject(ctx, s.ie.bucket, key)
		return !errors.Is(err, interfaces.ErrObjectNotFound)
	}

	build1 := randomBytes(11, 12<<20)
	build2 := append(append(append([]byte{}, build1[:3<<20]...), []byte("inserted patch")...), build1[3<<20:]...)
	solo := randomBytes(12, 3<<20)
	d := s.newDataset(t, dev, "store-set")
	a1 := s.publishAt(t, dev, "game-1.bin", build1, inNamespace("store-set"))
	a2 := s.publishAt(t, dev, "game-2.bin", build2, inNamespace("store-set"))
	lone := s.publishAt(t, dev, "solo.bin", solo, inNamespace("store-set"))
	s.addMembers(t, dev, d.ID, memberOf(a1, "", ""), memberOf(a2, "", ""), memberOf(lone, "", ""))
	v, _, err := s.datasets.Snapshot(ctx, dev, d.ID, "")
	if err != nil {
		t.Fatal(err)
	}

	if err := s.datasets.SetChunkStorage(ctx, dev, d.ID, true); !errors.Is(err, model.ErrForbidden) {
		t.Fatalf("it needs datasets.manage: %v", err)
	}
	if err := s.datasets.SetChunkStorage(ctx, owner, d.ID, true); err != nil {
		t.Fatal(err)
	}
	index()

	// With a long grace period: chunks serve, and the originals are still there.
	slow := chunkService(s, rig, time.Hour)
	if st := pass(slow); st.Converted != 2 || st.Skipped != 1 || st.Failed != 0 || st.Retired != 0 {
		t.Fatalf("pass: %+v", st)
	}
	if _, chunked, err := packs.ChunkedExtents(ctx, dev.TenantID, lone.ID); err != nil || chunked {
		t.Fatalf("a file sharing nothing stays whole: %v %v", chunked, err)
	}
	extents, chunked, err := packs.ChunkedExtents(ctx, dev.TenantID, a2.ID)
	if err != nil || !chunked || len(extents) < 3 {
		t.Fatalf("extents: %d %v %v", len(extents), chunked, err)
	}
	if !exists(a2.ObjectKey) {
		t.Fatal("the original went before its grace period")
	}
	if !bytes.Equal(read(a2.ID), build2) || !bytes.Equal(read(a1.ID), build1) || !bytes.Equal(read(lone.ID), solo) {
		t.Fatal("bytes read from chunks differ")
	}

	// A range across chunk boundaries.
	first, last := extents[0].Offset+int64(extents[0].Length)-10, extents[1].Offset+20
	got, err := s.download(dev, a2.ID, rangeHeader(first, last))
	if err != nil || !bytes.Equal(got, build2[first:last+1]) {
		t.Fatalf("range: %d bytes, %v", len(got), err)
	}
	// One object does not hold the file, so no provider link can be given.
	link, err := s.delivery.CreateDownloadURL(ctx, dev, a2.ID, delivery.URLRequest{Mode: delivery.ModeStorage})
	if err != nil || link.Via != delivery.ModeToken {
		t.Fatalf("link: %+v %v", link, err)
	}
	// Shared chunks are stored once, across both builds.
	stats, err := s.datasets.ChunkStorageStats(ctx, dev, d.ID)
	if err != nil || stats.Files != 2 || stats.LogicalBytes != int64(len(build1)+len(build2)) || stats.ChunkBytes >= int64(len(build1))*13/10 {
		t.Fatalf("stats: %+v %v", stats, err)
	}

	// A damaged chunk is an error, never wrong bytes.
	victim := extents[1]
	good, err := s.ie.storage.OpenRange(ctx, s.ie.bucket, victim.Key, 0, -1, interfaces.ReadCondition{})
	if err != nil {
		t.Fatal(err)
	}
	original, _ := io.ReadAll(good)
	good.Close()
	bad := append([]byte{}, original...)
	bad[0] ^= 0xFF
	s.ie.storage.PutObject(victim.Key, bad)
	if got, err := s.download(dev, a2.ID, ""); err == nil {
		t.Fatalf("a damaged chunk gave %d bytes with no error", len(got))
	}
	s.ie.storage.PutObject(victim.Key, original)

	// Past the grace period the originals go, and everything still reads.
	fast := chunkService(s, rig, time.Nanosecond)
	if st := pass(fast); st.Retired != 2 {
		t.Fatalf("retire: %+v", st)
	}
	if exists(a2.ObjectKey) || exists(a1.ObjectKey) || !exists(lone.ObjectKey) {
		t.Fatal("the originals should have gone, and only those")
	}
	if !bytes.Equal(read(a2.ID), build2) || !bytes.Equal(read(a1.ID), build1) {
		t.Fatal("bytes differ once the originals have gone")
	}
	// The other read paths: a dataset's content check, and a shard.
	if err := s.datasets.IndexVersion(ctx, owner, d.ID, v.ID); err != nil {
		t.Fatal(err)
	}
	index()
	var shard int64
	err = s.delivery.StreamShard(ctx, dev, []string{a1.ID, a2.ID}, "", func(f delivery.ShardFile) error {
		n, err := io.Copy(io.Discard, f.Body)
		shard += n
		return err
	})
	if err != nil || shard != int64(len(build1)+len(build2)) {
		t.Fatalf("shard: %d, %v", shard, err)
	}
	// A hold after chunking keeps the chunks; candidates leave held files out.
	third := s.publishAt(t, dev, "held.bin", append(append([]byte{}, build1...), 'x'), inNamespace("store-set"))
	if _, err := s.lifecycle.PlaceHoldFor(ctx, owner, model.LegalHold{Scope: model.ScopeAsset, ScopeKey: third.ID, Reason: "matter 7"}); err != nil {
		t.Fatal(err)
	}
	s.addMembers(t, dev, d.ID, memberOf(third, "", ""))
	if _, _, err := s.datasets.Snapshot(ctx, dev, d.ID, ""); err != nil {
		t.Fatal(err)
	}
	index()
	pass(fast)
	if _, chunked, _ := packs.ChunkedExtents(ctx, dev.TenantID, third.ID); chunked {
		t.Fatal("a held file was chunked")
	}

	// Deleting one build keeps every chunk the other needs, and its own go.
	onlyA2 := []string{}
	a1Hashes := map[string]bool{}
	a1Extents, _, _ := packs.ChunkedExtents(ctx, dev.TenantID, a1.ID)
	for _, e := range a1Extents {
		a1Hashes[e.SHA256] = true
	}
	for _, e := range extents {
		if !a1Hashes[e.SHA256] {
			onlyA2 = append(onlyA2, e.Key)
		}
	}
	if len(onlyA2) == 0 {
		t.Fatal("the patched build has no chunk of its own")
	}
	if _, err := s.lifecycle.RequestDeletion(ctx, dev, a2.ID); err != nil {
		t.Fatal(err)
	}
	var cycle asset.CycleStats
	if err := s.lifecycle.ProcessDeletions(ctx, &cycle); err != nil || cycle.WaitingForPack != 1 {
		t.Fatalf("deletion: %+v %v", cycle, err)
	}
	if st := pass(fast); st.Deleted == 0 {
		t.Fatalf("collect: %+v", st)
	}
	for _, key := range onlyA2 {
		if exists(key) {
			t.Fatalf("chunk %s outlived its only user", key)
		}
	}
	makeDue(t, s, a2.ID)
	cycle = asset.CycleStats{}
	if err := s.lifecycle.ProcessDeletions(ctx, &cycle); err != nil || cycle.Completed != 1 {
		var why string
		_ = s.db.Pool.QueryRow(ctx, `SELECT COALESCE(last_error, '') FROM deletion_operations WHERE asset_id = $1`, a2.ID).Scan(&why)
		t.Fatalf("deletion completes: %+v %v: %s", cycle, err, why)
	}
	if !bytes.Equal(read(a1.ID), build1) {
		t.Fatal("the other build lost a chunk it needed")
	}

	// Rehydrating makes it one object again, with its provider link back, and
	// the chunks nothing else needs go.
	if n, err := s.datasets.RehydrateDataset(ctx, dev, d.ID); !errors.Is(err, model.ErrForbidden) {
		t.Fatalf("rehydrating needs datasets.manage: %d %v", n, err)
	}
	n, err := s.datasets.RehydrateDataset(ctx, owner, d.ID)
	if err != nil || n != 1 {
		t.Fatalf("rehydrate: %d %v", n, err)
	}
	if !bytes.Equal(read(a1.ID), build1) {
		t.Fatal("bytes differ while rehydrating")
	}
	if st := pass(fast); st.Rehydrated != 1 {
		t.Fatalf("rehydrate: %+v", st)
	}
	if !exists(a1.ObjectKey) {
		t.Fatal("no object after rehydrating")
	}
	if _, chunked, _ := packs.ChunkedExtents(ctx, dev.TenantID, a1.ID); chunked {
		t.Fatal("still chunked")
	}
	if !bytes.Equal(read(a1.ID), build1) {
		t.Fatal("bytes differ after rehydrating")
	}
	if link, err := s.delivery.CreateDownloadURL(ctx, dev, a1.ID, delivery.URLRequest{Mode: delivery.ModeStorage}); err != nil || link.Via == delivery.ModeToken {
		t.Fatalf("link after rehydrating: %+v %v", link, err)
	}
	pass(fast)
	if st, err := repo.Stats(ctx, dev.TenantID, d.ID); err != nil || st.Files != 0 || st.Enabled {
		t.Fatalf("stats after: %+v %v", st, err)
	}
	var left int
	if err := s.db.Pool.QueryRow(ctx, `SELECT count(*) FROM stored_chunks WHERE tenant_id = $1`, dev.TenantID).Scan(&left); err != nil || left != 0 {
		t.Fatalf("%d chunks left: %v", left, err)
	}
}
