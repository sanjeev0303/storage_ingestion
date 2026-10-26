package integration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// These tests need TEST_DATABASE_URL pointing at a dedicated test
// database with every migration (through 000015) applied. They create
// and delete their own rows; never point them at production.

// farPast puts this run's tasks ahead of any other due task in the test
// database, so the claims below pick them first.
var farPast = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

type reconcileEnv struct {
	db      *postgres.DB
	uploads *postgres.UploadRepository
	tasks   *postgres.ReconciliationRepository
	tenant  string
}

func openReconcileEnv(t *testing.T) *reconcileEnv {
	t.Helper()
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set; PostgreSQL reconciliation tests were not run")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	db, err := postgres.Open(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}

	env := &reconcileEnv{
		db:      db,
		uploads: postgres.NewUploadRepository(db),
		tasks:   postgres.NewReconciliationRepository(db),
		tenant:  fmt.Sprintf("reconcile-test-%d", time.Now().UnixNano()),
	}
	t.Cleanup(func() {
		ctx := context.Background()
		// Tasks go with their sessions (ON DELETE CASCADE); events, written
		// by completions, are removed by tenant.
		for _, table := range []string{"outbox_events", "event_inbox", "dead_letters", "event_discrepancies"} {
			_, _ = db.Pool.Exec(ctx, `DELETE FROM `+table+` WHERE tenant_id = $1`, env.tenant)
		}
		_, _ = db.Pool.Exec(ctx, `DELETE FROM upload_sessions WHERE tenant_id = $1`, env.tenant)
		db.Close()
	})
	return env
}

// session inserts an upload session in the given status.
func (e *reconcileEnv) session(t *testing.T, id string, status model.UploadStatus) {
	t.Helper()
	now := time.Now().UTC()
	s := model.UploadSession{
		ID: e.tenant + "-" + id, TenantID: e.tenant, OwnerID: "owner",
		ObjectKey: "quarantine/" + e.tenant + "/" + id, FileName: "f.bin", ContentType: "application/octet-stream",
		ExpectedSize: 10, Status: status, CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(time.Hour),
	}
	if err := e.uploads.Create(context.Background(), &s); err != nil {
		t.Fatalf("create session %s: %v", id, err)
	}
}

// The database scan for leftover provider sessions finds each one where
// its session staged: a finished session must come back with its binding.
func TestPostgresFinishedSessionsCarryTheirBinding(t *testing.T) {
	env := openReconcileEnv(t)
	env.session(t, "done", model.UploadStatusAborted)
	now := time.Now()
	// IDs sort after the tenant's prefix, so paging from it reaches ours.
	sessions, err := env.tasks.FinishedSessions(context.Background(), now.Add(-time.Hour), now.Add(time.Hour), env.tenant, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range sessions {
		if s.ID != env.tenant+"-done" {
			continue
		}
		if s.StorageConnectionID != model.PlatformConnectionID || s.StagingConnectionID != model.PlatformConnectionID {
			t.Fatalf("a finished session without its binding: %+v", s)
		}
		return
	}
	t.Fatalf("the finished session was not listed: %+v", sessions)
}

// task inserts a PENDING task due at farPast for a session.
func (e *reconcileEnv) task(t *testing.T, id string, maxAttempts int) string {
	t.Helper()
	taskID := e.tenant + "-task-" + id
	_, err := e.db.Pool.Exec(context.Background(), `
		INSERT INTO reconciliation_tasks
			(id, tenant_id, upload_id, operation, state, storage_provider, object_key,
			 max_attempts, next_attempt_at)
		VALUES ($1, $2, $3, 'COMPLETE', 'PENDING', 's3', 'k', $4, $5)
	`, taskID, e.tenant, e.tenant+"-"+id, maxAttempts, farPast)
	if err != nil {
		t.Fatalf("insert task: %v", err)
	}
	return taskID
}

func (e *reconcileEnv) taskState(t *testing.T, taskID string) (string, string, int) {
	t.Helper()
	var state, owner string
	var attempts int
	err := e.db.Pool.QueryRow(context.Background(),
		`SELECT state, COALESCE(lease_owner, ''), attempt_count FROM reconciliation_tasks WHERE id = $1`, taskID,
	).Scan(&state, &owner, &attempts)
	if err != nil {
		t.Fatalf("read task: %v", err)
	}
	return state, owner, attempts
}

func TestPostgresConcurrentClaimsNeverOverlap(t *testing.T) {
	env := openReconcileEnv(t)
	ctx := context.Background()

	const total = 12
	ours := map[string]bool{}
	for i := 0; i < total; i++ {
		id := fmt.Sprintf("u%d", i)
		env.session(t, id, model.UploadStatusCompleting)
		ours[env.task(t, id, 5)] = true
	}

	now := time.Now().UTC()
	var mu sync.Mutex
	claimedBy := map[string]string{}
	var wg sync.WaitGroup
	for w := 0; w < 6; w++ {
		wg.Add(1)
		go func(owner string) {
			defer wg.Done()
			got, err := env.tasks.Claim(ctx, owner, now, time.Minute, total/6)
			if err != nil {
				t.Errorf("%s claim: %v", owner, err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			for _, task := range got {
				if prev, dup := claimedBy[task.ID]; dup {
					t.Errorf("task %s claimed by both %s and %s", task.ID, prev, owner)
				}
				claimedBy[task.ID] = owner
			}
		}(fmt.Sprintf("worker-%d", w))
	}
	wg.Wait()

	for id := range ours {
		state, owner, attempts := env.taskState(t, id)
		if state != "PENDING" || owner == "" || attempts != 1 || claimedBy[id] != owner {
			t.Errorf("task %s: state %s owner %q attempts %d (claimed by %q)", id, state, owner, attempts, claimedBy[id])
		}
	}
}

func TestPostgresExpiredLeaseIsRecoveredAndStaleWriteRefused(t *testing.T) {
	env := openReconcileEnv(t)
	ctx := context.Background()
	env.session(t, "lease", model.UploadStatusCompleting)
	taskID := env.task(t, "lease", 5)

	now := time.Now().UTC()
	if got, err := env.tasks.Claim(ctx, "worker-a", now, time.Minute, 1); err != nil || len(got) != 1 || got[0].ID != taskID {
		t.Fatalf("worker A claim: %v, %+v", err, got)
	}
	if got, _ := env.tasks.Claim(ctx, "worker-b", now.Add(30*time.Second), time.Minute, 1); len(got) != 0 && got[0].ID == taskID {
		t.Fatal("a leased task was claimed again before its lease expired")
	}

	later := now.Add(2 * time.Minute)
	got, err := env.tasks.Claim(ctx, "worker-b", later, time.Minute, 1)
	if err != nil || len(got) != 1 || got[0].ID != taskID || got[0].AttemptCount != 2 {
		t.Fatalf("worker B should recover the expired lease: %v, %+v", err, got)
	}

	if err := env.tasks.Finish(ctx, taskID, "worker-a", later, model.ReconcileSucceeded, "", ""); !errors.Is(err, model.ErrLeaseLost) {
		t.Fatalf("the stale worker must not write, got %v", err)
	}
	if err := env.tasks.Finish(ctx, taskID, "worker-b", later, model.ReconcileSucceeded, "", ""); err != nil {
		t.Fatalf("the current owner must write: %v", err)
	}
	if state, owner, _ := env.taskState(t, taskID); state != "SUCCEEDED" || owner != "" {
		t.Fatalf("expected SUCCEEDED with no lease, got %s/%q", state, owner)
	}
}

func TestPostgresRetryIsScheduledDurably(t *testing.T) {
	env := openReconcileEnv(t)
	ctx := context.Background()
	env.session(t, "retry", model.UploadStatusCompleting)
	taskID := env.task(t, "retry", 5)

	now := time.Now().UTC()
	if _, err := env.tasks.Claim(ctx, "worker-a", now, time.Minute, 1); err != nil {
		t.Fatal(err)
	}
	next := now.Add(10 * time.Minute)
	if err := env.tasks.Reschedule(ctx, taskID, "worker-a", now, next, "timeout"); err != nil {
		t.Fatalf("reschedule: %v", err)
	}

	var due time.Time
	var lastError string
	if err := env.db.Pool.QueryRow(ctx,
		`SELECT next_attempt_at, last_error FROM reconciliation_tasks WHERE id = $1`, taskID,
	).Scan(&due, &lastError); err != nil {
		t.Fatal(err)
	}
	if !due.Equal(next.Truncate(time.Microsecond)) || lastError != "timeout" {
		t.Fatalf("retry not persisted: due %s, error %q", due, lastError)
	}

	for _, task := range mustClaim(t, env, now.Add(5*time.Minute)) {
		if task.ID == taskID {
			t.Fatal("claimed before its retry time")
		}
	}
	found := false
	for _, task := range mustClaim(t, env, next.Add(time.Second)) {
		found = found || task.ID == taskID
	}
	if !found {
		t.Fatal("not claimable after its retry time")
	}
}

func mustClaim(t *testing.T, env *reconcileEnv, now time.Time) []model.ReconciliationTask {
	t.Helper()
	got, err := env.tasks.Claim(context.Background(), "worker-x", now, time.Minute, 50)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestPostgresExhaustedTaskStaysVisibleAndIsNotReEnqueued(t *testing.T) {
	env := openReconcileEnv(t)
	ctx := context.Background()
	env.session(t, "exhaust", model.UploadStatusCompleting)
	sessionID := env.tenant + "-exhaust"

	// Old enough to be past the grace period.
	if _, err := env.db.Pool.Exec(ctx, `UPDATE upload_sessions SET updated_at = $2 WHERE id = $1`,
		sessionID, farPast); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	if _, err := env.tasks.EnqueueDue(ctx, now, time.Minute, "s3", 1); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	var taskID string
	if err := env.db.Pool.QueryRow(ctx,
		`SELECT id FROM reconciliation_tasks WHERE upload_id = $1 AND state = 'PENDING'`, sessionID,
	).Scan(&taskID); err != nil {
		t.Fatalf("expected an enqueued COMPLETE task: %v", err)
	}

	// Enqueueing again creates no duplicate.
	if _, err := env.tasks.EnqueueDue(ctx, now, time.Minute, "s3", 1); err != nil {
		t.Fatal(err)
	}
	if n := countTasks(t, env, sessionID); n != 1 {
		t.Fatalf("expected one task, got %d", n)
	}

	if _, err := env.tasks.Claim(ctx, "worker-a", now, time.Minute, 50); err != nil {
		t.Fatal(err)
	}
	if err := env.tasks.Finish(ctx, taskID, "worker-a", now, model.ReconcileExhausted, "", "AccessDenied"); err != nil {
		t.Fatalf("finish exhausted: %v", err)
	}

	if _, err := env.tasks.EnqueueDue(ctx, now.Add(time.Hour), time.Minute, "s3", 1); err != nil {
		t.Fatal(err)
	}
	if n := countTasks(t, env, sessionID); n != 1 {
		t.Fatalf("an exhausted task must not be replaced, got %d tasks", n)
	}
	if state, _, _ := env.taskState(t, taskID); state != "EXHAUSTED" {
		t.Fatalf("expected EXHAUSTED, got %s", state)
	}
}

func countTasks(t *testing.T, env *reconcileEnv, uploadID string) int {
	t.Helper()
	var n int
	if err := env.db.Pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM reconciliation_tasks WHERE upload_id = $1`, uploadID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestPostgresConditionalStatusUpdate(t *testing.T) {
	env := openReconcileEnv(t)
	ctx := context.Background()
	env.session(t, "cas", model.UploadStatusUploading)
	id := env.tenant + "-cas"
	now := time.Now().UTC()

	if err := env.uploads.TransitionStatus(ctx, env.tenant, id,
		[]model.UploadStatus{model.UploadStatusUploading}, model.UploadStatusCompleting, "", now); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	// The abort that lost the race must not overwrite the newer state.
	err := env.uploads.TransitionStatus(ctx, env.tenant, id,
		[]model.UploadStatus{model.UploadStatusUploading}, model.UploadStatusAborting, model.FailureAbortedByClient, now)
	if !errors.Is(err, model.ErrInvalidUploadState) {
		t.Fatalf("expected ErrInvalidUploadState, got %v", err)
	}
	// Another tenant cannot touch it.
	err = env.uploads.TransitionStatus(ctx, "other-tenant", id,
		[]model.UploadStatus{model.UploadStatusCompleting}, model.UploadStatusCompleted, "", now)
	if !errors.Is(err, model.ErrUploadNotFound) {
		t.Fatalf("expected not found for another tenant, got %v", err)
	}

	got, _ := env.uploads.GetByID(ctx, env.tenant, id)
	if got.Status != model.UploadStatusCompleting {
		t.Fatalf("expected COMPLETING, got %s", got.Status)
	}
}

func TestPostgresTransactionRollsBack(t *testing.T) {
	env := openReconcileEnv(t)
	ctx := context.Background()
	env.session(t, "tx", model.UploadStatusUploading)
	id := env.tenant + "-tx"

	tm := postgres.NewTransactionManager(env.db)
	err := tm.WithinTransaction(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE upload_sessions SET status = 'FAILED' WHERE id = $1`, id); err != nil {
			return err
		}
		return errors.New("fail after the write")
	})
	if err == nil {
		t.Fatal("expected the transaction to fail")
	}

	got, _ := env.uploads.GetByID(ctx, env.tenant, id)
	if got.Status != model.UploadStatusUploading {
		t.Fatalf("the write must be rolled back, status is %s", got.Status)
	}
}

func TestPostgresEnqueueDerivesTheOperationFromSessionState(t *testing.T) {
	env := openReconcileEnv(t)
	ctx := context.Background()

	cases := []struct {
		id        string
		status    model.UploadStatus
		expired   bool
		storageID string
		want      model.ReconciliationOperation // "" = no task
	}{
		{"init", model.UploadStatusPending, false, "", model.ReconcileInitiate},
		{"started", model.UploadStatusPending, false, "provider-1", ""},
		{"expired", model.UploadStatusUploading, true, "provider-2", model.ReconcileExpire},
		{"completing", model.UploadStatusCompleting, false, "provider-3", model.ReconcileComplete},
		{"aborting", model.UploadStatusAborting, false, "provider-4", model.ReconcileAbort},
		// Verification belongs to the processing worker since Phase 5.4.
		{"completed", model.UploadStatusCompleted, false, "provider-5", ""},
		{"verified", model.UploadStatusVerified, false, "provider-6", ""},
		{"failed", model.UploadStatusFailed, false, "provider-7", ""},
	}

	now := time.Now().UTC()
	for _, c := range cases {
		env.session(t, c.id, c.status)
		expires := now.Add(time.Hour)
		if c.expired {
			expires = now.Add(-time.Minute)
		}
		// Settled long ago, so the grace period does not hold it back.
		_, err := env.db.Pool.Exec(ctx, `
			UPDATE upload_sessions
			SET updated_at = $2, created_at = $2, expires_at = $3, storage_upload_id = NULLIF($4, '')
			WHERE id = $1
		`, env.tenant+"-"+c.id, farPast, expires, c.storageID)
		if err != nil {
			t.Fatal(err)
		}
	}

	if _, err := env.tasks.EnqueueDue(ctx, now, time.Minute, "s3", 3); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	for _, c := range cases {
		var operation string
		err := env.db.Pool.QueryRow(ctx,
			`SELECT operation FROM reconciliation_tasks WHERE upload_id = $1`, env.tenant+"-"+c.id,
		).Scan(&operation)
		switch {
		case c.want == "" && err == nil:
			t.Errorf("%s: expected no task, got %s", c.id, operation)
		case c.want != "" && err != nil:
			t.Errorf("%s: expected %s task: %v", c.id, c.want, err)
		case c.want != "" && operation != string(c.want):
			t.Errorf("%s: expected %s, got %s", c.id, c.want, operation)
		}
	}
}
