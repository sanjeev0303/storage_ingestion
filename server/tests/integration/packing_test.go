package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/compaction"
	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/storage/memory"
	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/storage/registry"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/config"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/metrics"
	assetservice "github.com/sanjeev0303/object-storage-platform/internal/service/asset"
	"github.com/sanjeev0303/object-storage-platform/internal/service/catalog"
	datasetservice "github.com/sanjeev0303/object-storage-platform/internal/service/dataset"
	"github.com/sanjeev0303/object-storage-platform/internal/service/delivery"
	"github.com/sanjeev0303/object-storage-platform/internal/service/packing"
)

// Phase 13: small published assets packed into one object per pack, over
// PostgreSQL and the stack's in-memory storage. Tenant connections each get
// a bucket of their own.

type packingRig struct {
	s      *securityStack
	repo   *postgres.PackRepository
	router *registry.Registry

	mu      sync.Mutex
	buckets map[string]*memory.ObjectStorage // by connection ID
}

func (s *securityStack) packingRig(t *testing.T) *packingRig {
	t.Helper()
	r := &packingRig{s: s, repo: postgres.NewPackRepository(s.db), buckets: map[string]*memory.ObjectStorage{}}
	cfg := config.Storage{BYOSEnabled: true, ClientCacheSize: 16, ClientCacheTTL: time.Minute, ConnectionCacheTTL: time.Millisecond,
		OperationTimeout: time.Minute, MaxConcurrentPerConnection: 8, TransferPartBytes: 5 << 20}
	reg, err := registry.New(registry.Dependencies{
		Config: cfg,
		Platform: interfaces.StorageLocation{
			StoragePlace: interfaces.StoragePlace{ConnectionID: model.PlatformConnectionID, StagingBucket: s.ie.bucket,
				StagingPrefix: "quarantine", ReadyBucket: s.ie.bucket, ReadyPrefix: "published"},
			Provider: model.ProviderPlatform, Storage: s.ie.storage,
		},
		Connections: postgres.NewStorageConnectionRepository(s.db), Sealer: testKeyring(t),
		Build: func(_ context.Context, c model.StorageConnection, _ model.StorageSecrets) (interfaces.StorageProvider, func(), error) {
			return r.bucket(c.ID), func() {}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	r.router = reg
	return r
}

func (r *packingRig) bucket(connectionID string) *memory.ObjectStorage {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.buckets[connectionID] == nil {
		r.buckets[connectionID] = memory.NewObjectStorage()
	}
	return r.buckets[connectionID]
}

// worker is a pack worker for tenants: everything published is old enough,
// sources stay an hour, and retired packs' objects go at once.
func (r *packingRig) worker(t *testing.T, owner string, registry *metrics.Registry, tenants []string, change func(*config.Packing)) *packing.Service {
	t.Helper()
	cfg := config.Packing{MinObjectBytes: 1, MaxObjectBytes: 1024, TargetPackBytes: 1 << 20, MaxPackBytes: 2 << 20,
		MaxEntries: 1000, SourceConcurrency: 4, ActiveWriters: 2, TenantActivePacks: 1, TenantsPerCycle: 16, Tenants: tenants,
		Lease: 5 * time.Second, MaxJobDuration: time.Minute, MaxAttempts: 3, RetryBase: time.Millisecond, RetryMax: 5 * time.Millisecond,
		SourceRetireAfter: time.Hour, RewriteDeadRatio: 1, MaxDeletionDelay: time.Hour, RemoveAfter: 0, PollInterval: time.Second}
	if change != nil {
		change(&cfg)
	}
	svc, err := packing.NewService(packing.Dependencies{Packs: r.repo, Assets: postgres.NewAssetRepository(r.s.db), Router: r.router, Metrics: registry, Log: r.s.log,
		Shared: postgres.NewAssetRepository(r.s.db)},
		packing.Config{Packing: cfg, Owner: owner})
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func (r *packingRig) run(t *testing.T, w *packing.Service) packing.CycleStats {
	t.Helper()
	stats, err := w.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("packing cycle: %v (%+v)", err, stats)
	}
	return stats
}

// packs are the tenant's packs, oldest first.
func (r *packingRig) packs(t *testing.T, tenantID string) []model.Pack {
	t.Helper()
	ctx := context.Background()
	rows, err := r.s.db.Pool.Query(ctx, `SELECT id FROM packs WHERE tenant_id = $1 ORDER BY created_at, id`, tenantID)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	out := make([]model.Pack, len(ids))
	for i, id := range ids {
		if out[i], err = r.repo.GetPack(ctx, tenantID, id); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

func (r *packingRig) entries(t *testing.T, packID string) []model.PackEntry {
	t.Helper()
	entries, err := r.repo.Entries(context.Background(), packID)
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

// stateOf is each asset's entry state in the pack.
func (r *packingRig) stateOf(t *testing.T, packID string) map[string]model.PackEntryState {
	t.Helper()
	out := map[string]model.PackEntryState{}
	for _, e := range r.entries(t, packID) {
		out[e.AssetID] = e.State
	}
	return out
}

// smallAssets publishes n small catalog objects under prefix as p and
// returns their versions with their bytes, by asset ID.
func (s *securityStack) smallAssets(t *testing.T, p model.Principal, prefix string, n int) ([]model.Asset, map[string][]byte) {
	t.Helper()
	assets := make([]model.Asset, n)
	data := map[string][]byte{}
	for i := range assets {
		b := textData(fmt.Sprintf("%s-%d", prefix, i), 40+7*i)
		assets[i] = s.publishAt(t, p, fmt.Sprintf("%s/item-%d.txt", prefix, i), b)
		data[assets[i].ID] = b
	}
	return assets, data
}

func (s *securityStack) download(p model.Principal, assetID string, rangeHeader string) ([]byte, error) {
	d, err := s.delivery.Open(context.Background(), p, assetID, delivery.DownloadRequest{Range: rangeHeader})
	if err != nil {
		return nil, err
	}
	defer d.Body.Close()
	return io.ReadAll(d.Body)
}

func (s *securityStack) mustDownload(t *testing.T, p model.Principal, assetID string, want []byte) {
	t.Helper()
	got, err := s.download(p, assetID, "")
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("download %s: %v, %d bytes, want %d", assetID, err, len(got), len(want))
	}
}

func sha256Of(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// A pack holds its assets back to back, in asset order, under a manifest
// that describes it exactly. Every asset keeps its identity and is read as
// exactly its own bytes, whole or in part, before and after its standalone
// copy is retired; the catalog, its reconciliation and dataset validation
// see no difference. Assets too large, too new or held are not packed.
func TestPackingPacksSmallAssetsAndServesEachExactly(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	acme := s.team(t, "pack-basic")
	dev := acme.developer
	assets, data := s.smallAssets(t, dev, "small", 5)
	large := s.publishAt(t, dev, "large.txt", textData("large", 3000))
	heldAsset := s.publishAt(t, dev, "held.txt", textData("held", 50))
	if _, err := postgres.NewLegalHoldRepository(s.db).Place(ctx, model.LegalHold{ID: acme.org.ID + "-hold", TenantID: acme.org.ID,
		Scope: model.ScopeAsset, ScopeKey: heldAsset.ID, Reason: "case", PlacedBy: "it", PlacedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	rig := s.packingRig(t)
	tenants := []string{acme.org.ID}

	young := rig.worker(t, "w-young", nil, tenants, func(c *config.Packing) { c.MinAge = time.Hour })
	if stats := rig.run(t, young); stats.Planned != 0 {
		t.Fatalf("nothing is old enough yet: %+v", stats)
	}

	w := rig.worker(t, "w1", nil, tenants, nil)
	stats := rig.run(t, w)
	if stats.Planned != 1 || stats.Published != 1 || stats.Failed != 0 {
		t.Fatalf("one pack planned and published: %+v", stats)
	}
	packs := rig.packs(t, acme.org.ID)
	if len(packs) != 1 || packs[0].State != model.PackPublished || packs[0].EntryCount != 5 {
		t.Fatalf("packs: %+v", packs)
	}
	p := packs[0]

	// Back to back in path order (small/item-0 .. item-4); the stored pack
	// is exactly that.
	var want []byte
	for i, e := range rig.entries(t, p.ID) {
		a := assets[i]
		if e.AssetID != a.ID || e.Offset != int64(len(want)) || e.Length != a.SizeBytes || e.State != model.PackEntryPublished ||
			e.SourceKey != a.ObjectKey || e.SourceETag != a.StorageETag || e.ChecksumSHA256 != a.ChecksumSHA256 {
			t.Fatalf("entry %d: %+v for asset %s", i, e, a.ID)
		}
		want = append(want, data[a.ID]...)
	}
	stored, ok := s.ie.storage.Object(p.ObjectKey)
	if !ok || !bytes.Equal(stored, want) || p.TotalBytes != int64(len(want)) || p.SHA256 != sha256Of(want) {
		t.Fatalf("the pack object is its entries in order (%d bytes stored, %d expected)", len(stored), len(want))
	}
	if p.ObjectKey != "published/"+acme.org.ID+"/packs/"+p.ID+".pack" {
		t.Fatalf("pack key %s", p.ObjectKey)
	}
	doc, ok := s.ie.storage.Object(p.ManifestKey)
	if !ok || sha256Of(doc) != p.ManifestSHA256 {
		t.Fatal("the manifest is stored and its digest recorded")
	}
	manifest, err := compaction.ParseManifest(doc, 1000)
	if err != nil || manifest.PackID != p.ID || manifest.EntryCount != 5 || manifest.PackSHA256 != p.SHA256 {
		t.Fatalf("manifest: %+v %v", manifest, err)
	}
	for _, other := range []model.Asset{large, heldAsset} {
		if _, packed, err := rig.repo.PackedLocation(ctx, acme.org.ID, other.ID); packed || err != nil {
			t.Fatalf("asset %s must not be packed: %v", other.FileName, err)
		}
	}

	// Each asset is exactly its own bytes, whole or in part.
	for _, a := range assets {
		s.mustDownload(t, dev, a.ID, data[a.ID])
	}
	middle := assets[2]
	if got, err := s.download(dev, middle.ID, "bytes=5-9"); err != nil || !bytes.Equal(got, data[middle.ID][5:10]) {
		t.Fatalf("range: %q %v", got, err)
	}
	if got, err := s.download(dev, middle.ID, fmt.Sprintf("bytes=%d-", middle.SizeBytes-3)); err != nil || !bytes.Equal(got, data[middle.ID][middle.SizeBytes-3:]) {
		t.Fatalf("a range to the end stops at the entry's end: %q %v", got, err)
	}
	link, err := s.delivery.CreateDownloadURL(ctx, dev, middle.ID, delivery.URLRequest{Mode: delivery.ModeStorage})
	if err != nil || link.Via != delivery.ModeToken {
		t.Fatalf("a packed asset gets a token link, never a URL to its pack: %+v %v", link, err)
	}

	// Sources stay until their grace period is over, then go.
	for _, a := range assets {
		if _, ok := s.ie.storage.Object(a.ObjectKey); !ok {
			t.Fatal("a source is kept through its grace period")
		}
	}
	retirer := rig.worker(t, "w-retire", nil, tenants, func(c *config.Packing) { c.SourceRetireAfter = 0 })
	if stats := rig.run(t, retirer); stats.SourcesRetired != 5 || stats.Planned != 0 {
		t.Fatalf("every source retired, nothing left to plan: %+v", stats)
	}
	for _, a := range assets {
		if _, ok := s.ie.storage.Object(a.ObjectKey); ok {
			t.Fatal("a retired source is deleted")
		}
		s.mustDownload(t, dev, a.ID, data[a.ID])
	}
	if _, ok := s.ie.storage.Object(large.ObjectKey); !ok {
		t.Fatal("an unpacked asset keeps its object")
	}
	// A connection test reads back an object the connection holds: never a
	// packed source that was retired.
	held, err := postgres.NewStorageConnectionRepository(s.db).HeldObject(ctx, acme.org.ID, large.StorageConnectionID)
	if _, ok := s.ie.storage.Object(held); err != nil || !ok {
		t.Fatalf("the held object %q is not stored (%v)", held, err)
	}

	// Reconciliation finds every packed version in its pack and does not
	// take packs for unreferenced objects.
	report, err := s.reconciler.Reconcile(ctx, catalog.ReconcileOptions{TenantID: acme.org.ID, DryRun: true, CheckStorage: true})
	if err != nil || report.Counts[model.FindingObjectMissing] != 0 || report.Counts[model.FindingSizeMismatch] != 0 ||
		report.Counts[model.FindingUnreferenced] != 0 || report.StorageErrors != 0 {
		t.Fatalf("reconcile: %+v %v", report, err)
	}

	// Dataset validation reads packed members' bytes from their packs.
	ds := s.newDataset(t, dev, "packed")
	for _, a := range assets {
		s.addMembers(t, dev, ds.ID, memberOf(a, "x", "train"))
	}
	if _, _, err := s.datasets.Snapshot(ctx, dev, ds.ID, ""); err != nil {
		t.Fatal(err)
	}
	reread := s.profileOf(t, acme.admin, "reread", 0, ruleOf("bytes", "content_checksum", model.SeverityError, `{"reverify":true}`))
	job, _, body := s.validated(t, dev, ds.ID, datasetservice.ValidationInput{ProfileID: reread.ID, Mode: model.ModeFull}, false)
	if job.BytesRead == 0 || body.Content.Read != 5 || len(findingCodes(s.allFindings(t, dev, job.ReportID, ""))) != 0 {
		t.Fatalf("packed members validate: %+v %+v", job, body.Content)
	}
}

// A source whose bytes are not the ones recorded fails its pack and is
// never packed again; the others are packed without it.
func TestPackingExcludesAMismatchedSource(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	acme := s.team(t, "pack-mismatch")
	assets, _ := s.smallAssets(t, acme.developer, "mm", 4)
	bad := assets[1]
	if _, err := s.db.Pool.Exec(ctx, `UPDATE assets SET checksum_sha256 = $2 WHERE id = $1`, bad.ID, sha256Of([]byte("other"))); err != nil {
		t.Fatal(err)
	}
	rig := s.packingRig(t)
	w := rig.worker(t, "w1", nil, []string{acme.org.ID}, nil)

	if stats := rig.run(t, w); stats.Failed != 1 || stats.Published != 0 || stats.PacksRemoved != 1 {
		t.Fatalf("the pack fails and its objects are removed: %+v", stats)
	}
	failed := rig.packs(t, acme.org.ID)[0]
	states := rig.stateOf(t, failed.ID)
	if failed.State != model.PackDeleted || states[bad.ID] != model.PackEntryExcluded || states[assets[0].ID] != model.PackEntryReleased {
		t.Fatalf("failed pack %s: %v", failed.State, states)
	}
	if _, ok := s.ie.storage.Object(failed.ObjectKey); ok {
		t.Fatal("a failed pack's object is removed")
	}

	if stats := rig.run(t, w); stats.Published != 1 {
		t.Fatalf("the rest are packed: %+v", stats)
	}
	next := rig.packs(t, acme.org.ID)[1]
	if next.EntryCount != 3 {
		t.Fatalf("the excluded asset stays out: %+v", next)
	}
	if _, ok := rig.stateOf(t, next.ID)[bad.ID]; ok {
		t.Fatal("the excluded asset is never packed again")
	}
}

// An asset whose object vanished while the asset still records it is not
// a stale plan: planned again it would fail again, every cycle, forever. It
// is excluded once, and the others are packed without it.
func TestPackingExcludesASourceWhoseObjectIsGone(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	acme := s.team(t, "pack-gone")
	assets, _ := s.smallAssets(t, acme.developer, "gone", 4)
	gone := assets[1]
	if err := s.ie.storage.DeleteObject(ctx, "", gone.ObjectKey); err != nil {
		t.Fatal(err)
	}
	rig := s.packingRig(t)
	w := rig.worker(t, "w1", nil, []string{acme.org.ID}, nil)

	if stats := rig.run(t, w); stats.Failed != 1 || stats.Published != 0 {
		t.Fatalf("the pack fails: %+v", stats)
	}
	failed := rig.packs(t, acme.org.ID)[0]
	if states := rig.stateOf(t, failed.ID); states[gone.ID] != model.PackEntryExcluded || states[assets[0].ID] != model.PackEntryReleased {
		t.Fatalf("failed pack: %v", states)
	}

	if stats := rig.run(t, w); stats.Published != 1 {
		t.Fatalf("the rest are packed: %+v", stats)
	}
	if stats := rig.run(t, w); stats.Planned != 0 || stats.Failed != 0 {
		t.Fatalf("nothing is planned again for the missing object: %+v", stats)
	}
	if n := len(rig.packs(t, acme.org.ID)); n != 2 {
		t.Fatalf("%d packs, want the failed one and the published one", n)
	}
}

// Two workers planning at once reserve each asset once. A worker that
// stopped while writing leaves a pack another worker writes again from
// scratch; one that stopped while verifying leaves a pack that is verified
// and published as it was written. A repeated event changes nothing.
func TestPackingRecoversFromCrashesAndDuplicateWorkers(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	acme := s.team(t, "pack-crash")
	tenant := acme.org.ID
	assets, data := s.smallAssets(t, acme.developer, "crash", 6)
	rig := s.packingRig(t)
	w1 := rig.worker(t, "w1", nil, []string{tenant}, nil)
	w2 := rig.worker(t, "w2", nil, []string{tenant}, nil)

	var wg sync.WaitGroup
	for _, w := range []*packing.Service{w1, w2} {
		wg.Add(1)
		go func(w *packing.Service) {
			defer wg.Done()
			var stats packing.CycleStats
			if err := w.Plan(ctx, &stats); err != nil {
				t.Error(err)
			}
		}(w)
	}
	wg.Wait()
	packs := rig.packs(t, tenant)
	if len(packs) != 1 || packs[0].EntryCount != 6 {
		t.Fatalf("one pack, every asset once: %+v", packs)
	}
	p := packs[0]

	// Crash while writing: half a pack and an unfinished upload are left.
	claimed, err := rig.repo.ClaimPacks(ctx, "crashed", time.Now().UTC(), time.Minute, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: %v", err)
	}
	// A claim's owner is the worker's id and the attempt: "crashed#1".
	crashed := claimed[0].LeaseOwner
	if err := rig.repo.AdvancePack(ctx, p.ID, crashed, model.PackPlanned, model.PackWriting, interfaces.PackChange{}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	s.ie.storage.PutObject(p.ObjectKey, []byte("half a pack"))
	s.ie.storage.AddUpload(p.ObjectKey)
	if stats := rig.run(t, w2); stats.Built != 0 {
		t.Fatalf("a leased pack is left to its worker: %+v", stats)
	}
	expireLease(t, s, p.ID)
	if stats := rig.run(t, w2); stats.Published != 1 {
		t.Fatalf("another worker takes it over: %+v", stats)
	}
	var want []byte // in path order, the order they were made
	for _, a := range assets {
		want = append(want, data[a.ID]...)
	}
	if stored, _ := s.ie.storage.Object(p.ObjectKey); !bytes.Equal(stored, want) {
		t.Fatal("the pack is written again from scratch")
	}
	if uploads, _ := s.ie.storage.ListMultipartUploads(ctx, s.ie.bucket, p.ObjectKey); len(uploads) != 0 {
		t.Fatal("the unfinished upload is aborted")
	}

	// Crash while verifying, after a complete write.
	_, moreData := s.smallAssets(t, acme.developer, "verify", 3)
	if stats := rig.run(t, w1); stats.Planned != 1 || stats.Published != 1 {
		t.Fatalf("the next pack: %+v", stats)
	}
	extra, extraData := s.smallAssets(t, acme.developer, "verify2", 2)
	var planned packing.CycleStats
	if err := w1.Plan(ctx, &planned); err != nil || planned.Planned != 1 {
		t.Fatalf("plan: %+v %v", planned, err)
	}
	all := rig.packs(t, tenant)
	q := all[len(all)-1]
	again, err := rig.repo.ClaimPacks(ctx, "crashed", time.Now().UTC(), time.Minute, 1)
	if err != nil || len(again) != 1 {
		t.Fatalf("claim: %v", err)
	}
	crashed = again[0].LeaseOwner
	if err := rig.repo.AdvancePack(ctx, q.ID, crashed, model.PackPlanned, model.PackWriting, interfaces.PackChange{}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	var written []byte // in path order
	for _, a := range extra {
		written = append(written, extraData[a.ID]...)
	}
	meta, err := s.ie.storage.WriteObject(ctx, s.ie.bucket, q.ObjectKey, bytes.NewReader(written), int64(len(written)), "application/octet-stream")
	if err != nil {
		t.Fatal(err)
	}
	change := interfaces.PackChange{SHA256: sha256Of(written), StorageETag: meta.ETag, StorageVersionID: meta.VersionID}
	if err := rig.repo.AdvancePack(ctx, q.ID, crashed, model.PackWriting, model.PackVerifying, change, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	expireLease(t, s, q.ID)
	writes := s.ie.storage.WriteCalls
	if stats := rig.run(t, w2); stats.Published != 1 {
		t.Fatalf("verified and published: %+v", stats)
	}
	if s.ie.storage.WriteCalls != writes+1 || s.ie.storage.ETag(q.ObjectKey) != meta.ETag {
		t.Fatal("only the manifest is written; the pack is kept as it was")
	}
	for id, b := range extraData {
		s.mustDownload(t, acme.developer, id, b)
	}
	for id, b := range moreData {
		s.mustDownload(t, acme.developer, id, b)
	}

	// The event for a pack already published is a no-op.
	if result, err := w1.BuildPack(ctx, tenant, q.ID); err != nil || result == "pack published" {
		t.Fatalf("a repeated event: %q %v", result, err)
	}
	if got, _ := rig.repo.GetPack(ctx, tenant, q.ID); got.State != model.PackPublished || got.StorageETag != meta.ETag {
		t.Fatalf("unchanged: %+v", got)
	}
}

func expireLease(t *testing.T, s *securityStack, packID string) {
	t.Helper()
	if _, err := s.db.Pool.Exec(context.Background(), `UPDATE packs SET lease_expires_at = now() - interval '1 second' WHERE id = $1`, packID); err != nil {
		t.Fatal(err)
	}
}

// makeDue makes the asset's deletion due now, as if its recheck time had
// come.
func makeDue(t *testing.T, s *securityStack, assetID string) {
	t.Helper()
	if _, err := s.db.Pool.Exec(context.Background(), `UPDATE deletion_operations SET next_attempt_at = now() - interval '1 second' WHERE asset_id = $1`, assetID); err != nil {
		t.Fatal(err)
	}
}

func deletionOf(t *testing.T, s *securityStack, assetID string) (state string, attempts int) {
	t.Helper()
	err := s.db.Pool.QueryRow(context.Background(), `SELECT state, attempt_count FROM deletion_operations WHERE asset_id = $1`, assetID).Scan(&state, &attempts)
	if err != nil {
		t.Fatal(err)
	}
	return state, attempts
}

// Deleting a packed asset stops it being served at once. Its deletion
// waits, without using up attempts, until the pack is rewritten without it
// and the old pack's objects are gone; then it completes.
func TestPackedAssetDeletionWaitsForTheRewrite(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	acme := s.team(t, "pack-delete")
	dev := acme.developer
	assets, data := s.smallAssets(t, dev, "del", 4)
	rig := s.packingRig(t)
	tenants := []string{acme.org.ID}
	w := rig.worker(t, "w1", nil, tenants, func(c *config.Packing) { c.SourceRetireAfter = 0 })
	if stats := rig.run(t, w); stats.Published != 1 || stats.SourcesRetired != 4 {
		t.Fatalf("packed, and with no grace period the sources retired: %+v", stats)
	}
	old := rig.packs(t, acme.org.ID)[0]
	gone := assets[1]

	if _, err := s.lifecycle.RequestDeletion(ctx, dev, gone.ID); err != nil {
		t.Fatal(err)
	}
	var cycle assetservice.CycleStats
	if err := s.lifecycle.ProcessDeletions(ctx, &cycle); err != nil || cycle.WaitingForPack != 1 || cycle.Failed != 0 {
		t.Fatalf("the deletion waits for the pack: %+v %v", cycle, err)
	}
	if state, attempts := deletionOf(t, s, gone.ID); state != string(model.DeletionRetryWait) || attempts != 0 {
		t.Fatalf("waiting uses no attempt: %s %d", state, attempts)
	}
	if rig.stateOf(t, old.ID)[gone.ID] != model.PackEntryDeleted {
		t.Fatal("the entry is released at once")
	}
	if _, err := s.download(dev, gone.ID, ""); err == nil {
		t.Fatal("a deleted asset is not served from its pack")
	}
	makeDue(t, s, gone.ID)
	cycle = assetservice.CycleStats{}
	if err := s.lifecycle.ProcessDeletions(ctx, &cycle); err != nil || cycle.WaitingForPack != 1 {
		t.Fatalf("still waiting: %+v %v", cycle, err)
	}
	if _, attempts := deletionOf(t, s, gone.ID); attempts != 0 {
		t.Fatalf("still no attempt used: %d", attempts)
	}

	// One deleted entry of four is under the ratio; the deletion's wait
	// past MaxDeletionDelay forces the rewrite.
	if stats := rig.run(t, w); stats.Rewrites != 0 {
		t.Fatalf("not due yet: %+v", stats)
	}
	impatient := rig.worker(t, "w2", nil, tenants, func(c *config.Packing) { c.MaxDeletionDelay = time.Nanosecond })
	if stats := rig.run(t, impatient); stats.Rewrites != 1 {
		t.Fatalf("rewrite planned: %+v", stats)
	}
	if stats := rig.run(t, impatient); stats.Published != 1 || stats.PacksRemoved != 1 {
		t.Fatalf("rewrite published, old pack removed: %+v", stats)
	}
	packs := rig.packs(t, acme.org.ID)
	old, rewritten := packs[0], packs[1]
	if old.State != model.PackDeleted || rewritten.RewriteOf != old.ID || rewritten.EntryCount != 3 {
		t.Fatalf("old %s, new %+v", old.State, rewritten)
	}
	if _, ok := s.ie.storage.Object(old.ObjectKey); ok {
		t.Fatal("the old pack's object is deleted")
	}
	stored, _ := s.ie.storage.Object(rewritten.ObjectKey)
	if bytes.Contains(stored, data[gone.ID]) {
		t.Fatal("the deleted asset's bytes are not in the new pack")
	}
	for _, a := range assets {
		if a.ID != gone.ID {
			s.mustDownload(t, dev, a.ID, data[a.ID])
		}
	}
	if states := rig.stateOf(t, old.ID); states[assets[0].ID] != model.PackEntryMoved {
		t.Fatalf("live entries moved: %v", states)
	}

	makeDue(t, s, gone.ID)
	cycle = assetservice.CycleStats{}
	if err := s.lifecycle.ProcessDeletions(ctx, &cycle); err != nil || cycle.Completed != 1 {
		t.Fatalf("the deletion completes: %+v %v", cycle, err)
	}
	if state, _ := deletionOf(t, s, gone.ID); state != string(model.DeletionCompleted) {
		t.Fatalf("deletion %s", state)
	}
}

// A rewrite whose old pack has lost its object: every entry it tries to
// move fails. Each is excluded from the old pack once, so rewrites stop being
// planned instead of failing every cycle, the emptied pack is retired, and
// the deletion that asked for the rewrite completes.
func TestPackingRewriteOfADamagedPackDoesNotLoop(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	acme := s.team(t, "pack-loop")
	dev := acme.developer
	assets, _ := s.smallAssets(t, dev, "loop", 4)
	rig := s.packingRig(t)
	tenants := []string{acme.org.ID}
	w := rig.worker(t, "w1", nil, tenants, func(c *config.Packing) { c.SourceRetireAfter = 0 })
	if stats := rig.run(t, w); stats.Published != 1 || stats.SourcesRetired != 4 {
		t.Fatalf("packed and retired: %+v", stats)
	}
	old := rig.packs(t, acme.org.ID)[0]
	gone := assets[1]
	if _, err := s.lifecycle.RequestDeletion(ctx, dev, gone.ID); err != nil {
		t.Fatal(err)
	}
	var cycle assetservice.CycleStats
	if err := s.lifecycle.ProcessDeletions(ctx, &cycle); err != nil || cycle.WaitingForPack != 1 {
		t.Fatalf("waits for the pack: %+v %v", cycle, err)
	}
	// The pack object is lost out of band: no rewrite can read it.
	if err := s.ie.storage.DeleteObject(ctx, s.ie.bucket, old.ObjectKey); err != nil {
		t.Fatal(err)
	}

	impatient := rig.worker(t, "w2", nil, tenants, func(c *config.Packing) { c.MaxDeletionDelay = time.Nanosecond })
	failed := 0
	for range 10 {
		failed += rig.run(t, impatient).Failed
	}
	if failed != 3 {
		t.Fatalf("%d failed rewrites; want one for each of the three live entries, then none", failed)
	}
	if n := len(rig.packs(t, acme.org.ID)); n != 4 {
		t.Fatalf("%d packs; want the old one and three failed rewrites, no more", n)
	}
	states := rig.stateOf(t, old.ID)
	for _, a := range assets {
		want := model.PackEntryExcluded
		if a.ID == gone.ID {
			want = model.PackEntryDeleted
		}
		if states[a.ID] != want {
			t.Fatalf("old pack entry of %s is %s, want %s", a.ID, states[a.ID], want)
		}
	}
	makeDue(t, s, gone.ID)
	cycle = assetservice.CycleStats{}
	if err := s.lifecycle.ProcessDeletions(ctx, &cycle); err != nil || cycle.Completed != 1 {
		t.Fatalf("the deletion completes once the pack is retired: %+v %v", cycle, err)
	}
}

// A rewrite can move an entry before its standalone copy was retired (a
// grace period, a hold). The copy is still retired afterwards, and the
// asset is still served, from its new pack.
func TestPackingRetiresTheStandaloneCopyOfAMovedEntry(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	acme := s.team(t, "pack-moved")
	dev := acme.developer
	assets, data := s.smallAssets(t, dev, "moved", 4)
	rig := s.packingRig(t)
	tenants := []string{acme.org.ID}
	w := rig.worker(t, "w1", nil, tenants, nil) // a 24h grace period: nothing retired yet
	if stats := rig.run(t, w); stats.Published != 1 || stats.SourcesRetired != 0 {
		t.Fatalf("packed, sources kept: %+v", stats)
	}
	gone := assets[1]
	if _, err := s.lifecycle.RequestDeletion(ctx, dev, gone.ID); err != nil {
		t.Fatal(err)
	}
	var cycle assetservice.CycleStats
	if err := s.lifecycle.ProcessDeletions(ctx, &cycle); err != nil {
		t.Fatal(err)
	}
	impatient := rig.worker(t, "w2", nil, tenants, func(c *config.Packing) { c.MaxDeletionDelay = time.Nanosecond })
	rig.run(t, impatient)
	if stats := rig.run(t, impatient); stats.Published != 1 {
		t.Fatalf("rewrite published: %+v", stats)
	}

	retirer := rig.worker(t, "w3", nil, tenants, func(c *config.Packing) { c.SourceRetireAfter = 0 })
	if stats := rig.run(t, retirer); stats.SourcesRetired != 3 {
		t.Fatalf("%d standalone copies retired; want the three moved entries'", stats.SourcesRetired)
	}
	for _, a := range assets {
		if a.ID == gone.ID {
			continue
		}
		if _, ok := s.ie.storage.Object(a.ObjectKey); ok {
			t.Fatalf("the standalone copy of %s is still stored", a.ID)
		}
		s.mustDownload(t, dev, a.ID, data[a.ID])
	}
}

// An asset deleted while its pack is being built: the pack's source read
// finds it gone, the pack fails at once and is removed, the deletion
// completes, and the rest are packed again.
func TestPackingRacesWithDeletion(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	acme := s.team(t, "pack-race")
	dev := acme.developer
	assets, data := s.smallAssets(t, dev, "race", 3)
	rig := s.packingRig(t)
	w := rig.worker(t, "w1", nil, []string{acme.org.ID}, nil)
	var planned packing.CycleStats
	if err := w.Plan(ctx, &planned); err != nil || planned.Planned != 1 {
		t.Fatalf("plan: %+v %v", planned, err)
	}
	gone := assets[0]
	if _, err := s.lifecycle.RequestDeletion(ctx, dev, gone.ID); err != nil {
		t.Fatal(err)
	}
	var cycle assetservice.CycleStats
	if err := s.lifecycle.ProcessDeletions(ctx, &cycle); err != nil || cycle.WaitingForPack != 1 {
		t.Fatalf("a planned pack holds the asset too: %+v %v", cycle, err)
	}

	if stats := rig.run(t, w); stats.Failed != 1 || stats.Retried != 0 || stats.PacksRemoved != 1 {
		t.Fatalf("a vanished source fails the pack at once: %+v", stats)
	}
	failed := rig.packs(t, acme.org.ID)[0]
	if states := rig.stateOf(t, failed.ID); states[gone.ID] != model.PackEntryReleased || states[assets[1].ID] != model.PackEntryReleased {
		t.Fatalf("nothing is excluded for a deletion: %v", states)
	}

	makeDue(t, s, gone.ID)
	cycle = assetservice.CycleStats{}
	if err := s.lifecycle.ProcessDeletions(ctx, &cycle); err != nil || cycle.Completed != 1 {
		t.Fatalf("the deletion completes: %+v %v", cycle, err)
	}
	if stats := rig.run(t, w); stats.Published != 1 {
		t.Fatalf("the rest are packed: %+v", stats)
	}
	for _, a := range assets[1:] {
		s.mustDownload(t, dev, a.ID, data[a.ID])
	}
}

// A tenant's assets are packed on the connection they were published on,
// never mixed across connections or tenants, and never on a provider that
// cannot read a range: those assets stay as they are without holding up
// the tenant's others.
func TestPackingStaysOnEachConnection(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	acme := s.team(t, "pack-byos")
	other := s.team(t, "pack-byos-b")
	rig := s.packingRig(t)
	s.delivery.Route(rig.router)
	connections := postgres.NewStorageConnectionRepository(s.db)
	keyring := testKeyring(t)

	ranged := activeConnection(t, connections, acme.org.ID, "Ranged bucket", keyring)
	if _, err := connections.RecordTest(ctx, acme.org.ID, ranged.ID, interfaces.ConnectionTestRecord{OK: true, At: time.Now().UTC(),
		Capabilities: &model.ProviderCapabilities{PresignedUpload: true, Multipart: true, RangeReads: true}}); err != nil {
		t.Fatal(err)
	}
	plain := activeConnection(t, connections, acme.org.ID, "No ranges", keyring)
	foreign := activeConnection(t, connections, other.org.ID, "Other tenant", keyring)
	if _, err := connections.RecordTest(ctx, other.org.ID, foreign.ID, interfaces.ConnectionTestRecord{OK: true, At: time.Now().UTC(),
		Capabilities: &model.ProviderCapabilities{RangeReads: true}}); err != nil {
		t.Fatal(err)
	}

	// Assets published on a connection: their bytes move to its bucket.
	moveTo := func(a model.Asset, connectionID string, b []byte) {
		t.Helper()
		rig.bucket(connectionID).PutObject(a.ObjectKey, b)
		if err := s.ie.storage.DeleteObject(ctx, s.ie.bucket, a.ObjectKey); err != nil {
			t.Fatal(err)
		}
		// A binding never changes (migration 000015's trigger), and this
		// stack only publishes on the platform. The trigger is set aside for
		// this one statement, in its own transaction, to stand in for an
		// asset published on the connection; every binding here belongs to
		// the asset's own tenant, as the trigger would require.
		tx, err := s.db.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx, `ALTER TABLE assets DISABLE TRIGGER tr_assets_storage_tenant`); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `UPDATE assets SET storage_connection_id = $2 WHERE id = $1`, a.ID, connectionID); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `ALTER TABLE assets ENABLE TRIGGER tr_assets_storage_tenant`); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	all, data := s.smallAssets(t, acme.developer, "conn", 9)
	byConnection := map[string]string{}
	for i, a := range all {
		switch i % 3 {
		case 0:
			moveTo(a, plain.ID, data[a.ID])
			byConnection[a.ID] = plain.ID
		case 1:
			moveTo(a, ranged.ID, data[a.ID])
			byConnection[a.ID] = ranged.ID
		default:
			byConnection[a.ID] = model.PlatformConnectionID
		}
	}
	theirs, theirData := s.smallAssets(t, other.developer, "theirs", 2)
	for _, a := range theirs {
		moveTo(a, foreign.ID, theirData[a.ID])
	}

	registry := metrics.New()
	w := rig.worker(t, "w1", registry, []string{acme.org.ID, other.org.ID}, func(c *config.Packing) { c.SourceRetireAfter = 0 })
	for range 3 {
		rig.run(t, w)
	}
	if registry.Snapshot()["packing_unsupported_connections"] == 0 {
		t.Fatal("the connection without ranged reads is passed over")
	}
	packs := rig.packs(t, acme.org.ID)
	if len(packs) != 2 {
		t.Fatalf("one pack per packable connection: %+v", packs)
	}
	for _, p := range packs {
		if p.State != model.PackPublished || p.EntryCount != 3 {
			t.Fatalf("pack %+v", p)
		}
		for _, e := range rig.entries(t, p.ID) {
			if byConnection[e.AssetID] != p.ConnectionID {
				t.Fatalf("asset %s on %s packed on %s", e.AssetID, byConnection[e.AssetID], p.ConnectionID)
			}
		}
		own, elsewhere := s.ie.storage, rig.bucket(ranged.ID)
		if p.ConnectionID != model.PlatformConnectionID {
			own, elsewhere = rig.bucket(p.ConnectionID), s.ie.storage
		}
		if _, ok := own.Object(p.ObjectKey); !ok {
			t.Fatalf("pack %s is stored on its own connection", p.ID)
		}
		if _, ok := elsewhere.Object(p.ObjectKey); ok {
			t.Fatalf("pack %s is stored on another connection too", p.ID)
		}
	}
	theirPacks := rig.packs(t, other.org.ID)
	if len(theirPacks) != 1 || theirPacks[0].ConnectionID != foreign.ID {
		t.Fatalf("the other tenant's pack: %+v", theirPacks)
	}

	// Every asset still reads as itself, sources retired or not; another
	// tenant reads none of them.
	for _, a := range all {
		s.mustDownload(t, acme.developer, a.ID, data[a.ID])
		if _, err := s.download(other.developer, a.ID, ""); !errors.Is(err, delivery.ErrNotFound) {
			t.Fatalf("another tenant: %v", err)
		}
	}
	for _, a := range all {
		if byConnection[a.ID] == ranged.ID {
			if _, ok := rig.bucket(ranged.ID).Object(a.ObjectKey); ok {
				t.Fatal("a packed source on the tenant's bucket is retired there")
			}
		}
		if byConnection[a.ID] == plain.ID {
			if _, ok := rig.bucket(plain.ID).Object(a.ObjectKey); !ok {
				t.Fatal("an unpacked asset keeps its object")
			}
		}
	}
	for _, a := range theirs {
		s.mustDownload(t, other.developer, a.ID, theirData[a.ID])
	}
}
