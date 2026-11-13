package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// State machines refuse writes from a state they no longer hold: a stale
// worker, a sweep that read before a change, or a retry never moves a row
// backwards.

// guardTenant is an organization of its own; the security env removes its
// rows afterwards.
func guardTenant(t *testing.T, name string) (*securityEnv, string) {
	t.Helper()
	e := openSecurityEnv(t, nil)
	owner, _ := e.account(t, name)
	return e, e.organization(t, name, owner).ID
}

// guardAsset stores an AVAILABLE asset of dataset with its upload session.
func guardAsset(t *testing.T, db *postgres.DB, tenantID, id, dataset string, data []byte) model.Asset {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	sum := sha256.Sum256(data)
	sha := hex.EncodeToString(sum[:])
	s := model.UploadSession{ID: id + "-up", TenantID: tenantID, OwnerID: "owner", ObjectKey: "quarantine/" + tenantID + "/" + id,
		FileName: id + ".bin", ContentType: "application/octet-stream", ExpectedSize: int64(len(data)), ChecksumSHA256: sha,
		Status: model.UploadStatusCompleted, CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(time.Hour)}
	if err := postgres.NewUploadRepository(db).Create(ctx, &s); err != nil {
		t.Fatal(err)
	}
	a := model.Asset{ID: id, TenantID: tenantID, OwnerID: "owner", UploadID: s.ID, ObjectKey: "ready/" + tenantID + "/" + id,
		FileName: s.FileName, ContentType: s.ContentType, SizeBytes: s.ExpectedSize, ChecksumSHA256: sha,
		Status: model.AssetStatusAvailable, CreatedAt: now, UpdatedAt: now, Dataset: dataset}
	if err := postgres.NewAssetRepository(db).Create(ctx, &a); err != nil {
		t.Fatal(err)
	}
	return a
}

func column(t *testing.T, db *postgres.DB, query string, args ...any) string {
	t.Helper()
	var v *string
	if err := db.Pool.QueryRow(context.Background(), query, args...).Scan(&v); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	if v == nil {
		return "<null>"
	}
	return *v
}

// The expiry sweep reads OPEN uploads, then expires them. One the client
// completed in between stays queued, with its chunks.
func TestDeltaUploadExpiryLeavesAQueuedUploadAlone(t *testing.T) {
	e, tenant := guardTenant(t, "delta-guard")
	ctx := context.Background()
	store := postgres.NewDeltaUploadRepository(e.db)
	now := time.Now().UTC()
	d := model.DeltaUpload{ID: tenant + "-delta", TenantID: tenant, OwnerID: "owner", DatasetID: "ds", Namespace: "ns", BaseVersionID: "v1",
		Path: "f.bin", SizeBytes: 1, SHA256: "aa", ContentType: "application/octet-stream", ConnectionID: model.PlatformConnectionID,
		CreatedAt: now.Add(-25 * time.Hour), ExpiresAt: now.Add(-time.Hour)}
	if err := store.Create(ctx, d, []model.DeltaChunk{{Ordinal: 0, Offset: 0, Length: 1, SHA256: "aa"}}); err != nil {
		t.Fatal(err)
	}
	expired, err := store.Expired(ctx, now, 1000)
	if err != nil {
		t.Fatal(err)
	}
	listed := false
	for _, x := range expired {
		listed = listed || x.ID == d.ID
	}
	if !listed {
		t.Fatal("the sweep did not list the open upload")
	}
	if queued, err := store.Queue(ctx, d.ID); err != nil || !queued {
		t.Fatalf("queue: %v %v", queued, err)
	}
	if expiredNow, err := store.Expire(ctx, d.ID, now); err != nil || expiredNow {
		t.Fatalf("expire after queueing: %v %v", expiredNow, err)
	}
	state := func() string { return column(t, e.db, `SELECT state FROM delta_uploads WHERE id = $1`, d.ID) }
	if got := state(); got != model.DeltaQueued {
		t.Fatalf("a queued upload was expired by a sweep that read it open: %s", got)
	}
	if again, err := store.Queue(ctx, d.ID); err != nil || again {
		t.Fatalf("an upload that is no longer open was queued again: %v %v", again, err)
	}

	// A worker that lost its lease writes no outcome; the one holding it does.
	if _, err := e.db.Pool.Exec(ctx, `UPDATE delta_uploads SET state = 'ASSEMBLING', lease_owner = 'worker-2', lease_until = now() + interval '1 minute'
		WHERE id = $1`, d.ID); err != nil {
		t.Fatal(err)
	}
	if done, err := store.Finish(ctx, d.ID, "worker-1", model.DeltaFailed, "", "stale", now); err != nil || done {
		t.Fatalf("a worker without the lease finished the upload: %v %v", done, err)
	}
	if got := state(); got != model.DeltaAssembling {
		t.Fatalf("after a stale finish: %s", got)
	}
	if done, err := store.Finish(ctx, d.ID, "worker-2", model.DeltaDone, "up-1", "", now); err != nil || !done {
		t.Fatalf("the lease holder could not finish: %v %v", done, err)
	}
	if got := state(); got != model.DeltaDone {
		t.Fatalf("after finishing: %s", got)
	}
}

// A MIGRATE job cancelled while its last pass ran never re-points assets.
func TestCancelledMigrationNeverSwitchesAssets(t *testing.T) {
	e, tenant := guardTenant(t, "migrate-guard")
	ctx := context.Background()
	copies := postgres.NewCopyRepository(e.db)
	target := activeConnection(t, postgres.NewStorageConnectionRepository(e.db), tenant, "target", testKeyring(t))
	a := guardAsset(t, e.db, tenant, tenant+"-a1", "", []byte("migrate me"))
	jobID := tenant + "-job"
	now := time.Now().UTC()
	if _, err := e.db.Pool.Exec(ctx, `INSERT INTO copy_jobs (id, tenant_id, kind, dataset_id, target_connection_id, state, planned,
		created_by, created_at, updated_at, started_at, lease_owner, lease_expires_at)
		VALUES ($1, $2, 'MIGRATE', 'ds', $3, 'RUNNING', true, 'it', $4, $4, $4, 'worker-1', $4::timestamptz + interval '5 minutes')`,
		jobID, tenant, target.ID, now); err != nil {
		t.Fatal(err)
	}
	if err := copies.Cancel(ctx, tenant, jobID, now); err != nil {
		t.Fatal(err)
	}
	moved := []postgres.SwitchedAsset{{SourceConnectionID: model.PlatformConnectionID, SourceKey: a.ObjectKey, TargetKey: "moved/" + a.ID}}
	job, err := copies.GetJob(ctx, tenant, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if err := copies.FinishMigration(ctx, job, "worker-1", moved, map[string]any{}, now); !errors.Is(err, model.ErrCopyJobEnded) {
		t.Fatalf("finishing a cancelled migration: %v", err)
	}
	connection := func() string {
		return column(t, e.db, `SELECT storage_connection_id FROM assets WHERE id = $1`, a.ID)
	}
	if got := connection(); got != model.PlatformConnectionID {
		t.Fatalf("a cancelled migration re-pointed the asset to %s", got)
	}
	if job, _ := copies.GetJob(ctx, tenant, jobID); job.State != model.CopyCancelled {
		t.Fatalf("the cancelled job became %s", job.State)
	}

	// A running job under its own lease switches and completes in one go.
	if _, err := e.db.Pool.Exec(ctx, `UPDATE copy_jobs SET state = 'RUNNING', finished_at = NULL, lease_owner = 'worker-1',
		lease_expires_at = now() + interval '5 minutes' WHERE id = $1`, jobID); err != nil {
		t.Fatal(err)
	}
	if err := copies.FinishMigration(ctx, job, "worker-2", moved, map[string]any{}, now); !errors.Is(err, model.ErrCopyJobEnded) {
		t.Fatalf("another worker finished the migration: %v", err)
	}
	if err := copies.FinishMigration(ctx, job, "worker-1", moved, map[string]any{}, now); err != nil {
		t.Fatal(err)
	}
	if got := connection(); got != target.ID {
		t.Fatalf("the migration did not re-point the asset: %s", got)
	}
	if job, _ := copies.GetJob(ctx, tenant, jobID); job.State != model.CopyCompleted || !strings.Contains(string(job.Report), `"switched": 1`) {
		t.Fatalf("the migrated job: %s %s", job.State, job.Report)
	}
}

// Rotation discards a staged credential and the rollback slot: a later
// promotion cannot bring back an older credential, nor a rollback skip
// the rotated one. A staged credential not newer than the current one is
// never promoted.
func TestCredentialRotationResetsStagedAndRollbackSlots(t *testing.T) {
	e, tenant := guardTenant(t, "cred-guard")
	ctx := context.Background()
	repo := postgres.NewStorageConnectionRepository(e.db)
	c := activeConnection(t, repo, tenant, "creds", testKeyring(t))
	sealed := func(v int) interfaces.SealedCredential {
		tag := []byte{byte('0' + v)}
		return interfaces.SealedCredential{Version: v, KeyVersion: 1, Nonce: append([]byte("n"), tag...),
			Ciphertext: append([]byte("c"), tag...), WrappedKey: append([]byte("w"), tag...)}
	}
	at := time.Now().UTC()
	if _, err := repo.StageCredential(ctx, tenant, c.ID, sealed(2), "it", at); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.PromoteCredential(ctx, tenant, c.ID, 2, "it", at.Add(time.Hour), at); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.StageCredential(ctx, tenant, c.ID, sealed(3), "it", at); err != nil {
		t.Fatal(err)
	}
	current, err := repo.GetConnection(ctx, tenant, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.RotateCredential(ctx, tenant, c.ID, current.Revision, sealed(4), "it", at); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.StagedCredential(ctx, tenant, c.ID); !errors.Is(err, model.ErrNoStagedCredential) {
		t.Fatalf("rotation kept the staged credential: %v", err)
	}
	if _, err := repo.PromoteCredential(ctx, tenant, c.ID, 3, "it", at.Add(time.Hour), at); !errors.Is(err, model.ErrNoStagedCredential) {
		t.Fatalf("a staged credential older than the rotated one was promoted: %v", err)
	}
	if _, err := repo.RollbackCredential(ctx, tenant, c.ID, 4, "it", at); !errors.Is(err, model.ErrNoPreviousCredential) {
		t.Fatalf("a rollback went back past the rotated credential: %v", err)
	}
	got := column(t, e.db, `SELECT credential_version || ':' || encode(credential_wrapped_key, 'escape') || ':' ||
		coalesce(encode(staged_wrapped_key, 'escape'), '-') || ':' || coalesce(encode(previous_wrapped_key, 'escape'), '-')
		FROM storage_connections WHERE id = $1`, c.ID)
	if got != "4:w4:-:-" {
		t.Fatalf("version and wrapped keys after rotation: %s", got)
	}

	// A staged slot no newer than the current credential, however it got
	// there, is refused.
	if _, err := e.db.Pool.Exec(ctx, `UPDATE storage_connections SET staged_credential_version = 4, staged_key_version = 1,
		staged_nonce = 'n', staged_ciphertext = 'c', staged_wrapped_key = 'w' WHERE id = $1`, c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.PromoteCredential(ctx, tenant, c.ID, 4, "it", at.Add(time.Hour), at); !errors.Is(err, model.ErrStaleStagedCredential) {
		t.Fatalf("a staged credential not newer than the current one was promoted: %v", err)
	}
	if got := column(t, e.db, `SELECT encode(credential_ciphertext, 'escape') FROM storage_connections WHERE id = $1`, c.ID); got != "c4" {
		t.Fatalf("the current credential changed: %s", got)
	}
}

// A chunked asset never moves back from PUBLISHED or REHYDRATING, and
// RELEASED is final.
func TestChunkedAssetStatesOnlyMoveForward(t *testing.T) {
	e, tenant := guardTenant(t, "chunk-guard")
	ctx := context.Background()
	store := postgres.NewChunkStorageRepository(e.db)
	now := time.Now().UTC()
	c := postgres.ChunkCandidate{TenantID: tenant, AssetID: tenant + "-c1", ConnectionID: model.PlatformConnectionID,
		ObjectKey: "ready/k", ETag: "e", SHA256: "ab", SizeBytes: 10}
	state := func() string {
		return column(t, e.db, `SELECT state FROM chunked_assets WHERE asset_id = $1`, c.AssetID)
	}
	set := func(s string) {
		if _, err := e.db.Pool.Exec(ctx, `UPDATE chunked_assets SET state = $2 WHERE asset_id = $1`, c.AssetID, s); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.StartWriting(ctx, c, now); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.Pool.Exec(ctx, `INSERT INTO chunk_refs (tenant_id, connection_id, chunk_sha256, asset_id) VALUES ($1, 'platform', 'ab', $2)`,
		tenant, c.AssetID); err != nil {
		t.Fatal(err)
	}
	for _, from := range []string{"PUBLISHED", "REHYDRATING", "RELEASED"} {
		set(from)
		if err := store.Skip(ctx, c, "stale", now); err != nil {
			t.Fatal(err)
		}
		if err := store.StartWriting(ctx, c, now); !errors.Is(err, postgres.ErrChunkStateChanged) {
			t.Fatalf("writing chunks for a %s asset: %v", from, err)
		}
		if err := store.Abandon(ctx, c, "stale", now); err != nil {
			t.Fatal(err)
		}
		if got := state(); got != from {
			t.Fatalf("a stale conversion moved %s to %s", from, got)
		}
	}
	set("PUBLISHED")
	if n := column(t, e.db, `SELECT count(*)::text FROM chunk_refs WHERE asset_id = $1`, c.AssetID); n != "1" {
		t.Fatalf("an abandon took a published asset off its chunks: %s refs", n)
	}

	// Rehydrated finishes only a rehydration, and source retirement is
	// recorded only while chunks still serve the asset.
	src := postgres.ChunkSource{ChunkCandidate: c}
	if err := store.Rehydrated(ctx, src, false, "", "", now); !errors.Is(err, postgres.ErrChunkStateChanged) {
		t.Fatalf("rehydrated while published: %v", err)
	}
	if got := state(); got != "PUBLISHED" {
		t.Fatalf("an asset not rehydrating was released: %s", got)
	}
	set("RELEASED")
	_ = store.MarkSourceRetired(ctx, tenant, c.AssetID, now)
	if got := column(t, e.db, `SELECT source_retired_at::text FROM chunked_assets WHERE asset_id = $1`, c.AssetID); got != "<null>" {
		t.Fatalf("a released asset was marked retired: %s", got)
	}
	// A rehydration that found the original in place cannot finish once the
	// original has gone: the asset would point at nothing.
	set("REHYDRATING")
	if _, err := e.db.Pool.Exec(ctx, `UPDATE chunked_assets SET source_retired_at = $2 WHERE asset_id = $1`, c.AssetID, now); err != nil {
		t.Fatal(err)
	}
	if err := store.Rehydrated(ctx, src, false, "", "", now); !errors.Is(err, postgres.ErrChunkStateChanged) {
		t.Fatalf("rehydrated without the original: %v", err)
	}
	if got := state(); got != "REHYDRATING" {
		t.Fatalf("a rehydration without the original finished: %s", got)
	}
	if err := store.Rehydrated(ctx, src, true, "e2", "", now); err != nil {
		t.Fatal(err)
	}
	if got := state(); got != "RELEASED" {
		t.Fatalf("a rehydration that wrote the object back: %s", got)
	}
}

// A restore only moves forward, and a stale pass cannot write an archived
// tier back over a move.
func TestArchiveRestoreStatesOnlyMoveForward(t *testing.T) {
	e, tenant := guardTenant(t, "restore-guard")
	ctx := context.Background()
	store := postgres.NewTieringRepository(e.db)
	now := time.Now().UTC()
	x, _, err := store.CreateRestore(ctx, model.ArchiveRestore{ID: tenant + "-r1", TenantID: tenant, AssetID: tenant + "-a1",
		Days: 1, Speed: model.RestoreStandard, RequestedBy: "it", CreatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	state := func() string { return column(t, e.db, `SELECT state FROM archive_restores WHERE id = $1`, x.ID) }
	update := func(to string) {
		y := x
		y.State, y.UpdatedAt = to, now
		_ = store.UpdateRestore(ctx, y)
	}
	update("RESTORING")
	update("READY")
	if got := state(); got != "READY" {
		t.Fatalf("restore: %s", got)
	}
	update("RESTORING")
	update("FAILED")
	if got := state(); got != "READY" {
		t.Fatalf("a stale pass moved a READY restore to %s", got)
	}

	a := guardAsset(t, e.db, tenant, tenant+"-a2", "", []byte("archived once"))
	until := now.Add(time.Hour)
	_ = store.SetReadable(ctx, tenant, a.ID, model.TierArchive, &until, now)
	if got := column(t, e.db, `SELECT storage_tier FROM assets WHERE id = $1`, a.ID); got != string(model.TierStandard) {
		t.Fatalf("a restore pass wrote %s over an asset moved back to STANDARD", got)
	}
}

// A blocked asset is not offered as usable by readers outside delivery.
func TestBlockedAssetIsNotUsable(t *testing.T) {
	e, tenant := guardTenant(t, "block-guard")
	ctx := context.Background()
	a := guardAsset(t, e.db, tenant, tenant+"-b1", "ds", []byte("found bad later"))
	if _, err := e.db.Pool.Exec(ctx, `UPDATE assets SET storage_etag = 'e', published_at = now() - interval '1 day' WHERE id = $1`, a.ID); err != nil {
		t.Fatal(err)
	}
	if err := postgres.NewContentCheckRepository(e.db).BlockAsset(ctx, tenant, a.ID, model.RejectMalwareDetected, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	connectors := postgres.NewConnectorsRepository(e.db)
	if _, status, _, found, err := connectors.AssetByHash(ctx, tenant, "ds", a.ChecksumSHA256); err != nil || !found || status == string(model.AssetStatusAvailable) {
		t.Fatalf("Git LFS sees a blocked asset as %s (%v, %v)", status, found, err)
	}
	if _, _, status, err := connectors.AssetFacts(ctx, tenant, a.ID); err != nil || status == string(model.AssetStatusAvailable) {
		t.Fatalf("chat shares see a blocked asset as %s (%v)", status, err)
	}
	candidates, err := postgres.NewPackRepository(e.db).PackCandidates(ctx, tenant, interfaces.PackCursor{},
		interfaces.PackCandidateFilter{MinBytes: 1, MaxBytes: 1 << 30, PublishedBefore: time.Now()}, 10)
	if err != nil || len(candidates) != 0 {
		t.Fatalf("packing would take a blocked asset: %d %v", len(candidates), err)
	}

	// Clearing the block is guarded: once, and only for a blocked asset.
	checks := postgres.NewContentCheckRepository(e.db)
	if reason, err := checks.UnblockAsset(ctx, tenant, a.ID, time.Now().UTC()); err != nil || reason != model.RejectMalwareDetected {
		t.Fatalf("unblock: %q %v", reason, err)
	}
	if _, err := checks.UnblockAsset(ctx, tenant, a.ID, time.Now().UTC()); !errors.Is(err, model.ErrAssetNotBlocked) {
		t.Fatalf("unblocking twice: %v", err)
	}
	if _, err := checks.UnblockAsset(ctx, tenant, "no-such-asset", time.Now().UTC()); !errors.Is(err, model.ErrAssetNotFound) {
		t.Fatalf("unblocking a missing asset: %v", err)
	}
	if _, status, _, _, err := connectors.AssetByHash(ctx, tenant, "ds", a.ChecksumSHA256); err != nil || status != string(model.AssetStatusAvailable) {
		t.Fatalf("an unblocked asset reads as %s (%v)", status, err)
	}
}

// An operator gives an EXHAUSTED reconciliation task a fresh attempt
// budget; nothing else can be requeued.
func TestExhaustedReconciliationTaskIsRequeuedByAnOperator(t *testing.T) {
	e, tenant := guardTenant(t, "requeue-guard")
	ctx := context.Background()
	tasks := postgres.NewReconciliationRepository(e.db)
	now := time.Now().UTC()
	s := model.UploadSession{ID: tenant + "-up", TenantID: tenant, OwnerID: "owner", ObjectKey: "quarantine/" + tenant + "/up",
		FileName: "f.bin", ContentType: "application/octet-stream", ExpectedSize: 10, Status: model.UploadStatusCompleting,
		CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(time.Hour)}
	if err := postgres.NewUploadRepository(e.db).Create(ctx, &s); err != nil {
		t.Fatal(err)
	}
	taskID := tenant + "-task"
	if _, err := e.db.Pool.Exec(ctx, `INSERT INTO reconciliation_tasks (id, tenant_id, upload_id, operation, state, storage_provider,
		object_key, attempt_count, max_attempts, next_attempt_at, last_error, outcome)
		VALUES ($1, $2, $3, 'COMPLETE', 'EXHAUSTED', 's3', 'k', 5, 5, $4, 'provider unreachable', 'COMPLETING')`,
		taskID, tenant, s.ID, now); err != nil {
		t.Fatal(err)
	}
	if err := tasks.Requeue(ctx, taskID, now); err != nil {
		t.Fatal(err)
	}
	got := column(t, e.db, `SELECT state || ':' || attempt_count || ':' || coalesce(outcome, '-') || ':' || last_error
		FROM reconciliation_tasks WHERE id = $1`, taskID)
	if got != "PENDING:0:-:provider unreachable" {
		t.Fatalf("requeued task: %s", got)
	}
	if err := tasks.Requeue(ctx, taskID, now); !errors.Is(err, model.ErrTaskNotExhausted) {
		t.Fatalf("requeueing a PENDING task: %v", err)
	}
	if err := tasks.Requeue(ctx, "no-such-task", now); !errors.Is(err, model.ErrTaskNotExhausted) {
		t.Fatalf("requeueing a missing task: %v", err)
	}
}
