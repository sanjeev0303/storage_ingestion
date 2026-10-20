package integration

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	eventdto "github.com/sanjeev0303/object-storage-platform/internal/dto/event"
	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/retry"
	assetservice "github.com/sanjeev0303/object-storage-platform/internal/service/asset"
	"github.com/sanjeev0303/object-storage-platform/internal/service/events"
)

// The whole pipeline on the local stack: PostgreSQL, MinIO, ClamAV and
// ElasticMQ (TEST_DATABASE_URL, TEST_S3_BUCKET, TEST_CLAMAV_ADDRESS and
// TEST_SQS_ENDPOINT).

type stackPipeline struct {
	outbox *postgres.OutboxRepository
	inbox  *postgres.InboxRepository
	dead   *postgres.DeadLetterRepository
	queue  interfaces.MessageQueue
	tenant string
}

func openStackPipeline(t *testing.T, stack *contentStack) *stackPipeline {
	t.Helper()
	db := stack.env.db
	return &stackPipeline{
		outbox: postgres.NewOutboxRepository(db), inbox: postgres.NewInboxRepository(db), dead: postgres.NewDeadLetterRepository(db),
		queue: openTestSQS(t, 5), tenant: stack.env.tenant,
	}
}

func (p *stackPipeline) publish(t *testing.T) events.PublishStats {
	t.Helper()
	publisher, err := events.NewPublisher(p.outbox, p.queue, events.PublisherConfig{
		Owner: "it-publisher", BatchSize: 10, Concurrency: 2, Lease: time.Minute, PublishTimeout: 10 * time.Second,
		Retry: retry.Policy{BaseDelay: 50 * time.Millisecond, MaxDelay: 100 * time.Millisecond, MaxAttempts: 3}, TenantID: p.tenant,
	}, lifecycleLog, nil)
	if err != nil {
		t.Fatal(err)
	}
	stats, err := publisher.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	return stats
}

// consumeAll runs the queue's consumer until nothing more arrives.
func (p *stackPipeline) consumeAll(t *testing.T, queue string, handlers map[string]events.Handler) events.ConsumeStats {
	t.Helper()
	consumer, err := events.NewConsumer(p.queue, p.inbox, p.dead, handlers, events.ConsumerConfig{
		Name: queue, Owner: "it-consumer", BatchSize: 10, Concurrency: 2, Visibility: 2 * time.Minute,
		Wait: 2 * time.Second, HandlerTimeout: 2 * time.Minute,
		Retry: retry.Policy{BaseDelay: 100 * time.Millisecond, MaxDelay: 200 * time.Millisecond, MaxAttempts: 3},
	}, lifecycleLog, nil)
	if err != nil {
		t.Fatal(err)
	}
	var total events.ConsumeStats
	for i := 0; i < 10; i++ {
		stats, err := consumer.RunOnce(context.Background())
		if err != nil {
			t.Fatalf("consume: %v", err)
		}
		if stats.Received == 0 {
			break
		}
		total.Received += stats.Received
		total.Completed += stats.Completed
		total.Skipped += stats.Skipped
		total.DeadLettered += stats.DeadLettered
	}
	return total
}

func (p *stackPipeline) outcome(t *testing.T, queue, eventID string) string {
	t.Helper()
	rec, err := p.inbox.Get(context.Background(), queue, eventID)
	if err != nil {
		t.Fatalf("inbox %s: %v", eventID, err)
	}
	return string(rec.Status) + ": " + rec.Outcome
}

// latestEvent is the newest event of an aggregate.
func (p *stackPipeline) latestEvent(t *testing.T, db *postgres.DB, aggregateType, aggregateID string) model.OutboxEvent {
	t.Helper()
	var id string
	err := db.Pool.QueryRow(context.Background(), `
		SELECT id FROM outbox_events WHERE aggregate_type = $1 AND aggregate_id = $2
		ORDER BY aggregate_sequence DESC LIMIT 1
	`, aggregateType, aggregateID).Scan(&id)
	if err != nil {
		t.Fatalf("no event for %s %s: %v", aggregateType, aggregateID, err)
	}
	e, err := p.outbox.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestLocalStackUploadBecomesAvailableThroughTheEventPipeline(t *testing.T) {
	stack := openContentStack(t)
	pipeline := openStackPipeline(t, stack)
	svc := stack.service(t, serviceOptions{})

	// The client uploads and completes; the completion commits its event.
	pdf := append([]byte("%PDF-1.7\n"), bytes.Repeat([]byte("e"), 4096)...)
	id := stack.upload(t, "invoice.pdf", "application/pdf", pdf)
	completed := pipeline.latestEvent(t, stack.env.db, eventdto.AggregateUpload, id)
	if completed.Type != eventdto.TypeUploadCompleted || completed.Status != model.OutboxPending {
		t.Fatalf("unexpected event %+v", completed)
	}
	if jobs := stack.env.ourJobs(t); len(jobs) != 0 {
		t.Fatal("nothing runs before the event is consumed")
	}

	if stats := pipeline.publish(t); stats.Published != 1 {
		t.Fatalf("publish: %+v", stats)
	}
	if stats := pipeline.consumeAll(t, eventdto.QueueProcessing, events.ProcessingHandlers(svc.Intake)); stats.Completed != 1 {
		t.Fatalf("consume: %+v", stats)
	}

	// Verification, scanning and promotion ran from the event.
	asset, job := stack.assetFor(t, id)
	if asset.Status != model.AssetStatusAvailable || job.State != model.ProcessingSucceeded || job.ScanResult != model.ScanClean {
		t.Fatalf("asset %s, job %s, scan %s", asset.Status, job.State, job.ScanResult)
	}
	if _, err := stack.provider.HeadObject(context.Background(), stack.bucket, promotionPrefix+"/"+stack.env.tenant+"/"+asset.ID); err != nil {
		t.Fatalf("the promoted object must exist: %v", err)
	}
	if got := pipeline.outcome(t, eventdto.QueueProcessing, completed.ID); got != "COMPLETED: asset promoted" {
		t.Fatalf("inbox outcome %q", got)
	}
	// The poll that is still running as a safety net finds nothing to do.
	if stats := runCycle(t, svc); stats.Enqueued != 0 || stats.Claimed != 0 {
		t.Fatalf("the poll must find the work done: %+v", stats)
	}
}

func TestLocalStackDeletionAndHoldReleaseThroughTheEventPipeline(t *testing.T) {
	stack := openContentStack(t)
	lifecycleCleanup(t, stack.env)
	pipeline := openStackPipeline(t, stack)
	ctx := context.Background()

	first := stack.upload(t, "first.pdf", "application/pdf", []byte("%PDF-1.7\nfirst\n"))
	second := stack.upload(t, "second.pdf", "application/pdf", []byte("%PDF-1.7\nsecond\n"))
	runCycle(t, stack.service(t, serviceOptions{}))
	a, _ := stack.assetFor(t, first)
	b, _ := stack.assetFor(t, second)
	// The two completions wrote their upload.completed events; send them
	// on, so only the lifecycle events below are left.
	if stats := pipeline.publish(t); stats.Published != 2 {
		t.Fatalf("publish upload events: %+v", stats)
	}

	lifecycle, _ := localLifecycle(t, stack, nil, "worker-a")
	zero := time.Duration(0)
	if _, err := lifecycle.SetPolicy(ctx, model.RetentionPolicy{
		TenantID: stack.env.tenant, Scope: model.ScopeTenant, DeletionGrace: &zero, CreatedBy: "it", Reason: "no grace in tests",
	}); err != nil {
		t.Fatal(err)
	}
	hold, err := lifecycle.PlaceHold(ctx, model.LegalHold{TenantID: stack.env.tenant, Scope: model.ScopeAsset, ScopeKey: b.ID, Reason: "case 7", PlacedBy: "legal"})
	if err != nil {
		t.Fatal(err)
	}
	owner := member(stack.env.tenant, "owner", model.RoleDeveloper)
	for _, asset := range []model.Asset{a, b} {
		if _, err := lifecycle.RequestDeletion(ctx, owner, asset.ID); err != nil {
			t.Fatal(err)
		}
	}

	// Both deletions are due at once: the events run them, and the hold
	// protects the second.
	handlers := events.LifecycleHandlers(lifecycle.RunDeletion, lifecycle.ResumeAfterHold)
	if stats := pipeline.publish(t); stats.Published != 2 {
		t.Fatalf("publish: %+v", stats)
	}
	if stats := pipeline.consumeAll(t, eventdto.QueueLifecycle, handlers); stats.Completed != 2 {
		t.Fatalf("consume: %+v", stats)
	}
	if got, _ := stack.env.assets.GetByID(ctx, stack.env.tenant, a.ID); got.Status != model.AssetStatusDeleted {
		t.Fatalf("the first asset must be deleted, got %s", got.Status)
	}
	if _, err := stack.provider.HeadObject(ctx, stack.bucket, a.ObjectKey); !errors.Is(err, interfaces.ErrObjectNotFound) {
		t.Fatalf("the first asset's object must be gone, got %v", err)
	}
	deletions := postgres.NewDeletionRepository(stack.env.db)
	held, err := deletions.GetLive(ctx, stack.env.tenant, b.ID)
	if err != nil || held.State != model.DeletionProtected {
		t.Fatalf("the held deletion waits: %+v %v", held, err)
	}

	// Releasing the hold resumes the deletion right away instead of at the
	// next recheck.
	if err := lifecycle.ReleaseHold(ctx, stack.env.tenant, hold.ID, "legal"); err != nil {
		t.Fatal(err)
	}
	released := pipeline.latestEvent(t, stack.env.db, eventdto.AggregateLegalHold, hold.ID)
	if stats := pipeline.publish(t); stats.Published != 1 {
		t.Fatalf("publish: %+v", stats)
	}
	if stats := pipeline.consumeAll(t, eventdto.QueueLifecycle, handlers); stats.Completed != 1 {
		t.Fatalf("consume: %+v", stats)
	}
	if got := pipeline.outcome(t, eventdto.QueueLifecycle, released.ID); !strings.HasPrefix(got, "COMPLETED: 1 protected") {
		t.Fatalf("inbox outcome %q", got)
	}
	// The deletion schedule picks it up on its next pass, with no wait for
	// the recheck interval.
	var cycle assetservice.CycleStats
	if err := lifecycle.ProcessDeletions(ctx, &cycle); err != nil || cycle.Completed != 1 {
		t.Fatalf("the resumed deletion completes: %+v %v", cycle, err)
	}
	if got, _ := stack.env.assets.GetByID(ctx, stack.env.tenant, b.ID); got.Status != model.AssetStatusDeleted {
		t.Fatalf("the second asset must be deleted, got %s", got.Status)
	}
}
