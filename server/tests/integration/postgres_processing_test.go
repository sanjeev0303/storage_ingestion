package integration

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// Needs TEST_DATABASE_URL with every migration (through 000015) applied. EnqueueDue
// works on the whole database, so use a dedicated test database only.

type processingEnv struct {
	*reconcileEnv
	assets *postgres.AssetRepository
	jobs   *postgres.ProcessingJobRepository
}

func openProcessingEnv(t *testing.T) *processingEnv {
	env := openReconcileEnv(t)
	return &processingEnv{
		reconcileEnv: env,
		assets:       postgres.NewAssetRepository(env.db),
		jobs:         postgres.NewProcessingJobRepository(env.db),
	}
}

// ourJobs returns this run's jobs, keyed by upload ID.
func (e *processingEnv) ourJobs(t *testing.T) map[string]model.ProcessingJob {
	t.Helper()
	rows, err := e.db.Pool.Query(context.Background(),
		`SELECT upload_id, id, asset_id, stage, state FROM processing_jobs WHERE tenant_id = $1`, e.tenant)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	out := map[string]model.ProcessingJob{}
	for rows.Next() {
		var job model.ProcessingJob
		var stage, state string
		if err := rows.Scan(&job.UploadID, &job.ID, &job.AssetID, &stage, &state); err != nil {
			t.Fatal(err)
		}
		job.Stage, job.State = model.ProcessingStage(stage), model.ProcessingState(state)
		out[job.UploadID] = job
	}
	return out
}

func TestPostgresEnqueueCreatesOneAssetAndJobPerCompletedUpload(t *testing.T) {
	env := openProcessingEnv(t)
	ctx := context.Background()
	env.session(t, "done", model.UploadStatusCompleted)
	env.session(t, "verified", model.UploadStatusVerified)
	env.session(t, "uploading", model.UploadStatusUploading)

	now := time.Now().UTC()
	for i := 0; i < 3; i++ { // repeated and duplicate events
		if _, err := env.jobs.EnqueueDue(ctx, now, 5); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}

	jobs := env.ourJobs(t)
	if len(jobs) != 2 {
		t.Fatalf("expected 2 jobs, got %d", len(jobs))
	}
	for _, id := range []string{"done", "verified"} {
		job, ok := jobs[env.tenant+"-"+id]
		if !ok || job.Stage != model.StageVerify || job.State != model.ProcessingPending {
			t.Fatalf("%s: unexpected job %+v", id, job)
		}
		asset, err := env.assets.GetByID(ctx, env.tenant, job.AssetID)
		if err != nil || asset.Status != model.AssetStatusQuarantined || asset.UploadID != env.tenant+"-"+id {
			t.Fatalf("%s: unexpected asset %+v, %v", id, asset, err)
		}
	}
}

func TestPostgresProcessingClaimsAreExclusiveAndRecoverable(t *testing.T) {
	env := openProcessingEnv(t)
	ctx := context.Background()
	for i := 0; i < 6; i++ {
		env.session(t, fmt.Sprintf("u%d", i), model.UploadStatusCompleted)
	}
	if _, err := env.jobs.EnqueueDue(ctx, time.Now().UTC(), 5); err != nil {
		t.Fatal(err)
	}
	// Make this run's jobs due before any other in the database.
	if _, err := env.db.Pool.Exec(ctx, `UPDATE processing_jobs SET next_attempt_at = $2 WHERE tenant_id = $1`, env.tenant, farPast); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	var mu sync.Mutex
	owners := map[string]string{}
	var wg sync.WaitGroup
	for w := 0; w < 3; w++ {
		wg.Add(1)
		go func(owner string) {
			defer wg.Done()
			got, err := env.jobs.Claim(ctx, owner, now, time.Minute, 2)
			if err != nil {
				t.Errorf("claim: %v", err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			for _, job := range got {
				if prev, dup := owners[job.ID]; dup {
					t.Errorf("job %s claimed by %s and %s", job.ID, prev, owner)
				}
				owners[job.ID] = owner
			}
		}(fmt.Sprintf("worker-%d", w))
	}
	wg.Wait()

	// A crashed owner's job is claimable after its lease, and the crashed
	// owner can no longer write.
	var jobID, crashed string
	for id, owner := range owners {
		jobID, crashed = id, owner
		break
	}
	later := now.Add(2 * time.Minute)
	recovered := false
	for _, job := range mustClaimJobs(t, env, later) {
		recovered = recovered || job.ID == jobID
	}
	if !recovered {
		t.Fatal("an expired lease was not recovered")
	}
	if err := env.jobs.Finish(ctx, jobID, crashed, later, model.ProcessingSucceeded, ""); !errors.Is(err, model.ErrLeaseLost) {
		t.Fatalf("the crashed owner must not write, got %v", err)
	}
}

func mustClaimJobs(t *testing.T, env *processingEnv, now time.Time) []model.ProcessingJob {
	t.Helper()
	got, err := env.jobs.Claim(context.Background(), "worker-recover", now, time.Minute, 50)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestPostgresStageResultsAreDurable(t *testing.T) {
	env := openProcessingEnv(t)
	ctx := context.Background()
	env.session(t, "stages", model.UploadStatusCompleted)
	if _, err := env.jobs.EnqueueDue(ctx, time.Now().UTC(), 5); err != nil {
		t.Fatal(err)
	}
	job := env.ourJobs(t)[env.tenant+"-stages"]
	if _, err := env.db.Pool.Exec(ctx, `UPDATE processing_jobs SET next_attempt_at = $2 WHERE id = $1`, job.ID, farPast); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	var claimed model.ProcessingJob
	for _, j := range mustClaimJobs(t, env, now) {
		if j.ID == job.ID {
			claimed = j
		}
	}
	if claimed.ID == "" || claimed.StartedAt == nil || claimed.AttemptCount != 1 {
		t.Fatalf("claim did not record the start: %+v", claimed)
	}

	claimed.Stage = model.StagePromote
	claimed.ChecksumAlgorithm = "SHA-256"
	claimed.ChecksumVerifiedAt = &now
	claimed.DetectedContentType = "application/pdf"
	claimed.Scanner = "clamav"
	claimed.ScanResult = model.ScanClean
	claimed.ScannedAt = &now
	claimed.PromotionKey = "assets/" + env.tenant + "/" + claimed.AssetID
	// Each claim is its own lease owner: the worker's bare name is not.
	if claimed.LeaseOwner != "worker-recover#1" {
		t.Fatalf("lease owner %q", claimed.LeaseOwner)
	}
	if err := env.jobs.Renew(ctx, job.ID, "worker-recover", now, time.Hour); !errors.Is(err, model.ErrLeaseLost) {
		t.Fatalf("renew under the bare worker name: %v", err)
	}
	if err := env.jobs.Renew(ctx, job.ID, claimed.LeaseOwner, now, time.Hour); err != nil {
		t.Fatalf("renew: %v", err)
	}
	if err := env.jobs.SaveProgress(ctx, claimed, claimed.LeaseOwner, now); err != nil {
		t.Fatalf("save progress: %v", err)
	}

	var stage, algorithm, detected, scanner, scanResult, key string
	var verifiedAt, scannedAt time.Time
	err := env.db.Pool.QueryRow(ctx, `
		SELECT stage, checksum_algorithm, checksum_verified_at, detected_content_type,
		       scanner, scan_result, scanned_at, promotion_key
		FROM processing_jobs WHERE id = $1`, job.ID,
	).Scan(&stage, &algorithm, &verifiedAt, &detected, &scanner, &scanResult, &scannedAt, &key)
	if err != nil {
		t.Fatal(err)
	}
	if stage != "PROMOTE" || algorithm != "SHA-256" || !verifiedAt.Equal(now) || detected != "application/pdf" ||
		scanner != "clamav" || scanResult != "CLEAN" || !scannedAt.Equal(now) || key != claimed.PromotionKey {
		t.Fatalf("stage results not persisted: %s %s %s %s %s %s", stage, algorithm, detected, scanner, scanResult, key)
	}

	// Retry scheduling, then exhaustion, are recorded.
	if err := env.jobs.Reschedule(ctx, job.ID, claimed.LeaseOwner, now, now.Add(time.Hour), "clamd down"); err != nil {
		t.Fatal(err)
	}
	for _, j := range mustClaimJobs(t, env, now.Add(30*time.Minute)) {
		if j.ID == job.ID {
			t.Fatal("claimed before its retry time")
		}
	}
	again := mustClaimJobs(t, env, now.Add(2*time.Hour))
	var reclaimed model.ProcessingJob
	for _, j := range again {
		if j.ID == job.ID {
			reclaimed = j
		}
	}
	if err := env.jobs.Finish(ctx, job.ID, reclaimed.LeaseOwner, now.Add(2*time.Hour), model.ProcessingExhausted, "clamd down"); err != nil {
		t.Fatalf("finish: %v (claimed %d)", err, len(again))
	}
	var state string
	var completed *time.Time
	if err := env.db.Pool.QueryRow(ctx, `SELECT state, completed_at FROM processing_jobs WHERE id = $1`, job.ID).Scan(&state, &completed); err != nil {
		t.Fatal(err)
	}
	if state != "EXHAUSTED" || completed == nil {
		t.Fatalf("expected a visible EXHAUSTED job, got %s", state)
	}
}

func TestPostgresAssetTransitionsAreConditionalAndTenantScoped(t *testing.T) {
	env := openProcessingEnv(t)
	ctx := context.Background()
	env.session(t, "asset", model.UploadStatusCompleted)
	if _, err := env.jobs.EnqueueDue(ctx, time.Now().UTC(), 5); err != nil {
		t.Fatal(err)
	}
	assetID := env.ourJobs(t)[env.tenant+"-asset"].AssetID
	now := time.Now().UTC()

	// QUARANTINED cannot jump to AVAILABLE.
	err := env.assets.TransitionStatus(ctx, env.tenant, assetID,
		[]model.AssetStatus{model.AssetStatusQuarantined}, model.AssetStatusAvailable, "", "assets/x", now)
	if !errors.Is(err, model.ErrInvalidAssetState) {
		t.Fatalf("expected ErrInvalidAssetState, got %v", err)
	}

	if err := env.assets.TransitionStatus(ctx, env.tenant, assetID,
		[]model.AssetStatus{model.AssetStatusQuarantined}, model.AssetStatusProcessing, "", "", now); err != nil {
		t.Fatal(err)
	}

	// The database refuses an AVAILABLE asset still at a quarantine key.
	_, err = env.db.Pool.Exec(ctx, `UPDATE assets SET status = 'AVAILABLE' WHERE id = $1`, assetID)
	if err == nil {
		t.Fatal("an AVAILABLE asset under quarantine/ must violate the constraint")
	}

	// Promotion and deletion race: exactly one wins.
	var wg sync.WaitGroup
	results := make([]error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		results[0] = env.assets.TransitionStatus(ctx, env.tenant, assetID,
			[]model.AssetStatus{model.AssetStatusProcessing}, model.AssetStatusAvailable, "", "assets/"+env.tenant+"/"+assetID, now)
	}()
	go func() {
		defer wg.Done()
		results[1] = env.assets.TransitionStatus(ctx, env.tenant, assetID,
			[]model.AssetStatus{model.AssetStatusProcessing}, model.AssetStatusRejected, "operator", "", now)
	}()
	wg.Wait()
	if (results[0] == nil) == (results[1] == nil) {
		t.Fatalf("exactly one transition must win: %v / %v", results[0], results[1])
	}

	if _, err := env.assets.GetByID(ctx, "other-tenant", assetID); !errors.Is(err, model.ErrAssetNotFound) {
		t.Fatalf("another tenant must not see the asset, got %v", err)
	}
}
