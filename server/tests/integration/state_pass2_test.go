package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/service/audit"
	"github.com/sanjeev0303/object-storage-platform/internal/service/edge"
	"github.com/sanjeev0303/object-storage-platform/internal/service/erasure"
)

// A second pass over the state machines: each change is checked against
// the row as it is when it is written, not as it was read.

// waitForDoneOrLockWaiters returns once done reports true or n sessions
// of this database wait on a lock: the call under test either finished
// without waiting or reached the row the test holds.
func waitForDoneOrLockWaiters(t *testing.T, db *postgres.DB, done *atomic.Bool, n int) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if done.Load() {
			return
		}
		var waiting int
		if err := db.Pool.QueryRow(context.Background(), `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting >= n {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the call neither finished nor waited on the held lock")
}

// certificateSuccessors decodes the snapshots a certificate lists.
func certificateSuccessors(t *testing.T, er *erasure.Service, p model.Principal, id string) []model.Successor {
	t.Helper()
	cert, err := er.Certificate(context.Background(), p, id)
	if err != nil {
		t.Fatalf("certificate: %v", err)
	}
	var body struct {
		Superseded []model.Successor `json:"snapshots_superseded"`
	}
	if err := json.Unmarshal([]byte(cert.Body), &body); err != nil {
		t.Fatal(err)
	}
	return body.Superseded
}

// erasureRow records an erasure request directly, as a worker finds it.
func erasureRow(t *testing.T, store *postgres.ErasureRepository, tenantID, id, state string, sel model.ErasureSelector, created time.Time) {
	t.Helper()
	sel.SubjectKey = model.DefaultSubjectKey
	for _, s := range []*[]string{&sel.SubjectIDs, &sel.SHA256s, &sel.ObjectIDs} {
		if *s == nil {
			*s = []string{}
		}
	}
	e := model.Erasure{ID: id, TenantID: tenantID, Kind: model.ErasureOther, State: state, Reason: "it", Selector: sel,
		SubjectHashes: []string{}, RequestedBy: "it", ExecutedBy: "it", CreatedAt: created}
	if err := store.Create(context.Background(), e); err != nil {
		t.Fatal(err)
	}
}

// An erasure supersedes a snapshot of an archived dataset like any other:
// the dataset's archive status does not stop it halfway.
func TestErasureSupersedesASnapshotOfAnArchivedDataset(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	acme := s.team(t, "erasure-archived")
	dev, owner := acme.developer, acme.owner
	er := newErasureService(s)

	a := s.publishAt(t, dev, "arch/p5.txt", textData("subject five", 200), withMetadata(map[string]string{"subject_id": "P-5"}))
	d := s.newDataset(t, dev, "archived-cohort")
	s.addMembers(t, dev, d.ID, memberOf(a, "", ""))
	v1, _, err := s.datasets.Snapshot(ctx, dev, d.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Pool.Exec(ctx, `UPDATE datasets SET lifecycle_status = 'ARCHIVED', archived_at = now() WHERE id = $1`, d.ID); err != nil {
		t.Fatal(err)
	}
	e, err := er.Create(ctx, owner, erasure.CreateInput{Kind: "PII", Reason: "archived", Selector: model.ErasureSelector{SubjectIDs: []string{"P-5"}}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := er.Execute(ctx, owner, e.ID)
	if err != nil {
		t.Fatalf("execute over an archived dataset's snapshot: %v", err)
	}
	if len(got.Successors) != 1 {
		t.Fatalf("successors: %+v", got.Successors)
	}
	if by := column(t, s.db, `SELECT superseded_by FROM dataset_versions WHERE id = $1`, v1.ID); by == "<null>" {
		t.Fatal("the archived dataset's snapshot was not superseded")
	}
	if n := column(t, s.db, `SELECT count(*)::text FROM dataset_members WHERE dataset_id = $1`, d.ID); n != "0" {
		t.Fatalf("the draft still holds %s erased members", n)
	}
}

// An execute that stopped right after its claim leaves the snapshots for
// the worker: it makes the successors before it certifies anything.
func TestErasureWorkerFinishesAnExecuteThatStoppedAfterTheClaim(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	acme := s.team(t, "erasure-resume")
	dev, owner := acme.developer, acme.owner
	er := newErasureService(s)
	store := postgres.NewErasureRepository(s.db)

	// The file is gone already; a snapshot still names it.
	a := s.publishAt(t, dev, "resume/p6.txt", textData("subject six", 200), withMetadata(map[string]string{"subject_id": "P-6"}))
	d := s.newDataset(t, dev, "resume")
	s.addMembers(t, dev, d.ID, memberOf(a, "", ""))
	v1, _, err := s.datasets.Snapshot(ctx, dev, d.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Pool.Exec(ctx, `UPDATE assets SET status = 'DELETED' WHERE id = $1`, a.ID); err != nil {
		t.Fatal(err)
	}
	sel := model.ErasureSelector{SubjectIDs: []string{"P-6"}}
	e, err := er.Create(ctx, owner, erasure.CreateInput{Kind: "PII", Reason: "resume", Selector: sel})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := er.Search(ctx, owner, sel)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := store.Claim(ctx, acme.org.ID, e.ID, []string{model.ErasurePlanned}, false, plan, owner.UserID, nil, time.Now().UTC()); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}

	if _, err := er.Progress(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := er.Get(ctx, owner, e.ID)
	if err != nil || got.State != model.ErasureCompleted {
		t.Fatalf("the erasure: %s %v", got.State, err)
	}
	if by := column(t, s.db, `SELECT superseded_by FROM dataset_versions WHERE id = $1`, v1.ID); by == "<null>" {
		t.Fatal("completed with the snapshot not superseded")
	}
	if succ := certificateSuccessors(t, er, owner, e.ID); len(succ) != 1 || succ[0].VersionID != v1.ID {
		t.Fatalf("the certificate's snapshots: %+v", succ)
	}
	if n := column(t, s.db, `SELECT count(*)::text FROM dataset_members WHERE dataset_id = $1`, d.ID); n != "0" {
		t.Fatalf("the draft still holds %s erased members", n)
	}
}

// The worker never certifies an erasure while its execute is still making
// the successor snapshots.
func TestErasureIsNotCertifiedWhileExecuteRuns(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	acme := s.team(t, "erasure-early")
	dev, owner := acme.developer, acme.owner
	er := newErasureService(s)

	a := s.publishAt(t, dev, "early/p7.txt", textData("subject seven", 200), withMetadata(map[string]string{"subject_id": "P-7"}))
	d := s.newDataset(t, dev, "early")
	s.addMembers(t, dev, d.ID, memberOf(a, "", ""))
	if _, _, err := s.datasets.Snapshot(ctx, dev, d.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Pool.Exec(ctx, `UPDATE assets SET status = 'DELETED' WHERE id = $1`, a.ID); err != nil {
		t.Fatal(err)
	}
	e, err := er.Create(ctx, owner, erasure.CreateInput{Kind: "PII", Reason: "early", Selector: model.ErasureSelector{SubjectIDs: []string{"P-7"}}})
	if err != nil {
		t.Fatal(err)
	}

	// Holding the dataset row stops the execute at the successor snapshot,
	// after its claim.
	tx, err := s.db.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `SELECT 1 FROM datasets WHERE id = $1 FOR UPDATE`, d.ID); err != nil {
		t.Fatal(err)
	}
	executed := make(chan error, 1)
	go func() {
		_, err := er.Execute(ctx, owner, e.ID)
		executed <- err
	}()
	waitForLockWaiters(t, s.db, 1)
	var progressDone atomic.Bool
	progressed := make(chan error, 1)
	go func() {
		_, err := er.Progress(ctx)
		progressed <- err
		progressDone.Store(true)
	}()
	waitForDoneOrLockWaiters(t, s.db, &progressDone, 2)
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-executed; err != nil {
		t.Fatalf("execute: %v", err)
	}
	if err := <-progressed; err != nil {
		t.Fatalf("progress: %v", err)
	}
	if _, err := er.Progress(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := er.Get(ctx, owner, e.ID)
	if got.State != model.ErasureCompleted {
		t.Fatalf("the erasure: %s", got.State)
	}
	if succ := certificateSuccessors(t, er, owner, e.ID); len(succ) != 1 {
		t.Fatalf("certified with %d successor snapshots, not 1", len(succ))
	}
}

// Successors recorded by one writer survive another's stale state change,
// and a finished erasure takes no more successors.
func TestErasureSuccessorsAreNotOverwritten(t *testing.T) {
	e, tenant := guardTenant(t, "erasure-succ-guard")
	ctx := context.Background()
	store := postgres.NewErasureRepository(e.db)
	now := time.Now().UTC()
	succ := []model.Successor{{DatasetID: "ds", VersionID: "v1", Version: 1, SuccessorID: "v2", SuccessorVersion: 2, Removed: 1}}

	erasureRow(t, store, tenant, tenant+"-a", model.ErasureDeleting, model.ErasureSelector{}, now)
	ok, err := store.Transition(ctx, tenant, tenant+"-a", []string{model.ErasureDeleting}, model.ErasureWaiting, func(*model.Erasure) {
		// Written while the transition holds its earlier read.
		if ok, err := store.SetPrepared(ctx, tenant, tenant+"-a", succ, now); err != nil || !ok {
			t.Errorf("prepared: %v %v", ok, err)
		}
	})
	if err != nil || !ok {
		t.Fatalf("transition: %v %v", ok, err)
	}
	if got, _ := store.Get(ctx, tenant, tenant+"-a"); len(got.Successors) != 1 || got.PreparedAt == nil {
		t.Fatalf("a stale transition overwrote the successors: %+v prepared %v", got.Successors, got.PreparedAt)
	}

	erasureRow(t, store, tenant, tenant+"-b", model.ErasureCompleted, model.ErasureSelector{}, now)
	ok, err = store.SetPrepared(ctx, tenant, tenant+"-b", succ, now)
	if got, _ := store.Get(ctx, tenant, tenant+"-b"); ok || len(got.Successors) != 0 || got.PreparedAt != nil {
		t.Fatalf("a completed erasure took successors: %+v (%v %v)", got.Successors, ok, err)
	}
}

// One erasure that keeps failing neither stops the others in the same pass
// nor keeps its place at the front of the queue.
func TestErasureProgressCarriesOnPastAFailingErasure(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	acme := s.team(t, "erasure-starve")
	store := postgres.NewErasureRepository(s.db)
	// No signing key: completing an erasure fails.
	er := erasure.New(erasure.Deps{Store: store, Lifecycle: s.lifecycle, Catalog: s.catalog, Audit: s.audit, Log: s.log})

	now := time.Now().UTC()
	failing := acme.org.ID + "-failing"
	erasureRow(t, store, acme.org.ID, failing, model.ErasureDeleting, model.ErasureSelector{SubjectIDs: []string{"nobody-" + s.run}}, now.Add(-2*time.Hour))
	b := s.publishAt(t, acme.developer, "starve/b.txt", textData("starve b", 100))
	waiting := acme.org.ID + "-waiting"
	erasureRow(t, store, acme.org.ID, waiting, model.ErasureDeleting, model.ErasureSelector{SHA256s: []string{b.ChecksumSHA256}}, now.Add(-time.Hour))
	if err := store.PutItems(ctx, acme.org.ID, waiting, []model.ErasureItem{{AssetID: b.ID, ObjectID: b.ObjectID, ObjectVersion: b.Version,
		Path: "starve/b.txt", SHA256: b.ChecksumSHA256, SizeBytes: b.SizeBytes, ConnectionID: "platform", State: model.ErasureItemPending}}, now); err != nil {
		t.Fatal(err)
	}
	before := column(t, s.db, `SELECT updated_at::text FROM erasure_requests WHERE id = $1`, failing)

	_, _ = er.Progress(ctx)
	if got, _ := s.ie.assets.GetByID(ctx, acme.org.ID, b.ID); got.Status != model.AssetStatusSoftDeleted {
		t.Fatalf("the erasure after the failing one was not advanced: its file is %s", got.Status)
	}
	if after := column(t, s.db, `SELECT updated_at::text FROM erasure_requests WHERE id = $1`, failing); after == before {
		t.Fatal("the failing erasure kept its place at the front of the queue")
	}
}

// A FAILED erasure (a deletion was cancelled, say) can be cancelled, and
// then the worker stops looking at it.
func TestFailedErasureCanBeCancelled(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	acme := s.team(t, "erasure-failed")
	store := postgres.NewErasureRepository(s.db)
	er := newErasureService(s)
	id := acme.org.ID + "-failed"
	erasureRow(t, store, acme.org.ID, id, model.ErasureFailed, model.ErasureSelector{SubjectIDs: []string{"P-8"}}, time.Now().UTC())

	got, err := er.Cancel(ctx, acme.owner, id, "the file was restored on purpose")
	if err != nil || got.State != model.ErasureCancelled {
		t.Fatalf("cancelling a failed erasure: %s %v", got.State, err)
	}
	open, err := store.Open(ctx, 1000)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range open {
		if o.ID == id {
			t.Fatal("the worker still lists the cancelled erasure")
		}
	}
}

// A chunk that arrives while a close is under way is in the snapshot of a
// COMPLETE recording, or the recording is not COMPLETE.
func TestEdgeChunkRacingCloseIsNotLeftOut(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	acme := s.team(t, "edge-race")
	svc := edge.New(edge.Deps{Store: postgres.NewEdgeRepository(s.db), Access: s.access, Datasets: s.datasets, Audit: s.audit, Log: s.log})
	ds := s.newDataset(t, acme.owner, "edge-race")
	dev, _, err := svc.Register(ctx, acme.owner, "racer", ds.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	me := model.Principal{TenantID: acme.owner.TenantID, UserID: acme.owner.UserID, Permissions: model.PermUploadsWrite,
		Scope: model.Scope{TokenID: dev.TokenID, Datasets: []string{ds.Name}, Restricted: true, NoLinks: true, PathPrefix: dev.PathPrefix}}
	first := catalogUpload(dev.PathPrefix+"run-1/a.txt", textData("race a", 100))
	first.Dataset = ds.Name
	s.finishUpload(t, me, s.startUpload(t, me, first, textData("race a", 100)))
	run, err := svc.OpenRecording(ctx, me, "run-1", model.RecordingRun, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddChunk(ctx, me, run.ID, model.EdgeChunk{Path: first.Path, SHA256: first.ChecksumSHA256, SizeBytes: 100}); err != nil {
		t.Fatal(err)
	}

	// Holding the dataset row stops the close after it read the chunks.
	tx, err := s.db.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `SELECT 1 FROM datasets WHERE id = $1 FOR UPDATE`, ds.ID); err != nil {
		t.Fatal(err)
	}
	closed := make(chan error, 1)
	go func() {
		_, err := svc.Close(ctx, me, run.ID, 0, nil)
		closed <- err
	}()
	waitForLockWaiters(t, s.db, 1)
	late := model.EdgeChunk{Path: dev.PathPrefix + "run-1/b.txt", SHA256: sha256Of([]byte("late")), SizeBytes: 4}
	_, addErr := svc.AddChunk(ctx, me, run.ID, late)
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-closed; err != nil {
		t.Fatalf("close: %v", err)
	}

	rec, err := postgres.NewEdgeRepository(s.db).Recording(ctx, acme.org.ID, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	chunks := column(t, s.db, `SELECT count(*)::text FROM edge_chunks WHERE recording_id = $1`, run.ID)
	if rec.State == model.RecordingComplete {
		v, err := s.datasets.Version(ctx, acme.owner, ds.ID, rec.VersionID)
		if err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(v.MemberCount) != chunks {
			t.Fatalf("COMPLETE with %d files in its snapshot and %s chunks (late chunk: %v)", v.MemberCount, chunks, addErr)
		}
	}
}

// An expired version cannot be released: its member list is gone.
func TestReleaseRefusesAnExpiredVersion(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	acme := s.team(t, "release-expired")
	dev := acme.developer
	a := s.publishAt(t, dev, "exp/a.txt", textData("exp a", 100))
	d := s.newDataset(t, dev, "expired-release")
	s.addMembers(t, dev, d.ID, memberOf(a, "", ""))
	v1, _, err := s.datasets.Snapshot(ctx, dev, d.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Pool.Exec(ctx, `UPDATE dataset_versions SET expired_at = now(), expired_by = 'it' WHERE id = $1`, v1.ID); err != nil {
		t.Fatal(err)
	}
	tokens := s.signIn(t, acme.ownerUser.Email, acme.ownerPW)
	r := s.call(t, tokens.AccessToken, acme.org.ID, http.MethodPut, "/api/v1/datasets/"+d.ID+"/versions/"+v1.ID+"/release", `{"label":"DR1"}`)
	if r.status != http.StatusConflict || !strings.Contains(string(r.body), `"version_expired"`) {
		t.Fatalf("releasing an expired version: %s", r)
	}
	if label := column(t, s.db, `SELECT release_label FROM dataset_versions WHERE id = $1`, v1.ID); label != "<null>" {
		t.Fatalf("the expired version was labelled %s", label)
	}
}

// Promote, rollback and discard act on the credential the caller read: one
// staged or promoted meanwhile is not used in its place.
func TestCredentialSlotChangesNeedTheVersionRead(t *testing.T) {
	e, tenant := guardTenant(t, "cred-version")
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
	tested, err := repo.StagedCredential(ctx, tenant, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Discarded and staged again between the test and the promotion.
	if _, err := repo.DiscardStaged(ctx, tenant, c.ID, tested.Version, "it", at); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.StageCredential(ctx, tenant, c.ID, sealed(3), "it", at); err != nil {
		t.Fatal(err)
	}
	_, err = repo.PromoteCredential(ctx, tenant, c.ID, tested.Version, "it", at.Add(time.Hour), at)
	if got := column(t, e.db, `SELECT credential_version::text FROM storage_connections WHERE id = $1`, c.ID); got != "1" || !errors.Is(err, model.ErrCredentialChanged) {
		t.Fatalf("promoted version %s, not the tested %d (%v)", got, tested.Version, err)
	}
	if _, err := repo.DiscardStaged(ctx, tenant, c.ID, tested.Version, "it", at); !errors.Is(err, model.ErrCredentialChanged) {
		t.Fatalf("discarding a credential staged since: %v", err)
	}
	if _, err := repo.PromoteCredential(ctx, tenant, c.ID, 3, "it", at.Add(time.Hour), at); err != nil {
		t.Fatal(err)
	}
	// A rollback that read the connection before that promotion is refused.
	if _, err := repo.RollbackCredential(ctx, tenant, c.ID, 1, "it", at); !errors.Is(err, model.ErrCredentialChanged) {
		t.Fatalf("a stale rollback: %v", err)
	}
	if _, err := repo.RollbackCredential(ctx, tenant, c.ID, 3, "it", at); err != nil {
		t.Fatal(err)
	}
	if got := column(t, e.db, `SELECT credential_version::text FROM storage_connections WHERE id = $1`, c.ID); got != "1" {
		t.Fatalf("after the rollback: version %s", got)
	}
}

// A requeue and a soft delete of a quarantined asset cannot both win.
func TestQuarantineRequeueAndSoftDeleteExcludeEachOther(t *testing.T) {
	e, tenant := guardTenant(t, "requeue-delete")
	ctx := context.Background()
	jobs := postgres.NewProcessingJobRepository(e.db)
	now := time.Now().UTC()
	job := func(a model.Asset, state string) {
		if _, err := e.db.Pool.Exec(ctx, `INSERT INTO processing_jobs (id, tenant_id, asset_id, upload_id, stage, state, max_attempts, next_attempt_at)
			VALUES ($1, $2, $3, $4, 'SCAN', $5, 3, now())`, a.ID+"-job", tenant, a.ID, a.UploadID, state); err != nil {
			t.Fatal(err)
		}
	}

	// Soft-deleted after the requeue checked it: the requeue is refused.
	gone := guardAsset(t, e.db, tenant, tenant+"-gone", "rq", textData("gone", 50))
	job(gone, "EXHAUSTED")
	if _, err := e.db.Pool.Exec(ctx, `UPDATE assets SET status = 'SOFT_DELETED' WHERE id = $1`, gone.ID); err != nil {
		t.Fatal(err)
	}
	_, err := jobs.Requeue(ctx, tenant, gone.ID, now)
	if state := column(t, e.db, `SELECT state FROM processing_jobs WHERE asset_id = $1`, gone.ID); state != "EXHAUSTED" {
		t.Fatalf("a soft-deleted asset's job was requeued: %s (%v)", state, err)
	}

	// Requeued after the delete checked the job: the delete is refused.
	held := guardAsset(t, e.db, tenant, tenant+"-held", "rq", textData("held", 50))
	job(held, "PENDING")
	if _, err := e.db.Pool.Exec(ctx, `UPDATE assets SET status = 'QUARANTINED' WHERE id = $1`, held.ID); err != nil {
		t.Fatal(err)
	}
	op := model.DeletionOperation{ID: held.ID + "-del", TenantID: tenant, AssetID: held.ID, State: model.DeletionScheduled,
		Reason: model.DeletionReasonUser, RequestedBy: "it", PreviousStatus: model.AssetStatusQuarantined, PurgeAfter: now,
		MaxAttempts: 3, NextAttemptAt: now, CreatedAt: now, UpdatedAt: now}
	event := model.LifecycleEvent{ID: held.ID + "-ev", TenantID: tenant, AssetID: held.ID, Event: "deletion_requested", Outcome: "SCHEDULED", Actor: "it", CreatedAt: now}
	_, _, err = postgres.NewDeletionRepository(e.db).SoftDelete(ctx, op, []model.AssetStatus{model.AssetStatusQuarantined}, event)
	if status := column(t, e.db, `SELECT status FROM assets WHERE id = $1`, held.ID); status != "QUARANTINED" {
		t.Fatalf("an asset with a pending job was soft-deleted: %s (%v)", status, err)
	}
}

// Demoting an owner counts the other owners under their account locks: a
// deactivation not yet committed is waited for, not counted as active.
func TestOwnerDemotionWaitsForADeactivation(t *testing.T) {
	e := openSecurityEnv(t, nil)
	ctx := context.Background()
	acme := e.team(t, "owner-race")
	promote := func(userID, role string) error {
		_, err := e.tenancy.ChangeMember(ctx, interfaces.MemberChange{OrgID: acme.org.ID, UserID: userID, RoleID: role, Now: time.Now().UTC()},
			func(before model.Membership, after *model.Membership) model.AuditEvent {
				return audit.From(ctx, acme.owner, "member.role_change", "membership", userID)
			})
		return err
	}
	if err := promote(acme.adminUser.ID, model.RoleOwner); err != nil {
		t.Fatal(err)
	}
	// A suspended organization's owners are not locked by a deactivation.
	if _, err := e.db.Pool.Exec(ctx, `UPDATE organizations SET status = 'SUSPENDED' WHERE id = $1`, acme.org.ID); err != nil {
		t.Fatal(err)
	}

	tx, err := e.db.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `UPDATE users SET status = 'DEACTIVATED' WHERE id = $1`, acme.adminUser.ID); err != nil {
		t.Fatal(err)
	}
	var done atomic.Bool
	demoted := make(chan error, 1)
	go func() {
		demoted <- promote(acme.ownerUser.ID, model.RoleAdmin)
		done.Store(true)
	}()
	waitForDoneOrLockWaiters(t, e.db, &done, 1)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	err = <-demoted
	owners := column(t, e.db, `SELECT count(*)::text FROM memberships m JOIN users u ON u.id = m.user_id
		WHERE m.org_id = $1 AND m.role_id = 'owner' AND m.status = 'ACTIVE' AND u.status = 'ACTIVE'`, acme.org.ID)
	if !errors.Is(err, model.ErrLastOwner) || owners != "1" {
		t.Fatalf("demotion: %v, %s active owners left", err, owners)
	}
}

// A second acknowledgement leaves the first one's record alone.
func TestSecurityAlertIsAcknowledgedOnce(t *testing.T) {
	e, tenant := guardTenant(t, "alert-ack")
	ctx := context.Background()
	repo := postgres.NewAccessRepository(e.db)
	now := time.Now().UTC()
	a := model.SecurityAlert{ID: tenant + "-alert", TenantID: tenant, PrincipalID: "user:x", Kind: "READ_SPIKE", Detail: "it",
		Metrics: map[string]any{}, DedupKey: "it-" + tenant, CreatedAt: now}
	if _, err := repo.AddAlert(ctx, a); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Acknowledge(ctx, tenant, a.ID, "user:first", now); err != nil {
		t.Fatal(err)
	}
	second, err := repo.Acknowledge(ctx, tenant, a.ID, "user:second", now.Add(time.Minute))
	if err != nil || second.AcknowledgedBy != "user:first" {
		t.Fatalf("a second acknowledgement: %+v %v", second, err)
	}
	if by := column(t, e.db, `SELECT acknowledged_by FROM security_alerts WHERE id = $1`, a.ID); by != "user:first" {
		t.Fatalf("acknowledged by %s", by)
	}
}

// A role deleted after an invitation or a role change checked it is not
// given out: both check it again under the organization's lock.
func TestDeletedRoleIsNotGivenOut(t *testing.T) {
	e := openSecurityEnv(t, nil)
	ctx := context.Background()
	acme := e.team(t, "role-race")
	now := time.Now().UTC()
	role := model.Role{ID: acme.org.ID + "-role", OrgID: acme.org.ID, Name: "Reviewers", Permissions: model.PermDatasetsRead, CreatedBy: "it", CreatedAt: now}
	if err := e.tenancy.CreateRole(ctx, role); err != nil {
		t.Fatal(err)
	}
	if err := e.tenancy.DeleteRole(ctx, acme.org.ID, role.ID, now); err != nil {
		t.Fatal(err)
	}

	inv := model.Invitation{ID: acme.org.ID + "-inv", OrgID: acme.org.ID, Email: e.email("late-invitee"), RoleID: role.ID,
		Status: model.InvitationPending, CreatedBy: acme.ownerUser.ID, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
	err := e.tenancy.CreateInvitation(ctx, inv, []byte("hash-"+e.run))
	if n := column(t, e.db, `SELECT count(*)::text FROM invitations WHERE id = $1`, inv.ID); n != "0" {
		t.Fatalf("an invitation to a deleted role was created (%v)", err)
	}
	_, err = e.tenancy.ChangeMember(ctx, interfaces.MemberChange{OrgID: acme.org.ID, UserID: acme.devUser.ID, RoleID: role.ID, Now: now},
		func(before model.Membership, after *model.Membership) model.AuditEvent {
			return audit.From(ctx, acme.owner, "member.role_change", "membership", acme.devUser.ID)
		})
	if got := column(t, e.db, `SELECT role_id FROM memberships WHERE org_id = $1 AND user_id = $2`, acme.org.ID, acme.devUser.ID); got != model.RoleDeveloper {
		t.Fatalf("a member was given a deleted role: %s (%v)", got, err)
	}
}

// A lock sync request's position only moves forward, and a finished one
// stays finished.
func TestLockSyncAdvanceOnlyMovesForward(t *testing.T) {
	e, tenant := guardTenant(t, "lock-sync-fwd")
	ctx := context.Background()
	repo := postgres.NewLockSyncRepository(e.db)
	now := time.Now().UTC()
	req := model.LockSyncRequest{ID: tenant + "-sync", TenantID: tenant, Scope: model.ScopeTenant, Reason: "it", CreatedAt: now}
	if err := repo.Request(ctx, req); err != nil {
		t.Fatal(err)
	}
	if err := repo.Advance(ctx, req.ID, "asset-b", false, now); err != nil {
		t.Fatal(err)
	}
	_ = repo.Advance(ctx, req.ID, "asset-a", false, now)
	if pos := column(t, e.db, `SELECT position FROM lock_sync_requests WHERE id = $1`, req.ID); pos != "asset-b" {
		t.Fatalf("the position went back to %s", pos)
	}
	if err := repo.Advance(ctx, req.ID, "asset-c", true, now); err != nil {
		t.Fatal(err)
	}
	_ = repo.Advance(ctx, req.ID, "asset-c", false, now)
	if done := column(t, e.db, `SELECT done_at::text FROM lock_sync_requests WHERE id = $1`, req.ID); done == "<null>" {
		t.Fatal("a finished request was reopened")
	}
}

// A test of credentials replaced since is not recorded against the new
// ones.
func TestConnectionTestOfOldCredentialsIsDropped(t *testing.T) {
	e, tenant := guardTenant(t, "test-version")
	ctx := context.Background()
	repo := postgres.NewStorageConnectionRepository(e.db)
	c := activeConnection(t, repo, tenant, "tested", testKeyring(t))
	current, err := repo.GetConnection(ctx, tenant, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Rotated while a test of version 1 ran.
	if _, err := repo.RotateCredential(ctx, tenant, c.ID, current.Revision, interfaces.SealedCredential{Version: 2, KeyVersion: 1,
		Nonce: []byte("n2"), Ciphertext: []byte("c2"), WrappedKey: []byte("w2")}, "it", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	_, err = repo.RecordTest(ctx, tenant, c.ID, interfaces.ConnectionTestRecord{OK: false, ErrorCategory: "AUTH", At: time.Now().UTC(),
		CredentialVersion: current.CredentialVersion})
	if got := column(t, e.db, `SELECT last_error_category FROM storage_connections WHERE id = $1`, c.ID); got != "<null>" || !errors.Is(err, model.ErrCredentialChanged) {
		t.Fatalf("an old credential's failed test was recorded against the new one: %s (%v)", got, err)
	}
	if _, err := repo.RecordTest(ctx, tenant, c.ID, interfaces.ConnectionTestRecord{OK: false, ErrorCategory: "AUTH", At: time.Now().UTC(),
		CredentialVersion: 2}); err != nil {
		t.Fatal(err)
	}
	if got := column(t, e.db, `SELECT last_error_category FROM storage_connections WHERE id = $1`, c.ID); got != "AUTH" {
		t.Fatalf("a test of the current credential was not recorded: %s", got)
	}
}

// Starting an upload again with the key of a session that has ended is
// refused with a code the SDKs read as "start again", not given a URL
// that no longer works. A completed upload still answers with its status.
func TestUploadReplayOfAnEndedSessionIsRefused(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	acme := s.team(t, "upload-ended")
	tokens := s.signIn(t, acme.devUser.Email, acme.devPW)
	start := func(key string) httpResult {
		body := fmt.Sprintf(`{"file_name":"a.txt","content_type":"text/plain","size_bytes":5,"idempotency_key":%q}`, key)
		return s.call(t, tokens.AccessToken, acme.org.ID, http.MethodPost, "/api/v1/uploads", body)
	}
	for _, c := range []struct{ name, change string }{
		{"expired", `status = 'EXPIRED'`},
		{"aborted", `status = 'ABORTED'`},
		{"lapsed", `created_at = now() - interval '2 hours', expires_at = now() - interval '1 hour'`},
	} {
		key := "ended-" + c.name + "-" + s.run
		if r := start(key); r.status != http.StatusCreated {
			t.Fatalf("%s: start: %s", c.name, r)
		}
		if _, err := s.db.Pool.Exec(ctx, `UPDATE upload_sessions SET `+c.change+` WHERE tenant_id = $1 AND idempotancy_key = $2`, acme.org.ID, key); err != nil {
			t.Fatal(err)
		}
		r := start(key)
		if r.status != http.StatusConflict || !strings.Contains(string(r.body), `"session_ended"`) || !strings.Contains(string(r.body), "not active") {
			t.Fatalf("%s: replaying an ended session: %s", c.name, r)
		}
	}

	key := "ended-completed-" + s.run
	if r := start(key); r.status != http.StatusCreated {
		t.Fatalf("start: %s", r)
	}
	if _, err := s.db.Pool.Exec(ctx, `UPDATE upload_sessions SET status = 'COMPLETED', created_at = now() - interval '2 hours', expires_at = now() - interval '1 hour'
		WHERE tenant_id = $1 AND idempotancy_key = $2`, acme.org.ID, key); err != nil {
		t.Fatal(err)
	}
	if r := start(key); r.status != http.StatusOK || !strings.Contains(string(r.body), `"COMPLETED"`) {
		t.Fatalf("replaying a completed upload: %s", r)
	}
}
