package integration

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	eventdto "github.com/sanjeev0303/object-storage-platform/internal/dto/event"
	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
	sqsqueue "github.com/sanjeev0303/object-storage-platform/internal/infrastructure/queue/sqs"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/metrics"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/retry"
	datasetservice "github.com/sanjeev0303/object-storage-platform/internal/service/dataset"
	"github.com/sanjeev0303/object-storage-platform/internal/service/events"
)

// Phase 10 through the event pipeline: the outbox rows an import and a
// validation write, published to ElasticMQ and consumed by the dataset
// worker's handlers, delivered twice.

func TestDatasetEventsOverElasticMQ(t *testing.T) {
	s := openSecurityStack(t, nil)
	cfg := sqsTestConfig(t, 5, true)
	ctx := context.Background()
	// Uploads and the catalog write events of their own: every queue is
	// there so they publish too.
	queues := []string{eventdto.QueueDatasets, eventdto.QueueProcessing, eventdto.QueueLifecycle, eventdto.QueueIngestion}
	q, err := sqsqueue.New(ctx, cfg, queues...)
	if err != nil {
		t.Fatalf("connect to ElasticMQ: %v", err)
	}
	t.Cleanup(func() { deleteTestQueues(cfg, queues) })

	acme := s.team(t, "ds-events")
	dev := acme.developer
	objs := s.publishObjects(t, dev, "events", 3)
	d := s.newDataset(t, dev, "evented")
	manifest := ""
	for _, o := range objs {
		manifest += fmt.Sprintf(`{"object_id":%q,"label":"x","split":"train"}`, o.ObjectID) + "\n"
	}
	imp, _, err := s.datasets.ImportInline(ctx, dev, d.ID, datasetservice.ImportInput{IdempotencyKey: "events-1"}, strings.NewReader(manifest))
	if err != nil {
		t.Fatal(err)
	}

	outbox := postgres.NewOutboxRepository(s.db)
	publisher, err := events.NewPublisher(outbox, q, events.PublisherConfig{
		Owner: "publisher", BatchSize: 10, Concurrency: 2, Lease: 30 * time.Second, PublishTimeout: 10 * time.Second,
		Retry: retry.Policy{BaseDelay: 50 * time.Millisecond, MaxDelay: 100 * time.Millisecond, MaxAttempts: 3}, TenantID: acme.org.ID,
	}, s.log, metrics.New())
	if err != nil {
		t.Fatal(err)
	}
	worker := s.newDatasetService(t, nil)
	handlers := events.DatasetHandlers(
		func(ctx context.Context, tenantID, id string) (string, error) {
			return worker.RunImport(ctx, "consumer", tenantID, id)
		},
		func(ctx context.Context, tenantID, id string) (string, error) {
			return worker.RunValidation(ctx, "consumer", tenantID, id)
		},
	)
	consumer, err := events.NewConsumer(q, postgres.NewInboxRepository(s.db), postgres.NewDeadLetterRepository(s.db), handlers, events.ConsumerConfig{
		Name: eventdto.QueueDatasets, Owner: "consumer", BatchSize: 10, Concurrency: 2, Visibility: 30 * time.Second,
		Wait: 2 * time.Second, HandlerTimeout: time.Minute,
		Retry: retry.Policy{BaseDelay: 100 * time.Millisecond, MaxDelay: 200 * time.Millisecond, MaxAttempts: 3},
	}, s.log, metrics.New())
	if err != nil {
		t.Fatal(err)
	}

	// The import's event runs the import.
	if stats, err := publisher.RunOnce(ctx); err != nil || stats.Published == 0 || stats.Retried != 0 {
		t.Fatalf("publish import: %+v %v", stats, err)
	}
	if stats := consume(t, consumer); stats.Completed != 1 {
		t.Fatalf("consume import: %+v", stats)
	}
	if got := s.importOf(t, dev, d.ID, imp.ID); got.State != model.ImportCompleted || got.MembersAdded != 3 {
		t.Fatalf("import after its event: %+v", got)
	}

	// The validation's event runs the validation.
	if _, _, err := s.datasets.Snapshot(ctx, dev, d.ID, ""); err != nil {
		t.Fatal(err)
	}
	job, _, err := s.datasets.StartValidation(ctx, dev, d.ID, datasetservice.ValidationInput{ProfileID: "builtin:supervised-classification"})
	if err != nil {
		t.Fatal(err)
	}
	if stats, err := publisher.RunOnce(ctx); err != nil || stats.Published == 0 || stats.Retried != 0 {
		t.Fatalf("publish validation: %+v %v", stats, err)
	}
	if stats := consume(t, consumer); stats.Completed != 1 {
		t.Fatalf("consume validation: %+v", stats)
	}
	job, _ = s.datasets.Validation(ctx, dev, job.ID)
	if job.ReportID == "" || job.ProcessedItems != 3 {
		t.Fatalf("validation after its event: %+v", job)
	}
	findings := s.allFindings(t, dev, job.ReportID, "")

	// Delivered again (a republished event is a new generation): the inbox
	// drops it, and a handler that runs anyway finds nothing due.
	var eventID string
	if err := s.db.Pool.QueryRow(ctx, `SELECT id FROM outbox_events WHERE aggregate_id = $1 AND event_type = $2`,
		job.ID, eventdto.TypeValidationQueued).Scan(&eventID); err != nil {
		t.Fatal(err)
	}
	if err := outbox.Republish(ctx, eventID, 0, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if stats, err := publisher.RunOnce(ctx); err != nil || stats.Published != 1 {
		t.Fatalf("republish: %+v %v", stats, err)
	}
	if stats := consume(t, consumer); stats.Duplicates != 1 {
		t.Fatalf("the republished copy is a duplicate: %+v", stats)
	}
	stored, err := outbox.Get(ctx, eventID)
	if err != nil {
		t.Fatal(err)
	}
	env := eventdto.FromOutbox(stored)
	if note, err := handlers[eventdto.TypeValidationQueued](ctx, env); err != nil || !strings.Contains(note, "not due") {
		t.Fatalf("handled again: %q %v", note, err)
	}
	if again, _ := s.datasets.Validation(ctx, dev, job.ID); again.ReportID != job.ReportID || again.ProcessedItems != 3 ||
		len(s.allFindings(t, dev, job.ReportID, "")) != len(findings) {
		t.Fatalf("a second delivery changed the job: %+v", again)
	}
	// The event is stale for an operator, too.
	if reason, err := (events.Checks{Datasets: s.datasetDB}).Stale(ctx, env); err != nil || !strings.Contains(reason, "COMPLETED") {
		t.Fatalf("stale check: %q %v", reason, err)
	}
}
