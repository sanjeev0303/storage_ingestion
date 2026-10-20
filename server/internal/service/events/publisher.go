// Package events runs the event pipeline: the outbox publisher, queue
// consumers with their inbox, dead-letter operations and reconciliation.
//
// Delivery is at least once. An event is committed in the outbox together
// with the business change that caused it, published after the commit,
// and may reach a consumer more than once; consumers stay correct because
// the inbox and the domain's own conditional state changes make a repeat
// harmless. Nothing here is exactly once.
package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/dto/event"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/metrics"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/retry"
)

// recordTimeout bounds the database write that records an outcome. It
// runs even during shutdown: by then the outcome is already known.
const recordTimeout = 10 * time.Second

type PublisherConfig struct {
	// Owner identifies this publisher on the leases it takes.
	Owner       string
	BatchSize   int
	Concurrency int
	// Lease covers a claimed batch; a publisher that stops mid-way loses
	// its events to the next claim once it runs out.
	Lease          time.Duration
	PublishTimeout time.Duration
	// Retry is the publication budget: attempts, backoff and jitter.
	Retry retry.Policy
	// TenantID limits publishing to one tenant.
	TenantID string
}

// Publisher sends committed outbox events to the queue.
type Publisher struct {
	outbox  interfaces.OutboxRepository
	queue   interfaces.MessageQueue
	cfg     PublisherConfig
	log     *slog.Logger
	metrics *metrics.Registry
	now     func() time.Time
}

func NewPublisher(outbox interfaces.OutboxRepository, queue interfaces.MessageQueue, cfg PublisherConfig, log *slog.Logger, registry *metrics.Registry) (*Publisher, error) {
	if outbox == nil || queue == nil || log == nil {
		return nil, errors.New("publisher dependencies are required")
	}
	if cfg.Owner == "" || cfg.BatchSize <= 0 || cfg.Concurrency <= 0 || cfg.PublishTimeout <= 0 || cfg.Lease <= cfg.PublishTimeout+recordTimeout {
		return nil, errors.New("publisher needs an owner, a positive batch and concurrency, and a lease longer than the publish timeout plus 10s to record the outcome")
	}
	if cfg.Retry.MaxAttempts <= 0 || cfg.Retry.BaseDelay <= 0 || cfg.Retry.MaxDelay < cfg.Retry.BaseDelay {
		return nil, errors.New("publisher retry policy is invalid")
	}
	return &Publisher{
		outbox: outbox, queue: queue, cfg: cfg, log: log, metrics: registry,
		now: func() time.Time { return time.Now().UTC() },
	}, nil
}

// PublishStats counts what one batch did.
type PublishStats struct {
	Claimed      int
	Published    int
	Retried      int
	DeadLettered int
	// Released events were claimed but not tried: the queue was
	// throttling, or the publisher was shutting down.
	Released int
	// Unrecorded events reached the queue, or failed, but the outcome
	// could not be saved. They are sent again after their lease runs out.
	Unrecorded int
}

// Outcomes of one publication.
const (
	outcomePublished  = "published"
	outcomeRetried    = "retried"
	outcomeDead       = "dead_lettered"
	outcomeReleased   = "released"
	outcomeUnrecorded = "unrecorded"
)

func (s *PublishStats) add(outcome string) {
	switch outcome {
	case outcomePublished:
		s.Published++
	case outcomeRetried:
		s.Retried++
	case outcomeDead:
		s.DeadLettered++
	case outcomeReleased:
		s.Released++
	case outcomeUnrecorded:
		s.Unrecorded++
	}
}

// RunOnce claims one batch of due events and publishes it, at most
// Concurrency at a time. No transaction is open while the queue is called.
func (p *Publisher) RunOnce(ctx context.Context) (PublishStats, error) {
	claimedAt := p.now()
	events, err := p.outbox.Claim(ctx, p.cfg.Owner, claimedAt, p.cfg.Lease, p.cfg.BatchSize, p.cfg.TenantID)
	if err != nil {
		return PublishStats{}, fmt.Errorf("claim outbox events: %w", err)
	}
	stats := PublishStats{Claimed: len(events)}

	// A batch can take longer than its lease when the queue is slow. A send
	// started after this point could still be running when the lease runs
	// out and another publisher claims the event, so it is given back instead.
	sendBy := claimedAt.Add(p.cfg.Lease - p.cfg.PublishTimeout - recordTimeout)

	// Once the queue throttles, events not sent yet are given back with a
	// delay instead of adding to its load.
	var throttled atomic.Bool
	var mu sync.Mutex
	var wg sync.WaitGroup
	slots := make(chan struct{}, p.cfg.Concurrency)
	for _, e := range events {
		slots <- struct{}{}
		wg.Add(1)
		go func(e model.OutboxEvent) {
			defer wg.Done()
			defer func() { <-slots }()
			outcome := p.publish(ctx, e, sendBy, &throttled)
			mu.Lock()
			stats.add(outcome)
			mu.Unlock()
		}(e)
	}
	wg.Wait()
	return stats, nil
}

func (p *Publisher) publish(ctx context.Context, e model.OutboxEvent, sendBy time.Time, throttled *atomic.Bool) string {
	log := p.log.With("event_id", e.ID, "event_type", e.Type, "tenant_id", e.TenantID,
		"aggregate", e.AggregateType+"/"+e.AggregateID, "correlation_id", e.CorrelationID)
	record, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()

	if ctx.Err() != nil || throttled.Load() || p.now().After(sendBy) {
		next := p.now()
		if throttled.Load() {
			next = next.Add(p.cfg.Retry.Delay(1))
		}
		if err := p.outbox.Release(record, e.ID, p.cfg.Owner, p.now(), next); err != nil {
			log.Warn("release unsent event", "error", retry.Sanitize(err))
		}
		return outcomeReleased
	}

	// The claim counted an attempt abandoned by a publisher that stopped
	// mid-way. If that spent the budget, another send would go past it.
	if p.cfg.Retry.Exhausted(e.Attempts) {
		p.metrics.Add("outbox_publish_failures", 1)
		return p.dead(record, e, e.Attempts, model.FailureExhausted, "publication budget spent; the last attempt was abandoned", log)
	}

	body, err := json.Marshal(event.FromOutbox(e))
	if err != nil {
		return p.fail(record, e, retry.Permanent(fmt.Errorf("encode envelope: %w", err)), log)
	}

	started := time.Now()
	sendCtx, cancelSend := context.WithTimeout(ctx, p.cfg.PublishTimeout)
	_, err = p.queue.Publish(sendCtx, e.Destination, interfaces.OutgoingMessage{
		Body:     body,
		GroupKey: e.AggregateType + ":" + e.AggregateID,
		// A new generation per republication, so a deliberate resend is
		// not dropped as a duplicate of the first.
		DedupKey: fmt.Sprintf("%s-%d", e.ID, e.Republished),
	})
	cancelSend()
	p.metrics.Add("outbox_publish_ms_total", time.Since(started).Milliseconds())

	if err != nil && ctx.Err() != nil {
		// Cut short by shutdown, not refused by the queue, which may or may
		// not have it. Give it back; a consumer drops a duplicate.
		if err := p.outbox.Release(record, e.ID, p.cfg.Owner, p.now(), p.now()); err != nil {
			log.Warn("release interrupted event", "error", retry.Sanitize(err))
		}
		return outcomeReleased
	}
	if err != nil {
		if errors.Is(err, interfaces.ErrQueueThrottled) {
			// The queue's load, not this event: give it back after a backoff
			// without spending its budget, or sustained throttling would
			// dead-letter events that were never at fault.
			throttled.Store(true)
			p.metrics.Add("outbox_publish_throttled", 1)
			next := p.now().Add(p.cfg.Retry.Delay(e.Attempts + 1))
			if err := p.outbox.Release(record, e.ID, p.cfg.Owner, p.now(), next); err != nil {
				log.Warn("release throttled event", "error", retry.Sanitize(err))
			}
			log.Warn("queue throttled; event given back", "next_attempt_at", next)
			return outcomeReleased
		}
		return p.fail(record, e, err, log)
	}

	// The queue has the event. If this write fails the lease runs out, the
	// event is sent again, and the consumer's inbox drops the duplicate.
	if err := p.outbox.MarkPublished(record, e.ID, p.cfg.Owner, p.now()); err != nil {
		p.metrics.Add("outbox_publish_unrecorded", 1)
		log.Warn("event published but not recorded; it will be sent again", "error", retry.Sanitize(err))
		return outcomeUnrecorded
	}
	p.metrics.Add("outbox_published", 1)
	log.Debug("event published", "destination", e.Destination, "attempt", e.Attempts+1)
	return outcomePublished
}

// fail records a failed publication: retried with backoff and jitter, or
// dead-lettered when it is permanent or the budget is spent.
func (p *Publisher) fail(ctx context.Context, e model.OutboxEvent, cause error, log *slog.Logger) string {
	attempt := e.Attempts + 1
	reason := retry.Sanitize(cause)
	p.metrics.Add("outbox_publish_failures", 1)

	if retry.IsPermanent(cause) {
		return p.dead(ctx, e, attempt, model.FailurePermanent, reason, log)
	}
	if p.cfg.Retry.Exhausted(attempt) {
		return p.dead(ctx, e, attempt, model.FailureExhausted, reason, log)
	}

	now := p.now()
	next := now.Add(p.cfg.Retry.Delay(attempt))
	if err := p.outbox.Retry(ctx, e.ID, p.cfg.Owner, now, next, reason); err != nil {
		log.Error("schedule publish retry", "error", retry.Sanitize(err))
		return outcomeUnrecorded
	}
	p.metrics.Add("outbox_publish_retries", 1)
	log.Warn("publish failed; will retry", "attempt", attempt, "next_attempt_at", next, "error", reason)
	return outcomeRetried
}

// dead moves an event to the dead letters after attempts publication tries.
func (p *Publisher) dead(ctx context.Context, e model.OutboxEvent, attempts int, category, reason string, log *slog.Logger) string {
	now := p.now()
	dl := deadLetterOf(event.FromOutbox(e), model.SourcePublisher, "", category, reason, attempts, now)
	if attempts > 1 {
		dl.FirstFailedAt = e.CreatedAt
	}
	if err := p.outbox.Dead(ctx, e.ID, p.cfg.Owner, dl); err != nil {
		log.Error("record dead-lettered event", "error", retry.Sanitize(err))
		return outcomeUnrecorded
	}
	p.metrics.Add("outbox_dead_lettered", 1)
	log.Error("event could not be published; dead-lettered", "category", category, "attempts", attempts, "error", reason)
	return outcomeDead
}

// deadLetterOf describes a failed event for the dead-letter table. Only
// identifiers and the sanitized error are kept, never the payload.
func deadLetterOf(env event.Envelope, source, consumer, category, reason string, attempts int, now time.Time) model.DeadLetter {
	dl := model.DeadLetter{
		ID: event.NewID(), EventID: env.ID, Source: source, Consumer: consumer,
		EventType: env.Type, SchemaVersion: env.SchemaVersion, TenantID: env.TenantID,
		AggregateType: env.AggregateType, AggregateID: env.AggregateID, CorrelationID: env.CorrelationID,
		FailureCategory: category, LastError: reason, Attempts: attempts,
		FirstFailedAt: now, LastFailedAt: now, DeadLetteredAt: now, Status: model.DeadLetterOpen, UpdatedAt: now,
	}
	if !env.OccurredAt.IsZero() {
		created := env.OccurredAt
		dl.EventCreatedAt = &created
	}
	return dl
}
