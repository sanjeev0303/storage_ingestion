package integration

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	datasetservice "github.com/sanjeev0303/object-storage-platform/internal/service/dataset"
	"github.com/sanjeev0303/object-storage-platform/internal/service/domain"
	"github.com/sanjeev0303/object-storage-platform/internal/service/edge"
	"github.com/sanjeev0303/object-storage-platform/internal/service/erasure"
	"github.com/sanjeev0303/object-storage-platform/internal/service/ingestion"
	"github.com/sanjeev0303/object-storage-platform/internal/service/tiering"
)

// Each state machine here moves a row only from a state that allows the
// move, checked in the same statement that moves it.

// waitForLockWaiters returns once n sessions of this database wait on a
// lock: the calls under test have reached the row the test holds.
func waitForLockWaiters(t *testing.T, db *postgres.DB, n int) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
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
	t.Fatalf("fewer than %d sessions waited on the held lock", n)
}

func newErasureService(s *securityStack) *erasure.Service {
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{9}, ed25519.SeedSize))
	return erasure.New(erasure.Deps{Store: postgres.NewErasureRepository(s.db), Lifecycle: s.lifecycle, Catalog: s.catalog,
		Audit: s.audit, Log: s.log, Signer: erasure.Signer{KeyID: "it-race", Key: key}})
}

// A cancel that lands while an execute is under way never leaves a
// cancelled erasure whose snapshots, drafts or deletions already changed:
// the execute claims the request before it touches anything.
func TestErasureCancelRacingExecuteLeavesNoSideEffects(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	acme := s.team(t, "erasure-race")
	dev, owner := acme.developer, acme.owner
	er := newErasureService(s)

	a := s.publishAt(t, dev, "race/p9.txt", textData("subject nine", 300), withMetadata(map[string]string{"subject_id": "P-9"}))
	d := s.newDataset(t, dev, "race")
	s.addMembers(t, dev, d.ID, memberOf(a, "", "train"))
	v1, _, err := s.datasets.Snapshot(ctx, dev, d.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	e, err := er.Create(ctx, owner, erasure.CreateInput{Kind: "PII", Reason: "race", Selector: model.ErasureSelector{SubjectIDs: []string{"P-9"}}})
	if err != nil {
		t.Fatal(err)
	}

	// Holding the dataset row stops the execute at the successor snapshot.
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
	_, cancelErr := er.Cancel(ctx, owner, e.ID, "changed our mind")
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	execErr := <-executed

	got, err := er.Get(ctx, owner, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	old, _ := s.datasets.Version(ctx, dev, d.ID, v1.ID)
	asset, _ := s.ie.assets.GetByID(ctx, acme.org.ID, a.ID)
	if got.State == model.ErasureCancelled && (old.SupersededBy != "" || asset.Status != model.AssetStatusAvailable) {
		t.Fatalf("a cancelled erasure ran: snapshot superseded by %q, asset %s (execute: %v)", old.SupersededBy, asset.Status, execErr)
	}
	// The execute claimed the request first, so the cancel came too late.
	if execErr != nil || !errors.Is(cancelErr, model.ErrErasureState) || got.State != model.ErasureDeleting || len(got.Items) != 1 {
		t.Fatalf("execute %v, cancel %v, erasure %s with %d items", execErr, cancelErr, got.State, len(got.Items))
	}
}

// Two successors racing for one snapshot make one: the second finds it
// superseded. A destroyed erasure item stays destroyed.
func TestErasureSuccessorOnceAndItemsDoNotRegress(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	acme := s.team(t, "erasure-succ")
	dev := acme.developer
	store := postgres.NewErasureRepository(s.db)

	a := s.publishAt(t, dev, "succ/a.txt", textData("succ a", 100))
	b := s.publishAt(t, dev, "succ/b.txt", textData("succ b", 100))
	d := s.newDataset(t, dev, "succ")
	s.addMembers(t, dev, d.ID, memberOf(a, "", ""), memberOf(b, "", ""))
	v1, _, err := s.datasets.Snapshot(ctx, dev, d.ID, "")
	if err != nil {
		t.Fatal(err)
	}

	tx, err := s.db.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `SELECT 1 FROM datasets WHERE id = $1 FOR UPDATE`, d.ID); err != nil {
		t.Fatal(err)
	}
	var made atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, ok, err := store.Successor(ctx, acme.org.ID, v1.ID, []string{a.ID}, "er-1", "it", fmt.Sprintf("%s-succ-%d", acme.org.ID, i), time.Now().UTC())
			if err != nil {
				t.Errorf("successor: %v", err)
			}
			if ok {
				made.Add(1)
			}
		}()
	}
	waitForLockWaiters(t, s.db, 2)
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	var versions int
	if err := s.db.Pool.QueryRow(ctx, `SELECT count(*) FROM dataset_versions WHERE parent_version_id = $1`, v1.ID).Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if made.Load() != 1 || versions != 1 {
		t.Fatalf("one snapshot got %d successors (%d reported made)", versions, made.Load())
	}

	// Items: an erasure request row is needed for the foreign key.
	e := model.Erasure{ID: acme.org.ID + "-er", TenantID: acme.org.ID, Kind: model.ErasureOther, State: model.ErasureDeleting, Reason: "it",
		Selector:      model.ErasureSelector{SubjectIDs: []string{}, SHA256s: []string{}, ObjectIDs: []string{}, SubjectKey: model.DefaultSubjectKey},
		SubjectHashes: []string{}, RequestedBy: "it", CreatedAt: time.Now().UTC()}
	if err := store.Create(ctx, e); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	item := model.ErasureItem{AssetID: a.ID, Path: "succ/a.txt", SHA256: a.ChecksumSHA256, ConnectionID: "platform", State: model.ErasureItemDestroyed, DestroyedAt: &now}
	if err := store.PutItems(ctx, acme.org.ID, e.ID, []model.ErasureItem{item}, now); err != nil {
		t.Fatal(err)
	}
	item.State, item.DestroyedAt = model.ErasureItemDeleting, nil
	if err := store.PutItems(ctx, acme.org.ID, e.ID, []model.ErasureItem{item}, now); err != nil {
		t.Fatal(err)
	}
	items, err := store.Items(ctx, acme.org.ID, e.ID)
	if err != nil || len(items) != 1 || items[0].State != model.ErasureItemDestroyed || items[0].DestroyedAt == nil {
		t.Fatalf("a destroyed item went back: %+v %v", items, err)
	}
}

// An approved change is EXECUTING while it runs, never EXECUTED before it
// is made. An approval past its expiry can never be claimed, and turns
// EXPIRED when someone tries.
func TestApprovalIsExecutingWhileItRunsAndExpiresOnClaim(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	acme := s.team(t, "approval-exec")
	if _, err := s.lifecycle.SetGovernanceFor(ctx, acme.owner, model.GovernanceSettings{Profile: model.ProfileStandard, TwoPerson: true}, ""); err != nil {
		t.Fatal(err)
	}
	pending := func(reason string) (model.LegalHold, string) {
		t.Helper()
		hold, err := s.lifecycle.PlaceHoldFor(ctx, acme.owner, model.LegalHold{Scope: model.ScopeTenant, Reason: reason})
		if err != nil {
			t.Fatal(err)
		}
		var waits *model.ApprovalRequiredError
		if err := s.lifecycle.ReleaseHoldGoverned(ctx, acme.owner, hold.ID, "settled"); !errors.As(err, &waits) {
			t.Fatalf("releasing under the two-person rule: %v", err)
		}
		return hold, waits.ID
	}
	state := func(id string) string {
		return column(t, s.db, `SELECT state FROM governance_approvals WHERE id = $1`, id)
	}

	hold, id := pending("case 1")
	tx, err := s.db.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `SELECT 1 FROM legal_holds WHERE id = $1 FOR UPDATE`, hold.ID); err != nil {
		t.Fatal(err)
	}
	decided := make(chan error, 1)
	var result model.Approval
	go func() {
		var err error
		result, err = s.lifecycle.DecideFor(ctx, acme.admin, id, true, "checked")
		decided <- err
	}()
	waitForLockWaiters(t, s.db, 1)
	if got := state(id); got != "EXECUTING" {
		t.Fatalf("while the release runs the approval is %s", got)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-decided; err != nil || result.State != model.ApprovalExecuted || state(id) != model.ApprovalExecuted {
		t.Fatalf("after the release: %+v %v (stored %s)", result, err, state(id))
	}

	_, late := pending("case 2")
	if _, err := s.db.Pool.Exec(ctx, `UPDATE governance_approvals SET expires_at = now() - interval '1 minute' WHERE id = $1`, late); err != nil {
		t.Fatal(err)
	}
	if _, err := s.lifecycle.DecideFor(ctx, acme.admin, late, true, ""); !errors.Is(err, model.ErrApprovalDecided) {
		t.Fatalf("approving an expired request: %v", err)
	}
	if got := state(late); got != model.ApprovalExpired {
		t.Fatalf("an expired approval someone tried to approve is %s", got)
	}
}

// A deletion that failed before it started goes back to SCHEDULED when
// requeued, so it can still be restored; one whose storage deletion began
// can no longer be cancelled.
func TestDeletionRequeueAndCancelFollowWhatWasRemoved(t *testing.T) {
	env := openProcessingEnv(t)
	lifecycleCleanup(t, env)
	ctx := context.Background()
	repo := postgres.NewDeletionRepository(env.db)
	now := time.Now().UTC()

	// Failed before it started: restorable after the requeue.
	never := availableAsset(t, env, "never-started")
	if _, _, err := repo.SoftDelete(ctx, scheduled(never, now), []model.AssetStatus{model.AssetStatusAvailable}, event(env.tenant, "requested-1", now)); err != nil {
		t.Fatal(err)
	}
	claimed, _ := repo.Claim(ctx, "w", now.Add(time.Second), time.Minute, 50, env.tenant)
	if len(claimed) != 1 {
		t.Fatalf("claimed %d", len(claimed))
	}
	if err := repo.Fail(ctx, claimed[0].ID, "w", now.Add(2*time.Second), "storage unreachable", event(env.tenant, "failed-1", now)); err != nil {
		t.Fatal(err)
	}
	op, err := repo.Requeue(ctx, env.tenant, claimed[0].ID, now.Add(3*time.Second))
	if err != nil || op.State != model.DeletionScheduled {
		t.Fatalf("requeueing a deletion that never started: %s %v", op.State, err)
	}
	if _, err := repo.Restore(ctx, env.tenant, never.ID, now.Add(4*time.Second), event(env.tenant, "restored", now)); err != nil {
		t.Fatalf("a requeued deletion that never started is restorable: %v", err)
	}

	// Started, then waiting to retry: the cancel is refused.
	started := availableAsset(t, env, "started")
	if _, _, err := repo.SoftDelete(ctx, scheduled(started, now), []model.AssetStatus{model.AssetStatusAvailable}, event(env.tenant, "requested-2", now)); err != nil {
		t.Fatal(err)
	}
	claimed, _ = repo.Claim(ctx, "w", now.Add(5*time.Second), time.Minute, 50, env.tenant)
	if len(claimed) != 1 {
		t.Fatalf("claimed %d", len(claimed))
	}
	opID := claimed[0].ID
	if ok, err := repo.Start(ctx, opID, "w", now.Add(6*time.Second)); err != nil || !ok {
		t.Fatalf("start: %v %v", ok, err)
	}
	if err := repo.Retry(ctx, opID, "w", now.Add(7*time.Second), now.Add(7*time.Second), "storage timeout"); err != nil {
		t.Fatal(err)
	}
	if again, _ := repo.Claim(ctx, "w", now.Add(8*time.Second), time.Minute, 50, env.tenant); len(again) != 1 {
		t.Fatalf("claimed %d again", len(again))
	}
	if err := repo.Cancel(ctx, opID, "w", now.Add(9*time.Second), "asset is AVAILABLE"); err == nil {
		t.Fatal("a deletion whose storage deletion began was cancelled")
	}
	if got, _ := repo.Get(ctx, env.tenant, opID); got.State != model.DeletionRetryWait {
		t.Fatalf("after the refused cancel: %s", got.State)
	}

	// Requeueing a started one keeps it out of reach of a restore.
	if err := repo.Fail(ctx, opID, "w", now.Add(10*time.Second), "gave up", event(env.tenant, "failed-2", now)); err != nil {
		t.Fatal(err)
	}
	if op, err := repo.Requeue(ctx, env.tenant, opID, now.Add(11*time.Second)); err != nil || op.State != model.DeletionRetryWait {
		t.Fatalf("requeueing a started deletion: %s %v", op.State, err)
	}
}

// Two retries of a finished job at once revive it once: active_jobs counts
// it once, and no entry waits in a job that has ended.
func TestIngestionConcurrentRetriesCountTheJobOnce(t *testing.T) {
	e := openIngestEnv(t)
	ctx := context.Background()
	d := e.dataset(t, "double-retry", model.CompleteAll)
	files := []file{textFile("ok.txt", 100), textFile("short-1.txt", 100), textFile("short-2.txt", 100)}
	data := contents(files...)
	job := e.readyJob(t, d.ID, files...)
	grants := e.claim(t, job.ID, 3).Grants
	e.process(t, e.upload(t, grants[0], data["ok.txt"]))
	e.upload(t, grants[1], []byte("short"))
	e.upload(t, grants[2], []byte("short"))
	if j := e.job(t, job.ID); !j.State.Terminal() {
		t.Fatalf("job: %s", j.State)
	}
	if u := e.usage(t); u.ActiveJobs != 0 {
		t.Fatalf("a finished job is still active: %+v", u)
	}

	// Holding the usage row lets both retries read the job before either
	// revives it.
	tx, err := e.db.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `SELECT 1 FROM ingestion_tenant_usage WHERE tenant_id = $1 FOR UPDATE`, e.tenant); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for _, pos := range []int64{2, 3} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := e.svc.Retry(ctx, e.p, job.ID, []int64{pos}, false); err != nil {
				t.Errorf("retry %d: %v", pos, err)
			}
		}()
	}
	waitForLockWaiters(t, e.db, 2)
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if u := e.usage(t); u.ActiveJobs != 1 {
		t.Fatalf("two retries counted the job %d times", u.ActiveJobs)
	}
	j := e.job(t, job.ID)
	if pending := e.entries(t, job.ID, model.EntryPending); j.State != model.JobRunning || len(pending) != 2 {
		t.Fatalf("job %s with %d pending entries", j.State, len(pending))
	}
}

// An archived dataset takes no new ingestion job.
func TestIngestionRefusesAnArchivedDataset(t *testing.T) {
	e := openIngestEnv(t)
	ctx := context.Background()
	d := e.dataset(t, "archived-ingest", model.CompleteAll)
	e.exec(t, `UPDATE datasets SET lifecycle_status = 'ARCHIVED', archived_at = now() WHERE id = $1`, d.ID)
	*e.jobs++
	_, _, err := e.svc.CreateJob(ctx, e.p, ingestion.CreateJobInput{DatasetID: d.ID, IdempotencyKey: fmt.Sprintf("job-%d", *e.jobs)})
	if !errors.Is(err, model.ErrDatasetArchived) {
		t.Fatalf("an ingestion job on an archived dataset: %v", err)
	}
}

// A released snapshot never expires: it is kept, with its members, and the
// objects only it holds are not deleted.
func TestExpiryKeepsAReleasedVersion(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	acme := s.team(t, "expiry-release")
	dev := acme.developer
	svc := tiering.New(tiering.Deps{Store: postgres.NewTieringRepository(s.db), Assets: postgres.NewAssetRepository(s.db),
		Datasets: s.datasets, Audit: s.audit, Log: s.log,
		Delete: func(ctx context.Context, p model.Principal, assetID string) error {
			_, err := s.lifecycle.RequestDeletion(ctx, p, assetID)
			return err
		}}, tiering.Config{})

	// x1 is only in v1; v2 and v3 hold x2.
	ds := s.newDataset(t, dev, "released")
	x1 := s.publishAt(t, dev, "rel/x.txt", textData("x-one", 120))
	s.addMembers(t, dev, ds.ID, memberOf(x1, "", ""))
	v1, _, _ := s.datasets.Snapshot(ctx, dev, ds.ID, "")
	x2 := s.publishAt(t, dev, "rel/x.txt", textData("x-two", 130))
	if _, err := s.datasets.AddMembers(ctx, dev, ds.ID, []datasetservice.MemberInput{memberOf(x2, "", "")}, model.DuplicateReplace); err != nil {
		t.Fatal(err)
	}
	s.datasets.Snapshot(ctx, dev, ds.ID, "")
	y := s.publishAt(t, dev, "rel/y.txt", textData("y", 90))
	s.addMembers(t, dev, ds.ID, memberOf(y, "", ""))
	s.datasets.Snapshot(ctx, dev, ds.ID, "")
	if err := postgres.NewQualityRepository(s.db).SetRelease(ctx, acme.org.ID, v1.ID, "DR1", "", "it", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	done, err := svc.ExpireVersions(ctx, acme.owner, ds.ID, tiering.ExpiryInput{KeepLast: 1})
	if err != nil {
		t.Fatal(err)
	}
	kept := false
	for _, k := range done.Keep {
		kept = kept || k.ID == v1.ID
	}
	expiredAt := column(t, s.db, `SELECT expired_at::text FROM dataset_versions WHERE id = $1`, v1.ID)
	members := column(t, s.db, `SELECT count(*)::text FROM dataset_version_members WHERE version_id = $1`, v1.ID)
	if !kept || expiredAt != "<null>" || members != "1" {
		t.Fatalf("the release: kept %v, expired at %s, %s members (%+v)", kept, expiredAt, members, done)
	}
	if got, _ := postgres.NewAssetRepository(s.db).GetByID(ctx, acme.org.ID, x1.ID); got.Status != model.AssetStatusAvailable {
		t.Fatalf("the released version's object was deleted: %s", got.Status)
	}
}

// A withdrawn publication gets no DOI: the provider is not even asked.
func TestDOIIsNotMintedForAWithdrawnPublication(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	acme := s.team(t, "doi-withdrawn")
	var calls atomic.Int32
	dc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusCreated)
	}))
	defer dc.Close()
	svc := domain.New(domain.Deps{Store: postgres.NewDomainRepository(s.db), Datasets: s.datasets, Lineage: s.datasetDB, Audit: s.audit,
		Client: http.DefaultClient, PublicURL: "https://console.test", Log: s.log,
		DataCite: domain.DataCite{API: dc.URL, Repository: "ORG.REPO", Password: "secret", Prefix: "10.5072"}})

	a := s.publishAt(t, acme.developer, "pub/a.txt", textData("pub a", 100))
	d := s.newDataset(t, acme.developer, "pub")
	s.addMembers(t, acme.developer, d.ID, memberOf(a, "", ""))
	v, _, err := s.datasets.Snapshot(ctx, acme.developer, d.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	meta := model.PublicationMeta{Title: "Pub", Creators: []model.Creator{{Name: "Ada"}}, Publisher: "Acme", License: "CC-BY-4.0"}
	if _, err := svc.Publish(ctx, acme.owner, d.ID, v.ID, meta, false); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Withdraw(ctx, acme.owner, d.ID, v.ID, "wrong data"); err != nil {
		t.Fatal(err)
	}
	pub, err := svc.MintDOI(ctx, acme.owner, d.ID, v.ID)
	if err == nil || pub.DOI != "" || calls.Load() != 0 {
		t.Fatalf("minting for a withdrawn publication: %+v %v, DataCite asked %d times", pub, err, calls.Load())
	}
	if doi := column(t, s.db, `SELECT doi FROM publications WHERE version_id = $1`, v.ID); doi != "" {
		t.Fatalf("a DOI was recorded: %q", doi)
	}
}

// An adoption job moves only along QUEUED, READING, RUNNING, then
// COMPLETED or FAILED. A cancelled job takes no more entry outcomes.
func TestAdoptionMovesOnlyForward(t *testing.T) {
	e, tenant := guardTenant(t, "adoption-guard")
	ctx := context.Background()
	repo := postgres.NewAdoptionRepository(e.db)
	now := time.Now().UTC()
	job := model.AdoptionJob{ID: tenant + "-adopt", TenantID: tenant, ConnectionID: model.PlatformConnectionID, Source: model.AdoptManifest,
		SourceRef: "inline", InlineSource: "a.txt\n", Mode: model.AdoptionDetach, CreatedBy: "it", CreatedAt: now}
	if err := repo.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.Pool.Exec(ctx, `UPDATE adoption_jobs SET lease_owner = 'w', lease_expires_at = now() + interval '1 minute' WHERE id = $1`, job.ID); err != nil {
		t.Fatal(err)
	}
	state := func() string { return column(t, e.db, `SELECT state FROM adoption_jobs WHERE id = $1`, job.ID) }
	if err := repo.SetState(ctx, job.ID, "w", model.AdoptionCompleted, "", now); err == nil || state() != model.AdoptionQueued {
		t.Fatalf("a queued job completed without being read: %v, now %s", err, state())
	}
	if err := repo.SetState(ctx, job.ID, "w", model.AdoptionReading, "", now); err != nil {
		t.Fatal(err)
	}
	if err := repo.AddEntries(ctx, []model.AdoptionEntry{{JobID: job.ID, TenantID: tenant, Line: 1, ObjectKey: "a.txt", Path: "a.txt", State: model.AdoptPending}}, now); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetState(ctx, job.ID, "w", model.AdoptionRunning, "", now); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetState(ctx, job.ID, "w", model.AdoptionReading, "", now); err == nil || state() != model.AdoptionRunning {
		t.Fatalf("a running job went back to reading: %v, now %s", err, state())
	}

	if err := repo.Cancel(ctx, tenant, job.ID, now); err != nil {
		t.Fatal(err)
	}
	err := repo.FinishEntry(ctx, model.AdoptionEntry{JobID: job.ID, Line: 1, State: model.AdoptAdopted, AssetID: "late"}, now)
	if got := column(t, e.db, `SELECT state FROM adoption_entries WHERE job_id = $1 AND line = 1`, job.ID); got != model.AdoptPending || err == nil {
		t.Fatalf("a cancelled job took an entry outcome: entry %s, %v", got, err)
	}
}

// A COMPLETE recording is final: it takes no more chunks, and a close that
// read it before it completed does not mark it INCOMPLETE.
func TestEdgeCompleteRecordingIsFinal(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	acme := s.team(t, "edge-final")
	store := postgres.NewEdgeRepository(s.db)
	svc := edge.New(edge.Deps{Store: store, Access: s.access, Datasets: s.datasets, Audit: s.audit, Log: s.log})
	ds := s.newDataset(t, acme.owner, "edge-final")
	dev, _, err := svc.Register(ctx, acme.owner, "probe", ds.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	me := model.Principal{TenantID: acme.owner.TenantID, UserID: acme.owner.UserID, Permissions: model.PermUploadsWrite,
		Scope: model.Scope{TokenID: dev.TokenID, Datasets: []string{ds.Name}, Restricted: true, NoLinks: true, PathPrefix: dev.PathPrefix}}
	rec, err := svc.OpenRecording(ctx, me, "run-1", model.RecordingRun, 0)
	if err != nil {
		t.Fatal(err)
	}
	stale := rec
	if _, err := s.db.Pool.Exec(ctx, `UPDATE edge_recordings SET state = 'COMPLETE', closed_at = now() WHERE id = $1`, rec.ID); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	stale.State, stale.Missing, stale.ClosedAt = model.RecordingIncomplete, []string{"late close"}, &now
	_ = store.CloseRecording(ctx, stale, 0)
	if got := column(t, s.db, `SELECT state FROM edge_recordings WHERE id = $1`, rec.ID); got != model.RecordingComplete {
		t.Fatalf("a stale close overwrote COMPLETE with %s", got)
	}
	_, err = svc.AddChunk(ctx, me, rec.ID, model.EdgeChunk{Path: dev.PathPrefix + "run-1/late.bin", SHA256: sha256Of([]byte("late")), SizeBytes: 4})
	if err == nil {
		t.Fatal("a complete recording took a chunk")
	}
	if n := column(t, s.db, `SELECT count(*)::text FROM edge_chunks WHERE recording_id = $1`, rec.ID); n != "0" {
		t.Fatalf("%s chunks on a complete recording", n)
	}
}

// A profile or chunk job records an outcome only while it is RUNNING under
// the worker's lease.
func TestProfileAndChunkJobFinishNeedRunning(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	acme := s.team(t, "finish-running")
	a := s.publishAt(t, acme.developer, "fin/a.txt", textData("fin a", 100))
	d := s.newDataset(t, acme.developer, "fin")
	s.addMembers(t, acme.developer, d.ID, memberOf(a, "", ""))
	v, _, err := s.datasets.Snapshot(ctx, acme.developer, d.ID, "")
	if err != nil {
		t.Fatal(err)
	}

	// Left by its worker's owner name in a state other than RUNNING.
	if _, err := s.db.Pool.Exec(ctx, `INSERT INTO version_profiles (version_id, tenant_id, dataset_id, state) VALUES ($1, $2, $3, 'PENDING')
		ON CONFLICT (version_id) DO NOTHING`, v.ID, acme.org.ID, d.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Pool.Exec(ctx, `UPDATE version_profiles SET state = 'PENDING', lease_owner = 'w', lease_until = now() + interval '1 minute'
		WHERE version_id = $1`, v.ID); err != nil {
		t.Fatal(err)
	}
	quality := postgres.NewQualityRepository(s.db)
	_ = quality.FinishProfile(ctx, "w", model.VersionProfile{VersionID: v.ID, State: "DONE", Tables: []model.TableProfile{}, Skipped: []model.SkippedFile{}})
	if got := column(t, s.db, `SELECT state FROM version_profiles WHERE version_id = $1`, v.ID); got != "PENDING" {
		t.Fatalf("a profile that is not running was finished: %s", got)
	}

	chunks := postgres.NewChunkRepository(s.db)
	if err := chunks.Requeue(ctx, acme.org.ID, d.ID, v.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Pool.Exec(ctx, `UPDATE chunk_jobs SET lease_owner = 'w', lease_until = now() + interval '1 minute' WHERE version_id = $1`, v.ID); err != nil {
		t.Fatal(err)
	}
	_ = chunks.Finish(ctx, "w", model.ChunkJob{VersionID: v.ID, Skipped: []model.SkippedFile{}})
	if got := column(t, s.db, `SELECT state FROM chunk_jobs WHERE version_id = $1`, v.ID); got != "PENDING" {
		t.Fatalf("a chunk job that is not running was finished: %s", got)
	}
}
