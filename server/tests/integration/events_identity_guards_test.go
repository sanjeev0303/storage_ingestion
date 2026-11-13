package integration

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// Webhook deliveries, dead-letter retries, the inbox, platform accounts and
// deletions move only from a state that allows the move, checked in the
// statement (or transaction) that makes it.

// hookDeliveryFor stores an endpoint of tenant and one due delivery to it.
func hookDeliveryFor(t *testing.T, db *postgres.DB, tenant string) (*postgres.WebhookRepository, model.WebhookDelivery) {
	t.Helper()
	ctx := context.Background()
	repo := postgres.NewWebhookRepository(db)
	now := time.Now().UTC().Add(-time.Second)
	endpoint := model.WebhookEndpoint{ID: tenant + "-hook", TenantID: tenant, URL: "https://receiver.example.test/hook",
		EventTypes: []string{"webhook.test"}, Enabled: true, SecretKeyVersion: 1, SecretNonce: []byte("nonce"),
		SecretCipher: []byte("sealed"), CreatedBy: "it", CreatedAt: now, UpdatedAt: now}
	if err := repo.Create(ctx, endpoint); err != nil {
		t.Fatal(err)
	}
	d := model.WebhookDelivery{ID: tenant + "-delivery", EventID: tenant + "-event", EndpointID: endpoint.ID, TenantID: tenant,
		EventType: "webhook.test", Payload: []byte(`{"id":"x"}`), CreatedAt: now}
	if err := repo.Enqueue(ctx, d); err != nil {
		t.Fatal(err)
	}
	return repo, d
}

// leaseDelivery gives the delivery to owner until expires, as Claim does.
func leaseDelivery(t *testing.T, db *postgres.DB, id, owner string, expires time.Time) {
	t.Helper()
	if _, err := db.Pool.Exec(context.Background(),
		`UPDATE webhook_deliveries SET lease_owner = $2, lease_expires_at = $3 WHERE id = $1`, id, owner, expires); err != nil {
		t.Fatal(err)
	}
}

// A retry asked for while a worker is sending the delivery is refused: the
// lease stays the worker's, and the delivery is not sent twice at once.
// Once sent, a retry sends it again from a fresh attempt count.
func TestWebhookRedeliverRefusesADeliveryInFlight(t *testing.T) {
	e, tenant := guardTenant(t, "hook-redeliver")
	ctx := context.Background()
	repo, d := hookDeliveryFor(t, e.db, tenant)
	now := time.Now().UTC()
	leaseDelivery(t, e.db, d.ID, "worker-a", now.Add(time.Minute))

	_, err := repo.Redeliver(ctx, tenant, d.EndpointID, d.ID, now)
	if !errors.Is(err, model.ErrDeliveryInFlight) {
		t.Fatalf("a retry of a delivery being sent: %v", err)
	}
	if owner := column(t, e.db, `SELECT lease_owner FROM webhook_deliveries WHERE id = $1`, d.ID); owner != "worker-a" {
		t.Fatalf("the refused retry took the worker's lease: %s", owner)
	}

	if err := repo.Record(ctx, d.ID, "worker-a", now.Add(time.Second), true, 200, "", nil); err != nil {
		t.Fatal(err)
	}
	again, err := repo.Redeliver(ctx, tenant, d.EndpointID, d.ID, now.Add(2*time.Second))
	if err != nil || again.Status != model.HookPending || again.Attempts != 0 {
		t.Fatalf("a retry of a delivered delivery: %+v %v", again, err)
	}
	if _, err := repo.Redeliver(ctx, tenant, d.EndpointID, "missing", now); !errors.Is(err, model.ErrDeliveryNotFound) {
		t.Fatalf("a retry of an unknown delivery: %v", err)
	}
}

// A worker records its attempt only under its own live lease, on a
// delivery still PENDING. A lease that ran out may be another worker's
// by now, so the stale result is refused and changes nothing.
func TestWebhookRecordNeedsTheCallersLiveLease(t *testing.T) {
	e, tenant := guardTenant(t, "hook-record")
	ctx := context.Background()
	repo, d := hookDeliveryFor(t, e.db, tenant)
	now := time.Now().UTC()

	leaseDelivery(t, e.db, d.ID, "worker-a", now.Add(-time.Second))
	if err := repo.Record(ctx, d.ID, "worker-a", now, true, 200, "", nil); !errors.Is(err, model.ErrLeaseLost) {
		t.Fatalf("recording under an expired lease: %v", err)
	}
	if status := column(t, e.db, `SELECT status FROM webhook_deliveries WHERE id = $1`, d.ID); status != "PENDING" {
		t.Fatalf("the refused record changed the delivery: %s", status)
	}

	leaseDelivery(t, e.db, d.ID, "worker-b", now.Add(time.Minute))
	if err := repo.Record(ctx, d.ID, "worker-a", now, true, 200, "", nil); !errors.Is(err, model.ErrLeaseLost) {
		t.Fatalf("recording under another worker's lease: %v", err)
	}
	if err := repo.Record(ctx, d.ID, "worker-b", now, true, 200, "", nil); err != nil {
		t.Fatalf("recording under the caller's lease: %v", err)
	}
	if status := column(t, e.db, `SELECT status FROM webhook_deliveries WHERE id = $1`, d.ID); status != "DELIVERED" {
		t.Fatalf("after the record: %s", status)
	}
}

// Retrying a dead letter while the event is being published leaves the
// publisher's lease alone: taking it would publish the event twice.
func TestDeadLetterRetryLeavesAnInFlightPublicationAlone(t *testing.T) {
	e := openPipelineEnv(t)
	ctx := context.Background()
	ev := e.completed(t, "dl-in-flight")
	now := time.Now().UTC()
	if claimed, err := e.outbox.Claim(ctx, "publisher-a", now, time.Minute, 10, e.tenant); err != nil || len(claimed) != 1 {
		t.Fatalf("claim: %d %v", len(claimed), err)
	}
	dl := e.addDeadLetter(t, ev, processingQueue)

	if err := e.dead.Retry(ctx, dl.ID, "operator", "fixed", now.Add(time.Second)); !errors.Is(err, model.ErrEventInFlight) {
		t.Fatalf("retrying while the event is published: %v", err)
	}
	if stored := e.stored(t, ev.ID); stored.LeaseOwner != "publisher-a" {
		t.Fatalf("the refused retry took the publisher's lease: %+v", stored)
	}
	if open, _ := e.dead.Get(ctx, dl.ID); open.Status != model.DeadLetterOpen {
		t.Fatalf("the refused retry closed the dead letter: %s", open.Status)
	}

	// Once the publication is recorded, the retry goes ahead.
	if err := e.outbox.MarkPublished(ctx, ev.ID, "publisher-a", now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := e.dead.Retry(ctx, dl.ID, "operator", "fixed", now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	if stored := e.stored(t, ev.ID); stored.Status != model.OutboxPending || stored.Republished != 1 || stored.LeaseOwner != "" {
		t.Fatalf("after the retry: %+v", stored)
	}
}

// Reconciliation abandons a stalled inbox record only once its lease ran
// out; a record a consumer holds right now is left to that consumer.
func TestInboxAbandonLeavesALiveLeaseAlone(t *testing.T) {
	e := openPipelineEnv(t)
	ctx := context.Background()
	ev := e.completed(t, "abandon-live")
	rec := model.InboxRecord{Consumer: processingQueue, EventID: ev.ID, EventType: ev.Type, SchemaVersion: ev.SchemaVersion,
		TenantID: e.tenant, AggregateType: ev.AggregateType, AggregateID: ev.AggregateID, AggregateSequence: ev.AggregateSequence}
	now := time.Now().UTC()
	if result, _, err := e.inbox.Claim(ctx, rec, "consumer-a#1", now, time.Minute); err != nil || result != model.ClaimAcquired {
		t.Fatalf("claim: %s %v", result, err)
	}

	if err := e.inbox.Abandon(ctx, processingQueue, ev.ID, now.Add(time.Second), "lost"); !errors.Is(err, model.ErrLeaseLost) {
		t.Fatalf("abandoning a record under a live lease: %v", err)
	}
	if got := e.record(t, processingQueue, ev.ID); got.Status != model.InboxProcessing || got.LeaseOwner != "consumer-a#1" {
		t.Fatalf("the refused abandon changed the record: %+v", got)
	}

	// After the lease ran out it is abandoned, and the late consumer's
	// completion is refused.
	if err := e.inbox.Abandon(ctx, processingQueue, ev.ID, now.Add(2*time.Minute), "lost"); err != nil {
		t.Fatal(err)
	}
	if err := e.inbox.Complete(ctx, processingQueue, ev.ID, "consumer-a#1", now.Add(2*time.Minute), "done"); !errors.Is(err, model.ErrLeaseLost) {
		t.Fatalf("completing after the abandon: %v", err)
	}
}

// Two administrators deactivating each other at once leave one of them
// active: the check and the change run under one lock.
func TestConcurrentDeactivationsKeepAPlatformAdministrator(t *testing.T) {
	e := openSecurityEnv(t, nil)
	ctx := context.Background()
	parked := parkAdministrators(t, e)
	t.Cleanup(func() {
		if _, err := e.db.Pool.Exec(context.Background(), `UPDATE users SET platform_role = 'ADMIN' WHERE id = ANY($1)`, parked); err != nil {
			t.Errorf("restore administrators: %v", err)
		}
	})
	first, _ := e.account(t, "admin-one")
	second, _ := e.account(t, "admin-two")
	for _, u := range []model.User{first, second} {
		if err := e.identity.SetPlatformRole(ctx, operator, u.ID, model.PlatformAdmin); err != nil {
			t.Fatal(err)
		}
	}

	// Hold both rows so both deactivations get as far as they can before
	// either commits.
	tx, err := e.db.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `SELECT id FROM users WHERE id = ANY($1) FOR UPDATE`, []string{first.ID, second.ID}); err != nil {
		t.Fatal(err)
	}
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i, u := range []model.User{first, second} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = e.identity.SetUserStatus(ctx, operator, u.ID, false)
		}()
	}
	waitForLockWaiters(t, e.db, 2)
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	wg.Wait()

	refused := 0
	for _, err := range errs {
		if errors.Is(err, model.ErrForbidden) {
			refused++
		} else if err != nil {
			t.Fatalf("unexpected: %v", err)
		}
	}
	var active int
	if err := e.db.Pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE platform_role = 'ADMIN' AND status = 'ACTIVE'`).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if refused != 1 || active != 1 {
		t.Fatalf("expected one deactivation refused and one administrator left: %v, %d active", errs, active)
	}
}

// Deactivating the account of an active organization's last active owner
// is refused, as demoting or removing that owner is. Another owner, or a
// suspended organization, lets it through.
func TestDeactivationKeepsAnOrganizationsLastOwner(t *testing.T) {
	e := openSecurityEnv(t, nil)
	ctx := context.Background()
	owner, _ := e.account(t, "sole-owner")
	org := e.organization(t, "sole-owner-org", owner)

	if _, err := e.identity.SetUserStatus(ctx, operator, owner.ID, false); !errors.Is(err, model.ErrLastOwner) {
		t.Fatalf("deactivating the last active owner: %v", err)
	}
	if got, _ := e.users.GetUser(ctx, owner.ID); got.Status != model.UserActive {
		t.Fatalf("after the refused deactivation: %s", got.Status)
	}

	second, _ := e.account(t, "second-owner")
	e.join(t, e.as(t, org.ID, owner), second, model.RoleOwner)
	if _, err := e.identity.SetUserStatus(ctx, operator, owner.ID, false); err != nil {
		t.Fatalf("deactivating one of two owners: %v", err)
	}
	if _, err := e.identity.SetUserStatus(ctx, operator, second.ID, false); !errors.Is(err, model.ErrLastOwner) {
		t.Fatalf("deactivating the owner left: %v", err)
	}
	if _, err := e.identity.SetOrganizationStatus(ctx, operator, org.ID, model.OrgSuspended); err != nil {
		t.Fatal(err)
	}
	if _, err := e.identity.SetUserStatus(ctx, operator, second.ID, false); err != nil {
		t.Fatalf("deactivating the owner of a suspended organization: %v", err)
	}
}

// A hold found after removal started never makes the deletion PROTECTED:
// that would make it restorable with some objects already gone. It waits
// in RETRY_WAIT instead. One that never started is protected as before.
func TestStartedDeletionIsNeverProtected(t *testing.T) {
	env := openProcessingEnv(t)
	lifecycleCleanup(t, env)
	ctx := context.Background()
	repo := postgres.NewDeletionRepository(env.db)
	now := time.Now().UTC()

	never := availableAsset(t, env, "protect-never")
	if _, _, err := repo.SoftDelete(ctx, scheduled(never, now), []model.AssetStatus{model.AssetStatusAvailable}, event(env.tenant, "requested-1", now)); err != nil {
		t.Fatal(err)
	}
	claimed, _ := repo.Claim(ctx, "w", now.Add(time.Second), time.Minute, 50, env.tenant)
	if len(claimed) != 1 {
		t.Fatalf("claimed %d", len(claimed))
	}
	if err := repo.Protect(ctx, claimed[0].ID, "w", now.Add(2*time.Second), now.Add(3*time.Second), "legal hold"); err != nil {
		t.Fatal(err)
	}
	if got, _ := repo.Get(ctx, env.tenant, claimed[0].ID); got.State != model.DeletionProtected {
		t.Fatalf("a deletion that never started: %s", got.State)
	}
	if _, err := repo.Restore(ctx, env.tenant, never.ID, now.Add(4*time.Second), event(env.tenant, "restored", now)); err != nil {
		t.Fatal(err)
	}

	started := availableAsset(t, env, "protect-started")
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
	protectStarted := func(at time.Time) {
		t.Helper()
		if again, _ := repo.Claim(ctx, "w", at, time.Minute, 50, env.tenant); len(again) != 1 {
			t.Fatalf("claimed %d again", len(again))
		}
		if err := repo.Protect(ctx, opID, "w", at.Add(time.Second), at.Add(2*time.Second), "legal hold"); err != nil {
			t.Fatal(err)
		}
		if got, _ := repo.Get(ctx, env.tenant, opID); got.State != model.DeletionRetryWait {
			t.Fatalf("a started deletion became %s", got.State)
		}
		if _, err := repo.Restore(ctx, env.tenant, started.ID, at.Add(2*time.Second), event(env.tenant, "restore-refused", at)); !errors.Is(err, model.ErrNotRestorable) {
			t.Fatalf("restoring a started deletion: %v", err)
		}
	}
	protectStarted(now.Add(8 * time.Second))

	// A requeued started deletion has no attempt counted, and is still
	// started.
	if again, _ := repo.Claim(ctx, "w", now.Add(20*time.Second), time.Minute, 50, env.tenant); len(again) != 1 {
		t.Fatalf("claimed %d before failing", len(again))
	}
	if err := repo.Fail(ctx, opID, "w", now.Add(21*time.Second), "gave up", event(env.tenant, "failed", now)); err != nil {
		t.Fatal(err)
	}
	if op, err := repo.Requeue(ctx, env.tenant, opID, now.Add(22*time.Second)); err != nil || op.State != model.DeletionRetryWait || op.AttemptCount != 0 {
		t.Fatalf("requeue: %s %d %v", op.State, op.AttemptCount, err)
	}
	protectStarted(now.Add(23 * time.Second))
}

// "$1 - interval" makes PostgreSQL read $1 as an interval, so comparing it
// with a timestamp fails. The session touch drops its error, which hid this:
// last_used_at only moved on refresh.
func TestTimestampArithmeticQueriesRun(t *testing.T) {
	e := openSecurityEnv(t, nil)
	ctx := context.Background()
	user, _ := e.account(t, "last-used")
	start := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Microsecond)
	session := model.Session{ID: e.run + "-last-used", UserID: user.ID, CreatedAt: start,
		AccessExpiresAt: start.Add(time.Hour), ExpiresAt: start.Add(time.Hour)}
	access := []byte(e.run + "-last-used-access")
	if err := e.users.CreateSession(ctx, session, access, []byte(e.run+"-last-used-refresh")); err != nil {
		t.Fatal(err)
	}
	now := start.Add(5 * time.Minute)
	if _, _, err := e.users.SessionByAccess(ctx, access, now); err != nil {
		t.Fatal(err)
	}
	var lastUsed time.Time
	if err := e.db.Pool.QueryRow(ctx, `SELECT last_used_at FROM auth_sessions WHERE id = $1`, session.ID).Scan(&lastUsed); err != nil {
		t.Fatal(err)
	}
	if !lastUsed.Equal(now) {
		t.Fatalf("last_used_at %s after a use at %s", lastUsed, now)
	}

	if _, err := postgres.NewEvidenceRepository(e.db).SummariesDue(ctx, time.Now().UTC()); err != nil {
		t.Fatalf("training summary reminders: %v", err)
	}
}
