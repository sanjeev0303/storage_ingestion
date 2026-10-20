package events

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/dto/event"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/correlation"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/metrics"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/retry"
)

// Handler does the work one event asks for, through the owning service.
// It must be safe to repeat, since an event can arrive more than once, and
// returns a short note on what it did. An error wrapping
// model.ErrStaleEvent means the event no longer applies and is skipped; one
// marked retry.Permanent cannot succeed by retrying.
type Handler func(ctx context.Context, env event.Envelope) (string, error)

type ConsumerConfig struct {
	// Name is the queue the consumer reads, and its name in the inbox.
	Name string
	// Owner identifies this consumer on the inbox leases it takes.
	Owner       string
	BatchSize   int
	Concurrency int
	// Visibility is how long a received message stays hidden and the
	// inbox lease lasts; a heartbeat renews both while the handler runs.
	Visibility     time.Duration
	Wait           time.Duration
	HandlerTimeout time.Duration
	// Retry is the delivery budget per event: attempts, backoff, jitter.
	Retry retry.Policy
}

// Consumer takes events from one queue and hands them to their handlers.
// A message is acknowledged only after its outcome is recorded.
type Consumer struct {
	queue    interfaces.MessageQueue
	inbox    interfaces.InboxRepository
	dead     interfaces.DeadLetterRepository
	handlers map[string]Handler
	cfg      ConsumerConfig
	log      *slog.Logger
	metrics  *metrics.Registry
	now      func() time.Time
}

func NewConsumer(queue interfaces.MessageQueue, inbox interfaces.InboxRepository, dead interfaces.DeadLetterRepository,
	handlers map[string]Handler, cfg ConsumerConfig, log *slog.Logger, registry *metrics.Registry) (*Consumer, error) {
	if queue == nil || inbox == nil || dead == nil || len(handlers) == 0 || log == nil {
		return nil, errors.New("consumer dependencies are required")
	}
	if cfg.Name == "" || cfg.Owner == "" || cfg.BatchSize <= 0 || cfg.Concurrency <= 0 || cfg.HandlerTimeout <= 0 {
		return nil, errors.New("consumer needs a queue, an owner, and a positive batch, concurrency and handler timeout")
	}
	if cfg.Visibility < 3*time.Second || cfg.Wait < 0 {
		return nil, errors.New("consumer visibility must be at least 3s and wait not negative")
	}
	if cfg.Retry.MaxAttempts <= 0 || cfg.Retry.BaseDelay <= 0 || cfg.Retry.MaxDelay < cfg.Retry.BaseDelay {
		return nil, errors.New("consumer retry policy is invalid")
	}
	return &Consumer{
		queue: queue, inbox: inbox, dead: dead, handlers: handlers, cfg: cfg, log: log, metrics: registry,
		now: func() time.Time { return time.Now().UTC() },
	}, nil
}

// ConsumeStats counts what one batch did.
type ConsumeStats struct {
	Received     int
	Completed    int
	Skipped      int
	Duplicates   int
	Busy         int
	Retried      int
	DeadLettered int
	// Interrupted events stopped for shutdown; they come back after their
	// visibility timeout.
	Interrupted int
	// Unrecorded events could not have their outcome saved; they are
	// delivered again.
	Unrecorded int
	// Deferred events were handed back untried because an earlier event of
	// their group in the same batch did not finish.
	Deferred int
}

// Outcomes of one delivery.
const (
	outcomeCompleted   = "completed"
	outcomeSkipped     = "skipped"
	outcomeDuplicate   = "duplicate"
	outcomeBusy        = "busy"
	outcomeInterrupted = "interrupted"
	outcomeDeferred    = "deferred"
)

// settled reports whether a delivery is finished with, so the next event
// of its group may go ahead.
func settled(outcome string) bool {
	switch outcome {
	case outcomeCompleted, outcomeSkipped, outcomeDuplicate, outcomeDead:
		return true
	}
	return false
}

func (s *ConsumeStats) add(outcome string) {
	switch outcome {
	case outcomeCompleted:
		s.Completed++
	case outcomeSkipped:
		s.Skipped++
	case outcomeDuplicate:
		s.Duplicates++
	case outcomeBusy:
		s.Busy++
	case outcomeRetried:
		s.Retried++
	case outcomeDead:
		s.DeadLettered++
	case outcomeInterrupted:
		s.Interrupted++
	case outcomeUnrecorded:
		s.Unrecorded++
	case outcomeDeferred:
		s.Deferred++
	}
}

// Run consumes until ctx is cancelled. A failed receive is logged and
// tried again after a backoff.
func (c *Consumer) Run(ctx context.Context) {
	failures := 0
	for ctx.Err() == nil {
		stats, err := c.RunOnce(ctx)
		if err == nil {
			failures = 0
			if stats.Received > 0 {
				c.log.Info("events consumed", "queue", c.cfg.Name, "received", stats.Received,
					"completed", stats.Completed, "skipped", stats.Skipped, "duplicates", stats.Duplicates,
					"busy", stats.Busy, "retried", stats.Retried, "dead_lettered", stats.DeadLettered,
					"interrupted", stats.Interrupted, "unrecorded", stats.Unrecorded, "deferred", stats.Deferred)
			}
			continue
		}
		if ctx.Err() != nil {
			return
		}
		failures++
		delay := c.cfg.Retry.Delay(failures)
		c.log.Error("receive events failed", "queue", c.cfg.Name, "error", retry.Sanitize(err), "retry_in", delay)
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}

// RunOnce receives one batch and handles it, at most Concurrency groups at
// a time. A FIFO queue can return several events of one group (one
// aggregate) in a batch, so each group's events run one after another in
// the order received; different groups run side by side.
func (c *Consumer) RunOnce(ctx context.Context) (ConsumeStats, error) {
	messages, err := c.queue.Receive(ctx, c.cfg.Name, c.cfg.BatchSize, c.cfg.Visibility, c.cfg.Wait)
	if err != nil {
		return ConsumeStats{}, fmt.Errorf("receive from %s: %w", c.cfg.Name, err)
	}
	receivedAt := time.Now()
	stats := ConsumeStats{Received: len(messages)}
	c.metrics.Add("events_received", int64(len(messages)))

	var mu sync.Mutex
	var wg sync.WaitGroup
	slots := make(chan struct{}, c.cfg.Concurrency)
	for _, group := range groupsOf(messages) {
		slots <- struct{}{}
		wg.Add(1)
		go func(group []interfaces.QueueMessage) {
			defer wg.Done()
			defer func() { <-slots }()
			for i, msg := range group {
				outcome := c.handleInTurn(ctx, msg, receivedAt)
				mu.Lock()
				stats.add(outcome)
				mu.Unlock()
				if settled(outcome) {
					continue
				}
				// This event will come again, so the ones after it must wait
				// for it rather than overtake it. On a FIFO queue its group
				// is held while it is hidden, so handing these back now
				// cannot reorder them.
				handBack, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
				for _, later := range group[i+1:] {
					c.retryMessage(handBack, later, 0)
					mu.Lock()
					stats.add(outcomeDeferred)
					mu.Unlock()
				}
				cancel()
				return
			}
		}(group)
	}
	wg.Wait()
	return stats, nil
}

// handleInTurn handles a message that may have waited for a free slot or
// for the events before it in its group. Its visibility is renewed first
// if much of it has passed; if it has already run out the message may be
// with another consumer, so it is left alone.
func (c *Consumer) handleInTurn(ctx context.Context, msg interfaces.QueueMessage, receivedAt time.Time) string {
	if time.Since(receivedAt) > c.cfg.Visibility/3 && ctx.Err() == nil {
		err := c.queue.Extend(ctx, c.cfg.Name, msg.Receipt, c.cfg.Visibility)
		if errors.Is(err, interfaces.ErrReceiptExpired) {
			c.log.Warn("message visibility ran out while it waited; leaving it", "queue", c.cfg.Name, "message_id", msg.ID)
			return outcomeInterrupted
		}
		if err != nil {
			c.log.Warn("renew message visibility", "queue", c.cfg.Name, "message_id", msg.ID, "error", retry.Sanitize(err))
		}
	}
	return c.handle(ctx, msg)
}

// groupsOf splits a batch by message group, keeping the received order
// within each group. A message without a group is a group of its own.
func groupsOf(messages []interfaces.QueueMessage) [][]interfaces.QueueMessage {
	var groups [][]interfaces.QueueMessage
	index := map[string]int{}
	for _, msg := range messages {
		if msg.GroupKey != "" {
			if i, ok := index[msg.GroupKey]; ok {
				groups[i] = append(groups[i], msg)
				continue
			}
			index[msg.GroupKey] = len(groups)
		}
		groups = append(groups, []interfaces.QueueMessage{msg})
	}
	return groups
}

func (c *Consumer) handle(ctx context.Context, msg interfaces.QueueMessage) string {
	record, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()

	if ctx.Err() != nil {
		// Shutting down before it started: let another consumer have it now.
		c.retryMessage(record, msg, 0)
		return outcomeInterrupted
	}

	env, err := event.Decode(msg.Body)
	if err != nil {
		return c.reject(record, msg, env, err)
	}
	log := c.log.With("queue", c.cfg.Name, "event_id", env.ID, "event_type", env.Type, "tenant_id", env.TenantID,
		"aggregate", env.AggregateType+"/"+env.AggregateID, "correlation_id", env.CorrelationID,
		"receive_count", msg.ReceiveCount)
	handler, ok := c.handlers[env.Type]
	if !ok {
		return c.reject(record, msg, env, fmt.Errorf("%w: %s has no handler on queue %s", event.ErrUnknownType, env.Type, c.cfg.Name))
	}

	claim, rec, err := c.inbox.Claim(ctx, inboxRecordOf(c.cfg.Name, env), c.cfg.Owner, c.now(), c.cfg.Visibility)
	if err != nil && ctx.Err() != nil {
		// Cut short by shutdown: let another consumer have it now.
		c.retryMessage(record, msg, 0)
		return outcomeInterrupted
	}
	if err != nil {
		// Most likely the database is unavailable. The message comes back
		// after a backoff that grows with each delivery.
		delay := c.cfg.Retry.Delay(msg.ReceiveCount)
		c.retryMessage(record, msg, delay)
		log.Warn("could not claim event; will retry", "error", retry.Sanitize(err), "retry_in", delay)
		return outcomeRetried
	}
	switch claim {
	case model.ClaimDone, model.ClaimDead:
		c.metrics.Add("events_duplicates", 1)
		log.Info("duplicate delivery dropped", "inbox_status", rec.Status)
		c.ack(record, msg, log)
		return outcomeDuplicate
	case model.ClaimBusy:
		// Another worker holds it, or its retry is not due: look again
		// once that lease or wait is over.
		c.metrics.Add("events_concurrent_duplicates", 1)
		c.retryMessage(record, msg, c.busyDelay(rec))
		return outcomeBusy
	}

	if rec.Attempts > c.cfg.Retry.MaxAttempts {
		// The earlier attempts ended without an outcome (a crash, or a lost
		// lease) until the budget was spent.
		return c.giveUp(record, msg, env, rec, model.FailureExhausted, "no attempt recorded an outcome", log)
	}

	newer, err := c.inbox.NewerHandled(ctx, c.cfg.Name, env.AggregateType, env.AggregateID, env.AggregateSequence)
	if err != nil && ctx.Err() != nil {
		// Shutting down: write nothing, as below. The inbox lease and the
		// message's visibility run out and the event is delivered again.
		log.Info("event interrupted by shutdown")
		return outcomeInterrupted
	}
	if err != nil {
		return c.retryLater(record, msg, env, rec, fmt.Errorf("check event order: %w", err), log)
	}
	if newer > 0 {
		// Out of order: a later event of the same aggregate was handled, so
		// acting on this one could undo a newer decision.
		c.metrics.Add("events_stale", 1)
		return c.skip(record, msg, env, fmt.Sprintf("stale: event %d of this %s was already handled", newer, env.AggregateType), log)
	}

	started := time.Now()
	note, err := c.run(ctx, handler, env, msg, log)
	c.metrics.Add("events_handler_ms_total", time.Since(started).Milliseconds())
	if ctx.Err() != nil {
		// Shutting down: write nothing. The lease and the message's
		// visibility run out and the event is delivered again.
		log.Info("event interrupted by shutdown")
		return outcomeInterrupted
	}

	switch {
	case err == nil:
		if err := c.inbox.Complete(record, c.cfg.Name, env.ID, c.cfg.Owner, c.now(), note); err != nil {
			log.Error("record completed event", "error", retry.Sanitize(err))
			return outcomeUnrecorded
		}
		c.metrics.Add("events_completed", 1)
		log.Info("event handled", "outcome", note, "attempt", rec.Attempts, "duration_ms", time.Since(started).Milliseconds())
		c.ack(record, msg, log)
		return outcomeCompleted

	case errors.Is(err, model.ErrStaleEvent):
		c.metrics.Add("events_stale", 1)
		return c.skip(record, msg, env, retry.Sanitize(err), log)

	case retry.IsPermanent(err):
		return c.giveUp(record, msg, env, rec, model.FailurePermanent, retry.Sanitize(err), log)

	case c.cfg.Retry.Exhausted(rec.Attempts):
		return c.giveUp(record, msg, env, rec, model.FailureExhausted, retry.Sanitize(err), log)
	}
	return c.retryLater(record, msg, env, rec, err, log)
}

// run calls the handler while a heartbeat renews the inbox lease and the
// message's visibility. Losing the lease stops the handler.
func (c *Consumer) run(ctx context.Context, handler Handler, env event.Envelope, msg interfaces.QueueMessage, log *slog.Logger) (string, error) {
	work, cancel := context.WithTimeout(correlation.With(ctx, env.CorrelationID), c.cfg.HandlerTimeout)
	defer cancel()
	beat, stopBeat := context.WithCancel(work)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(c.cfg.Visibility / 3)
		defer ticker.Stop()
		for {
			select {
			case <-beat.Done():
				return
			case <-ticker.C:
			}
			err := c.inbox.Extend(beat, c.cfg.Name, env.ID, c.cfg.Owner, c.now(), c.cfg.Visibility)
			if errors.Is(err, model.ErrLeaseLost) {
				log.Warn("inbox lease lost; stopping the handler")
				cancel()
				return
			}
			if err != nil && beat.Err() == nil {
				log.Warn("renew inbox lease", "error", retry.Sanitize(err))
			}
			if err := c.queue.Extend(beat, c.cfg.Name, msg.Receipt, c.cfg.Visibility); err != nil && beat.Err() == nil {
				log.Warn("renew message visibility", "error", retry.Sanitize(err))
			}
		}
	}()

	note, err := handler(work, env)
	stopBeat()
	wg.Wait()
	return note, err
}

// retryLater records a failed attempt and has the queue deliver the
// message again after the backoff.
func (c *Consumer) retryLater(ctx context.Context, msg interfaces.QueueMessage, env event.Envelope, rec model.InboxRecord, cause error, log *slog.Logger) string {
	delay := c.cfg.Retry.Delay(rec.Attempts)
	reason := retry.Sanitize(cause)
	if err := c.inbox.Fail(ctx, c.cfg.Name, env.ID, c.cfg.Owner, c.now(), c.now().Add(delay), reason); err != nil {
		log.Error("record failed attempt", "error", retry.Sanitize(err))
		return outcomeUnrecorded
	}
	c.retryMessage(ctx, msg, delay)
	c.metrics.Add("events_retried", 1)
	log.Warn("event failed; will retry", "attempt", rec.Attempts, "max_attempts", c.cfg.Retry.MaxAttempts,
		"retry_in", delay, "error", reason)
	return outcomeRetried
}

func (c *Consumer) skip(ctx context.Context, msg interfaces.QueueMessage, env event.Envelope, reason string, log *slog.Logger) string {
	if err := c.inbox.Skip(ctx, c.cfg.Name, env.ID, c.cfg.Owner, c.now(), reason); err != nil {
		log.Error("record skipped event", "error", retry.Sanitize(err))
		return outcomeUnrecorded
	}
	c.metrics.Add("events_skipped", 1)
	log.Info("event skipped", "reason", reason)
	c.ack(ctx, msg, log)
	return outcomeSkipped
}

// giveUp dead-letters the event for this consumer, then acknowledges the
// message: the dead_letters table, not the queue, keeps it from now on.
func (c *Consumer) giveUp(ctx context.Context, msg interfaces.QueueMessage, env event.Envelope, rec model.InboxRecord, category, reason string, log *slog.Logger) string {
	now := c.now()
	dl := deadLetterOf(env, model.SourceConsumer, c.cfg.Name, category, reason, rec.Attempts, now)
	if rec.Attempts > 1 {
		dl.FirstFailedAt = rec.FirstSeenAt
	}
	if err := c.inbox.Dead(ctx, c.cfg.Name, env.ID, c.cfg.Owner, dl); err != nil {
		log.Error("record dead-lettered event", "error", retry.Sanitize(err))
		return outcomeUnrecorded
	}
	c.metrics.Add("events_dead_lettered", 1)
	log.Error("event dead-lettered", "category", category, "attempts", rec.Attempts, "error", reason)
	c.ack(ctx, msg, log)
	return outcomeDead
}

// reject dead-letters a message no handler can process: malformed, of an
// unknown type or of an unsupported schema version. Retrying cannot help,
// so the message is acknowledged once the dead letter is recorded. Only a
// digest of an unreadable message is kept, never its content.
func (c *Consumer) reject(ctx context.Context, msg interfaces.QueueMessage, env event.Envelope, cause error) string {
	category := failureCategory(cause)
	dl := deadLetterOf(env, model.SourceConsumer, c.cfg.Name, category, retry.Sanitize(cause), msg.ReceiveCount, c.now())
	if env.ID == "" {
		dl.EventID, dl.MessageDigest = unreadable(msg)
	}
	log := c.log.With("queue", c.cfg.Name, "event_id", dl.EventID, "event_type", env.Type, "category", category)
	if err := c.dead.Add(ctx, dl); err != nil {
		// Without the record the message must stay in the queue.
		c.retryMessage(ctx, msg, c.cfg.Retry.Delay(msg.ReceiveCount))
		log.Error("record rejected message", "error", retry.Sanitize(err))
		return outcomeRetried
	}
	c.metrics.Add("events_rejected", 1)
	log.Error("message rejected; dead-lettered", "error", retry.Sanitize(cause))
	c.ack(ctx, msg, log)
	return outcomeDead
}

// busyDelay is how long to leave a message that another worker holds, or
// whose retry is not due yet.
func (c *Consumer) busyDelay(rec model.InboxRecord) time.Duration {
	until := c.now().Add(c.cfg.Retry.BaseDelay)
	if rec.LeaseExpiresAt != nil && rec.LeaseExpiresAt.After(until) {
		until = *rec.LeaseExpiresAt
	}
	if rec.NextAttemptAt != nil && rec.NextAttemptAt.After(until) {
		until = *rec.NextAttemptAt
	}
	return min(until.Sub(c.now())+time.Second, c.cfg.Retry.MaxDelay)
}

func (c *Consumer) ack(ctx context.Context, msg interfaces.QueueMessage, log *slog.Logger) {
	if err := c.queue.Ack(ctx, c.cfg.Name, msg.Receipt); err != nil {
		// The outcome is recorded: a redelivery finds it and is dropped.
		c.metrics.Add("events_ack_failures", 1)
		log.Warn("acknowledge message", "error", retry.Sanitize(err))
	}
}

func (c *Consumer) retryMessage(ctx context.Context, msg interfaces.QueueMessage, delay time.Duration) {
	if err := c.queue.Retry(ctx, c.cfg.Name, msg.Receipt, delay); err != nil {
		// The visibility timeout brings it back anyway.
		c.log.Warn("delay message", "queue", c.cfg.Name, "message_id", msg.ID, "error", retry.Sanitize(err))
	}
}

// failureCategory names why a message could not be decoded.
func failureCategory(err error) string {
	switch {
	case errors.Is(err, event.ErrUnknownType):
		return model.FailureUnknownType
	case errors.Is(err, event.ErrUnsupportedVersion):
		return model.FailureUnsupportedVersion
	}
	return model.FailureMalformed
}

// unreadable identifies a message whose envelope has no event ID: by its
// queue message ID, and by a digest of its body instead of the body.
func unreadable(msg interfaces.QueueMessage) (eventID, digest string) {
	sum := sha256.Sum256(msg.Body)
	return "message:" + msg.ID, hex.EncodeToString(sum[:])
}

func inboxRecordOf(consumer string, env event.Envelope) model.InboxRecord {
	return model.InboxRecord{
		Consumer: consumer, EventID: env.ID, EventType: env.Type, SchemaVersion: env.SchemaVersion,
		TenantID: env.TenantID, AggregateType: env.AggregateType, AggregateID: env.AggregateID,
		AggregateSequence: env.AggregateSequence, CorrelationID: env.CorrelationID,
	}
}
