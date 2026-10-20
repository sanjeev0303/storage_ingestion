// Package memory is an in-process interfaces.MessageQueue for tests. It
// behaves like the SQS FIFO setup: messages of one group are delivered in
// order, and like SQS one receive can return several consecutive messages
// of a group, but none while an earlier one of the group is in flight. A
// repeated dedup key within the window is dropped,
// and a message received more than MaxReceives times moves to the
// queue's dead-letter list. Fail injects errors. It keeps nothing across
// restarts and is not meant for a deployment.
package memory

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
)

type message struct {
	id        string
	receipt   string
	body      []byte
	group     string
	sentAt    time.Time
	visibleAt time.Time
	receives  int
	inFlight  bool
}

type queue struct {
	messages []*message // publication order
	dead     []*message
	dedup    map[string]time.Time
}

// Queue is safe for concurrent use.
type Queue struct {
	mu     sync.Mutex
	queues map[string]*queue
	nextID int

	// MaxReceives is how often a message is delivered before it moves to
	// the dead-letter list.
	MaxReceives int
	// DedupWindow drops a message whose dedup key was seen this recently;
	// zero turns deduplication off.
	DedupWindow time.Duration
	// Now is the clock; tests may replace it.
	Now func() time.Time
	// Fail, when set, runs before every operation ("publish", "receive",
	// "ack", "retry", "extend") and fails it with the returned error.
	Fail func(operation, queue string) error
}

var _ interfaces.MessageQueue = (*Queue)(nil)

func New() *Queue {
	return &Queue{
		queues:      make(map[string]*queue),
		MaxReceives: 5,
		DedupWindow: 5 * time.Minute,
		Now:         time.Now,
	}
}

func (q *Queue) Publish(ctx context.Context, name string, msg interfaces.OutgoingMessage) (string, error) {
	if err := q.check(ctx, "publish", name); err != nil {
		return "", err
	}
	q.mu.Lock()
	defer q.mu.Unlock()

	now := q.Now()
	target := q.queue(name)
	if q.DedupWindow > 0 && msg.DedupKey != "" {
		if seen, ok := target.dedup[msg.DedupKey]; ok && now.Sub(seen) < q.DedupWindow {
			return "duplicate-" + msg.DedupKey, nil // accepted, not delivered again
		}
		target.dedup[msg.DedupKey] = now
	}

	q.nextID++
	m := &message{
		id: fmt.Sprintf("msg-%d", q.nextID), body: slices.Clone(msg.Body),
		group: msg.GroupKey, sentAt: now, visibleAt: now,
	}
	target.messages = append(target.messages, m)
	return m.id, nil
}

func (q *Queue) Receive(ctx context.Context, name string, limit int, visibility, wait time.Duration) ([]interfaces.QueueMessage, error) {
	if err := q.check(ctx, "receive", name); err != nil {
		return nil, err
	}
	// Long polling: look again every few milliseconds until wait ends.
	deadline := time.Now().Add(wait)
	for {
		got := q.take(name, limit, visibility)
		if len(got) > 0 || !time.Now().Before(deadline) {
			return got, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// take delivers up to limit visible messages in order. Like SQS FIFO it
// returns consecutive messages of one group together, but nothing from a
// group with a message already in flight, and nothing after a message of
// the group that is not visible yet.
func (q *Queue) take(name string, limit int, visibility time.Duration) []interfaces.QueueMessage {
	q.mu.Lock()
	defer q.mu.Unlock()

	now := q.Now()
	target := q.queue(name)
	busy := map[string]bool{}
	for _, m := range target.messages {
		if m.group != "" && m.inFlight && m.visibleAt.After(now) {
			busy[m.group] = true
		}
	}

	var out []interfaces.QueueMessage
	var kept []*message
	for _, m := range target.messages {
		if len(out) >= limit || m.visibleAt.After(now) || busy[m.group] {
			kept = append(kept, m)
			if m.group != "" {
				busy[m.group] = true // later messages of the group wait for this one
			}
			continue
		}
		if m.receives >= q.MaxReceives {
			target.dead = append(target.dead, m) // the queue's own redrive
			continue
		}

		// Taken in this receive: the group stays open for its next message.
		q.nextID++
		m.receives++
		m.inFlight = true
		m.visibleAt = now.Add(visibility)
		m.receipt = fmt.Sprintf("receipt-%d", q.nextID)
		kept = append(kept, m)
		out = append(out, interfaces.QueueMessage{
			ID: m.id, Body: slices.Clone(m.body), Receipt: m.receipt, ReceiveCount: m.receives, SentAt: m.sentAt,
			GroupKey: m.group,
		})
	}
	target.messages = kept
	return out
}

func (q *Queue) Ack(ctx context.Context, name, receipt string) error {
	if err := q.check(ctx, "ack", name); err != nil {
		return err
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	target := q.queue(name)
	for i, m := range target.messages {
		if m.receipt == receipt {
			target.messages = slices.Delete(target.messages, i, i+1)
			return nil
		}
	}
	return interfaces.ErrReceiptExpired
}

func (q *Queue) Retry(ctx context.Context, name, receipt string, delay time.Duration) error {
	return q.setVisibility(ctx, "retry", name, receipt, delay)
}

func (q *Queue) Extend(ctx context.Context, name, receipt string, visibility time.Duration) error {
	return q.setVisibility(ctx, "extend", name, receipt, visibility)
}

func (q *Queue) setVisibility(ctx context.Context, operation, name, receipt string, d time.Duration) error {
	if err := q.check(ctx, operation, name); err != nil {
		return err
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, m := range q.queue(name).messages {
		if m.receipt == receipt && m.inFlight {
			m.visibleAt = q.Now().Add(d)
			return nil
		}
	}
	return interfaces.ErrReceiptExpired
}

func (q *Queue) ReceiveDeadLetters(ctx context.Context, name string, limit int, visibility time.Duration) ([]interfaces.QueueMessage, error) {
	if err := q.check(ctx, "receive", name); err != nil {
		return nil, err
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	var out []interfaces.QueueMessage
	for _, m := range q.queue(name).dead {
		if len(out) >= limit {
			break
		}
		q.nextID++
		m.receipt = fmt.Sprintf("receipt-%d", q.nextID)
		out = append(out, interfaces.QueueMessage{
			ID: m.id, Body: slices.Clone(m.body), Receipt: m.receipt, ReceiveCount: m.receives, SentAt: m.sentAt,
		})
	}
	return out, nil
}

func (q *Queue) AckDeadLetter(ctx context.Context, name, receipt string) error {
	if err := q.check(ctx, "ack", name); err != nil {
		return err
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	target := q.queue(name)
	for i, m := range target.dead {
		if m.receipt == receipt {
			target.dead = slices.Delete(target.dead, i, i+1)
			return nil
		}
	}
	return interfaces.ErrReceiptExpired
}

func (q *Queue) Stats(ctx context.Context, name string) (interfaces.QueueStats, error) {
	if err := ctx.Err(); err != nil {
		return interfaces.QueueStats{}, err
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	now := q.Now()
	target := q.queue(name)
	stats := interfaces.QueueStats{DeadLetters: len(target.dead)}
	for _, m := range target.messages {
		if m.visibleAt.After(now) {
			stats.InFlight++
		} else {
			stats.Waiting++
		}
	}
	return stats, nil
}

func (q *Queue) check(ctx context.Context, operation, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if q.Fail != nil {
		return q.Fail(operation, name)
	}
	return nil
}

func (q *Queue) queue(name string) *queue {
	target, ok := q.queues[name]
	if !ok {
		target = &queue{dedup: make(map[string]time.Time)}
		q.queues[name] = target
	}
	return target
}
