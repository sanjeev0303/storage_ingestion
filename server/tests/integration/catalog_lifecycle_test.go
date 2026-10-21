package integration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	eventdto "github.com/sanjeev0303/object-storage-platform/internal/dto/event"
	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/service/catalog"
	"github.com/sanjeev0303/object-storage-platform/internal/service/delivery"
)

// Phase 9: versions through the lifecycle, reconciliation, and records
// from before the catalog.

// liveDeletion is an asset's live deletion operation.
func (s *securityStack) liveDeletion(t *testing.T, tenantID, assetID string) model.DeletionOperation {
	t.Helper()
	op, err := postgres.NewDeletionRepository(s.db).GetLive(context.Background(), tenantID, assetID)
	if err != nil {
		t.Fatalf("live deletion of %s: %v", assetID, err)
	}
	return op
}

func (s *securityStack) stored(key string) bool {
	_, err := s.ie.storage.HeadObject(context.Background(), s.ie.bucket, key)
	return err == nil
}

func TestVersionAwareDeletionAndRetention(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	acme := s.team(t, "cat-del")
	dev := acme.developer
	v1 := s.publishAt(t, dev, "kept.txt", textData("1", 50))
	v2 := s.publishAt(t, dev, "kept.txt", textData("2", 60))
	v3 := s.publishAt(t, dev, "kept.txt", textData("3", 70))
	objectID := v1.ObjectID
	current := func() model.CatalogObject {
		t.Helper()
		o, err := s.catalog.Get(ctx, dev, objectID)
		if err != nil {
			t.Fatal(err)
		}
		return o
	}

	// Deleting an older version leaves the current one, and its purge
	// removes only that version's bytes.
	if _, err := s.catalog.DeleteVersion(ctx, dev, objectID, 1); err != nil {
		t.Fatal(err)
	}
	if o := current(); o.CurrentAssetID != v3.ID {
		t.Fatalf("deleting version 1 changed the current version: %+v", o)
	}
	if _, err := s.lifecycle.RunDeletion(ctx, acme.org.ID, s.liveDeletion(t, acme.org.ID, v1.ID).ID); err != nil {
		t.Fatal(err)
	}
	if s.stored(v1.ObjectKey) || !s.stored(v2.ObjectKey) || !s.stored(v3.ObjectKey) {
		t.Fatal("the purge removes exactly the deleted version's object")
	}

	// Deleting the current version makes the newest remaining one current.
	if _, err := s.catalog.DeleteVersion(ctx, dev, objectID, 3); err != nil {
		t.Fatal(err)
	}
	if o := current(); o.CurrentAssetID != v2.ID || o.CurrentVersion != 2 || o.SizeBytes != 60 {
		t.Fatalf("after deleting the current version: %+v", o)
	}
	// Restoring it does not take the current version back; promoting does.
	if _, err := s.lifecycle.Restore(ctx, dev, v3.ID); err != nil {
		t.Fatal(err)
	}
	o := current()
	if o.CurrentAssetID != v2.ID {
		t.Fatalf("a restored version is not current by itself: %+v", o)
	}
	if o, err := s.catalog.Promote(ctx, dev, objectID, 3, o.Revision); err != nil || o.CurrentAssetID != v3.ID {
		t.Fatalf("promote: %+v %v", o, err)
	}

	// Retention protects one version: its deletion is accepted, its bytes
	// stay until the retention ends.
	thirtyDays := 30 * 24 * time.Hour
	if _, err := s.lifecycle.SetPolicyFor(ctx, acme.admin, model.RetentionPolicy{
		Scope: model.ScopeAsset, ScopeKey: v2.ID, MinRetention: &thirtyDays, Reason: "contract",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.catalog.DeleteVersion(ctx, dev, objectID, 2); err != nil {
		t.Fatal(err)
	}
	op := s.liveDeletion(t, acme.org.ID, v2.ID)
	if _, err := s.lifecycle.RunDeletion(ctx, acme.org.ID, op.ID); err != nil {
		t.Fatal(err)
	}
	if after, err := postgres.NewDeletionRepository(s.db).Get(ctx, acme.org.ID, op.ID); err != nil ||
		after.State == model.DeletionCompleted || !s.stored(v2.ObjectKey) {
		t.Fatalf("a retained version's bytes must stay: %+v %v", after, err)
	}

	// A legal hold on the current version, then a logical deletion of the
	// object: every version goes to the lifecycle, the object leaves the
	// listing, and the held version's bytes stay.
	if _, err := s.lifecycle.PlaceHoldFor(ctx, acme.admin, model.LegalHold{Scope: model.ScopeAsset, ScopeKey: v3.ID, Reason: "case 42"}); err != nil {
		t.Fatal(err)
	}
	deleted, inFlight, err := s.catalog.DeleteObject(ctx, dev, objectID)
	if err != nil || inFlight != 0 || deleted.Published() || deleted.DeletedAt == nil {
		t.Fatalf("logical deletion: %+v %d %v", deleted, inFlight, err)
	}
	if page, _ := s.catalog.List(ctx, dev, catalog.ListRequest{AnyDataset: true}); len(page.Objects) != 0 {
		t.Fatalf("a deleted object is not listed: %+v", page.Objects)
	}
	if _, err := s.lifecycle.RunDeletion(ctx, acme.org.ID, s.liveDeletion(t, acme.org.ID, v3.ID).ID); err != nil {
		t.Fatal(err)
	}
	if !s.stored(v3.ObjectKey) {
		t.Fatal("a held version's bytes must stay")
	}

	// The path keeps its object and history: a new upload is version 4.
	v4 := s.publishAt(t, dev, "kept.txt", textData("4", 80))
	if o := current(); v4.ObjectID != objectID || v4.Version != 4 || o.CurrentAssetID != v4.ID || o.DeletedAt != nil {
		t.Fatalf("publishing after a logical deletion: %+v, %+v", v4, o)
	}
}

func TestPurgeIsBlockedWhileTheCatalogReferencesTheVersion(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	acme := s.team(t, "cat-ref")
	dev := acme.developer
	v1 := s.publishAt(t, dev, "ref.txt", textData("1", 50))
	if _, err := s.catalog.DeleteVersion(ctx, dev, v1.ObjectID, 1); err != nil {
		t.Fatal(err)
	}
	// A catalog that still names the version current, however it came to:
	// the purge refuses to remove bytes an object depends on.
	if _, err := s.db.Pool.Exec(ctx, `UPDATE catalog_objects SET current_asset_id = $1, current_version = 1 WHERE id = $2`,
		v1.ID, v1.ObjectID); err != nil {
		t.Fatal(err)
	}
	op := s.liveDeletion(t, acme.org.ID, v1.ID)
	if _, err := s.lifecycle.RunDeletion(ctx, acme.org.ID, op.ID); err != nil {
		t.Fatal(err)
	}
	after, err := postgres.NewDeletionRepository(s.db).Get(ctx, acme.org.ID, op.ID)
	if err != nil || after.State != model.DeletionProtected || !strings.Contains(after.ProtectedReason, "catalog object") {
		t.Fatalf("the purge must wait for the catalog reference: %+v %v", after, err)
	}
	if !s.stored(v1.ObjectKey) {
		t.Fatal("the bytes must stay")
	}
}

func TestProviderVersioningIsRecordedWhereSupported(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	acme := s.team(t, "cat-pv")
	dev := acme.developer

	// A bucket without versioning: no provider version ID; every logical
	// version still has its own key.
	plain := s.publishAt(t, dev, "plain.txt", textData("p", 40))
	if c, _ := s.ie.storage.Capabilities(ctx, s.ie.bucket); c.ProviderVersioning || plain.StorageVersionID != "" {
		t.Fatalf("unversioned bucket: %+v", plain)
	}

	// A versioned bucket: the published object's version ID is recorded,
	// and checks read that exact version.
	s.ie.storage.Versioning = true
	versioned := s.publishAt(t, dev, "versioned.txt", textData("v", 40))
	c, _ := s.ie.storage.Capabilities(ctx, s.ie.bucket)
	if !c.ProviderVersioning || versioned.StorageVersionID == "" {
		t.Fatalf("versioned bucket: %+v, capabilities %+v", versioned, c)
	}
	if v, err := s.catalog.Version(ctx, dev, versioned.ObjectID, 1); err != nil || v.StorageVersionID != versioned.StorageVersionID {
		t.Fatalf("the version carries its provider version: %+v %v", v, err)
	}
	report, err := s.reconciler.Reconcile(ctx, catalog.ReconcileOptions{TenantID: acme.org.ID, DryRun: true, CheckStorage: true})
	if err != nil || report.Counts[model.FindingObjectMissing] != 0 || report.VersionsChecked != 2 {
		t.Fatalf("both versions are found, each its own way: %+v %v", report, err)
	}
	// Overwritten out of band, the key holds another version now, but the
	// recorded one is still there: it is what is checked.
	s.ie.storage.PutObject(versioned.ObjectKey, textData("other", 40))
	report, err = s.reconciler.Reconcile(ctx, catalog.ReconcileOptions{TenantID: acme.org.ID, DryRun: true, CheckStorage: true})
	if err != nil || report.Counts[model.FindingObjectMissing] != 0 {
		t.Fatalf("an overwrite of the key does not hide the recorded version: %+v %v", report, err)
	}
	// Once that version itself is gone it is reported, although the key
	// still holds bytes.
	if err := s.ie.storage.DeleteObjectVersion(ctx, s.ie.bucket, versioned.ObjectKey, versioned.StorageVersionID); err != nil {
		t.Fatal(err)
	}
	report, err = s.reconciler.Reconcile(ctx, catalog.ReconcileOptions{TenantID: acme.org.ID, DryRun: true, CheckStorage: true})
	if err != nil || report.Counts[model.FindingObjectMissing] != 1 || report.Findings[0].AssetID != versioned.ID {
		t.Fatalf("the recorded version is checked, not the key: %+v %v", report, err)
	}
}

func TestDuplicateEventsCreateOneVersion(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	acme := s.team(t, "cat-dupe")
	dev := acme.developer

	data := textData("once", 70)
	upload := s.startUpload(t, dev, catalogUpload("once.txt", data), data)
	if _, err := s.ie.single.CompleteFor(ctx, dev, upload.ID); err != nil {
		t.Fatal(err)
	}
	// upload.completed delivered three times.
	for i := 0; i < 3; i++ {
		if _, err := s.ie.proc.Intake(ctx, acme.org.ID, upload.ID); err != nil {
			t.Fatal(err)
		}
	}
	o := s.object(t, dev, "", "once.txt")
	if page, _ := s.catalog.Versions(ctx, dev, o.ID, 0, 10); len(page.Versions) != 1 || o.Versions != 1 {
		t.Fatalf("one upload, one version: %d versions, %+v", len(page.Versions), o)
	}

	// Without versioning, a new version replaces the current one: the
	// replaced version is handed to the lifecycle by an event, and a
	// duplicate of the event does nothing more.
	if _, err := s.catalog.SetVersioning(ctx, dev, o.ID, o.Revision, model.VersioningDisabled); err != nil {
		t.Fatal(err)
	}
	first, err := s.catalog.Version(ctx, dev, o.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	second := s.publishAt(t, dev, "once.txt", textData("twice", 70))
	var events int
	if err := s.db.Pool.QueryRow(ctx, `
		SELECT count(*) FROM outbox_events WHERE tenant_id = $1 AND event_type = $2 AND aggregate_id = $3
	`, acme.org.ID, eventdto.TypeVersionReplaced, first.ID).Scan(&events); err != nil || events != 1 {
		t.Fatalf("catalog.version_replaced is written with the publication: %d %v", events, err)
	}
	note, err := s.catalog.RetireReplaced(ctx, acme.org.ID, o.ID, first.ID)
	if err != nil || !strings.Contains(note, "lifecycle") {
		t.Fatalf("first delivery: %q %v", note, err)
	}
	again, err := s.catalog.RetireReplaced(ctx, acme.org.ID, o.ID, first.ID)
	if err != nil || !strings.Contains(again, "no longer AVAILABLE") {
		t.Fatalf("duplicate delivery: %q %v", again, err)
	}
	if replaced, _ := s.catalog.Version(ctx, dev, o.ID, 1); replaced.Status != model.AssetStatusSoftDeleted {
		t.Fatalf("the replaced version is deleted: %+v", replaced)
	}
	if after := s.object(t, dev, "", "once.txt"); after.CurrentAssetID != second.ID {
		t.Fatalf("the new version is current: %+v", after)
	}
	// A replaced version that became current again is kept.
	if note, _ := s.catalog.RetireReplaced(ctx, acme.org.ID, o.ID, second.ID); !strings.Contains(note, "current") {
		t.Fatalf("a current version is never retired: %q", note)
	}
}

func TestReconcileRepairsIncompletePublication(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	acme := s.team(t, "cat-fix")
	dev := acme.developer
	v1 := s.publishAt(t, dev, "fix.txt", textData("1", 50))
	v2 := s.publishAt(t, dev, "fix.txt", textData("2", 60))

	// A publication cut off between the asset and the catalog: version 2
	// AVAILABLE without a number, the object still on version 1 with
	// stale copied metadata.
	for _, q := range []string{
		`UPDATE assets SET version = NULL WHERE id = '` + v2.ID + `'`,
		`UPDATE catalog_objects SET current_asset_id = '` + v1.ID + `', current_version = 1, size_bytes = 1 WHERE id = '` + v1.ObjectID + `'`,
	} {
		if _, err := s.db.Pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	dry, err := s.reconciler.Reconcile(ctx, catalog.ReconcileOptions{TenantID: acme.org.ID, DryRun: true})
	if err != nil || dry.Counts[model.FindingUnnumbered] != 1 || dry.Counts[model.FindingCurrentStale] != 1 || dry.Repaired != 0 {
		t.Fatalf("dry run: %+v %v", dry, err)
	}
	if o := s.object(t, dev, "", "fix.txt"); o.SizeBytes != 1 {
		t.Fatal("a dry run changes nothing")
	}
	fixed, err := s.reconciler.Reconcile(ctx, catalog.ReconcileOptions{TenantID: acme.org.ID})
	if err != nil || fixed.Repaired != 2 {
		t.Fatalf("repair: %+v %v", fixed, err)
	}
	o := s.object(t, dev, "", "fix.txt")
	numbered, _ := s.catalog.Version(ctx, dev, o.ID, 3)
	if o.CurrentAssetID != v1.ID || o.SizeBytes != 50 || numbered.ID != v2.ID {
		t.Fatalf("after repair: %+v, version 3 %+v", o, numbered)
	}

	// A current version that is no longer AVAILABLE is replaced by the
	// newest one that is.
	if _, err := s.catalog.DeleteVersion(ctx, dev, o.ID, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Pool.Exec(ctx, `UPDATE catalog_objects SET current_asset_id = $1, current_version = 1 WHERE id = $2`, v1.ID, o.ID); err != nil {
		t.Fatal(err)
	}
	fixed, err = s.reconciler.Reconcile(ctx, catalog.ReconcileOptions{TenantID: acme.org.ID})
	if err != nil || fixed.Counts[model.FindingCurrentInvalid] != 1 {
		t.Fatalf("invalid current: %+v %v", fixed, err)
	}
	if o := s.object(t, dev, "", "fix.txt"); o.CurrentAssetID != v2.ID {
		t.Fatalf("the newest AVAILABLE version is current: %+v", o)
	}
	clean, err := s.reconciler.Reconcile(ctx, catalog.ReconcileOptions{TenantID: acme.org.ID, DryRun: true})
	if err != nil || len(clean.Findings) != 0 {
		t.Fatalf("nothing left to find: %+v %v", clean, err)
	}
	if successes(s.auditEvents(t, acme.org.ID, "catalog.reconciled")) != 2 {
		t.Fatal("repairs are audited")
	}
}

func TestReconcileReportsStorageFindingsAndNeverDeletes(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	acme := s.team(t, "cat-store")
	dev := acme.developer
	assets := []model.Asset{
		s.publishAt(t, dev, "one.txt", textData("1", 50)),
		s.publishAt(t, dev, "two.txt", textData("2", 50)),
		s.publishAt(t, dev, "three.txt", textData("3", 50)),
	}
	// Checked in asset ID order: the first check meets a storage error,
	// the second finds its object gone.
	sort.Slice(assets, func(i, j int) bool { return assets[i].ID < assets[j].ID })
	if err := s.ie.storage.DeleteObject(ctx, s.ie.bucket, assets[1].ObjectKey); err != nil {
		t.Fatal(err)
	}
	ghost := "published/" + acme.org.ID + "/no-such-asset"
	s.ie.storage.PutObject(ghost, textData("ghost", 20))
	s.ie.storage.HeadErr = errors.New("connection reset")

	report, err := s.reconciler.Reconcile(ctx, catalog.ReconcileOptions{TenantID: acme.org.ID, CheckStorage: true})
	if err != nil {
		t.Fatal(err)
	}
	if report.StorageErrors != 1 || report.Counts[model.FindingObjectMissing] != 1 || report.Counts[model.FindingUnreferenced] != 1 {
		t.Fatalf("report: %+v", report)
	}
	for _, f := range report.Findings {
		if f.Kind == model.FindingObjectMissing && f.AssetID != assets[1].ID {
			t.Fatalf("missing object reported for the wrong version: %+v", f)
		}
		if f.Kind == model.FindingUnreferenced && f.Key != ghost {
			t.Fatalf("unreferenced object: %+v", f)
		}
	}
	// One failed check proves nothing: nothing was deleted or changed.
	if !s.stored(ghost) {
		t.Fatal("an unreferenced object is never deleted by the catalog")
	}
	for _, a := range assets {
		if v, err := s.catalog.Version(ctx, dev, a.ObjectID, 1); err != nil || v.Status != model.AssetStatusAvailable {
			t.Fatalf("version of %s: %+v %v", a.ID, v, err)
		}
	}
}

func TestMigrationKeepsExistingRecordsWorking(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	acme := s.team(t, "cat-legacy")
	dev := acme.developer

	// Assets published before the catalog: no object, no version.
	legacy := func(name string, publishedAt time.Time) model.Asset {
		t.Helper()
		now := time.Now().UTC()
		id := fmt.Sprintf("legacy-%d-%s", uploadSeq.Add(1), s.run)
		session := model.UploadSession{
			ID: "up-" + id, TenantID: acme.org.ID, OwnerID: acme.devUser.ID, ObjectKey: "quarantine/" + acme.org.ID + "/up-" + id,
			FileName: name, ContentType: "text/plain", ExpectedSize: 30, Status: model.UploadStatusVerified,
			CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(time.Hour),
		}
		if err := s.ie.uploads.Create(ctx, &session); err != nil {
			t.Fatal(err)
		}
		asset := model.Asset{
			ID: "ast-" + id, TenantID: acme.org.ID, OwnerID: acme.devUser.ID, UploadID: session.ID,
			ObjectKey: "published/" + acme.org.ID + "/ast-" + id, FileName: name, ContentType: "text/plain", SizeBytes: 30,
			Status: model.AssetStatusAvailable, CreatedAt: now, UpdatedAt: now,
		}
		if err := s.ie.assets.Create(ctx, &asset); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Pool.Exec(ctx, `UPDATE assets SET published_at = $2 WHERE id = $1`, asset.ID, publishedAt); err != nil {
			t.Fatal(err)
		}
		s.ie.storage.PutObject(asset.ObjectKey, textData(id, 30))
		return asset
	}
	old := legacy("legacy.txt", time.Now().Add(-2*time.Hour))
	newer := legacy("legacy.txt", time.Now().Add(-time.Hour))
	odd := legacy("what?.txt", time.Now().Add(-time.Hour))

	// The migration runs again without changing anything.
	migration, err := os.ReadFile(filepath.Join("..", "..", "migrations", "000013_catalog.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Pool.Exec(ctx, string(migration)); err != nil {
		t.Fatalf("re-running 000013: %v", err)
	}
	// Legacy assets keep working by their asset IDs.
	if _, err := s.delivery.CreateDownloadURL(ctx, dev, newer.ID, delivery.URLRequest{}); err != nil {
		t.Fatalf("delivery of a legacy asset: %v", err)
	}

	// The backfill makes them versions: in a dry run, reported only.
	dry, err := s.reconciler.Reconcile(ctx, catalog.ReconcileOptions{TenantID: acme.org.ID, DryRun: true})
	if err != nil || dry.Counts[model.FindingUncatalogued] != 3 {
		t.Fatalf("dry run: %+v %v", dry, err)
	}
	if _, err := s.reconciler.Reconcile(ctx, catalog.ReconcileOptions{TenantID: acme.org.ID}); err != nil {
		t.Fatal(err)
	}
	o := s.object(t, dev, "", "legacy.txt")
	page, _ := s.catalog.Versions(ctx, dev, o.ID, 0, 10)
	if len(page.Versions) != 2 || o.CurrentAssetID != newer.ID {
		t.Fatalf("legacy.txt: %d versions, current %s (want %s)", len(page.Versions), o.CurrentAssetID, newer.ID)
	}
	if _, err := s.catalog.Lookup(ctx, dev, "", "legacy/"+odd.ID); err != nil {
		t.Fatalf("an asset whose name is no valid path is attached under legacy/: %v", err)
	}
	if v, _ := s.catalog.Version(ctx, dev, o.ID, 1); v.ID != old.ID && v.ID != newer.ID {
		t.Fatalf("version 1: %+v", v)
	}
	if again, _ := s.reconciler.Reconcile(ctx, catalog.ReconcileOptions{TenantID: acme.org.ID, DryRun: true}); again.Counts[model.FindingUncatalogued] != 0 {
		t.Fatalf("the backfill is done once: %+v", again)
	}
}
