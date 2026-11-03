package integration

import (
	"bytes"
	"context"
	"io"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	s3storage "github.com/sanjeev0303/object-storage-platform/internal/infrastructure/storage/s3"

	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/retry"
	assetservice "github.com/sanjeev0303/object-storage-platform/internal/service/asset"
	"github.com/sanjeev0303/object-storage-platform/internal/service/chunkstore"
	"github.com/sanjeev0303/object-storage-platform/internal/service/copyengine"
)

func indexAllChunks(t *testing.T, s *securityStack) {
	t.Helper()
	for {
		found, err := s.datasets.ChunkOnce(context.Background(), "worker")
		if err != nil {
			t.Fatal(err)
		}
		if !found {
			return
		}
	}
}

// A damaged chunk is written back by a scrub: from the original while it is
// there, then from a verified replica, and never from a source that differs.
func TestChunkRepair(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	rig := s.packingRig(t)
	s.delivery.Route(rig.router)
	acme := s.team(t, "chunkfix")
	dev, owner := acme.developer, acme.owner
	copies := copyengine.NewService(copyengine.Dependencies{Store: postgres.NewCopyRepository(s.db), Router: rig.router, Packs: postgres.NewPackRepository(s.db),
		Alerts: postgres.NewAccessRepository(s.db), Audit: s.audit}, copyengine.Config{Owner: "it-fix", Batch: 100}, s.log)
	packs := postgres.NewPackRepository(s.db)

	build1 := randomBytes(41, 8<<20)
	build2 := append(append(append([]byte{}, build1[:2<<20]...), []byte("patch")...), build1[2<<20:]...)
	d := s.newDataset(t, dev, "fix-set")
	a1 := s.publishAt(t, dev, "one.bin", build1, inNamespace("fix-set"))
	a2 := s.publishAt(t, dev, "two.bin", build2, inNamespace("fix-set"))
	s.addMembers(t, dev, d.ID, memberOf(a1, "", ""), memberOf(a2, "", ""))
	if _, _, err := s.datasets.Snapshot(ctx, dev, d.ID, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.datasets.SetChunkStorage(ctx, owner, d.ID, true); err != nil {
		t.Fatal(err)
	}
	indexAllChunks(t, s)
	if st, err := chunkService(s, rig, time.Hour).RunOnce(ctx); err != nil || st.Converted != 2 {
		t.Fatalf("convert: %+v %v", st, err)
	}
	if _, err := s.db.Pool.Exec(ctx, `UPDATE datasets SET scrub_days = 1 WHERE id = $1`, d.ID); err != nil {
		t.Fatal(err)
	}
	extents, _, err := packs.ChunkedExtents(ctx, dev.TenantID, a2.ID)
	if err != nil || len(extents) < 3 {
		t.Fatalf("extents: %d %v", len(extents), err)
	}
	stored := func(key string) []byte {
		t.Helper()
		body, err := s.ie.storage.OpenRange(ctx, s.ie.bucket, key, 0, -1, interfaces.ReadCondition{})
		if err != nil {
			t.Fatal(err)
		}
		defer body.Close()
		raw, _ := io.ReadAll(body)
		return raw
	}
	scrubAgain := func() copyengine.ScrubStats {
		t.Helper()
		if _, err := s.db.Pool.Exec(ctx, `DELETE FROM scrub_results WHERE tenant_id = $1`, dev.TenantID); err != nil {
			t.Fatal(err)
		}
		// Only this organization's files: a test database holds others'.
		if _, err := s.db.Pool.Exec(ctx, `UPDATE datasets SET scrub_days = 0 WHERE tenant_id <> $1`, dev.TenantID); err != nil {
			t.Fatal(err)
		}
		stats, err := copies.Scrub(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return stats
	}
	lastChunk := extents[len(extents)-1]
	good := stored(lastChunk.Key)
	damage := func() {
		bad := append([]byte{}, good...)
		bad[10] ^= 0xFF
		s.ie.storage.PutObject(lastChunk.Key, bad)
	}

	// Nothing wrong: nothing is written.
	if st := scrubAgain(); st.Checked != 2 || st.Damaged != 0 {
		t.Fatalf("clean scrub: %+v", st)
	}

	// The original is still there, so it repairs the chunk.
	damage()
	if st := scrubAgain(); st.Damaged == 0 || st.Repaired != st.Damaged {
		t.Fatalf("repair from the original: %+v", st)
	}
	if !bytes.Equal(stored(lastChunk.Key), good) {
		t.Fatal("the chunk was not written back")
	}
	s.mustDownload(t, dev, a2.ID, build2)

	// With the originals gone and no replica it is reported, not repaired.
	if st, err := chunkService(s, rig, time.Nanosecond).RunOnce(ctx); err != nil || st.Retired != 2 {
		t.Fatalf("retire: %+v %v", st, err)
	}
	damage()
	if st := scrubAgain(); st.Damaged == 0 || st.Repaired != 0 {
		t.Fatalf("nothing to repair from: %+v", st)
	}
	if _, err := s.download(dev, a2.ID, ""); err == nil {
		t.Fatal("a damaged chunk read with no error")
	}

	// A replica that differs is refused, and writes nothing.
	addReplica := func(data []byte) {
		t.Helper()
		key := "replicas/" + a2.ID
		s.ie.storage.PutObject(key, data)
		if _, err := s.db.Pool.Exec(ctx, `INSERT INTO asset_replicas (tenant_id, asset_id, connection_id, object_key, size_bytes, checksum_sha256, replicated_at, verified_at)
			VALUES ($1, $2, 'platform', $3, $4, $5, now(), now()) ON CONFLICT (tenant_id, asset_id, connection_id) DO UPDATE SET verified_at = now()`,
			dev.TenantID, a2.ID, key, len(data), sha256Of(build2)); err != nil {
			t.Fatal(err)
		}
	}
	wrong := append([]byte{}, build2...)
	wrong[lastChunk.Offset+5] ^= 0xFF
	addReplica(wrong)
	before := stored(lastChunk.Key)
	if st := scrubAgain(); st.Repaired != 0 {
		t.Fatalf("a replica that differs repaired it: %+v", st)
	}
	if !bytes.Equal(stored(lastChunk.Key), before) {
		t.Fatal("a replica that differs wrote a chunk")
	}

	// A verified replica repairs it, and the other build sharing chunks reads too.
	addReplica(build2)
	if st := scrubAgain(); st.Repaired == 0 {
		t.Fatalf("repair from the replica: %+v", st)
	}
	s.mustDownload(t, dev, a2.ID, build2)
	s.mustDownload(t, dev, a1.ID, build1)
	if st := scrubAgain(); st.Damaged != 0 {
		t.Fatalf("still damaged after the repair: %+v", st)
	}
}

// lockFreeView is storage whose Object Lock cannot be seen, as if it had been
// turned on after the files were chunked.
type lockFreeView struct {
	interfaces.StorageProvider
}

type realBucketRouter struct {
	storage interfaces.StorageProvider
	bucket  string
}

func (r realBucketRouter) Locate(context.Context, string, string) (interfaces.StorageLocation, error) {
	return interfaces.StorageLocation{Provider: model.ProviderPlatform, Storage: r.storage,
		StoragePlace: interfaces.StoragePlace{ConnectionID: model.PlatformConnectionID, StagingBucket: r.bucket, StagingPrefix: "quarantine", ReadyBucket: r.bucket, ReadyPrefix: "published"}}, nil
}

// Against a real bucket with Object Lock on (MinIO): nothing on it is converted;
// a file already kept as chunks is made one object before it is locked; and the
// lock then stops a real delete.
func TestChunkStorageOnARealLockedBucket(t *testing.T) {
	_, plainBucket := openTestProvider(t)
	if os.Getenv("TEST_S3_CREATE_BUCKET") != "true" {
		t.Skip("needs TEST_S3_CREATE_BUCKET=true to create a locked test bucket")
	}
	cfg, err := s3storage.LoadConfig()
	if err != nil {
		t.Skip("S3 configuration incomplete")
	}
	ctx := context.Background()
	client, err := s3storage.NewClient(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	bucket := plainBucket + "-locked"
	if _, err := client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(bucket)}); err != nil {
		if _, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket), ObjectLockEnabledForBucket: aws.Bool(true)}); err != nil {
			t.Skipf("this provider cannot create a bucket with Object Lock: %v", err)
		}
	}
	real := s3storage.NewObjectStorage(client)

	s := openSecurityStack(t, nil)
	acme := s.team(t, "chunkworm")
	dev, owner := acme.developer, acme.owner
	repo := postgres.NewChunkStorageRepository(s.db)
	packs := postgres.NewPackRepository(s.db)

	build1 := randomBytes(51, 3<<20)
	build2 := append(append([]byte{}, build1...), []byte("more")...)
	d := s.newDataset(t, dev, "worm-set")
	a1 := s.publishAt(t, dev, "one.bin", build1, inNamespace("worm-set"))
	a2 := s.publishAt(t, dev, "two.bin", build2, inNamespace("worm-set"))
	s.addMembers(t, dev, d.ID, memberOf(a1, "", ""), memberOf(a2, "", ""))
	if _, _, err := s.datasets.Snapshot(ctx, dev, d.ID, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.datasets.SetChunkStorage(ctx, owner, d.ID, true); err != nil {
		t.Fatal(err)
	}
	indexAllChunks(t, s)

	// The files' objects, for real, in the locked bucket.
	for _, f := range []struct {
		asset model.Asset
		data  []byte
	}{{a1, build1}, {a2, build2}} {
		out, err := client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(f.asset.ObjectKey), Body: bytes.NewReader(f.data)})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Pool.Exec(ctx, `UPDATE assets SET storage_etag = $2, storage_version_id = $3 WHERE id = $1`,
			f.asset.ID, aws.ToString(out.ETag), aws.ToString(out.VersionId)); err != nil {
			t.Fatal(err)
		}
	}
	readReal := func(key string) []byte {
		t.Helper()
		out, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
		if err != nil {
			t.Fatalf("read %s: %v", key, err)
		}
		defer out.Body.Close()
		raw, _ := io.ReadAll(out.Body)
		return raw
	}
	service := func(storage interfaces.StorageProvider, retire time.Duration) *chunkstore.Service {
		return chunkstore.New(chunkstore.Deps{Store: repo, Packs: packs, Router: realBucketRouter{storage, bucket}, Log: s.log,
			SourceRetireAfter: retire, UnusedAfter: time.Nanosecond, RecheckAfter: time.Nanosecond})
	}

	// Seen as it is, the bucket takes no conversion, and the objects stay.
	st, err := service(real, time.Hour).RunOnce(ctx)
	if err != nil || st.Converted != 0 || st.Failed != 2 {
		t.Fatalf("on a locked bucket: %+v %v", st, err)
	}
	if !bytes.Equal(readReal(a1.ObjectKey), build1) {
		t.Fatal("the object changed")
	}

	// Files chunked before the lock came on.
	if st, err := service(lockFreeView{real}, time.Nanosecond).RunOnce(ctx); err != nil || st.Converted != 2 || st.Retired != 2 {
		t.Fatalf("convert: %+v %v", st, err)
	}
	if _, err := client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(a1.ObjectKey)}); err == nil {
		t.Fatal("the original is still there")
	}

	lifecycle, err := assetservice.NewService(assetservice.Dependencies{
		Assets: s.ie.assets, Uploads: s.ie.uploads, Jobs: s.ie.processingEnv.jobs,
		Quarantine: postgres.NewQuarantineRepository(s.db), Policies: postgres.NewRetentionPolicyRepository(s.db),
		Holds: postgres.NewLegalHoldRepository(s.db), Deletions: postgres.NewDeletionRepository(s.db),
		Events: postgres.NewLifecycleEventRepository(s.db), Cursors: postgres.NewCursorRepository(s.db), Storage: real,
		Packs: packs, Shared: s.ie.assets, Locks: postgres.NewObjectLockRepository(s.db), LockSync: postgres.NewLockSyncRepository(s.db),
	}, assetservice.Config{Owner: "it-worm", QuarantineBucket: bucket, DeliveryBucket: bucket, DeliveryPrefix: "published",
		Platform:  assetservice.PlatformRetention{DefaultQuarantineRetention: time.Hour, QuarantineRetentionFloor: time.Hour},
		BatchSize: 50, Concurrency: 2, Lease: time.Minute, RecheckInterval: time.Minute, MaxDeletionsPerRun: 50,
		Retry: retry.Policy{BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond, MaxAttempts: 3}}, s.log)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lifecycle.PlaceHold(ctx, model.LegalHold{TenantID: dev.TenantID, Scope: model.ScopeAsset, ScopeKey: a1.ID, Reason: "matter", PlacedBy: owner.UserID}); err != nil {
		t.Fatal(err)
	}
	// The first try asks for one object; no lock goes astray meanwhile.
	if err := lifecycle.SyncRequestedLocks(ctx, &assetservice.CycleStats{}); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := s.db.Pool.QueryRow(ctx, `SELECT state FROM chunked_assets WHERE asset_id = $1`, a1.ID).Scan(&state); err != nil || state != "REHYDRATING" {
		t.Fatalf("state: %q %v", state, err)
	}
	if st, err := service(real, time.Nanosecond).RunOnce(ctx); err != nil || st.Rehydrated != 1 {
		t.Fatalf("rehydrate: %+v %v", st, err)
	}
	if !bytes.Equal(readReal(a1.ObjectKey), build1) {
		t.Fatal("the object written back differs")
	}
	// The lock follows from the queue, on the provider, without a sweep.
	if err := lifecycle.SyncRequestedLocks(ctx, &assetservice.CycleStats{}); err != nil {
		t.Fatal(err)
	}
	fresh, err := s.ie.assets.GetByID(ctx, dev.TenantID, a1.ID)
	if err != nil || fresh.StorageVersionID == "" {
		t.Fatalf("asset: %+v %v", fresh, err)
	}
	got, err := real.ObjectLockStatus(ctx, bucket, fresh.ObjectKey, fresh.StorageVersionID)
	if err != nil || !got.LegalHold {
		t.Fatalf("the provider's lock: %+v %v", got, err)
	}
	if err := real.DeleteObjectVersion(ctx, bucket, fresh.ObjectKey, fresh.StorageVersionID); err == nil {
		t.Fatal("a held version was deleted")
	}
	// Leave the shared test bucket clean.
	t.Cleanup(func() {
		_ = real.LockObjectVersion(context.Background(), bucket, fresh.ObjectKey, fresh.StorageVersionID, interfaces.VersionLock{})
		_ = real.DeleteObject(context.Background(), bucket, fresh.ObjectKey)
		_ = real.DeleteObject(context.Background(), bucket, a2.ObjectKey)
	})
}
