package integration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	eventdto "github.com/sanjeev0303/object-storage-platform/internal/dto/event"
	sqsqueue "github.com/sanjeev0303/object-storage-platform/internal/infrastructure/queue/sqs"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/retry"
	"github.com/sanjeev0303/object-storage-platform/internal/service/events"
)

// The SQS adapter against ElasticMQ (TEST_SQS_ENDPOINT, for example
// http://127.0.0.1:9324). Every test creates queues of its own and removes
// them afterwards.

func sqsTestConfig(t *testing.T, maxReceives int, create bool) sqsqueue.Config {
	t.Helper()
	endpoint := os.Getenv("TEST_SQS_ENDPOINT")
	if endpoint == "" {
		t.Skip("TEST_SQS_ENDPOINT is not set; queue adapter tests were not run")
	}
	return sqsqueue.Config{
		Region: "us-east-1", Endpoint: endpoint, AccessKeyID: "test", SecretAccessKey: "test",
		Prefix: fmt.Sprintf("it-%d-", time.Now().UnixNano()), MaxReceiveCount: maxReceives, CreateQueues: create,
	}
}

func openTestSQS(t *testing.T, maxReceives int) *sqsqueue.Queue {
	t.Helper()
	cfg := sqsTestConfig(t, maxReceives, true)
	queues := []string{eventdto.QueueProcessing, eventdto.QueueLifecycle}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	q, err := sqsqueue.New(ctx, cfg, queues...)
	if err != nil {
		t.Fatalf("connect to ElasticMQ: %v", err)
	}
	t.Cleanup(func() { deleteTestQueues(cfg, queues) })
	return q
}

// deleteTestQueues removes a test's queues with the SDK directly: the
// adapter itself never deletes a queue.
func deleteTestQueues(cfg sqsqueue.Config, queues []string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(cfg.Region),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, "")))
	if err != nil {
		return
	}
	client := sqs.NewFromConfig(awsCfg, func(o *sqs.Options) { o.BaseEndpoint = aws.String(cfg.Endpoint) })
	for _, q := range queues {
		for _, name := range []string{cfg.Prefix + q + ".fifo", cfg.Prefix + q + "-dlq.fifo"} {
			out, err := client.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String(name)})
			if err == nil {
				_, _ = client.DeleteQueue(ctx, &sqs.DeleteQueueInput{QueueUrl: out.QueueUrl})
			}
		}
	}
}

func receiveOne(t *testing.T, q interfaces.MessageQueue, queue string, wait time.Duration) interfaces.QueueMessage {
	t.Helper()
	got, err := q.Receive(context.Background(), queue, 1, 30*time.Second, wait)
	if err != nil || len(got) != 1 {
		t.Fatalf("expected one message from %s, got %d (%v)", queue, len(got), err)
	}
	return got[0]
}

func TestSQSAdapterAgainstElasticMQ(t *testing.T) {
	q := openTestSQS(t, 2)
	ctx := context.Background()
	processing, lifecycle := eventdto.QueueProcessing, eventdto.QueueLifecycle
	send := func(queue, body, group, dedup string) {
		t.Helper()
		if _, err := q.Publish(ctx, queue, interfaces.OutgoingMessage{Body: []byte(body), GroupKey: group, DedupKey: dedup}); err != nil {
			t.Fatalf("publish %s: %v", body, err)
		}
	}

	// A group arrives in order; a repeated deduplication ID is dropped.
	send(processing, "a1", "a", "a1")
	send(processing, "a2", "a", "a2")
	send(processing, "b1", "b", "b1")
	send(processing, "a1 again", "a", "a1")
	var bodies []string
	for i := 0; i < 10 && len(bodies) < 3; i++ {
		got, err := q.Receive(ctx, processing, 10, 30*time.Second, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range got {
			bodies = append(bodies, string(m.Body))
			// The consumer keeps a batch's events of one group in order by
			// this key, so the adapter must return the group it was sent with.
			if want := string(m.Body[:1]); m.GroupKey != want {
				t.Fatalf("message %q: group %q, want %q", m.Body, m.GroupKey, want)
			}
			if err := q.Ack(ctx, processing, m.Receipt); err != nil {
				t.Fatal(err)
			}
		}
	}
	if len(bodies) != 3 || slices.Index(bodies, "a1") > slices.Index(bodies, "a2") || !slices.Contains(bodies, "b1") {
		t.Fatalf("expected a1 before a2, and b1: %v", bodies)
	}
	if got, _ := q.Receive(ctx, processing, 10, 30*time.Second, time.Second); len(got) != 0 {
		t.Fatalf("the repeat must have been dropped, got %q", got[0].Body)
	}

	// Retry hides the message for its delay; Extend keeps it hidden.
	send(lifecycle, "r", "r", "r")
	first := receiveOne(t, q, lifecycle, time.Second)
	if err := q.Retry(ctx, lifecycle, first.Receipt, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	if got, _ := q.Receive(ctx, lifecycle, 1, 30*time.Second, 0); len(got) != 0 {
		t.Fatal("the message stays hidden during its delay")
	}
	second := receiveOne(t, q, lifecycle, 5*time.Second)
	if second.ReceiveCount != 2 || second.SentAt.IsZero() {
		t.Fatalf("expected the second delivery, got %+v", second)
	}
	if err := q.Extend(ctx, lifecycle, second.Receipt, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if stats, err := q.Stats(ctx, lifecycle); err != nil || stats.InFlight != 1 {
		t.Fatalf("one message in flight: %+v %v", stats, err)
	}

	// Past MaxReceiveCount the queue moves it to its dead-letter queue.
	if err := q.Retry(ctx, lifecycle, second.Receipt, 0); err != nil {
		t.Fatal(err)
	}
	if got, _ := q.Receive(ctx, lifecycle, 1, 30*time.Second, 2*time.Second); len(got) != 0 {
		t.Fatal("a third delivery must not happen")
	}
	var dead []interfaces.QueueMessage
	for i := 0; i < 10 && len(dead) == 0; i++ {
		dead, _ = q.ReceiveDeadLetters(ctx, lifecycle, 10, 30*time.Second)
		if len(dead) == 0 {
			time.Sleep(200 * time.Millisecond)
		}
	}
	if len(dead) != 1 || string(dead[0].Body) != "r" {
		t.Fatalf("expected the message in the dead-letter queue, got %d", len(dead))
	}
	if err := q.AckDeadLetter(ctx, lifecycle, dead[0].Receipt); err != nil {
		t.Fatal(err)
	}
	if stats, err := q.Stats(ctx, lifecycle); err != nil || stats != (interfaces.QueueStats{}) {
		t.Fatalf("expected an empty queue: %+v %v", stats, err)
	}

	// Without the queues a worker does not start.
	cfg := sqsTestConfig(t, 2, false)
	if _, err := sqsqueue.New(ctx, cfg, processing); !errors.Is(err, interfaces.ErrQueueNotFound) {
		t.Fatalf("expected queue not found, got %v", err)
	}
}

func TestSQSQueuesThatVanishAreRecreatedLocally(t *testing.T) {
	cfg := sqsTestConfig(t, 2, true)
	ctx := context.Background()
	q, err := sqsqueue.New(ctx, cfg, eventdto.QueueProcessing)
	if err != nil {
		t.Fatal(err)
	}
	queues := []string{eventdto.QueueProcessing}
	t.Cleanup(func() { deleteTestQueues(cfg, queues) })

	// ElasticMQ keeps queues in memory: a restart loses them.
	deleteTestQueues(cfg, queues)
	msg := interfaces.OutgoingMessage{Body: []byte("x"), GroupKey: "g", DedupKey: "x"}
	_, err = q.Publish(ctx, eventdto.QueueProcessing, msg)
	if !errors.Is(err, interfaces.ErrQueueNotFound) || retry.IsPermanent(err) {
		t.Fatalf("expected a retryable queue-not-found error, got %v", err)
	}
	// The failed call brought the queues back; the publisher's retry works.
	if _, err := q.Publish(ctx, eventdto.QueueProcessing, msg); err != nil {
		t.Fatalf("the retry must find the recreated queue: %v", err)
	}
	if got := receiveOne(t, q, eventdto.QueueProcessing, 2*time.Second); string(got.Body) != "x" {
		t.Fatalf("unexpected body %q", got.Body)
	}
}

func TestEventPipelineOverElasticMQ(t *testing.T) {
	e := openPipelineEnv(t)
	q := openTestSQS(t, 5)
	ctx := context.Background()
	ev := e.completed(t, "u1")

	publisher, err := events.NewPublisher(e.outbox, q, events.PublisherConfig{
		Owner: "publisher", BatchSize: 10, Concurrency: 2, Lease: 30 * time.Second, PublishTimeout: 10 * time.Second,
		Retry: retry.Policy{BaseDelay: 50 * time.Millisecond, MaxDelay: 100 * time.Millisecond, MaxAttempts: 3}, TenantID: e.tenant,
	}, e.log, e.metrics)
	if err != nil {
		t.Fatal(err)
	}
	if stats := e.publish(t, publisher); stats.Published != 1 {
		t.Fatalf("publish: %+v", stats)
	}
	// A publisher that sends the same generation again after a crash is
	// deduplicated by SQS itself.
	stored := e.stored(t, ev.ID)
	if _, err := q.Publish(ctx, eventdto.QueueProcessing, interfaces.OutgoingMessage{
		Body: envelopeOf(t, stored), GroupKey: stored.AggregateType + ":" + stored.AggregateID, DedupKey: stored.ID + "-0",
	}); err != nil {
		t.Fatal(err)
	}

	handler := &recorder{}
	consumer, err := events.NewConsumer(q, e.inbox, e.dead, handler.handlers(), events.ConsumerConfig{
		Name: eventdto.QueueProcessing, Owner: "consumer", BatchSize: 10, Concurrency: 2, Visibility: 10 * time.Second,
		Wait: 2 * time.Second, HandlerTimeout: time.Minute,
		Retry: retry.Policy{BaseDelay: 100 * time.Millisecond, MaxDelay: 200 * time.Millisecond, MaxAttempts: 3},
	}, e.log, e.metrics)
	if err != nil {
		t.Fatal(err)
	}
	if stats := consume(t, consumer); stats.Completed != 1 {
		t.Fatalf("consume: %+v", stats)
	}
	if stats := consume(t, consumer); stats.Received != 0 {
		t.Fatalf("the deduplicated copy must not arrive: %+v", stats)
	}

	// A deliberate republication is a new generation: SQS delivers it and
	// the inbox drops it.
	if err := e.outbox.Republish(ctx, ev.ID, 0, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if stats := e.publish(t, publisher); stats.Published != 1 {
		t.Fatalf("republish: %+v", stats)
	}
	if stats := consume(t, consumer); stats.Duplicates != 1 {
		t.Fatalf("the republished copy is a duplicate: %+v", stats)
	}
	if rec := e.record(t, eventdto.QueueProcessing, ev.ID); rec.Status != model.InboxCompleted || handler.count(ev.ID) != 1 {
		t.Fatalf("inbox %+v, calls %d", rec, handler.count(ev.ID))
	}
}
