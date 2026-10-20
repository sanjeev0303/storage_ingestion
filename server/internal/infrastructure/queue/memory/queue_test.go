package memory

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
)

type clock struct{ now time.Time }

func (c *clock) Now() time.Time          { return c.now }
func (c *clock) advance(d time.Duration) { c.now = c.now.Add(d) }
func newQueue(c *clock) *Queue           { q := New(); q.Now = c.Now; return q }
func msg(body, group, dedup string) interfaces.OutgoingMessage {
	return interfaces.OutgoingMessage{Body: []byte(body), GroupKey: group, DedupKey: dedup}
}

func TestGroupsAreDeliveredInOrderAndNotWhileOneIsInFlight(t *testing.T) {
	c := &clock{now: time.Now()}
	q := newQueue(c)
	ctx := context.Background()
	for _, body := range []string{"a1", "a2"} {
		_, _ = q.Publish(ctx, "q", msg(body, "a", body))
	}
	_, _ = q.Publish(ctx, "q", msg("b1", "b", "b1"))

	// Like SQS FIFO, one receive returns consecutive messages of a group
	// together, in order, with the group each belongs to.
	got, _ := q.Receive(ctx, "q", 10, time.Minute, 0)
	if len(got) != 3 || string(got[0].Body) != "a1" || string(got[1].Body) != "a2" || string(got[2].Body) != "b1" {
		t.Fatalf("expected a1, a2, b1 in order, got %d messages", len(got))
	}
	if got[0].GroupKey != "a" || got[2].GroupKey != "b" {
		t.Fatalf("expected each message's group, got %q and %q", got[0].GroupKey, got[2].GroupKey)
	}

	// a1 goes back for a retry: while it is hidden its group delivers nothing,
	// so a2, also handed back, cannot overtake it.
	_ = q.Retry(ctx, "q", got[0].Receipt, 30*time.Second)
	_ = q.Retry(ctx, "q", got[1].Receipt, 0)
	if again, _ := q.Receive(ctx, "q", 10, time.Minute, 0); len(again) != 0 {
		t.Fatalf("a group with a message in flight must wait, got %d", len(again))
	}
	c.advance(31 * time.Second)
	next, _ := q.Receive(ctx, "q", 1, time.Minute, 0)
	if len(next) != 1 || string(next[0].Body) != "a1" {
		t.Fatal("once visible again the group starts from a1")
	}
}

func TestUngroupedMessagesDoNotWaitForEachOther(t *testing.T) {
	c := &clock{now: time.Now()}
	q := newQueue(c)
	ctx := context.Background()
	_, _ = q.Publish(ctx, "q", msg("x", "", "x"))
	_, _ = q.Publish(ctx, "q", msg("y", "", "y"))
	if first, _ := q.Receive(ctx, "q", 1, time.Minute, 0); len(first) != 1 {
		t.Fatal("expected the first message")
	}
	if second, _ := q.Receive(ctx, "q", 1, time.Minute, 0); len(second) != 1 || string(second[0].Body) != "y" {
		t.Fatal("a message without a group must not wait for another one in flight")
	}
}

func TestRetryDelaysAndRedriveMovesToTheDeadLetters(t *testing.T) {
	c := &clock{now: time.Now()}
	q := newQueue(c)
	q.MaxReceives = 2
	ctx := context.Background()
	_, _ = q.Publish(ctx, "q", msg("x", "g", "x"))

	first, _ := q.Receive(ctx, "q", 1, time.Minute, 0)
	if err := q.Retry(ctx, "q", first[0].Receipt, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if got, _ := q.Receive(ctx, "q", 1, time.Minute, 0); len(got) != 0 {
		t.Fatal("a delayed message must stay hidden")
	}
	c.advance(31 * time.Second)
	second, _ := q.Receive(ctx, "q", 1, time.Minute, 0)
	if len(second) != 1 || second[0].ReceiveCount != 2 {
		t.Fatalf("expected the second delivery, got %+v", second)
	}
	if err := q.Ack(ctx, "q", first[0].Receipt); !errors.Is(err, interfaces.ErrReceiptExpired) {
		t.Fatal("an old receipt must not acknowledge the new delivery")
	}

	c.advance(2 * time.Minute) // visibility runs out without an ack
	if got, _ := q.Receive(ctx, "q", 1, time.Minute, 0); len(got) != 0 {
		t.Fatal("after MaxReceives the message moves to the dead letters")
	}
	dead, _ := q.ReceiveDeadLetters(ctx, "q", 10, time.Minute)
	if len(dead) != 1 || q.AckDeadLetter(ctx, "q", dead[0].Receipt) != nil {
		t.Fatalf("expected one dead letter, got %d", len(dead))
	}
	if stats, _ := q.Stats(ctx, "q"); stats != (interfaces.QueueStats{}) {
		t.Fatalf("queue should be empty, got %+v", stats)
	}
}

func TestDedupWindowDropsRepeats(t *testing.T) {
	c := &clock{now: time.Now()}
	q := newQueue(c)
	ctx := context.Background()
	_, _ = q.Publish(ctx, "q", msg("x", "g", "same"))
	_, _ = q.Publish(ctx, "q", msg("x", "g", "same"))
	if stats, _ := q.Stats(ctx, "q"); stats.Waiting != 1 {
		t.Fatalf("a repeat within the window must be dropped, got %+v", stats)
	}
	c.advance(6 * time.Minute)
	_, _ = q.Publish(ctx, "q", msg("x", "g", "same"))
	if stats, _ := q.Stats(ctx, "q"); stats.Waiting != 2 {
		t.Fatalf("after the window the message is accepted again, got %+v", stats)
	}
}

func TestFailInjectsErrors(t *testing.T) {
	q := New()
	q.Fail = func(op, queue string) error {
		if op == "publish" {
			return interfaces.ErrQueueThrottled
		}
		return nil
	}
	if _, err := q.Publish(context.Background(), "q", msg("x", "g", "x")); !errors.Is(err, interfaces.ErrQueueThrottled) {
		t.Fatalf("expected the injected error, got %v", err)
	}
}
