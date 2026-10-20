package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	eventdto "github.com/sanjeev0303/object-storage-platform/internal/dto/event"
	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
	memqueue "github.com/sanjeev0303/object-storage-platform/internal/infrastructure/queue/memory"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/metrics"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/retry"
	"github.com/sanjeev0303/object-storage-platform/internal/service/compaction"
	"github.com/sanjeev0303/object-storage-platform/internal/service/events"
)

// Phase 6 against PostgreSQL (TEST_DATABASE_URL, migrations through
// 000010) and the in-memory queue, whose failure hooks and clock make
// crashes, outages and redeliveries deterministic. Publishers and
// reconcilers are limited to the test's tenant.

const processingQueue = eventdto.QueueProcessing

type pipelineEnv struct {
	*processingEnv
	outbox        *postgres.OutboxRepository
	inbox         *postgres.InboxRepository
	dead          *postgres.DeadLetterRepository
	discrepancies *postgres.DiscrepancyRepository
	queue         *memqueue.Queue
	clock         *queueClock
	logs          *lockedBuffer
	log           *slog.Logger
	metrics       *metrics.Registry
}

// queueClock moves the queue's time forward, so visibility timeouts pass
// without waiting.
type queueClock struct {
	mu     sync.Mutex
	offset time.Duration
}

func (c *queueClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Add(c.offset)
}

func (c *queueClock) advance(d time.Duration) {
	c.mu.Lock()
	c.offset += d
	c.mu.Unlock()
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func openPipelineEnv(t *testing.T) *pipelineEnv {
	t.Helper()
	env := openProcessingEnv(t)
	clock := &queueClock{}
	queue := memqueue.New()
	queue.DedupWindow = 0 // these tests deliver duplicates on purpose
	queue.Now = clock.now
	logs := &lockedBuffer{}
	t.Cleanup(func() {
		_, _ = env.db.Pool.Exec(context.Background(), `DELETE FROM lifecycle_cursors WHERE name = 'events.dead_letters:' || $1`, env.tenant)
	})
	return &pipelineEnv{
		processingEnv: env,
		outbox:        postgres.NewOutboxRepository(env.db),
		inbox:         postgres.NewInboxRepository(env.db),
		dead:          postgres.NewDeadLetterRepository(env.db),
		discrepancies: postgres.NewDiscrepancyRepository(env.db),
		queue:         queue, clock: clock, logs: logs,
		log:     slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		metrics: metrics.New(),
	}
}

// completed moves a new upload to COMPLETED with its upload.completed
// event, as the multipart service does.
func (e *pipelineEnv) completed(t *testing.T, name string) model.OutboxEvent {
	t.Helper()
	e.session(t, name, model.UploadStatusCompleting)
	id := e.tenant + "-" + name
	now := time.Now().UTC()
	ev := eventdto.NewUploadCompleted(e.tenant, id, now, "corr-"+name)
	err := e.uploads.TransitionStatus(context.Background(), e.tenant, id,
		[]model.UploadStatus{model.UploadStatusCompleting}, model.UploadStatusCompleted, "", now, ev)
	if err != nil {
		t.Fatalf("complete %s: %v", name, err)
	}
	return ev
}

func (e *pipelineEnv) stored(t *testing.T, eventID string) model.OutboxEvent {
	t.Helper()
	ev, err := e.outbox.Get(context.Background(), eventID)
	if err != nil {
		t.Fatalf("outbox event %s: %v", eventID, err)
	}
	return ev
}

func (e *pipelineEnv) record(t *testing.T, consumer, eventID string) model.InboxRecord {
	t.Helper()
	rec, err := e.inbox.Get(context.Background(), consumer, eventID)
	if err != nil {
		t.Fatalf("inbox record %s: %v", eventID, err)
	}
	return rec
}

func (e *pipelineEnv) deadLettersOf(t *testing.T, eventID string) []model.DeadLetter {
	t.Helper()
	rows, err := e.db.Pool.Query(context.Background(), `SELECT id FROM dead_letters WHERE event_id = $1 ORDER BY dead_lettered_at`, eventID)
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

	var out []model.DeadLetter
	for _, id := range ids {
		dl, err := e.dead.Get(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, dl)
	}
	return out
}

func (e *pipelineEnv) onlyDeadLetter(t *testing.T, eventID string) model.DeadLetter {
	t.Helper()
	letters := e.deadLettersOf(t, eventID)
	if len(letters) != 1 {
		t.Fatalf("expected one dead letter for %s, got %d", eventID, len(letters))
	}
	return letters[0]
}

// expireLeases makes every lease of the tenant's events run out, as time
// would after a crash.
func (e *pipelineEnv) expireLeases(t *testing.T) {
	t.Helper()
	past := time.Now().UTC().Add(-time.Second)
	for _, table := range []string{"outbox_events", "event_inbox"} {
		if _, err := e.db.Pool.Exec(context.Background(),
			`UPDATE `+table+` SET lease_expires_at = $2 WHERE tenant_id = $1 AND lease_owner IS NOT NULL`, e.tenant, past); err != nil {
			t.Fatal(err)
		}
	}
}

func (e *pipelineEnv) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := e.db.Pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatal(err)
	}
}

func (e *pipelineEnv) newPublisher(t *testing.T, outbox interfaces.OutboxRepository, owner string, change ...func(*events.PublisherConfig)) *events.Publisher {
	t.Helper()
	cfg := events.PublisherConfig{
		Owner: owner, BatchSize: 10, Concurrency: 1, Lease: 30 * time.Second, PublishTimeout: 5 * time.Second,
		Retry: retry.Policy{BaseDelay: 50 * time.Millisecond, MaxDelay: 100 * time.Millisecond, MaxAttempts: 3}, TenantID: e.tenant,
	}
	for _, c := range change {
		c(&cfg)
	}
	p, err := events.NewPublisher(outbox, e.queue, cfg, e.log, e.metrics)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func (e *pipelineEnv) publish(t *testing.T, p *events.Publisher) events.PublishStats {
	t.Helper()
	stats, err := p.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	return stats
}

// publishAll sends every due event of the tenant.
func (e *pipelineEnv) publishAll(t *testing.T) {
	t.Helper()
	p := e.newPublisher(t, e.outbox, "publisher")
	for i := 0; i < 10; i++ {
		if stats := e.publish(t, p); stats.Claimed == 0 {
			return
		}
	}
}

func (e *pipelineEnv) newConsumer(t *testing.T, inbox interfaces.InboxRepository, owner string, handlers map[string]events.Handler, change ...func(*events.ConsumerConfig)) *events.Consumer {
	t.Helper()
	cfg := events.ConsumerConfig{
		Name: processingQueue, Owner: owner, BatchSize: 10, Concurrency: 4, Visibility: 3 * time.Second,
		HandlerTimeout: 10 * time.Second,
		Retry:          retry.Policy{BaseDelay: 100 * time.Millisecond, MaxDelay: 200 * time.Millisecond, MaxAttempts: 3},
	}
	for _, c := range change {
		c(&cfg)
	}
	c, err := events.NewConsumer(e.queue, inbox, e.dead, handlers, cfg, e.log, e.metrics)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func consume(t *testing.T, c *events.Consumer) events.ConsumeStats {
	t.Helper()
	stats, err := c.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	return stats
}

func (e *pipelineEnv) checks() events.Checks {
	return events.Checks{
		Uploads: e.uploads, Assets: e.assets, Jobs: e.jobs,
		Deletions: postgres.NewDeletionRepository(e.db), Holds: postgres.NewLegalHoldRepository(e.db),
	}
}

func (e *pipelineEnv) newReconciler(t *testing.T, change ...func(*events.ReconcilerConfig)) *events.Reconciler {
	t.Helper()
	cfg := events.ReconcilerConfig{
		Queues:     []string{eventdto.QueueProcessing, eventdto.QueueLifecycle},
		StuckAfter: 5 * time.Minute, Window: time.Hour, MaxRepublish: 3, BatchSize: 50, TenantID: e.tenant,
	}
	for _, c := range change {
		c(&cfg)
	}
	r, err := events.NewReconciler(e.outbox, e.inbox, e.dead, e.discrepancies, postgres.NewCursorRepository(e.db),
		e.queue, e.checks(), cfg, e.log, e.metrics)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func reconcile(t *testing.T, r *events.Reconciler) events.ReconcileReport {
	t.Helper()
	report, err := r.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return report
}

func findings(report events.ReconcileReport, kind, eventID string) []model.EventDiscrepancy {
	var out []model.EventDiscrepancy
	for _, f := range report.Findings {
		if f.Kind == kind && (eventID == "" || f.EventID == eventID) {
			out = append(out, f)
		}
	}
	return out
}

// envelopeOf is the message body the publisher sends for an event.
func envelopeOf(t *testing.T, ev model.OutboxEvent) []byte {
	t.Helper()
	body, err := json.Marshal(eventdto.FromOutbox(ev))
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// recorder is a handler that counts its calls per event and can be told
// to fail, or to wait.
type recorder struct {
	mu    sync.Mutex
	calls map[string]int
	// fail decides the outcome of the n-th call for an event.
	fail func(env eventdto.Envelope, n int) error
	// started is closed when the first call begins; release, when set,
	// keeps every call waiting until it is closed or ctx ends.
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (r *recorder) handle(ctx context.Context, env eventdto.Envelope) (string, error) {
	r.mu.Lock()
	if r.calls == nil {
		r.calls = map[string]int{}
	}
	r.calls[env.ID]++
	n := r.calls[env.ID]
	fail := r.fail
	r.mu.Unlock()

	if r.started != nil {
		r.once.Do(func() { close(r.started) })
	}
	if r.release != nil {
		select {
		case <-r.release:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	if fail != nil {
		if err := fail(env, n); err != nil {
			return "", err
		}
	}
	return "handled", nil
}

func (r *recorder) count(eventID string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[eventID]
}

func (r *recorder) total() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, c := range r.calls {
		n += c
	}
	return n
}

func (r *recorder) handlers(types ...string) map[string]events.Handler {
	if len(types) == 0 {
		types = []string{eventdto.TypeUploadCompleted}
	}
	out := map[string]events.Handler{}
	for _, t := range types {
		out[t] = r.handle
	}
	return out
}

// ─── 1, 2: atomic outbox writes ──────────────────────────────────────

func TestEventsCommitWithTheirBusinessChange(t *testing.T) {
	e := openPipelineEnv(t)
	ctx := context.Background()

	ev := e.completed(t, "u1")
	stored := e.stored(t, ev.ID)
	if stored.Status != model.OutboxPending || stored.AggregateSequence != 1 || stored.Destination != processingQueue ||
		stored.CorrelationID != "corr-u1" || !strings.Contains(string(stored.Payload), e.tenant+"-u1") {
		t.Fatalf("unexpected outbox row %+v", stored)
	}
	if session, _ := e.uploads.GetByID(ctx, e.tenant, e.tenant+"-u1"); session.Status != model.UploadStatusCompleted {
		t.Fatalf("session is %s", session.Status)
	}

	// Soft delete and hold release write theirs the same way.
	now := time.Now().UTC()
	asset := availableAsset(t, e.processingEnv, "a1")
	op := scheduled(asset, now)
	deletion := eventdto.NewDeletionScheduled(e.tenant, asset.ID, op.ID, now, "")
	if _, created, err := postgres.NewDeletionRepository(e.db).SoftDelete(ctx, op,
		[]model.AssetStatus{model.AssetStatusAvailable}, event(e.tenant, "deleted", now), deletion); err != nil || !created {
		t.Fatalf("soft delete: %v", err)
	}
	if got := e.stored(t, deletion.ID); got.AggregateID != asset.ID || got.Destination != eventdto.QueueLifecycle {
		t.Fatalf("unexpected deletion event %+v", got)
	}

	holds := postgres.NewLegalHoldRepository(e.db)
	hold, err := holds.Place(ctx, model.LegalHold{ID: e.tenant + "-hold", TenantID: e.tenant, Scope: model.ScopeTenant, Reason: "case", PlacedBy: "it", PlacedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	released := eventdto.NewHoldReleased(e.tenant, hold.ID, now, "")
	if err := holds.Release(ctx, e.tenant, hold.ID, "it", now, released); err != nil {
		t.Fatal(err)
	}
	if got := e.stored(t, released.ID); got.Type != eventdto.TypeHoldReleased {
		t.Fatalf("unexpected hold event %+v", got)
	}
}

func TestEventsRollBackWithTheirBusinessChange(t *testing.T) {
	e := openPipelineEnv(t)
	ctx := context.Background()
	now := time.Now().UTC()

	// The event cannot be written (its ID is taken): the completion is
	// rolled back with it.
	e.session(t, "u1", model.UploadStatusCompleting)
	id := e.tenant + "-u1"
	taken := eventdto.NewUploadCompleted(e.tenant, id, now, "")
	if err := e.outbox.Append(ctx, taken); err != nil {
		t.Fatal(err)
	}
	err := e.uploads.TransitionStatus(ctx, e.tenant, id,
		[]model.UploadStatus{model.UploadStatusCompleting}, model.UploadStatusCompleted, "", now, taken)
	if err == nil {
		t.Fatal("a failed event write must fail the transition")
	}
	if session, _ := e.uploads.GetByID(ctx, e.tenant, id); session.Status != model.UploadStatusCompleting {
		t.Fatalf("the completion must roll back, session is %s", session.Status)
	}

	// The business change is refused: no event either.
	e.session(t, "u2", model.UploadStatusUploading)
	refused := eventdto.NewUploadCompleted(e.tenant, e.tenant+"-u2", now, "")
	err = e.uploads.TransitionStatus(ctx, e.tenant, e.tenant+"-u2",
		[]model.UploadStatus{model.UploadStatusCompleting}, model.UploadStatusCompleted, "", now, refused)
	if !errors.Is(err, model.ErrInvalidUploadState) {
		t.Fatalf("expected the refusal, got %v", err)
	}
	if _, err := e.outbox.Get(ctx, refused.ID); !errors.Is(err, model.ErrEventNotFound) {
		t.Fatalf("a refused change must leave no event, got %v", err)
	}

	// The same for a soft delete.
	deletions := postgres.NewDeletionRepository(e.db)
	asset := availableAsset(t, e.processingEnv, "a1")
	op := scheduled(asset, now)
	clash := eventdto.NewDeletionScheduled(e.tenant, asset.ID, op.ID, now, "")
	clash.ID = taken.ID
	if _, _, err := deletions.SoftDelete(ctx, op, []model.AssetStatus{model.AssetStatusAvailable}, event(e.tenant, "deleted", now), clash); err == nil {
		t.Fatal("a failed event write must fail the soft delete")
	}
	if got, _ := e.assets.GetByID(ctx, e.tenant, asset.ID); got.Status != model.AssetStatusAvailable {
		t.Fatalf("the soft delete must roll back, asset is %s", got.Status)
	}
	if _, err := deletions.GetLive(ctx, e.tenant, asset.ID); !errors.Is(err, model.ErrDeletionNotFound) {
		t.Fatalf("no deletion may remain, got %v", err)
	}
}

// ─── 3, 4, 5, 6: publisher crashes and duplicate delivery ────────────

func TestPublisherRecoversTheEventsOfACrashedPublisher(t *testing.T) {
	e := openPipelineEnv(t)
	ctx := context.Background()
	ev := e.completed(t, "u1")

	// A publisher claims the event and dies before sending it.
	if claimed, err := e.outbox.Claim(ctx, "crashed-publisher", time.Now().UTC(), time.Minute, 10, e.tenant); err != nil || len(claimed) != 1 {
		t.Fatalf("claim: %d %v", len(claimed), err)
	}
	other := e.newPublisher(t, e.outbox, "publisher-b")
	if stats := e.publish(t, other); stats.Claimed != 0 {
		t.Fatal("while the lease runs nobody else takes the event")
	}

	e.expireLeases(t)
	if report := reconcile(t, e.newReconciler(t)); report.Outbox.ExpiredLeases < 1 {
		t.Fatalf("the expired lease must be reported, got %+v", report.Outbox)
	}
	if stats := e.publish(t, other); stats.Published != 1 {
		t.Fatalf("the next publisher recovers it: %+v", stats)
	}
	stored := e.stored(t, ev.ID)
	if stored.Status != model.OutboxPublished || stored.Attempts != 1 || stored.LeaseOwner != "" || stored.PublishedAt == nil {
		t.Fatalf("expected published with the abandoned attempt counted: %+v", stored)
	}
	if stats, _ := e.queue.Stats(ctx, processingQueue); stats.Waiting != 1 {
		t.Fatalf("one message expected, got %+v", stats)
	}
}

func TestABudgetSpentByStoppedPublishersIsNotExceeded(t *testing.T) {
	e := openPipelineEnv(t)
	ctx := context.Background()
	ev := e.completed(t, "u1")

	// Publishers take the event in turn and stop mid-attempt. Each takeover
	// of an expired lease counts the abandoned attempt, so with the next
	// claim the event has used all three of its attempts.
	for i := 0; i < 3; i++ {
		if claimed, err := e.outbox.Claim(ctx, fmt.Sprintf("stopped-%d", i), time.Now().UTC(), time.Minute, 10, e.tenant); err != nil || len(claimed) != 1 {
			t.Fatalf("claim %d: %d %v", i, len(claimed), err)
		}
		e.expireLeases(t)
	}

	if stats := e.publish(t, e.newPublisher(t, e.outbox, "publisher")); stats.DeadLettered != 1 || stats.Published != 0 {
		t.Fatalf("a spent budget is dead-lettered without a fourth send: %+v", stats)
	}
	if stats, _ := e.queue.Stats(ctx, processingQueue); stats.Waiting != 0 {
		t.Fatalf("nothing may reach the queue, got %+v", stats)
	}
	if stored := e.stored(t, ev.ID); stored.Status != model.OutboxDead || stored.Attempts != 3 {
		t.Fatalf("expected DEAD after exactly three attempts: %+v", stored)
	}
	if dl := e.onlyDeadLetter(t, ev.ID); dl.Source != model.SourcePublisher || dl.FailureCategory != model.FailureExhausted || dl.Attempts != 3 {
		t.Fatalf("dead letter %+v", dl)
	}
}

func TestABatchThatOutrunsItsLeaseGivesTheRestBack(t *testing.T) {
	e := openPipelineEnv(t)
	ctx := context.Background()
	first, second := e.completed(t, "u1"), e.completed(t, "u2")

	// A slow queue: each send takes 300ms. With this lease the publisher may
	// start sends for only 100ms after its claim, so that every send ends
	// and is recorded while it still holds the lease.
	e.queue.Fail = func(op, queue string) error {
		if op == "publish" {
			time.Sleep(300 * time.Millisecond)
		}
		return nil
	}
	publisher := e.newPublisher(t, e.outbox, "publisher", func(c *events.PublisherConfig) {
		c.Lease, c.PublishTimeout = 10*time.Second+600*time.Millisecond, 500*time.Millisecond
	})
	if stats := e.publish(t, publisher); stats.Published != 1 || stats.Released != 1 {
		t.Fatalf("one sent in time, the other given back unsent: %+v", stats)
	}
	sent, unsent := e.stored(t, first.ID), e.stored(t, second.ID)
	if sent.Status != model.OutboxPublished {
		sent, unsent = unsent, sent
	}
	if sent.Status != model.OutboxPublished || unsent.Status != model.OutboxPending || unsent.Attempts != 0 || unsent.LeaseOwner != "" {
		t.Fatalf("sent %+v, unsent %+v", sent, unsent)
	}
	if stats, _ := e.queue.Stats(ctx, processingQueue); stats.Waiting != 1 {
		t.Fatalf("only the sent event may be in the queue, got %+v", stats)
	}
}

// unrecordedPublish is a publisher that dies after the queue accepted the
// event and before the outbox recorded it.
type unrecordedPublish struct{ interfaces.OutboxRepository }

func (unrecordedPublish) MarkPublished(context.Context, string, string, time.Time) error {
	return errors.New("publisher stopped before recording the publication")
}

func TestEventAcceptedButNotRecordedIsSentAgainAndHandledOnce(t *testing.T) {
	e := openPipelineEnv(t)
	ctx := context.Background()
	ev := e.completed(t, "u1")

	if stats := e.publish(t, e.newPublisher(t, unrecordedPublish{e.outbox}, "publisher-a")); stats.Unrecorded != 1 {
		t.Fatalf("expected an unrecorded publication: %+v", stats)
	}
	if stored := e.stored(t, ev.ID); stored.Status != model.OutboxPending {
		t.Fatalf("the outbox must not claim a publication it did not record: %s", stored.Status)
	}
	e.expireLeases(t)
	if stats := e.publish(t, e.newPublisher(t, e.outbox, "publisher-b")); stats.Published != 1 {
		t.Fatalf("the event is sent again: %+v", stats)
	}
	if stats, _ := e.queue.Stats(ctx, processingQueue); stats.Waiting != 2 {
		t.Fatalf("the queue now holds the event twice, got %+v", stats)
	}

	// Both copies share the event's group, so one receive returns them and
	// the consumer runs them in turn: the second finds the first completed.
	handler := &recorder{}
	consumer := e.newConsumer(t, e.inbox, "consumer-a", handler.handlers())
	if stats := consume(t, consumer); stats.Received != 2 || stats.Completed != 1 || stats.Duplicates != 1 {
		t.Fatalf("the first copy is handled and the second dropped: %+v", stats)
	}
	if handler.count(ev.ID) != 1 {
		t.Fatalf("the handler ran %d times", handler.count(ev.ID))
	}
	if rec := e.record(t, processingQueue, ev.ID); rec.Status != model.InboxCompleted || rec.Attempts != 1 {
		t.Fatalf("inbox %+v", rec)
	}
	if stats, _ := e.queue.Stats(ctx, processingQueue); stats != (interfaces.QueueStats{}) {
		t.Fatalf("both copies must be acknowledged, got %+v", stats)
	}
}

// ─── 7: concurrent delivery ──────────────────────────────────────────

func TestConcurrentDeliveriesOfOneEventRunItOnce(t *testing.T) {
	e := openPipelineEnv(t)
	ctx := context.Background()
	ev := e.stored(t, e.completed(t, "u1").ID)
	// Two copies in different groups, so two consumers hold them at once.
	for _, group := range []string{"copy-1", "copy-2"} {
		if _, err := e.queue.Publish(ctx, processingQueue, interfaces.OutgoingMessage{Body: envelopeOf(t, ev), GroupKey: group, DedupKey: group}); err != nil {
			t.Fatal(err)
		}
	}

	handler := &recorder{started: make(chan struct{}), release: make(chan struct{})}
	oneAtATime := func(c *events.ConsumerConfig) { c.BatchSize = 1 }
	first := e.newConsumer(t, e.inbox, "consumer-a", handler.handlers(), oneAtATime)
	second := e.newConsumer(t, e.inbox, "consumer-b", handler.handlers(), oneAtATime)

	done := make(chan events.ConsumeStats)
	go func() { done <- consume(t, first) }()
	<-handler.started

	if stats := consume(t, second); stats.Busy != 1 {
		t.Fatalf("the second copy must find the event busy: %+v", stats)
	}
	close(handler.release)
	if stats := <-done; stats.Completed != 1 {
		t.Fatalf("the first copy completes: %+v", stats)
	}

	// The busy copy comes back after its delay and is dropped.
	e.clock.advance(time.Minute)
	if stats := consume(t, second); stats.Duplicates != 1 {
		t.Fatalf("the returning copy is a duplicate: %+v", stats)
	}
	if handler.count(ev.ID) != 1 {
		t.Fatalf("the handler ran %d times", handler.count(ev.ID))
	}
	if n := e.metrics.Snapshot()["events_concurrent_duplicates"]; n != 1 {
		t.Fatalf("one idempotency conflict expected, got %d", n)
	}
}

// ─── 8, 9: consumer crashes ──────────────────────────────────────────

func TestConsumerCrashBeforeTheBusinessCommitIsRetried(t *testing.T) {
	e := openPipelineEnv(t)
	ev := e.completed(t, "u1")
	e.publishAll(t)

	// The first consumer dies in the middle of the handler, before its
	// change commits: it records nothing more.
	crashing := &recorder{started: make(chan struct{}), release: make(chan struct{})}
	ctx, crash := context.WithCancel(context.Background())
	done := make(chan events.ConsumeStats)
	go func() {
		stats, _ := e.newConsumer(t, e.inbox, "consumer-a", crashing.handlers()).RunOnce(ctx)
		done <- stats
	}()
	<-crashing.started
	crash()
	if stats := <-done; stats.Interrupted != 1 {
		t.Fatalf("expected an interrupted delivery: %+v", stats)
	}
	if rec := e.record(t, processingQueue, ev.ID); rec.Status != model.InboxProcessing || rec.Attempts != 1 {
		t.Fatalf("nothing is recorded by a crashed consumer: %+v", rec)
	}

	// Its lease and the message's visibility run out; another consumer
	// does the work.
	e.expireLeases(t)
	e.clock.advance(time.Minute)
	healthy := &recorder{}
	if stats := consume(t, e.newConsumer(t, e.inbox, "consumer-b", healthy.handlers())); stats.Completed != 1 {
		t.Fatalf("redelivery: %+v", stats)
	}
	if rec := e.record(t, processingQueue, ev.ID); rec.Status != model.InboxCompleted || rec.Attempts != 2 || healthy.count(ev.ID) != 1 {
		t.Fatalf("inbox %+v, calls %d", rec, healthy.count(ev.ID))
	}
}

// completionLost loses the first inbox completion, as a consumer that
// crashed right after the business change committed would.
type completionLost struct {
	interfaces.InboxRepository
	lost atomic.Bool
}

func (c *completionLost) Complete(ctx context.Context, consumer, eventID, owner string, now time.Time, outcome string) error {
	if c.lost.CompareAndSwap(false, true) {
		return errors.New("connection lost after the business commit")
	}
	return c.InboxRepository.Complete(ctx, consumer, eventID, owner, now, outcome)
}

func TestConsumerCrashAfterTheBusinessCommitDoesNotRepeatIt(t *testing.T) {
	e := openPipelineEnv(t)
	// The business change is idempotent, as the domain's conditional
	// state changes are: applying it twice changes nothing.
	var mu sync.Mutex
	applied := map[string]bool{}
	effects := 0
	handler := &recorder{fail: func(env eventdto.Envelope, n int) error {
		mu.Lock()
		defer mu.Unlock()
		if !applied[env.AggregateID] {
			applied[env.AggregateID] = true
			effects++
		}
		return nil
	}}

	// The acknowledgement is lost.
	first := e.completed(t, "u1")
	e.publishAll(t)
	var ackFailed atomic.Bool
	e.queue.Fail = func(op, queue string) error {
		if op == "ack" && ackFailed.CompareAndSwap(false, true) {
			return errors.New("connection reset")
		}
		return nil
	}
	consumer := e.newConsumer(t, e.inbox, "consumer-a", handler.handlers())
	if stats := consume(t, consumer); stats.Completed != 1 {
		t.Fatalf("first delivery: %+v", stats)
	}
	e.clock.advance(time.Minute)
	if stats := consume(t, consumer); stats.Duplicates != 1 {
		t.Fatalf("the unacknowledged message comes back as a duplicate: %+v", stats)
	}
	if handler.count(first.ID) != 1 {
		t.Fatalf("the handler must not run again, ran %d times", handler.count(first.ID))
	}

	// The inbox completion is lost after the business commit.
	e.queue.Fail = nil
	second := e.completed(t, "u2")
	e.publishAll(t)
	if stats := consume(t, e.newConsumer(t, &completionLost{InboxRepository: e.inbox}, "consumer-b", handler.handlers())); stats.Unrecorded != 1 {
		t.Fatalf("expected the lost completion: %+v", stats)
	}
	e.expireLeases(t)
	e.clock.advance(time.Minute)
	if stats := consume(t, consumer); stats.Completed != 1 {
		t.Fatalf("redelivery: %+v", stats)
	}
	if handler.count(second.ID) != 2 || effects != 2 {
		t.Fatalf("the handler ran again (%d) but the change applied once per upload (%d)", handler.count(second.ID), effects)
	}
}

// ─── 10, 11, 12: retries and dead letters ────────────────────────────

func TestRetryableFailuresAreRetriedWithBackoff(t *testing.T) {
	e := openPipelineEnv(t)
	ev := e.completed(t, "u1")
	e.publishAll(t)
	handler := &recorder{fail: func(_ eventdto.Envelope, n int) error {
		if n <= 2 {
			return errors.New("scanner unavailable")
		}
		return nil
	}}
	consumer := e.newConsumer(t, e.inbox, "consumer-a", handler.handlers(), func(c *events.ConsumerConfig) { c.Retry.MaxAttempts = 5 })

	before := time.Now().UTC()
	if stats := consume(t, consumer); stats.Retried != 1 {
		t.Fatalf("first attempt: %+v", stats)
	}
	rec := e.record(t, processingQueue, ev.ID)
	if rec.Status != model.InboxFailed || rec.Attempts != 1 || rec.LastError != "scanner unavailable" || rec.NextAttemptAt == nil ||
		rec.NextAttemptAt.Before(before.Add(50*time.Millisecond)) || rec.NextAttemptAt.After(time.Now().Add(100*time.Millisecond)) {
		t.Fatalf("expected a retry 50-100ms out: %+v", rec)
	}
	if stats := consume(t, consumer); stats.Received != 0 {
		t.Fatal("the message stays hidden during the backoff")
	}

	time.Sleep(250 * time.Millisecond)
	if stats := consume(t, consumer); stats.Retried != 1 {
		t.Fatalf("second attempt: %+v", stats)
	}
	time.Sleep(250 * time.Millisecond)
	if stats := consume(t, consumer); stats.Completed != 1 {
		t.Fatalf("third attempt: %+v", stats)
	}
	if rec := e.record(t, processingQueue, ev.ID); rec.Status != model.InboxCompleted || rec.Attempts != 3 {
		t.Fatalf("inbox %+v", rec)
	}
}

func TestPermanentFailuresAreNotRetried(t *testing.T) {
	e := openPipelineEnv(t)
	ctx := context.Background()
	ev := e.completed(t, "u1")
	e.publishAll(t)
	handler := &recorder{fail: func(eventdto.Envelope, int) error {
		return retry.Permanent(errors.New("the upload belongs to another region"))
	}}
	if stats := consume(t, e.newConsumer(t, e.inbox, "consumer-a", handler.handlers())); stats.DeadLettered != 1 {
		t.Fatalf("expected a dead letter: %+v", stats)
	}
	dl := e.onlyDeadLetter(t, ev.ID)
	if dl.FailureCategory != model.FailurePermanent || dl.Source != model.SourceConsumer || dl.Consumer != processingQueue ||
		dl.Attempts != 1 || dl.Status != model.DeadLetterOpen || handler.count(ev.ID) != 1 {
		t.Fatalf("dead letter %+v, calls %d", dl, handler.count(ev.ID))
	}
	if rec := e.record(t, processingQueue, ev.ID); rec.Status != model.InboxDead {
		t.Fatalf("inbox %+v", rec)
	}
	if stats, _ := e.queue.Stats(ctx, processingQueue); stats != (interfaces.QueueStats{}) {
		t.Fatalf("the dead-lettered message is acknowledged, got %+v", stats)
	}

	// The publisher's side: a message the queue can never take.
	other := e.completed(t, "u2")
	e.queue.Fail = func(op, queue string) error {
		return retry.Permanent(fmt.Errorf("send: %w", interfaces.ErrMessageTooLarge))
	}
	if stats := e.publish(t, e.newPublisher(t, e.outbox, "publisher")); stats.DeadLettered != 1 {
		t.Fatalf("expected the publisher to dead-letter: %+v", stats)
	}
	if stored := e.stored(t, other.ID); stored.Status != model.OutboxDead || stored.Attempts != 1 {
		t.Fatalf("outbox %+v", stored)
	}
	if dl := e.onlyDeadLetter(t, other.ID); dl.Source != model.SourcePublisher || dl.FailureCategory != model.FailurePermanent {
		t.Fatalf("dead letter %+v", dl)
	}
}

func TestExhaustedRetriesEndInTheDeadLetterTable(t *testing.T) {
	e := openPipelineEnv(t)
	ctx := context.Background()
	ev := e.completed(t, "u1")
	e.publishAll(t)
	handler := &recorder{fail: func(eventdto.Envelope, int) error { return errors.New("dependency timeout") }}
	consumer := e.newConsumer(t, e.inbox, "consumer-a", handler.handlers())

	for i := 0; i < 6 && len(e.deadLettersOf(t, ev.ID)) == 0; i++ {
		consume(t, consumer)
		time.Sleep(250 * time.Millisecond)
	}
	dl := e.onlyDeadLetter(t, ev.ID)
	rec := e.record(t, processingQueue, ev.ID)
	if dl.FailureCategory != model.FailureExhausted || dl.Attempts != 3 || dl.LastError != "dependency timeout" ||
		!dl.FirstFailedAt.Equal(rec.FirstSeenAt) || !dl.LastFailedAt.After(dl.FirstFailedAt) || handler.count(ev.ID) != 3 {
		t.Fatalf("dead letter %+v, calls %d", dl, handler.count(ev.ID))
	}
	if stats, _ := e.queue.Stats(ctx, processingQueue); stats != (interfaces.QueueStats{}) {
		t.Fatalf("the message is acknowledged once dead-lettered, got %+v", stats)
	}

	// Publication runs out of attempts the same way.
	other := e.completed(t, "u2")
	e.queue.Fail = func(op, queue string) error { return errors.New("connection reset by peer") }
	publisher := e.newPublisher(t, e.outbox, "publisher")
	for i := 0; i < 5 && e.stored(t, other.ID).Status == model.OutboxPending; i++ {
		e.publish(t, publisher)
		time.Sleep(150 * time.Millisecond)
	}
	if stored := e.stored(t, other.ID); stored.Status != model.OutboxDead || stored.Attempts != 3 {
		t.Fatalf("outbox %+v", stored)
	}
	if dl := e.onlyDeadLetter(t, other.ID); dl.Source != model.SourcePublisher || dl.FailureCategory != model.FailureExhausted || dl.Attempts != 3 {
		t.Fatalf("dead letter %+v", dl)
	}
}

// ─── 13, 14: operator retry and stale replays ────────────────────────

func TestDeadLettersAreRetriedAndReplayedByAnOperator(t *testing.T) {
	e := openPipelineEnv(t)
	ctx := context.Background()
	admin := events.NewAdmin(e.outbox, e.inbox, e.dead, e.checks())
	var broken atomic.Bool
	broken.Store(true)
	handler := &recorder{fail: func(eventdto.Envelope, int) error {
		if broken.Load() {
			return retry.Permanent(errors.New("misconfigured scanner"))
		}
		return nil
	}}
	consumer := e.newConsumer(t, e.inbox, "consumer-a", handler.handlers())

	ev := e.completed(t, "u1")
	e.publishAll(t)
	consume(t, consumer)
	dl := e.onlyDeadLetter(t, ev.ID)

	if err := admin.Retry(ctx, dl.ID, "operator", ""); err == nil {
		t.Fatal("a retry needs a reason")
	}
	broken.Store(false)
	if err := admin.Retry(ctx, dl.ID, "operator", "scanner configuration fixed"); err != nil {
		t.Fatal(err)
	}
	closed, _ := e.dead.Get(ctx, dl.ID)
	if closed.Status != model.DeadLetterRetried || closed.ResolvedBy != "operator" || closed.ResolutionNote != "scanner configuration fixed" {
		t.Fatalf("dead letter %+v", closed)
	}
	if stored := e.stored(t, ev.ID); stored.Status != model.OutboxPending || stored.Republished != 1 || stored.Attempts != 0 {
		t.Fatalf("outbox %+v", stored)
	}
	if rec := e.record(t, processingQueue, ev.ID); rec.Status != model.InboxFailed || rec.Attempts != 0 {
		t.Fatalf("inbox %+v", rec)
	}
	e.publishAll(t)
	if stats := consume(t, consumer); stats.Completed != 1 {
		t.Fatalf("the retried event is handled: %+v", stats)
	}
	if err := admin.Retry(ctx, dl.ID, "operator", "again"); !errors.Is(err, model.ErrDeadLetterClosed) {
		t.Fatalf("a closed dead letter cannot be retried again, got %v", err)
	}

	// Replay: the same work as a new operation, under a new identity.
	broken.Store(true)
	other := e.completed(t, "u2")
	e.publishAll(t)
	consume(t, consumer)
	dl = e.onlyDeadLetter(t, other.ID)
	broken.Store(false)
	newID, err := admin.Replay(ctx, dl.ID, "operator", "run it as a new operation")
	if err != nil {
		t.Fatal(err)
	}
	replay := e.stored(t, newID)
	if replay.ID == other.ID || replay.CausationID != other.ID || replay.CorrelationID != other.CorrelationID || replay.AggregateSequence != 2 {
		t.Fatalf("replay %+v", replay)
	}
	if closed, _ := e.dead.Get(ctx, dl.ID); closed.Status != model.DeadLetterReplayed || closed.ReplayEventID != newID {
		t.Fatalf("dead letter %+v", closed)
	}
	e.publishAll(t)
	if stats := consume(t, consumer); stats.Completed != 1 || handler.count(newID) != 1 {
		t.Fatalf("the replay is handled: %+v", stats)
	}
}

func (e *pipelineEnv) addDeadLetter(t *testing.T, ev model.OutboxEvent, consumer string) model.DeadLetter {
	t.Helper()
	now := time.Now().UTC()
	dl := model.DeadLetter{
		ID: eventdto.NewID(), EventID: ev.ID, Source: model.SourceConsumer, Consumer: consumer,
		EventType: ev.Type, SchemaVersion: ev.SchemaVersion, TenantID: e.tenant, AggregateType: ev.AggregateType,
		AggregateID: ev.AggregateID, FailureCategory: model.FailureExhausted, LastError: "failed", Attempts: 3,
		FirstFailedAt: now, LastFailedAt: now, DeadLetteredAt: now, Status: model.DeadLetterOpen, UpdatedAt: now,
	}
	if err := e.dead.Add(context.Background(), dl); err != nil {
		t.Fatal(err)
	}
	return dl
}

func TestStaleEventsAreNotReplayed(t *testing.T) {
	e := openPipelineEnv(t)
	ctx := context.Background()
	admin := events.NewAdmin(e.outbox, e.inbox, e.dead, e.checks())
	now := time.Now().UTC()

	// upload.completed whose processing already finished.
	ev := e.completed(t, "u1")
	job, err := e.jobs.EnqueueUpload(ctx, e.tenant, e.tenant+"-u1", now, 3)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := e.jobs.ClaimJob(ctx, job.ID, "it", now, time.Minute); err != nil || !ok {
		t.Fatalf("claim job: %v", err)
	}
	if err := e.jobs.Finish(ctx, job.ID, "it", now, model.ProcessingSucceeded, ""); err != nil {
		t.Fatal(err)
	}
	dl := e.addDeadLetter(t, ev, processingQueue)
	if err := admin.Retry(ctx, dl.ID, "operator", "retry"); !errors.Is(err, model.ErrStaleEvent) || !strings.Contains(err.Error(), "SUCCEEDED") {
		t.Fatalf("a retry after processing ended must be refused, got %v", err)
	}
	if _, err := admin.Replay(ctx, dl.ID, "operator", "replay"); !errors.Is(err, model.ErrStaleEvent) {
		t.Fatalf("so must a replay, got %v", err)
	}
	if got, _ := e.dead.Get(ctx, dl.ID); got.Status != model.DeadLetterOpen {
		t.Fatalf("a refused retry leaves the dead letter open, got %s", got.Status)
	}

	// Reconciliation marks it for the operator and does nothing else.
	report := reconcile(t, e.newReconciler(t))
	if len(findings(report, events.KindSourceChanged, ev.ID)) != 1 {
		t.Fatalf("expected a source-changed finding: %+v", report.Findings)
	}
	if got, _ := e.dead.Get(ctx, dl.ID); got.Status != model.DeadLetterOpen || !strings.Contains(got.SourceChanged, "SUCCEEDED") {
		t.Fatalf("dead letter %+v", got)
	}

	// A deletion an owner restored since.
	deletions := postgres.NewDeletionRepository(e.db)
	asset := availableAsset(t, e.processingEnv, "a1")
	op := scheduled(asset, now)
	scheduledEvent := eventdto.NewDeletionScheduled(e.tenant, asset.ID, op.ID, now, "")
	if _, _, err := deletions.SoftDelete(ctx, op, []model.AssetStatus{model.AssetStatusAvailable}, event(e.tenant, "deleted", now), scheduledEvent); err != nil {
		t.Fatal(err)
	}
	if _, err := deletions.Restore(ctx, e.tenant, asset.ID, now, event(e.tenant, "restored", now)); err != nil {
		t.Fatal(err)
	}
	dl = e.addDeadLetter(t, scheduledEvent, eventdto.QueueLifecycle)
	if err := admin.Retry(ctx, dl.ID, "operator", "retry"); !errors.Is(err, model.ErrStaleEvent) || !strings.Contains(err.Error(), "CANCELLED") {
		t.Fatalf("a deletion of a restored asset must not be replayed, got %v", err)
	}
}

// ─── 15, 16: undeliverable messages ──────────────────────────────────

func TestUnknownTypesAndUnsupportedVersionsAreDeadLettered(t *testing.T) {
	e := openPipelineEnv(t)
	ctx := context.Background()
	now := time.Now().UTC()
	unknown := model.OutboxEvent{
		ID: eventdto.NewID(), Type: "upload.teleported", SchemaVersion: 1, AggregateType: eventdto.AggregateUpload,
		AggregateID: e.tenant + "-x", TenantID: e.tenant, Payload: []byte(`{"upload_id": "x"}`), Destination: processingQueue,
		Status: model.OutboxPending, NextAttemptAt: now, CreatedAt: now, UpdatedAt: now,
	}
	future := eventdto.NewUploadCompleted(e.tenant, e.tenant+"-y", now, "")
	future.SchemaVersion = 9
	if err := e.outbox.Append(ctx, unknown, future); err != nil {
		t.Fatal(err)
	}
	e.publishAll(t)

	handler := &recorder{}
	if stats := consume(t, e.newConsumer(t, e.inbox, "consumer-a", handler.handlers())); stats.DeadLettered != 2 || handler.total() != 0 {
		t.Fatalf("both must be dead-lettered without reaching a handler: %+v", stats)
	}
	if dl := e.onlyDeadLetter(t, unknown.ID); dl.FailureCategory != model.FailureUnknownType || dl.Consumer != processingQueue {
		t.Fatalf("dead letter %+v", dl)
	}
	if dl := e.onlyDeadLetter(t, future.ID); dl.FailureCategory != model.FailureUnsupportedVersion {
		t.Fatalf("dead letter %+v", dl)
	}
	if stats, _ := e.queue.Stats(ctx, processingQueue); stats != (interfaces.QueueStats{}) {
		t.Fatalf("rejected messages are acknowledged, got %+v", stats)
	}
}

func TestMalformedMessagesAreDeadLetteredByDigest(t *testing.T) {
	e := openPipelineEnv(t)
	ctx := context.Background()
	garbage := []byte(`{"id": "x", "type": "upload.completed", "secret": "do-not-store"`)
	sum := sha256.Sum256(garbage)
	digest := hex.EncodeToString(sum[:])
	t.Cleanup(func() {
		_, _ = e.db.Pool.Exec(context.Background(), `DELETE FROM dead_letters WHERE message_digest = $1`, digest)
	})

	valid := e.stored(t, e.completed(t, "u1").ID)
	missingField := eventdto.FromOutbox(valid)
	missingField.Payload = json.RawMessage(`{}`)
	body, _ := json.Marshal(missingField)
	for i, b := range [][]byte{garbage, body} {
		key := fmt.Sprint("g", i)
		if _, err := e.queue.Publish(ctx, processingQueue, interfaces.OutgoingMessage{Body: b, GroupKey: key, DedupKey: key}); err != nil {
			t.Fatal(err)
		}
	}

	handler := &recorder{}
	if stats := consume(t, e.newConsumer(t, e.inbox, "consumer-a", handler.handlers())); stats.DeadLettered != 2 || handler.total() != 0 {
		t.Fatalf("both are malformed: %+v", stats)
	}
	var id, eventID, lastError, category string
	err := e.db.Pool.QueryRow(ctx, `SELECT id, event_id, last_error, failure_category FROM dead_letters WHERE message_digest = $1`, digest).
		Scan(&id, &eventID, &lastError, &category)
	if err != nil || !strings.HasPrefix(eventID, "message:") || category != model.FailureMalformed || strings.Contains(lastError, "do-not-store") {
		t.Fatalf("unreadable message must be kept as a digest only: %s %s %q %v", eventID, category, lastError, err)
	}
	if dl := e.onlyDeadLetter(t, valid.ID); dl.FailureCategory != model.FailureMalformed {
		t.Fatalf("an incomplete payload is malformed: %+v", dl)
	}
}

// ─── 17: ordering ────────────────────────────────────────────────────

func TestEventsOfOneAggregateStayInOrder(t *testing.T) {
	e := openPipelineEnv(t)
	ctx := context.Background()
	now := time.Now().UTC()
	older := eventdto.NewDeletionScheduled(e.tenant, e.tenant+"-asset", "op-1", now, "")
	newer := eventdto.NewDeletionScheduled(e.tenant, e.tenant+"-asset", "op-2", now, "")
	if err := e.outbox.Append(ctx, older); err != nil {
		t.Fatal(err)
	}
	if err := e.outbox.Append(ctx, newer); err != nil {
		t.Fatal(err)
	}

	// The publisher holds the newer event back until the older one is out.
	claimed, err := e.outbox.Claim(ctx, "p", time.Now().UTC(), time.Minute, 10, e.tenant)
	if err != nil || len(claimed) != 1 || claimed[0].ID != older.ID || claimed[0].AggregateSequence != 1 {
		t.Fatalf("only the older event may be claimed: %+v %v", claimed, err)
	}
	if err := e.outbox.Release(ctx, older.ID, "p", time.Now().UTC(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	publisher := e.newPublisher(t, e.outbox, "publisher")
	if first, second := e.publish(t, publisher), e.publish(t, publisher); first.Published != 1 || second.Published != 1 {
		t.Fatalf("one event per batch, in order: %+v %+v", first, second)
	}
	// Like SQS FIFO, one receive returns both, the older one first.
	received, _ := e.queue.Receive(ctx, eventdto.QueueLifecycle, 10, time.Minute, 0)
	if len(received) != 2 || !bytes.Contains(received[0].Body, []byte(older.ID)) || !bytes.Contains(received[1].Body, []byte(newer.ID)) {
		t.Fatalf("the queue delivers the older event first, got %d messages", len(received))
	}

	// A consumer that still gets them out of order skips the older one.
	reversed := memqueue.New()
	for _, ev := range []model.OutboxEvent{e.stored(t, newer.ID), e.stored(t, older.ID)} {
		if _, err := reversed.Publish(ctx, eventdto.QueueLifecycle, interfaces.OutgoingMessage{Body: envelopeOf(t, ev), GroupKey: ev.ID, DedupKey: ev.ID}); err != nil {
			t.Fatal(err)
		}
	}
	handler := &recorder{}
	consumer, err := events.NewConsumer(reversed, e.inbox, e.dead, handler.handlers(eventdto.TypeDeletionScheduled), events.ConsumerConfig{
		Name: eventdto.QueueLifecycle, Owner: "consumer", BatchSize: 1, Concurrency: 1, Visibility: 3 * time.Second,
		HandlerTimeout: time.Minute, Retry: retry.Policy{BaseDelay: 100 * time.Millisecond, MaxDelay: 200 * time.Millisecond, MaxAttempts: 3},
	}, e.log, e.metrics)
	if err != nil {
		t.Fatal(err)
	}
	if stats := consume(t, consumer); stats.Completed != 1 {
		t.Fatalf("the newer event first: %+v", stats)
	}
	if stats := consume(t, consumer); stats.Skipped != 1 {
		t.Fatalf("the late older event is skipped: %+v", stats)
	}
	if handler.count(newer.ID) != 1 || handler.count(older.ID) != 0 {
		t.Fatal("a stale event must not reach its handler")
	}
	if rec := e.record(t, eventdto.QueueLifecycle, older.ID); rec.Status != model.InboxSkipped || !strings.Contains(rec.Outcome, "stale") {
		t.Fatalf("inbox %+v", rec)
	}
}

func TestConcurrentWritersOfOneAggregateDoNotCollide(t *testing.T) {
	e := openPipelineEnv(t)
	ctx := context.Background()
	asset := e.tenant + "-asset"

	// Writers that lock no business row (operator replays, reconciliation)
	// all number their event MAX+1. Without the aggregate lock, most of
	// them would fail on the unique sequence index.
	const writers = 8
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs <- e.outbox.Append(ctx, eventdto.NewDeletionScheduled(e.tenant, asset, fmt.Sprintf("op-%d", i), time.Now().UTC(), ""))
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("a concurrent writer failed: %v", err)
		}
	}
	recent, err := e.outbox.List(ctx, "", 200)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[int64]bool{}
	for _, ev := range recent {
		if ev.AggregateID == asset {
			seen[ev.AggregateSequence] = true
		}
	}
	for seq := int64(1); seq <= writers; seq++ {
		if !seen[seq] {
			t.Fatalf("expected sequences 1..%d without gaps, got %v", writers, seen)
		}
	}

	// Emitting a missing event: of two reconcilers that found the same gap,
	// only one writes it.
	upload := e.tenant + "-upload"
	added := make(chan bool, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := e.outbox.AppendFirst(ctx, eventdto.NewUploadCompleted(e.tenant, upload, time.Now().UTC(), ""))
			if err != nil {
				t.Error(err)
			}
			added <- ok
		}()
	}
	wg.Wait()
	close(added)
	wrote := 0
	for ok := range added {
		if ok {
			wrote++
		}
	}
	if wrote != 1 {
		t.Fatalf("exactly one reconciler may emit the event, %d did", wrote)
	}
}

func TestABatchRunsOneAggregatesEventsInOrder(t *testing.T) {
	e := openPipelineEnv(t)
	ctx := context.Background()
	now := time.Now().UTC()
	older := eventdto.NewDeletionScheduled(e.tenant, e.tenant+"-asset", "op-1", now, "")
	newer := eventdto.NewDeletionScheduled(e.tenant, e.tenant+"-asset", "op-2", now, "")
	for _, ev := range []model.OutboxEvent{older, newer} {
		if err := e.outbox.Append(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}

	// A FIFO queue hands both events of the aggregate to one receive.
	q := memqueue.New()
	group := older.AggregateType + ":" + older.AggregateID
	for _, ev := range []model.OutboxEvent{e.stored(t, older.ID), e.stored(t, newer.ID)} {
		if _, err := q.Publish(ctx, eventdto.QueueLifecycle, interfaces.OutgoingMessage{Body: envelopeOf(t, ev), GroupKey: group, DedupKey: ev.ID}); err != nil {
			t.Fatal(err)
		}
	}
	handler := &recorder{fail: func(env eventdto.Envelope, n int) error {
		if env.ID == older.ID && n == 1 {
			return errors.New("dependency unavailable")
		}
		return nil
	}}
	consumer, err := events.NewConsumer(q, e.inbox, e.dead, handler.handlers(eventdto.TypeDeletionScheduled), events.ConsumerConfig{
		Name: eventdto.QueueLifecycle, Owner: "consumer", BatchSize: 10, Concurrency: 4, Visibility: 3 * time.Second,
		HandlerTimeout: time.Minute, Retry: retry.Policy{BaseDelay: 100 * time.Millisecond, MaxDelay: 200 * time.Millisecond, MaxAttempts: 3},
	}, e.log, e.metrics)
	if err != nil {
		t.Fatal(err)
	}

	// The older event fails; the newer one waits for it instead of running
	// beside it, which would get it handled first and the older one skipped
	// as stale when it came back.
	if stats := consume(t, consumer); stats.Received != 2 || stats.Retried != 1 || stats.Deferred != 1 {
		t.Fatalf("first batch: %+v", stats)
	}
	if handler.count(newer.ID) != 0 {
		t.Fatal("the newer event must not run while the older one is unfinished")
	}

	// Once the retry is due both come back and run in order; neither is stale.
	time.Sleep(300 * time.Millisecond)
	if stats := consume(t, consumer); stats.Completed != 2 || stats.Skipped != 0 {
		t.Fatalf("second batch: %+v", stats)
	}
	if handler.count(older.ID) != 2 || handler.count(newer.ID) != 1 {
		t.Fatalf("older ran %d times, newer %d", handler.count(older.ID), handler.count(newer.ID))
	}
}

// ─── 18, 19: throttling, network and database failures ───────────────

func TestThrottlingAndNetworkFailuresAreRetried(t *testing.T) {
	e := openPipelineEnv(t)
	first, second := e.completed(t, "u1"), e.completed(t, "u2")
	var calls atomic.Int32
	e.queue.Fail = func(op, queue string) error {
		if op != "publish" {
			return nil
		}
		switch calls.Add(1) {
		case 1:
			return fmt.Errorf("send: %w", interfaces.ErrQueueThrottled)
		case 2:
			return &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection reset")}
		}
		return nil
	}
	publisher := e.newPublisher(t, e.outbox, "publisher")

	// Throttled: the whole batch is given back with a delay instead of
	// adding to the load, and no event spends an attempt on it. The queue's
	// load is not the event's fault; counting it would dead-letter events
	// under sustained throttling.
	before := time.Now().UTC()
	if stats := e.publish(t, publisher); stats.Released != 2 || stats.Retried != 0 {
		t.Fatalf("first batch: %+v", stats)
	}
	for _, id := range []string{first.ID, second.ID} {
		if stored := e.stored(t, id); stored.Attempts != 0 || stored.Status != model.OutboxPending || !stored.NextAttemptAt.After(before) {
			t.Fatalf("a throttled batch keeps its budget and waits: %+v", stored)
		}
	}

	time.Sleep(150 * time.Millisecond)
	if stats := e.publish(t, publisher); stats.Retried != 1 || stats.Published != 1 {
		t.Fatalf("second batch, one network failure: %+v", stats)
	}
	time.Sleep(150 * time.Millisecond)
	if stats := e.publish(t, publisher); stats.Published != 1 {
		t.Fatalf("third batch: %+v", stats)
	}
	for _, id := range []string{first.ID, second.ID} {
		if stored := e.stored(t, id); stored.Status != model.OutboxPublished {
			t.Fatalf("%s is %s", id, stored.Status)
		}
	}
	if n := e.metrics.Snapshot()["outbox_publish_throttled"]; n != 1 {
		t.Fatalf("one throttle expected, got %d", n)
	}
}

type outboxDown struct{ interfaces.OutboxRepository }

func (outboxDown) Claim(context.Context, string, time.Time, time.Duration, int, string) ([]model.OutboxEvent, error) {
	return nil, errors.New("dial tcp: connection refused")
}

type inboxDown struct {
	interfaces.InboxRepository
	down atomic.Bool
}

func (d *inboxDown) Claim(ctx context.Context, rec model.InboxRecord, owner string, now time.Time, lease time.Duration) (string, model.InboxRecord, error) {
	if d.down.Load() {
		return "", model.InboxRecord{}, errors.New("database unavailable")
	}
	return d.InboxRepository.Claim(ctx, rec, owner, now, lease)
}

func TestDatabaseOutagesLoseNothing(t *testing.T) {
	e := openPipelineEnv(t)
	ctx := context.Background()
	ev := e.completed(t, "u1")

	if _, err := e.newPublisher(t, outboxDown{e.outbox}, "publisher").RunOnce(ctx); err == nil {
		t.Fatal("a failed claim must be reported")
	}
	if stats, _ := e.queue.Stats(ctx, processingQueue); stats.Waiting != 0 {
		t.Fatal("nothing may be sent without a claim")
	}
	e.publishAll(t)

	inbox := &inboxDown{InboxRepository: e.inbox}
	inbox.down.Store(true)
	handler := &recorder{}
	consumer := e.newConsumer(t, inbox, "consumer-a", handler.handlers())
	if stats := consume(t, consumer); stats.Retried != 1 || handler.total() != 0 {
		t.Fatalf("without the inbox nothing runs: %+v", stats)
	}
	if stats, _ := e.queue.Stats(ctx, processingQueue); stats.InFlight != 1 {
		t.Fatalf("the message stays in the queue, got %+v", stats)
	}

	inbox.down.Store(false)
	e.clock.advance(time.Hour)
	if stats := consume(t, consumer); stats.Completed != 1 || handler.count(ev.ID) != 1 {
		t.Fatalf("after the outage: %+v", stats)
	}
}

// ─── 20: shutdown ────────────────────────────────────────────────────

func TestShutdownLeavesWorkForTheNextWorker(t *testing.T) {
	e := openPipelineEnv(t)
	ids := []string{e.completed(t, "u1").ID, e.completed(t, "u2").ID, e.completed(t, "u3").ID}

	// The publisher stops in the middle of its batch: what it had not sent
	// goes back, untried.
	ctx, stop := context.WithCancel(context.Background())
	e.queue.Fail = func(op, queue string) error {
		if op == "publish" {
			stop()
			return context.Canceled
		}
		return nil
	}
	stats, err := e.newPublisher(t, e.outbox, "publisher-a").RunOnce(ctx)
	if err != nil || stats.Released != 3 {
		t.Fatalf("expected every event released: %+v %v", stats, err)
	}
	for _, id := range ids {
		if stored := e.stored(t, id); stored.Status != model.OutboxPending || stored.Attempts != 0 || stored.LeaseOwner != "" {
			t.Fatalf("outbox %+v", stored)
		}
	}
	e.queue.Fail = nil
	e.publishAll(t)

	// The consumer stops while handling the first event: it records nothing
	// for it and hands back the two it had not started.
	handler := &recorder{started: make(chan struct{}), release: make(chan struct{})}
	consumerCtx, stopConsumer := context.WithCancel(context.Background())
	done := make(chan events.ConsumeStats)
	go func() {
		stats, _ := e.newConsumer(t, e.inbox, "consumer-a", handler.handlers(), func(c *events.ConsumerConfig) { c.Concurrency = 1 }).RunOnce(consumerCtx)
		done <- stats
	}()
	<-handler.started
	stopConsumer()
	if stats := <-done; stats.Interrupted != 3 {
		t.Fatalf("expected three interrupted deliveries: %+v", stats)
	}

	e.expireLeases(t)
	e.clock.advance(time.Minute)
	healthy := &recorder{}
	if stats := consume(t, e.newConsumer(t, e.inbox, "consumer-b", healthy.handlers())); stats.Completed != 3 {
		t.Fatalf("the next consumer finishes all three: %+v", stats)
	}
	for _, id := range ids {
		if rec := e.record(t, processingQueue, id); rec.Status != model.InboxCompleted || healthy.count(id) != 1 {
			t.Fatalf("inbox %+v", rec)
		}
	}
}

// ─── 21: reconciliation ──────────────────────────────────────────────

func TestReconciliationRepairsStuckEvents(t *testing.T) {
	ago := func() time.Time { return time.Now().UTC().Add(-20 * time.Minute) }

	t.Run("published but never consumed", func(t *testing.T) {
		e := openPipelineEnv(t)
		ctx := context.Background()
		ev := e.completed(t, "u1")
		e.publishAll(t)
		e.exec(t, `UPDATE outbox_events SET published_at = $2 WHERE id = $1`, ev.ID, ago())
		reconciler := e.newReconciler(t, func(c *events.ReconcilerConfig) { c.MaxRepublish = 1 })

		if f := findings(reconcile(t, reconciler), events.KindUnconsumed, ev.ID); len(f) != 1 || f[0].Action != "republish" || f[0].Outcome != model.DiscrepancyRepaired {
			t.Fatalf("expected a republish: %+v", f)
		}
		if stored := e.stored(t, ev.ID); stored.Status != model.OutboxPending || stored.Republished != 1 {
			t.Fatalf("outbox %+v", stored)
		}
		e.publishAll(t)
		e.exec(t, `UPDATE outbox_events SET published_at = $2 WHERE id = $1`, ev.ID, ago())
		if f := findings(reconcile(t, reconciler), events.KindUnconsumed, ev.ID); len(f) != 1 || f[0].Action != "dead-letter" {
			t.Fatalf("after the last republish it is dead-lettered: %+v", f)
		}
		dl := e.onlyDeadLetter(t, ev.ID)
		if dl.FailureCategory != model.FailureUnconsumed || dl.Consumer != processingQueue || dl.Status != model.DeadLetterOpen {
			t.Fatalf("dead letter %+v", dl)
		}

		// When a copy finally arrives and is handled, the dead letter closes.
		if stats := consume(t, e.newConsumer(t, e.inbox, "consumer", (&recorder{}).handlers())); stats.Completed != 1 {
			t.Fatalf("late delivery: %+v", stats)
		}
		if got, _ := e.dead.Get(ctx, dl.ID); got.Status != model.DeadLetterResolved || got.ResolvedBy != "system" {
			t.Fatalf("dead letter %+v", got)
		}
	})

	t.Run("a discarded dead letter stays closed", func(t *testing.T) {
		e := openPipelineEnv(t)
		ctx := context.Background()
		ev := e.completed(t, "u1")
		e.publishAll(t)
		e.exec(t, `UPDATE outbox_events SET published_at = $2 WHERE id = $1`, ev.ID, ago())
		reconciler := e.newReconciler(t, func(c *events.ReconcilerConfig) { c.MaxRepublish = 0 })
		if f := findings(reconcile(t, reconciler), events.KindUnconsumed, ev.ID); len(f) != 1 || f[0].Action != "dead-letter" {
			t.Fatalf("expected a dead letter: %+v", f)
		}

		// The operator drops it on purpose. Reconciliation must neither open
		// a new dead letter for it nor send it again.
		dl := e.onlyDeadLetter(t, ev.ID)
		if err := e.dead.Close(ctx, dl.ID, model.DeadLetterDiscarded, "operator", "dropped on purpose", time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		if f := findings(reconcile(t, reconciler), events.KindUnconsumed, ev.ID); len(f) != 0 {
			t.Fatalf("a discarded event must stay discarded: %+v", f)
		}
		if open := e.deadLettersOf(t, ev.ID); len(open) != 1 || open[0].Status != model.DeadLetterDiscarded {
			t.Fatalf("dead letters %+v", open)
		}
		if stored := e.stored(t, ev.ID); stored.Status != model.OutboxPublished || stored.Republished != 0 {
			t.Fatalf("outbox %+v", stored)
		}
	})

	t.Run("a stalled record stops at the republish limit", func(t *testing.T) {
		e := openPipelineEnv(t)
		ctx := context.Background()
		ev := e.stored(t, e.completed(t, "u1").ID)
		e.publishAll(t)
		got, _ := e.queue.Receive(ctx, processingQueue, 1, time.Minute, 0)
		_ = e.queue.Ack(ctx, processingQueue, got[0].Receipt)
		if _, _, err := e.inbox.Claim(ctx, model.InboxRecord{
			Consumer: processingQueue, EventID: ev.ID, EventType: ev.Type, SchemaVersion: ev.SchemaVersion, TenantID: e.tenant,
			AggregateType: ev.AggregateType, AggregateID: ev.AggregateID, AggregateSequence: ev.AggregateSequence,
		}, "vanished", time.Now().UTC(), time.Minute); err != nil {
			t.Fatal(err)
		}
		e.exec(t, `UPDATE event_inbox SET lease_expires_at = $2, updated_at = $2 WHERE event_id = $1`, ev.ID, ago())
		// It has already been sent again as often as allowed.
		e.exec(t, `UPDATE outbox_events SET republished = 3 WHERE id = $1`, ev.ID)

		if f := findings(reconcile(t, e.newReconciler(t)), events.KindInboxStalled, ev.ID); len(f) != 1 || f[0].Action != "dead-letter" {
			t.Fatalf("expected a dead letter instead of another republish: %+v", f)
		}
		if stored := e.stored(t, ev.ID); stored.Status != model.OutboxPublished || stored.Republished != 3 {
			t.Fatalf("it must not be sent again: %+v", stored)
		}
		if dl := e.onlyDeadLetter(t, ev.ID); dl.FailureCategory != model.FailureUnconsumed || dl.Consumer != processingQueue {
			t.Fatalf("dead letter %+v", dl)
		}
		// The open dead letter keeps it out of the next pass.
		if f := findings(reconcile(t, e.newReconciler(t)), events.KindInboxStalled, ev.ID); len(f) != 0 {
			t.Fatalf("found again: %+v", f)
		}
	})

	t.Run("stalled inbox record", func(t *testing.T) {
		e := openPipelineEnv(t)
		ctx := context.Background()
		ev := e.stored(t, e.completed(t, "u1").ID)
		e.publishAll(t)
		// A consumer took it and vanished, and the message was lost too.
		got, _ := e.queue.Receive(ctx, processingQueue, 1, time.Minute, 0)
		_ = e.queue.Ack(ctx, processingQueue, got[0].Receipt)
		if _, _, err := e.inbox.Claim(ctx, model.InboxRecord{
			Consumer: processingQueue, EventID: ev.ID, EventType: ev.Type, SchemaVersion: ev.SchemaVersion, TenantID: e.tenant,
			AggregateType: ev.AggregateType, AggregateID: ev.AggregateID, AggregateSequence: ev.AggregateSequence,
		}, "vanished", time.Now().UTC(), time.Minute); err != nil {
			t.Fatal(err)
		}
		e.exec(t, `UPDATE event_inbox SET lease_expires_at = $2, updated_at = $2 WHERE event_id = $1`, ev.ID, ago())

		if f := findings(reconcile(t, e.newReconciler(t)), events.KindInboxStalled, ev.ID); len(f) != 1 || f[0].Outcome != model.DiscrepancyRepaired {
			t.Fatalf("expected a repair: %+v", f)
		}
		if rec := e.record(t, processingQueue, ev.ID); rec.Status != model.InboxFailed {
			t.Fatalf("inbox %+v", rec)
		}
		e.publishAll(t)
		if stats := consume(t, e.newConsumer(t, e.inbox, "consumer", (&recorder{}).handlers())); stats.Completed != 1 {
			t.Fatalf("the republished event is handled: %+v", stats)
		}
	})

	t.Run("native dead-letter queue", func(t *testing.T) {
		e := openPipelineEnv(t)
		ctx := context.Background()
		e.queue.MaxReceives = 1
		ev := e.completed(t, "u1")
		e.publishAll(t)
		// Received once by a consumer that crashed before recording
		// anything; the queue then gives up on it.
		if got, _ := e.queue.Receive(ctx, processingQueue, 1, time.Second, 0); len(got) != 1 {
			t.Fatal("expected the message")
		}
		e.clock.advance(time.Minute)
		if got, _ := e.queue.Receive(ctx, processingQueue, 1, time.Second, 0); len(got) != 0 {
			t.Fatal("the queue must have moved it to its dead-letter queue")
		}

		if f := findings(reconcile(t, e.newReconciler(t)), events.KindNativeDeadLetter, ev.ID); len(f) != 1 || f[0].Outcome != model.DiscrepancyRepaired {
			t.Fatalf("expected the drain: %+v", f)
		}
		dl := e.onlyDeadLetter(t, ev.ID)
		if dl.Source != model.SourceNativeDLQ || dl.FailureCategory != model.FailureRedrive || dl.Consumer != processingQueue {
			t.Fatalf("dead letter %+v", dl)
		}
		if rec := e.record(t, processingQueue, ev.ID); rec.Status != model.InboxDead {
			t.Fatalf("inbox %+v", rec)
		}
		if stats, _ := e.queue.Stats(ctx, processingQueue); stats.DeadLetters != 0 {
			t.Fatal("the drained message must be gone from the queue")
		}
	})

	t.Run("completed upload without its event", func(t *testing.T) {
		e := openPipelineEnv(t)
		ctx := context.Background()
		e.session(t, "old", model.UploadStatusCompleted)
		id := e.tenant + "-old"
		e.exec(t, `UPDATE upload_sessions SET updated_at = $2 WHERE id = $1`, id, ago())

		report := reconcile(t, e.newReconciler(t))
		f := findings(report, events.KindMissingEvent, "")
		if len(f) != 1 || f[0].AggregateID != id || f[0].Outcome != model.DiscrepancyRepaired {
			t.Fatalf("expected the missing event: %+v", report.Findings)
		}
		emitted, err := e.outbox.Get(ctx, f[0].EventID)
		if err != nil || emitted.Type != eventdto.TypeUploadCompleted || emitted.AggregateID != id {
			t.Fatalf("emitted %+v %v", emitted, err)
		}
		recorded, _ := e.discrepancies.List(ctx, 100)
		found := false
		for _, d := range recorded {
			found = found || (d.Kind == events.KindMissingEvent && d.AggregateID == id)
		}
		if !found {
			t.Fatal("the repair must be recorded")
		}
	})

	t.Run("dry run changes nothing", func(t *testing.T) {
		e := openPipelineEnv(t)
		ctx := context.Background()
		e.session(t, "old", model.UploadStatusCompleted)
		e.exec(t, `UPDATE upload_sessions SET updated_at = $2 WHERE id = $1`, e.tenant+"-old", ago())
		ev := e.completed(t, "u1")
		e.publishAll(t)
		e.exec(t, `UPDATE outbox_events SET published_at = $2 WHERE id = $1`, ev.ID, ago())

		report := reconcile(t, e.newReconciler(t, func(c *events.ReconcilerConfig) { c.DryRun = true }))
		if len(report.Findings) != 2 {
			t.Fatalf("expected two findings: %+v", report.Findings)
		}
		for _, f := range report.Findings {
			if f.Outcome != model.DiscrepancyReported || !strings.HasPrefix(f.Action, "dry run: ") {
				t.Fatalf("a dry run only reports: %+v", f)
			}
		}
		if stored := e.stored(t, ev.ID); stored.Status != model.OutboxPublished || stored.Republished != 0 {
			t.Fatalf("outbox changed: %+v", stored)
		}
		var events int
		_ = e.db.Pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE tenant_id = $1`, e.tenant).Scan(&events)
		var recorded int
		_ = e.db.Pool.QueryRow(ctx, `SELECT count(*) FROM event_discrepancies WHERE tenant_id = $1`, e.tenant).Scan(&recorded)
		if events != 1 || recorded != 0 {
			t.Fatalf("a dry run writes nothing: %d events, %d discrepancies", events, recorded)
		}
	})
}

// ─── 23: no secrets in records or logs ───────────────────────────────

func TestFailureRecordsAndLogsCarryNoSignedURLsOrPayloads(t *testing.T) {
	e := openPipelineEnv(t)
	signed := "https://bucket.s3.amazonaws.com/quarantine/x?X-Amz-Credential=AKIAEXAMPLE&X-Amz-Signature=0123abcd"
	handler := &recorder{fail: func(eventdto.Envelope, int) error {
		return retry.Permanent(fmt.Errorf("download %s: AccessDenied", signed))
	}}
	ev := e.completed(t, "u1")
	e.publishAll(t)
	consume(t, e.newConsumer(t, e.inbox, "consumer", handler.handlers()))

	other := e.completed(t, "u2")
	e.queue.Fail = func(op, queue string) error {
		if op == "publish" {
			return errors.New("send to " + signed + " failed")
		}
		return nil
	}
	e.publish(t, e.newPublisher(t, e.outbox, "publisher"))

	dl := e.onlyDeadLetter(t, ev.ID)
	stored := e.stored(t, other.ID)
	logs := e.logs.String()
	for name, text := range map[string]string{
		"dead letter": dl.LastError, "inbox": e.record(t, processingQueue, ev.ID).LastError,
		"outbox": stored.LastError, "logs": logs,
	} {
		if strings.Contains(text, "AKIAEXAMPLE") || strings.Contains(text, "0123abcd") {
			t.Fatalf("%s holds a credential or signature: %s", name, text)
		}
	}
	if !strings.Contains(dl.LastError, "?[redacted]") || !strings.Contains(stored.LastError, "?[redacted]") {
		t.Fatalf("the URL is kept without its query: %q / %q", dl.LastError, stored.LastError)
	}
	if strings.Contains(logs, `"payload"`) || strings.Contains(logs, "upload_id") {
		t.Fatal("event payloads must never be logged")
	}
}

// ─── compaction of the event tables ──────────────────────────────────

func TestCompactionKeepsWhatTheEventPipelineNeeds(t *testing.T) {
	e := openPipelineEnv(t)
	ctx := context.Background()
	old := time.Now().UTC().Add(-60 * 24 * time.Hour)

	// Three events of one aggregate, published long ago: the latest stays,
	// so the aggregate's numbering continues from it.
	aggregate := e.tenant + "-asset"
	var ids []string
	for i := 0; i < 3; i++ {
		ev := eventdto.NewDeletionScheduled(e.tenant, aggregate, fmt.Sprint("op-", i), old, "")
		if err := e.outbox.Append(ctx, ev); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, ev.ID)
	}
	// An older event of another aggregate stays while a dead letter is
	// open for it.
	held := eventdto.NewDeletionScheduled(e.tenant, e.tenant+"-other", "op-held", old, "")
	later := eventdto.NewDeletionScheduled(e.tenant, e.tenant+"-other", "op-later", old, "")
	if err := e.outbox.Append(ctx, held); err != nil {
		t.Fatal(err)
	}
	if err := e.outbox.Append(ctx, later); err != nil {
		t.Fatal(err)
	}
	e.exec(t, `UPDATE outbox_events SET status = 'PUBLISHED', published_at = $2 WHERE tenant_id = $1`, e.tenant, old)
	e.addDeadLetter(t, e.stored(t, held.ID), eventdto.QueueLifecycle)

	// Inbox: an old completed record goes, an unfinished one stays.
	for i, id := range []string{ids[0], ids[1]} {
		ev := e.stored(t, id)
		rec := model.InboxRecord{
			Consumer: eventdto.QueueLifecycle, EventID: ev.ID, EventType: ev.Type, SchemaVersion: ev.SchemaVersion, TenantID: e.tenant,
			AggregateType: ev.AggregateType, AggregateID: ev.AggregateID, AggregateSequence: ev.AggregateSequence,
		}
		if _, _, err := e.inbox.Claim(ctx, rec, "it", time.Now().UTC(), time.Minute); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			if err := e.inbox.Complete(ctx, eventdto.QueueLifecycle, ev.ID, "it", time.Now().UTC(), "done"); err != nil {
				t.Fatal(err)
			}
		}
	}
	e.exec(t, `UPDATE event_inbox SET completed_at = $2, updated_at = $2 WHERE tenant_id = $1`, e.tenant, old)

	svc, err := compaction.NewService(postgres.NewCompactor(e.db), compaction.Config{
		BatchSize: 100, MaxBatches: 10, Only: []string{"outbox_events", "event_inbox"},
		AuditRetention: 365 * 24 * time.Hour, OperationalRetention: 30 * 24 * time.Hour, IdempotencyTTL: 7 * 24 * time.Hour,
	}, e.log, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RunOnce(ctx); err != nil {
		t.Fatalf("compaction: %v", err)
	}

	gone := func(id string) bool {
		_, err := e.outbox.Get(ctx, id)
		return errors.Is(err, model.ErrEventNotFound)
	}
	if !gone(ids[0]) || !gone(ids[1]) || gone(ids[2]) || gone(held.ID) || gone(later.ID) {
		t.Fatalf("expected only the two older events of the first aggregate gone: %v %v %v, held %v, later %v",
			gone(ids[0]), gone(ids[1]), gone(ids[2]), gone(held.ID), gone(later.ID))
	}
	if _, err := e.inbox.Get(ctx, eventdto.QueueLifecycle, ids[0]); !errors.Is(err, model.ErrEventNotFound) {
		t.Fatalf("the old completed inbox record must go, got %v", err)
	}
	if rec := e.record(t, eventdto.QueueLifecycle, ids[1]); rec.Status != model.InboxProcessing {
		t.Fatalf("an unfinished inbox record stays: %+v", rec)
	}

	next := eventdto.NewDeletionScheduled(e.tenant, aggregate, "op-3", time.Now().UTC(), "")
	if err := e.outbox.Append(ctx, next); err != nil {
		t.Fatal(err)
	}
	if seq := e.stored(t, next.ID).AggregateSequence; seq != 4 {
		t.Fatalf("the aggregate's numbering must continue at 4, got %d", seq)
	}
}
