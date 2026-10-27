package integration

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
	s3storage "github.com/sanjeev0303/object-storage-platform/internal/infrastructure/storage/s3"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/retry"
	assetservice "github.com/sanjeev0303/object-storage-platform/internal/service/asset"
	cleanupservice "github.com/sanjeev0303/object-storage-platform/internal/service/cleanup"
	"github.com/sanjeev0303/object-storage-platform/internal/service/compaction"
	uploadservice "github.com/sanjeev0303/object-storage-platform/internal/service/upload"
)

// Phase 5.6 against the local stack. Needs every migration (through 000015).

var lifecycleLog = slog.New(slog.NewTextHandler(io.Discard, nil))

// lifecycleCleanup removes what lifecycle tests write that the session
// cascade does not reach.
func lifecycleCleanup(t *testing.T, env *processingEnv) {
	t.Cleanup(func() {
		ctx := context.Background()
		for _, table := range []string{"lifecycle_events", "legal_holds", "retention_policies", "orphan_candidates"} {
			_, _ = env.db.Pool.Exec(ctx, `DELETE FROM `+table+` WHERE tenant_id = $1`, env.tenant)
		}
		_, _ = env.db.Pool.Exec(ctx, `DELETE FROM inventory_scans WHERE prefix LIKE '%' || $1 || '%'`, env.tenant)
		_, _ = env.db.Pool.Exec(ctx, `DELETE FROM lifecycle_cursors WHERE name LIKE '%' || $1`, env.tenant)
	})
}

// availableAsset inserts an AVAILABLE asset for a fresh VERIFIED session.
func availableAsset(t *testing.T, env *processingEnv, name string) model.Asset {
	t.Helper()
	env.session(t, name, model.UploadStatusVerified)
	now := time.Now().UTC().Truncate(time.Microsecond)
	asset := model.Asset{
		ID: env.tenant + "-" + name + "-asset", TenantID: env.tenant, OwnerID: "owner", UploadID: env.tenant + "-" + name,
		ObjectKey: "assets/" + env.tenant + "/" + env.tenant + "-" + name + "-asset", FileName: name + ".pdf",
		ContentType: "application/pdf", SizeBytes: 10, Status: model.AssetStatusAvailable, CreatedAt: now, UpdatedAt: now,
	}
	if err := env.assets.Create(context.Background(), &asset); err != nil {
		t.Fatalf("create asset: %v", err)
	}
	return asset
}

func scheduled(asset model.Asset, now time.Time) model.DeletionOperation {
	return model.DeletionOperation{
		ID: asset.ID + "-op-" + fmt.Sprint(now.UnixNano()), TenantID: asset.TenantID, AssetID: asset.ID,
		State: model.DeletionScheduled, Reason: model.DeletionReasonUser, RequestedBy: "user:owner",
		PreviousStatus: asset.Status, PurgeAfter: now, MaxAttempts: 3, NextAttemptAt: now, CreatedAt: now, UpdatedAt: now,
	}
}

func event(tenant, name string, now time.Time) model.LifecycleEvent {
	return model.LifecycleEvent{ID: fmt.Sprintf("%s-%s-%d", tenant, name, now.UnixNano()), TenantID: tenant, Event: name, Actor: "test", CreatedAt: now}
}

func TestPostgresDeletionWorkflow(t *testing.T) {
	env := openProcessingEnv(t)
	lifecycleCleanup(t, env)
	ctx := context.Background()
	repo := postgres.NewDeletionRepository(env.db)
	holds := postgres.NewLegalHoldRepository(env.db)
	asset := availableAsset(t, env, "del")
	now := time.Now().UTC().Truncate(time.Microsecond)

	// Concurrent requests create one deletion.
	var wg sync.WaitGroup
	created := make(chan bool, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			op := scheduled(asset, now)
			op.ID = fmt.Sprintf("%s-race-%d", asset.ID, i)
			_, ok, err := repo.SoftDelete(ctx, op, []model.AssetStatus{model.AssetStatusAvailable}, event(env.tenant, "deletion_requested", now.Add(time.Duration(i))))
			if err != nil {
				t.Errorf("soft delete: %v", err)
			}
			created <- ok
		}(i)
	}
	wg.Wait()
	close(created)
	n := 0
	for ok := range created {
		if ok {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("concurrent requests must create exactly one deletion, created %d", n)
	}
	got, _ := env.assets.GetByID(ctx, env.tenant, asset.ID)
	if got.Status != model.AssetStatusSoftDeleted {
		t.Fatalf("asset %s", got.Status)
	}

	// Restore while scheduled.
	op, err := repo.Restore(ctx, env.tenant, asset.ID, now, event(env.tenant, "restored", now))
	if err != nil || op.State != model.DeletionCancelled {
		t.Fatalf("restore: %+v %v", op, err)
	}
	if got, _ := env.assets.GetByID(ctx, env.tenant, asset.ID); got.Status != model.AssetStatusAvailable {
		t.Fatalf("restored asset is %s", got.Status)
	}

	// Request again; a worker claims it; nobody else can. The event's time is
	// past the four concurrent ones above, so its ID never matches the winner's.
	if _, _, err := repo.SoftDelete(ctx, scheduled(asset, now), []model.AssetStatus{model.AssetStatusAvailable}, event(env.tenant, "deletion_requested", now.Add(4))); err != nil {
		t.Fatal(err)
	}
	claimed, err := repo.Claim(ctx, "worker-a", now.Add(time.Second), time.Minute, 50, env.tenant)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: %d %v", len(claimed), err)
	}
	if again, _ := repo.Claim(ctx, "worker-b", now.Add(2*time.Second), time.Minute, 50, env.tenant); len(again) != 0 {
		t.Fatal("a leased deletion was claimed twice")
	}
	opID := claimed[0].ID

	// A hold stops the start, atomically.
	hold, err := holds.Place(ctx, model.LegalHold{ID: env.tenant + "-hold", TenantID: env.tenant, Scope: model.ScopeAsset, ScopeKey: asset.ID,
		Reason: "case", PlacedBy: "legal", PlacedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	if started, err := repo.Start(ctx, opID, "worker-a", now.Add(3*time.Second)); err != nil || started {
		t.Fatalf("a held deletion must not start: %v %v", started, err)
	}
	if _, err := repo.Start(ctx, opID, "worker-b", now.Add(3*time.Second)); !errors.Is(err, model.ErrLeaseLost) {
		t.Fatalf("another worker must not start it, got %v", err)
	}
	if err := holds.Release(ctx, env.tenant, hold.ID, "legal", now); err != nil {
		t.Fatal(err)
	}
	if started, err := repo.Start(ctx, opID, "worker-a", now.Add(4*time.Second)); err != nil || !started {
		t.Fatalf("after release the deletion starts: %v %v", started, err)
	}
	if _, err := repo.Restore(ctx, env.tenant, asset.ID, now, event(env.tenant, "restored", now)); !errors.Is(err, model.ErrNotRestorable) {
		t.Fatalf("a started deletion is not restorable, got %v", err)
	}

	// A download link must not outlive the asset.
	if err := postgres.NewDownloadTokenRepository(env.db).Create(ctx, model.DownloadToken{
		ID: env.tenant + "-tok", TokenHash: env.tenant + "-hash", TenantID: env.tenant, AssetID: asset.ID,
		PrincipalID: "owner", CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.MarkStorageDeleted(ctx, opID, "worker-a", now.Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := repo.Complete(ctx, opID, "worker-a", now.Add(6*time.Second), event(env.tenant, "deletion_completed", now.Add(6*time.Second))); err != nil {
		t.Fatal(err)
	}
	final, _ := repo.Get(ctx, env.tenant, opID)
	tombstone, _ := env.assets.GetByID(ctx, env.tenant, asset.ID)
	token, _ := postgres.NewDownloadTokenRepository(env.db).GetByHash(ctx, env.tenant+"-hash")
	if final.State != model.DeletionCompleted || final.StorageDeletedAt == nil || tombstone.Status != model.AssetStatusDeleted || token.RevokedAt == nil {
		t.Fatalf("completion: op %s, asset %s, token revoked %v", final.State, tombstone.Status, token.RevokedAt)
	}
	if _, err := repo.Requeue(ctx, env.tenant, opID, now); !errors.Is(err, model.ErrInvalidAssetState) {
		t.Fatalf("only a FAILED deletion may be requeued, got %v", err)
	}
}

func TestPostgresRetryBeforeStartStaysRestorable(t *testing.T) {
	env := openProcessingEnv(t)
	lifecycleCleanup(t, env)
	ctx := context.Background()
	repo := postgres.NewDeletionRepository(env.db)
	asset := availableAsset(t, env, "retry")
	now := time.Now().UTC()

	if _, _, err := repo.SoftDelete(ctx, scheduled(asset, now), []model.AssetStatus{model.AssetStatusAvailable}, event(env.tenant, "x", now)); err != nil {
		t.Fatal(err)
	}
	claimed, _ := repo.Claim(ctx, "w", now.Add(time.Second), time.Minute, 50, env.tenant)
	if len(claimed) != 1 {
		t.Fatal("expected a claim")
	}
	if err := repo.Retry(ctx, claimed[0].ID, "w", now.Add(2*time.Second), now.Add(time.Hour), "database timeout"); err != nil {
		t.Fatal(err)
	}
	op, _ := repo.Get(ctx, env.tenant, claimed[0].ID)
	if op.State != model.DeletionScheduled || op.LeaseOwner != "" {
		t.Fatalf("a retry before the start must keep the deletion restorable: %+v", op)
	}

	// Another live record pointing at the same object is a reference.
	other := asset
	other.ID, other.UploadID = env.tenant+"-other", env.tenant+"-retry-2"
	env.session(t, "retry-2", model.UploadStatusVerified)
	if err := env.assets.Create(ctx, &other); err != nil {
		t.Fatal(err)
	}
	refs, err := repo.References(ctx, asset, []string{asset.ObjectKey})
	if err != nil || len(refs) != 1 || refs[0] != "asset "+other.ID+" is AVAILABLE" {
		t.Fatalf("references %v %v", refs, err)
	}
}

func TestPostgresPolicyVersionsAndHolds(t *testing.T) {
	env := openProcessingEnv(t)
	lifecycleCleanup(t, env)
	ctx := context.Background()
	policies := postgres.NewRetentionPolicyRepository(env.db)
	now := time.Now().UTC().Truncate(time.Second)
	days := func(n int) *time.Duration { d := time.Duration(n) * 24 * time.Hour; return &d }

	for i, p := range []model.RetentionPolicy{
		{MinRetention: days(10), EffectiveFrom: now.Add(-48 * time.Hour)},
		{MinRetention: days(20), MaxAge: days(90), EffectiveFrom: now.Add(-time.Hour)},
		{MinRetention: days(30), EffectiveFrom: now.Add(24 * time.Hour)}, // scheduled for tomorrow
	} {
		p.ID, p.TenantID, p.Scope, p.CreatedBy, p.Reason, p.CreatedAt = fmt.Sprintf("%s-p%d", env.tenant, i), env.tenant, model.ScopeTenant, "ops", "test", now
		stored, err := policies.AddVersion(ctx, p)
		if err != nil || stored.Version != i+1 {
			t.Fatalf("version %d: %+v %v", i+1, stored, err)
		}
	}
	current, ok, err := policies.Effective(ctx, env.tenant, model.ScopeTenant, "", now)
	if err != nil || !ok || current.Version != 2 || *current.MinRetention != 20*24*time.Hour || *current.MaxAge != 90*24*time.Hour {
		t.Fatalf("effective now: %+v %v", current, err)
	}
	later, _, _ := policies.Effective(ctx, env.tenant, model.ScopeTenant, "", now.Add(25*time.Hour))
	if later.Version != 3 {
		t.Fatalf("a future version takes effect at its time, got v%d", later.Version)
	}
	if history, _ := policies.History(ctx, env.tenant, model.ScopeTenant, ""); len(history) != 3 || history[0].Version != 3 {
		t.Fatalf("history %+v", history)
	}
	if _, ok, _ := policies.Effective(ctx, "another-tenant", model.ScopeTenant, "", now); ok {
		t.Fatal("a policy must not leak to another tenant")
	}

	holds := postgres.NewLegalHoldRepository(env.db)
	for _, h := range []model.LegalHold{
		{ID: env.tenant + "-h1", Scope: model.ScopeDataset, ScopeKey: "finance"},
		{ID: env.tenant + "-h2", Scope: model.ScopeAsset, ScopeKey: "a1"},
	} {
		h.TenantID, h.Reason, h.PlacedBy, h.PlacedAt = env.tenant, "case", "legal", now
		if _, err := holds.Place(ctx, h); err != nil {
			t.Fatal(err)
		}
	}
	if active, _ := holds.Active(ctx, env.tenant, "finance", "a2"); len(active) != 1 || active[0].ID != env.tenant+"-h1" {
		t.Fatalf("dataset hold: %+v", active)
	}
	if active, _ := holds.Active(ctx, env.tenant, "", "a1"); len(active) != 1 {
		t.Fatalf("asset hold: %+v", active)
	}
	if active, _ := holds.Active(ctx, "another-tenant", "finance", "a1"); len(active) != 0 {
		t.Fatal("holds must not leak to another tenant")
	}
	if err := holds.Release(ctx, env.tenant, env.tenant+"-h1", "legal", now); err != nil {
		t.Fatal(err)
	}
	if err := holds.Release(ctx, env.tenant, env.tenant+"-h1", "legal", now); !errors.Is(err, model.ErrHoldNotFound) {
		t.Fatalf("releasing twice: %v", err)
	}
}

func TestPostgresInventoryObservations(t *testing.T) {
	env := openProcessingEnv(t)
	lifecycleCleanup(t, env)
	ctx := context.Background()
	repo := postgres.NewInventoryRepository(env.db)
	prefix := "assets/" + env.tenant + "/"
	now := time.Now().UTC().Truncate(time.Microsecond)

	scan1, ok, err := repo.OpenScan(ctx, model.ScanObjects, "", "it-bucket", prefix, "worker-a", env.tenant+"-s1", now, time.Minute)
	if err != nil || !ok {
		t.Fatalf("open: %v %v", ok, err)
	}
	if _, ok, _ := repo.OpenScan(ctx, model.ScanObjects, "", "it-bucket", prefix, "worker-b", env.tenant+"-s1b", now, time.Minute); ok {
		t.Fatal("a second worker must not open a leased scan")
	}

	candidate := model.OrphanCandidate{
		ID: env.tenant + "-c1", Bucket: "it-bucket", ObjectKey: prefix + "x", ETag: `"e1"`, SizeBytes: 4,
		Category: model.OrphanDeadPromotionCopy, TenantID: env.tenant, Reason: "rejected",
	}
	for i := 0; i < 2; i++ { // a replayed page
		marked, err := repo.Mark(ctx, candidate, scan1.ID, now)
		if err != nil || marked.Observations != 1 {
			t.Fatalf("mark: %+v %v", marked, err)
		}
	}
	scan1.Pages, scan1.ObjectsSeen = 1, 1
	if err := repo.FinishScan(ctx, scan1, "worker-a", now, model.ScanCompleted); err != nil {
		t.Fatal(err)
	}
	if last, ok, _ := repo.LastCompleted(ctx, model.ScanObjects, "", "it-bucket", prefix); !ok || last.ObjectsSeen != 1 {
		t.Fatalf("last completed scan %+v", last)
	}

	scan2, _, _ := repo.OpenScan(ctx, model.ScanObjects, "", "it-bucket", prefix, "worker-a", env.tenant+"-s2", now.Add(time.Hour), time.Minute)
	marked, _ := repo.Mark(ctx, candidate, scan2.ID, now.Add(time.Hour))
	if marked.Observations != 2 || !marked.FirstSeenAt.Equal(now) {
		t.Fatalf("second sighting: %+v", marked)
	}
	// Still running: not due yet.
	if due, _ := repo.DueCandidates(ctx, "", "it-bucket", prefix, now.Add(time.Hour), 2, 3, 10); len(due) != 0 {
		t.Fatal("a candidate last seen in an unfinished scan must not be due")
	}
	if err := repo.FinishScan(ctx, scan2, "worker-a", now.Add(time.Hour), model.ScanCompleted); err != nil {
		t.Fatal(err)
	}
	if due, _ := repo.DueCandidates(ctx, "", "it-bucket", prefix, now.Add(time.Hour), 2, 3, 10); len(due) != 1 {
		t.Fatal("seen twice, last in a completed scan: due")
	}

	// A different ETag is a different object: the count starts over.
	scan3, _, _ := repo.OpenScan(ctx, model.ScanObjects, "", "it-bucket", prefix, "worker-a", env.tenant+"-s3", now.Add(2*time.Hour), time.Minute)
	candidate.ETag = `"e2"`
	if replaced, _ := repo.Mark(ctx, candidate, scan3.ID, now.Add(2*time.Hour)); replaced.Observations != 1 || !replaced.FirstSeenAt.Equal(now.Add(2*time.Hour)) {
		t.Fatalf("replaced object: %+v", replaced)
	}
	if err := repo.Resolve(ctx, env.tenant+"-c1", model.CandidateCleared, "replaced", now); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresCompactionRulesKeepWhatMatters(t *testing.T) {
	env := openProcessingEnv(t)
	lifecycleCleanup(t, env)
	ctx := context.Background()
	t.Cleanup(func() {
		for _, sql := range []string{
			`DELETE FROM delivery_audit_events WHERE tenant_id = $1`,
			`DELETE FROM processing_events WHERE tenant_id = $1`,
			`DELETE FROM reconciliation_findings WHERE tenant_id = $1`,
		} {
			_, _ = env.db.Pool.Exec(context.Background(), sql, env.tenant)
		}
	})

	now := time.Now().UTC()
	old := now.Add(-2 * 365 * 24 * time.Hour) // past every retention
	opOld := now.Add(-60 * 24 * time.Hour)    // past operational, within audit retention
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := env.db.Pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}

	// Audit rows: one past audit retention, one only 60 days old.
	for i, at := range []time.Time{old, opOld} {
		exec(`INSERT INTO delivery_audit_events (id, tenant_id, principal_id, asset_id, operation, outcome, created_at)
		      VALUES ($1, $2, 'p', 'a', 'METADATA', 'ALLOWED', $3)`, fmt.Sprintf("%s-d%d", env.tenant, i), env.tenant, at)
		exec(`INSERT INTO processing_events (id, tenant_id, asset_id, event, actor, created_at)
		      VALUES ($1, $2, 'a', 'verified', 'w', $3)`, fmt.Sprintf("%s-p%d", env.tenant, i), env.tenant, at)
		exec(`INSERT INTO lifecycle_events (id, tenant_id, event, actor, created_at) VALUES ($1, $2, 'x', 'w', $3)`,
			fmt.Sprintf("%s-l%d", env.tenant, i), env.tenant, at)
	}

	// Upload sessions: an old finished one, an old one still uploading.
	env.session(t, "done", model.UploadStatusVerified)
	env.session(t, "busy", model.UploadStatusUploading)
	exec(`UPDATE upload_sessions SET idempotancy_key = id, created_at = $2, updated_at = $2, expires_at = $3
	      WHERE tenant_id = $1`, env.tenant, opOld, now.Add(time.Hour))
	exec(`INSERT INTO upload_parts (tenant_id, upload_id, part_number, etag, size_bytes, status) VALUES ($1, $2, 1, '"e"', 5, 'UPLOADED')`,
		env.tenant, env.tenant+"-done")
	exec(`INSERT INTO upload_parts (tenant_id, upload_id, part_number, etag, size_bytes, status) VALUES ($1, $2, 1, '"e"', 5, 'UPLOADED')`,
		env.tenant, env.tenant+"-busy")

	// Scans: two old completed scans of one prefix; the newer must stay.
	prefix := "compaction-" + env.tenant + "/"
	exec(`INSERT INTO inventory_scans (id, bucket, prefix, status, started_at, updated_at, completed_at)
	      VALUES ($1, 'b', $2, 'COMPLETED', $3, $3, $3), ($4, 'b', $2, 'COMPLETED', $5, $5, $5)`,
		env.tenant+"-old-scan", prefix, old, env.tenant+"-new-scan", opOld)

	svc, err := compaction.NewService(postgres.NewCompactor(env.db), compaction.Config{
		BatchSize: 2, MaxBatches: 1000,
		AuditRetention: 365 * 24 * time.Hour, OperationalRetention: 30 * 24 * time.Hour, IdempotencyTTL: 7 * 24 * time.Hour,
	}, lifecycleLog, nil)
	if err != nil {
		t.Fatal(err)
	}

	dry, err := compaction.NewService(postgres.NewCompactor(env.db), compaction.Config{
		BatchSize: 2, MaxBatches: 1000, DryRun: true,
		AuditRetention: 365 * 24 * time.Hour, OperationalRetention: 30 * 24 * time.Hour, IdempotencyTTL: 7 * 24 * time.Hour,
	}, lifecycleLog, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dry.RunOnce(ctx); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	count := func(sql string, args ...any) int {
		t.Helper()
		var n int
		if err := env.db.Pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return n
	}
	if count(`SELECT count(*) FROM delivery_audit_events WHERE tenant_id = $1`, env.tenant) != 2 {
		t.Fatal("a dry run must not delete")
	}

	results, err := svc.RunOnce(ctx)
	if err != nil {
		t.Fatalf("compaction: %v", err)
	}
	for _, r := range results {
		if r.Status != "COMPLETED" {
			t.Fatalf("%s: %+v", r.Rule, r)
		}
	}

	for _, table := range []string{"delivery_audit_events", "processing_events", "lifecycle_events"} {
		if n := count(`SELECT count(*) FROM `+table+` WHERE tenant_id = $1`, env.tenant); n != 1 {
			t.Fatalf("%s: only the audit row past audit retention may go, %d left", table, n)
		}
	}
	if n := count(`SELECT count(*) FROM upload_sessions WHERE tenant_id = $1 AND idempotancy_key IS NOT NULL`, env.tenant); n != 1 {
		t.Fatalf("only the finished session loses its idempotency key, %d keys left", n)
	}
	if count(`SELECT count(*) FROM upload_sessions WHERE id = $1 AND idempotancy_key IS NOT NULL`, env.tenant+"-busy") != 1 {
		t.Fatal("an upload still in flight must keep its idempotency key")
	}
	if n := count(`SELECT count(*) FROM upload_parts WHERE tenant_id = $1`, env.tenant); n != 1 ||
		count(`SELECT count(*) FROM upload_parts WHERE upload_id = $1`, env.tenant+"-busy") != 1 {
		t.Fatal("only the verified upload's parts may go")
	}
	if count(`SELECT count(*) FROM inventory_scans WHERE prefix = $1`, prefix) != 1 ||
		count(`SELECT count(*) FROM inventory_scans WHERE id = $1`, env.tenant+"-new-scan") != 1 {
		t.Fatal("the latest completed scan of a prefix must be kept")
	}
	if count(`SELECT count(*) FROM upload_sessions WHERE tenant_id = $1`, env.tenant) != 2 {
		t.Fatal("upload sessions themselves are never compacted")
	}
}

// localLifecycle builds the lifecycle and cleanup services over Postgres
// and MinIO, with test-sized grace and safety intervals.
func localLifecycle(t *testing.T, stack *contentStack, storage interfaces.LifecycleStorage, owner string) (*assetservice.Service, *cleanupservice.Service) {
	t.Helper()
	db := stack.env.db
	if storage == nil {
		storage = stack.provider
	}
	lifecycle, err := assetservice.NewService(assetservice.Dependencies{
		Assets: stack.env.assets, Uploads: stack.env.uploads, Jobs: stack.env.jobs,
		Quarantine: postgres.NewQuarantineRepository(db), Policies: postgres.NewRetentionPolicyRepository(db),
		Holds: postgres.NewLegalHoldRepository(db), Deletions: postgres.NewDeletionRepository(db),
		Events: postgres.NewLifecycleEventRepository(db), Cursors: postgres.NewCursorRepository(db), Storage: storage,
	}, assetservice.Config{
		Owner: owner, QuarantineBucket: stack.bucket, DeliveryBucket: stack.bucket, DeliveryPrefix: promotionPrefix,
		Platform:  assetservice.PlatformRetention{DefaultQuarantineRetention: time.Hour, QuarantineRetentionFloor: time.Hour},
		BatchSize: 50, Concurrency: 2, Lease: time.Second, RecheckInterval: time.Minute, MaxDeletionsPerRun: 50,
		Retry: retry.Policy{BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond, MaxAttempts: 3}, TenantID: stack.env.tenant,
	}, lifecycleLog)
	if err != nil {
		t.Fatal(err)
	}
	cleanup, err := cleanupservice.NewService(cleanupservice.Dependencies{
		Storage: storage, Inventory: postgres.NewInventoryRepository(db), Cursors: postgres.NewCursorRepository(db),
		Uploads: stack.env.uploads, Assets: stack.env.assets, Jobs: stack.env.jobs, Holds: postgres.NewLegalHoldRepository(db),
		Events: postgres.NewLifecycleEventRepository(db),
	}, cleanupservice.Config{
		Owner: owner, QuarantineBucket: stack.bucket, DeliveryBucket: stack.bucket, DeliveryPrefix: promotionPrefix,
		TenantID: stack.env.tenant, PageSize: 1, PagesPerRun: 100, Lease: time.Minute, MinAge: time.Millisecond,
		MinObservations: 2, SafetyInterval: time.Millisecond, FailedUploadRetention: time.Hour, DeleteOrphans: true,
		MaxDeletionsPerRun: 50, MaxAttempts: 3, MaxScanErrors: 3, CheckBatch: 50,
	}, lifecycleLog)
	if err != nil {
		t.Fatal(err)
	}
	return lifecycle, cleanup
}

// crashAfterDelete cancels the worker after its first storage deletion.
type crashAfterDelete struct {
	*s3storage.ObjectStorage
	cancel context.CancelFunc
	once   sync.Once
}

func (c *crashAfterDelete) DeleteObject(ctx context.Context, bucket, key string) error {
	err := c.ObjectStorage.DeleteObject(ctx, bucket, key)
	c.once.Do(c.cancel)
	return err
}

func TestLocalStackDeletionSurvivesACrash(t *testing.T) {
	stack := openContentStack(t)
	lifecycleCleanup(t, stack.env)
	id := stack.upload(t, "gone.pdf", "application/pdf", []byte("%PDF-1.7\nto be deleted\n"))
	runCycle(t, stack.service(t, serviceOptions{}))
	asset, _ := stack.assetFor(t, id)
	if asset.Status != model.AssetStatusAvailable {
		t.Fatalf("asset is %s", asset.Status)
	}
	// Leave a stray quarantine source as a crashed promotion would.
	putObject(t, stack.provider, stack.bucket, "quarantine/"+stack.env.tenant+"/"+id, []byte("leftover"))

	lifecycle, _ := localLifecycle(t, stack, nil, "worker-a")
	// No grace: the request is due at once.
	zero := time.Duration(0)
	if _, err := lifecycle.SetPolicy(context.Background(), model.RetentionPolicy{
		TenantID: stack.env.tenant, Scope: model.ScopeTenant, DeletionGrace: &zero, CreatedBy: "it", Reason: "no grace in tests",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := lifecycle.RequestDeletion(context.Background(), member(stack.env.tenant, "owner", model.RoleDeveloper), asset.ID); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	crashing, _ := localLifecycle(t, stack, &crashAfterDelete{ObjectStorage: stack.provider, cancel: cancel}, "worker-a")
	var crashed assetservice.CycleStats
	_ = crashing.ProcessDeletions(ctx, &crashed)
	if got, _ := stack.env.assets.GetByID(context.Background(), stack.env.tenant, asset.ID); got.Status != model.AssetStatusSoftDeleted {
		t.Fatalf("after the crash the asset must stay soft-deleted, got %s", got.Status)
	}

	time.Sleep(1100 * time.Millisecond) // the crashed worker's lease expires
	recovering, _ := localLifecycle(t, stack, nil, "worker-b")
	var stats assetservice.CycleStats
	if err := recovering.ProcessDeletions(context.Background(), &stats); err != nil || stats.Completed != 1 {
		t.Fatalf("recovery: %+v %v", stats, err)
	}
	for _, key := range []string{asset.ObjectKey, "quarantine/" + stack.env.tenant + "/" + id} {
		if _, err := stack.provider.HeadObject(context.Background(), stack.bucket, key); !errors.Is(err, interfaces.ErrObjectNotFound) {
			t.Fatalf("%s must be gone from MinIO, got %v", key, err)
		}
	}
	if got, _ := stack.env.assets.GetByID(context.Background(), stack.env.tenant, asset.ID); got.Status != model.AssetStatusDeleted {
		t.Fatalf("asset must be tombstoned, got %s", got.Status)
	}
}

func TestLocalStackOrphanSweepAcrossPages(t *testing.T) {
	stack := openContentStack(t)
	lifecycleCleanup(t, stack.env)
	ctx := context.Background()

	// A rejected asset with a promotion copy left behind, and an object
	// with no record at all.
	stack.env.session(t, "rej", model.UploadStatusVerified)
	now := time.Now().UTC()
	rejected := model.Asset{
		ID: stack.env.tenant + "-rej", TenantID: stack.env.tenant, OwnerID: "owner", UploadID: stack.env.tenant + "-rej",
		ObjectKey: "quarantine/" + stack.env.tenant + "/rej", FileName: "r.pdf", ContentType: "application/pdf", SizeBytes: 4,
		Status: model.AssetStatusRejected, RejectionReason: "malware_detected", CreatedAt: now, UpdatedAt: now,
	}
	if err := stack.env.assets.Create(ctx, &rejected); err != nil {
		t.Fatal(err)
	}
	deadCopy := promotionPrefix + "/" + stack.env.tenant + "/" + rejected.ID
	ghost := promotionPrefix + "/" + stack.env.tenant + "/no-such-asset"
	for _, key := range []string{deadCopy, ghost} {
		putObject(t, stack.provider, stack.bucket, key, []byte("data"))
		key := key
		t.Cleanup(func() { _ = stack.provider.DeleteObject(context.Background(), stack.bucket, key) })
	}
	time.Sleep(5 * time.Millisecond) // past the (test-sized) grace period

	_, cleanup := localLifecycle(t, stack, nil, "worker-a")
	run := func() cleanupservice.CycleStats {
		var stats cleanupservice.CycleStats
		if err := cleanup.Scan(ctx, &stats); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if err := cleanup.Sweep(ctx, &stats); err != nil {
			t.Fatalf("sweep: %v", err)
		}
		return stats
	}
	first := run()
	if first.Marked != 2 || first.Deleted != 0 || first.PagesScanned < 2 {
		t.Fatalf("first pass marks only (page size 1): %+v", first)
	}
	time.Sleep(5 * time.Millisecond)
	second := run()
	if second.Deleted != 1 || second.Kept != 1 {
		t.Fatalf("the proven orphan is swept, the unrecorded one kept: %+v", second)
	}
	if _, err := stack.provider.HeadObject(ctx, stack.bucket, deadCopy); !errors.Is(err, interfaces.ErrObjectNotFound) {
		t.Fatalf("the dead copy must be gone, got %v", err)
	}
	if _, err := stack.provider.HeadObject(ctx, stack.bucket, ghost); err != nil {
		t.Fatalf("an object with no record must stay without opt-in: %v", err)
	}

	var scans int
	if err := stack.env.db.Pool.QueryRow(ctx, `SELECT count(*) FROM inventory_scans WHERE prefix = $1 AND status = 'COMPLETED'`,
		promotionPrefix+"/"+stack.env.tenant+"/").Scan(&scans); err != nil || scans != 2 {
		t.Fatalf("both scans must be recorded as completed: %d %v", scans, err)
	}
}

func TestLocalStackAbandonedMultipartCleanupIsIdempotent(t *testing.T) {
	env := openReconcileEnv(t)
	provider, bucket := openTestProvider(t)
	ctx := context.Background()
	multipart, err := uploadservice.NewMultipartService(env.uploads, postgres.NewMultipartUploadRepository(env.db), provider,
		uploadservice.MultipartServiceConfig{Bucket: bucket, MaxSize: 1 << 30, SessionTTL: time.Hour, PartURLTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}

	env.session(t, "abandoned", model.UploadStatusAborted)
	key := "quarantine/" + env.tenant + "/abandoned"
	if _, err := provider.BeginMultipart(ctx, bucket, key, "application/octet-stream"); err != nil {
		t.Fatal(err)
	}
	session, err := env.uploads.GetByID(ctx, env.tenant, env.tenant+"-abandoned")
	if err != nil {
		t.Fatal(err)
	}
	session.ObjectKey = key

	first, err := multipart.AbortLeftoverSessions(ctx, []model.UploadSession{*session}, false)
	if err != nil || len(first) != 1 || first[0].Outcome != model.FindingDone {
		t.Fatalf("first cleanup: %+v %v", first, err)
	}
	second, err := multipart.AbortLeftoverSessions(ctx, []model.UploadSession{*session}, false)
	if err != nil || len(second) != 0 {
		t.Fatalf("a repeated cleanup must find nothing left: %+v %v", second, err)
	}
	if ids, _ := provider.ListMultipartUploads(ctx, bucket, key); len(ids) != 0 {
		t.Fatalf("the provider still holds sessions %v", ids)
	}
}

func TestProviderVersionedDelete(t *testing.T) {
	_, plainBucket := openTestProvider(t)
	bucket := plainBucket + "-versioned"
	cfg, err := s3storage.LoadConfig()
	if err != nil {
		t.Skip("S3 configuration incomplete")
	}
	ctx := context.Background()
	client, err := s3storage.NewClient(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("TEST_S3_CREATE_BUCKET") != "true" {
		t.Skip("needs TEST_S3_CREATE_BUCKET=true to create a versioned test bucket")
	}
	if _, err := client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(bucket)}); err != nil {
		if _, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
			t.Fatalf("create versioned test bucket: %v", err)
		}
	}
	if _, err := client.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{
		Bucket:                  aws.String(bucket),
		VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled},
	}); err != nil {
		t.Skipf("this provider does not offer versioning here: %v", err)
	}

	storage := s3storage.NewObjectStorage(client)
	if status, err := storage.VersioningStatus(ctx, bucket); err != nil || status != "Enabled" {
		t.Fatalf("versioning status %q %v", status, err)
	}
	// The catalog learns what the provider does by asking it.
	if c, err := storage.Capabilities(ctx, bucket); err != nil || !c.ProviderVersioning {
		t.Fatalf("a versioned bucket's capabilities: %+v %v", c, err)
	}
	if c, err := storage.Capabilities(ctx, plainBucket); err != nil || c.ProviderVersioning {
		t.Fatalf("an unversioned bucket's capabilities: %+v %v", c, err)
	}
	key := fmt.Sprintf("versioned/%d", time.Now().UnixNano())
	var versions []string
	for _, body := range []string{"first", "second"} {
		out, err := client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), Body: bytes.NewReader([]byte(body))})
		if err != nil {
			t.Fatal(err)
		}
		versions = append(versions, aws.ToString(out.VersionId))
	}
	t.Cleanup(func() {
		for _, v := range versions {
			_ = storage.DeleteObjectVersion(context.Background(), bucket, key, v)
		}
	})

	// Delete exactly the second version: the first stays, no delete marker.
	if err := storage.DeleteObjectVersion(ctx, bucket, key, versions[1]); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.HeadObjectVersion(ctx, bucket, key, versions[1]); !errors.Is(err, interfaces.ErrObjectNotFound) {
		t.Fatalf("the deleted version must be gone, got %v", err)
	}
	current, err := storage.HeadObject(ctx, bucket, key)
	if err != nil || current.VersionID != versions[0] {
		t.Fatalf("the earlier version must be current again: %+v %v", current, err)
	}

	// Deleting by key removes every version of exactly that key, delete
	// markers included; a key it only prefixes keeps its version.
	kept := func(k string) int {
		out, err := client.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{Bucket: aws.String(bucket), Prefix: aws.String(k)})
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, v := range out.Versions {
			if aws.ToString(v.Key) == k {
				n++
			}
		}
		for _, m := range out.DeleteMarkers {
			if aws.ToString(m.Key) == k {
				n++
			}
		}
		return n
	}
	put := func(k, body string) {
		if _, err := client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(k), Body: bytes.NewReader([]byte(body))}); err != nil {
			t.Fatal(err)
		}
	}
	whole, sibling, hidden := key+"-whole", key+"-whole0", key+"-hidden"
	put(whole, "one")
	put(whole, "two")
	put(sibling, "another record")
	t.Cleanup(func() { _ = storage.DeleteObject(context.Background(), bucket, sibling) })
	if err := storage.DeleteObject(ctx, bucket, whole); err != nil {
		t.Fatal(err)
	}
	if n := kept(whole); n != 0 {
		t.Fatalf("%d versions or markers of the deleted key remain", n)
	}
	if n := kept(sibling); n != 1 {
		t.Fatalf("a key sharing the prefix must keep its version, has %d", n)
	}
	// A retry finds the key gone and leaves no marker either.
	if err := storage.DeleteObject(ctx, bucket, whole); err != nil || kept(whole) != 0 {
		t.Fatalf("a repeated delete: %v, %d left", err, kept(whole))
	}

	// What a key-only delete (older code) left, a marker hiding a version,
	// goes on the next delete.
	put(hidden, "hidden bytes")
	if _, err := client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String(hidden)}); err != nil {
		t.Fatal(err)
	}
	if kept(hidden) != 2 {
		t.Fatalf("a key-only delete keeps the version behind a marker: %d", kept(hidden))
	}
	if err := storage.DeleteObject(ctx, bucket, hidden); err != nil || kept(hidden) != 0 {
		t.Fatalf("the hidden version must go: %v, %d left", err, kept(hidden))
	}
}
