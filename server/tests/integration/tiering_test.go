package integration

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	datasetservice "github.com/sanjeev0303/object-storage-platform/internal/service/dataset"
	"github.com/sanjeev0303/object-storage-platform/internal/service/tiering"
)

// Tier rules move old, unread objects to the archive, skipping held ones;
// an archived object answers "restore required" until it is restored; the
// cost budget stops reads past it; and expiring snapshots frees what only
// they held.
func TestTieringRestoresBudgetsAndExpiry(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	acme := s.team(t, "tiering")
	rig := s.packingRig(t)
	s.delivery.Route(rig.router)
	connections := postgres.NewStorageConnectionRepository(s.db)
	conn := activeConnection(t, connections, acme.org.ID, "Glacier bucket", testKeyring(t), model.ProviderAWSS3)
	bucket := rig.bucket(conn.ID)

	// Four objects on the connection, published ten days ago.
	var assets []model.Asset
	for i := 0; i < 4; i++ {
		b := textData(fmt.Sprintf("tier-%d", i), 300+i)
		a := s.publishAt(t, acme.developer, fmt.Sprintf("tier/f%d.bin", i), b)
		bucket.PutObject(a.ObjectKey, b)
		tx, err := s.db.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `SELECT set_config('osp.migrating_assets', 'on', true)`); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `UPDATE assets SET storage_connection_id = $2, storage_etag = $3,
			published_at = now() - interval '10 days' WHERE id = $1`, a.ID, conn.ID, bucket.ETag(a.ObjectKey)); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		assets = append(assets, a)
	}
	if _, err := postgres.NewLegalHoldRepository(s.db).Place(ctx, model.LegalHold{ID: acme.org.ID + "-tier-hold", TenantID: acme.org.ID,
		Scope: model.ScopeAsset, ScopeKey: assets[3].ID, Reason: "case", PlacedBy: "it", PlacedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}

	repo := postgres.NewTieringRepository(s.db)
	svc := tiering.New(tiering.Deps{Store: repo, Assets: postgres.NewAssetRepository(s.db), Router: rig.router,
		Datasets: s.datasets, Audit: s.audit, Alerts: postgres.NewAccessRepository(s.db), Log: s.log,
		Delete: func(ctx context.Context, p model.Principal, assetID string) error {
			_, err := s.lifecycle.RequestDeletion(ctx, p, assetID)
			return err
		}}, tiering.Config{})
	s.delivery.Tiered(svc)

	if _, err := svc.PutRule(ctx, acme.developer, "", tiering.RuleInput{Name: "cold", Tier: model.TierArchive, MinAgeDays: 1}); !errors.Is(err, model.ErrForbidden) {
		t.Fatalf("a developer cannot set tier rules: %v", err)
	}
	if _, err := svc.PutRule(ctx, acme.owner, "", tiering.RuleInput{Name: "now", Tier: model.TierArchive}); !errors.Is(err, model.ErrInvalidTiering) {
		t.Fatalf("a rule that would move new uploads at once is refused: %v", err)
	}
	if _, err := svc.PutRule(ctx, acme.owner, "", tiering.RuleInput{Name: "cold", Tier: model.TierArchive, MinAgeDays: 7, UnreadDays: 1}); err != nil {
		t.Fatal(err)
	}
	stats, err := svc.RunOnce(ctx)
	if err != nil || stats.Moved != 3 || stats.Failed != 0 {
		moves, _ := svc.Moves(ctx, acme.owner)
		t.Fatalf("three moved, the held one not: %+v %v %+v", stats, err, moves)
	}
	for i, a := range assets {
		got, _ := postgres.NewAssetRepository(s.db).GetByID(ctx, acme.org.ID, a.ID)
		want := model.TierArchive
		if i == 3 {
			want = model.TierStandard
		}
		if got.StorageTier != want || bucket.Tier(a.ObjectKey) != want {
			t.Fatalf("asset %d: %s in the catalog, %s in the bucket, want %s", i, got.StorageTier, bucket.Tier(a.ObjectKey), want)
		}
	}
	if again, _ := svc.RunOnce(ctx); again.Moved != 0 {
		t.Fatalf("nothing moves twice: %+v", again)
	}

	// Archived: reads need a restore first.
	if _, err := s.download(acme.developer, assets[0].ID, ""); !errors.Is(err, model.ErrRestoreRequired) {
		t.Fatalf("an archived object is not read before its restore: %v", err)
	}
	s.mustDownload(t, acme.developer, assets[3].ID, textData("tier-3", 303))

	x, err := svc.RequestRestore(ctx, acme.developer, assets[0].ID, tiering.RestoreInput{Days: 2, Speed: model.RestoreFast})
	if err != nil || x.State != "PENDING" {
		t.Fatalf("restore: %+v %v", x, err)
	}
	if again, _ := svc.RequestRestore(ctx, acme.developer, assets[0].ID, tiering.RestoreInput{}); again.ID != x.ID {
		t.Fatal("a second request joins the open restore")
	}
	if st, _ := svc.RunOnce(ctx); st.RestoresStarted != 1 {
		t.Fatalf("restore started: %+v", st)
	}
	if st, _ := svc.RunOnce(ctx); st.RestoresReady != 0 {
		t.Fatalf("not ready while the provider restores: %+v", st)
	}
	bucket.FinishRestore(assets[0].ObjectKey, time.Now().Add(48*time.Hour))
	if st, _ := svc.RunOnce(ctx); st.RestoresReady != 1 {
		t.Fatalf("ready once the provider is done: %+v", st)
	}
	s.mustDownload(t, acme.developer, assets[0].ID, textData("tier-0", 300))

	// A snapshot's archived members, restored together, with the cost.
	d := s.newDataset(t, acme.developer, "tier-set")
	s.addMembers(t, acme.developer, d.ID, memberOf(assets[1], "", ""), memberOf(assets[2], "", ""))
	v, _, err := s.datasets.Snapshot(ctx, acme.developer, d.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	est, err := svc.VersionEstimate(ctx, acme.developer, d.ID, v.ID, "")
	if err != nil || est.SealedFiles != 2 || est.Restore.Hours == 0 {
		t.Fatalf("estimate: %+v %v", est, err)
	}
	vr, err := svc.RestoreVersion(ctx, acme.developer, d.ID, v.ID, tiering.RestoreInput{Days: 1})
	if err != nil || vr.Archived != 2 || vr.Requested != 2 {
		t.Fatalf("version restore: %+v %v", vr, err)
	}

	// The report sees the archive, and a BLOCK budget stops reads past it.
	report, err := svc.Report(ctx, acme.owner)
	if err != nil {
		t.Fatal(err)
	}
	archived := int64(0)
	for _, l := range report.Stored {
		if l.Tier == model.TierArchive {
			archived += l.Objects
		}
	}
	if archived != 3 {
		t.Fatalf("report: %+v", report.Stored)
	}
	if _, err := svc.SetBudget(ctx, acme.owner, model.CostBudget{MonthlyRequests: 1, Action: "BLOCK"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.download(acme.developer, assets[3].ID, ""); !errors.Is(err, model.ErrCostBudget) {
		t.Fatalf("past the budget, reads stop: %v", err)
	}
	if _, err := svc.SetBudget(ctx, acme.owner, model.CostBudget{Action: "ALERT"}); err != nil {
		t.Fatal(err)
	}
	s.mustDownload(t, acme.developer, assets[3].ID, textData("tier-3", 303))

	// Expiry: an object version only old snapshots hold is freed.
	e := s.newDataset(t, acme.developer, "expiry-set")
	x1 := s.publishAt(t, acme.developer, "exp/x.txt", textData("x-one", 120))
	s.addMembers(t, acme.developer, e.ID, memberOf(x1, "", ""))
	v1, _, _ := s.datasets.Snapshot(ctx, acme.developer, e.ID, "")
	x2 := s.publishAt(t, acme.developer, "exp/x.txt", textData("x-two", 130))
	if _, err := s.datasets.AddMembers(ctx, acme.developer, e.ID, []datasetservice.MemberInput{memberOf(x2, "", "")}, model.DuplicateReplace); err != nil {
		t.Fatal(err)
	}
	v2, _, _ := s.datasets.Snapshot(ctx, acme.developer, e.ID, "")
	y := s.publishAt(t, acme.developer, "exp/y.txt", textData("y", 90))
	s.addMembers(t, acme.developer, e.ID, memberOf(y, "", ""))
	v3, _, _ := s.datasets.Snapshot(ctx, acme.developer, e.ID, "")
	if x1.ObjectID != x2.ObjectID || x1.ID == x2.ID {
		t.Fatalf("x.txt has two versions: %s %s", x1.ID, x2.ID)
	}

	preview, err := svc.ExpireVersions(ctx, acme.owner, e.ID, tiering.ExpiryInput{KeepLast: 1, DryRun: true})
	if err != nil || len(preview.Expire) != 2 || len(preview.Keep) != 1 || preview.Keep[0].ID != v3.ID ||
		preview.FreedObjects != 1 || preview.FreedBytes != 120 {
		t.Fatalf("preview: %+v %v", preview, err)
	}
	if _, err := svc.ExpireVersions(ctx, acme.developer, e.ID, tiering.ExpiryInput{KeepLast: 1}); !errors.Is(err, model.ErrForbidden) {
		t.Fatalf("expiry needs datasets.manage: %v", err)
	}
	done, err := svc.ExpireVersions(ctx, acme.owner, e.ID, tiering.ExpiryInput{KeepLast: 1})
	if err != nil || done.DeletionsRequested != 1 || len(done.Refused) != 0 {
		t.Fatalf("expire: %+v %v", done, err)
	}
	var expired int
	if err := s.db.Pool.QueryRow(ctx, `SELECT count(*) FROM dataset_versions WHERE id = ANY($1) AND expired_at IS NOT NULL`,
		[]string{v1.ID, v2.ID}).Scan(&expired); err != nil || expired != 2 {
		t.Fatalf("expired versions: %d %v", expired, err)
	}
	if got, _ := postgres.NewAssetRepository(s.db).GetByID(ctx, acme.org.ID, x1.ID); got.Status == model.AssetStatusAvailable {
		t.Fatalf("x1's deletion was asked for: %s", got.Status)
	}
	s.mustDownload(t, acme.developer, x2.ID, textData("x-two", 130))
}

// Packed files move with their pack, once every file in it may; a
// restore restores the pack, and every file in it reads again.
func TestTieringMovesWholePacks(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	acme := s.team(t, "tier-packs")
	rig := s.packingRig(t)
	s.delivery.Route(rig.router)
	connections := postgres.NewStorageConnectionRepository(s.db)
	conn := activeConnection(t, connections, acme.org.ID, "Packed glacier", testKeyring(t), model.ProviderAWSS3)
	if _, err := connections.RecordTest(ctx, acme.org.ID, conn.ID, interfaces.ConnectionTestRecord{OK: true, At: time.Now().UTC(),
		Capabilities: &model.ProviderCapabilities{PresignedUpload: true, Multipart: true, RangeReads: true}}); err != nil {
		t.Fatal(err)
	}
	bucket := rig.bucket(conn.ID)

	var assets []model.Asset
	for i := 0; i < 3; i++ {
		b := textData(fmt.Sprintf("packed-%d", i), 50+i)
		a := s.publishAt(t, acme.developer, fmt.Sprintf("tp/f%d.txt", i), b)
		bucket.PutObject(a.ObjectKey, b)
		tx, _ := s.db.Pool.Begin(ctx)
		_, _ = tx.Exec(ctx, `SELECT set_config('osp.migrating_assets', 'on', true)`)
		if _, err := tx.Exec(ctx, `UPDATE assets SET storage_connection_id = $2, storage_etag = $3,
			published_at = now() - interval '10 days' WHERE id = $1`, a.ID, conn.ID, bucket.ETag(a.ObjectKey)); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		a.StorageConnectionID = conn.ID
		assets = append(assets, a)
	}
	if stats := rig.run(t, rig.worker(t, "w-tp", nil, []string{acme.org.ID}, nil)); stats.Published != 1 {
		t.Fatalf("one pack: %+v", stats)
	}
	pack := rig.packs(t, acme.org.ID)[0]

	svc := tiering.New(tiering.Deps{Store: postgres.NewTieringRepository(s.db), Assets: postgres.NewAssetRepository(s.db),
		Router: rig.router, Datasets: s.datasets, Audit: s.audit, Log: s.log}, tiering.Config{})
	s.delivery.Tiered(svc)
	if _, err := svc.PutRule(ctx, acme.owner, "", tiering.RuleInput{Name: "cold", Tier: model.TierArchive, MinAgeDays: 7, UnreadDays: 1}); err != nil {
		t.Fatal(err)
	}
	stats, err := svc.RunOnce(ctx)
	if err != nil || stats.Moved != 3 {
		t.Fatalf("the pack's three files moved: %+v %v", stats, err)
	}
	if bucket.Tier(pack.ObjectKey) != model.TierArchive {
		t.Fatalf("the pack object is archived: %s", bucket.Tier(pack.ObjectKey))
	}
	if _, err := s.download(acme.developer, assets[1].ID, ""); !errors.Is(err, model.ErrRestoreRequired) {
		t.Fatalf("a file of an archived pack needs a restore: %v", err)
	}
	if _, err := svc.RequestRestore(ctx, acme.developer, assets[1].ID, tiering.RestoreInput{Days: 1}); err != nil {
		t.Fatal(err)
	}
	_, _ = svc.RunOnce(ctx)
	bucket.FinishRestore(pack.ObjectKey, time.Now().Add(24*time.Hour))
	if st, _ := svc.RunOnce(ctx); st.RestoresReady != 1 {
		t.Fatalf("ready: %+v", st)
	}
	for i, a := range assets {
		s.mustDownload(t, acme.developer, a.ID, textData(fmt.Sprintf("packed-%d", i), 50+i))
	}
}
