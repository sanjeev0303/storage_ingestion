package events

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/dto/event"
	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/queue/memory"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/retry"
)

// acceptingInbox claims every event as new and records completions; the
// consumer calls nothing else on the success path.
type acceptingInbox struct {
	interfaces.InboxRepository
	mu       sync.Mutex
	done     []string
	claimed  []string // the owner of each claim
	finished []string // the owner each completion was written as
	released []string // the owner each shutdown release was written as
}

func (i *acceptingInbox) Claim(_ context.Context, rec model.InboxRecord, owner string, _ time.Time, _ time.Duration) (string, model.InboxRecord, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.claimed = append(i.claimed, owner)
	rec.Attempts = 1
	return model.ClaimAcquired, rec, nil
}

func (i *acceptingInbox) Release(_ context.Context, _, _, owner string, _ time.Time) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.released = append(i.released, owner)
	return nil
}

func (i *acceptingInbox) NewerHandled(context.Context, string, string, string, int64) (int64, error) {
	return 0, nil
}

func (i *acceptingInbox) Extend(context.Context, string, string, string, time.Time, time.Duration) error {
	return nil
}

func (i *acceptingInbox) Complete(_ context.Context, _, eventID, owner string, _ time.Time, _ string) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.done = append(i.done, eventID)
	i.finished = append(i.finished, owner)
	return nil
}

func publishImport(t *testing.T, q *memory.Queue, id string) {
	t.Helper()
	e := event.NewImportQueued("tenant", id, time.Now(), "")
	e.AggregateSequence = 1 // the outbox repository assigns it on insert
	body, err := json.Marshal(event.FromOutbox(e))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.Publish(context.Background(), "test", interfaces.OutgoingMessage{Body: body, GroupKey: "import:" + id, DedupKey: id}); err != nil {
		t.Fatal(err)
	}
}

// recordingInbox notes whether the outcome could still be written.
type recordingInbox struct {
	acceptingInbox
	expired bool
}

func (i *recordingInbox) Complete(ctx context.Context, consumer, eventID, owner string, now time.Time, note string) error {
	i.mu.Lock()
	i.expired = ctx.Err() != nil
	i.mu.Unlock()
	return i.acceptingInbox.Complete(ctx, consumer, eventID, owner, now, note)
}

// A scan can take far longer than recording its outcome may. The event's success
// must still be recorded: losing it runs the whole event again.
func TestASlowHandlerStillRecordsItsOutcome(t *testing.T) {
	old := recordTimeout
	recordTimeout = 50 * time.Millisecond
	t.Cleanup(func() { recordTimeout = old })
	q := memory.New()
	publishImport(t, q, "slow")
	inbox := &recordingInbox{}
	c, err := NewConsumer(q, inbox, struct {
		interfaces.DeadLetterRepository
	}{}, map[string]Handler{event.TypeImportQueued: func(context.Context, event.Envelope) (string, error) {
		time.Sleep(4 * recordTimeout)
		return "done", nil
	}}, ConsumerConfig{Name: "test", Owner: "test", BatchSize: 10, Concurrency: 2, Visibility: 30 * time.Second, Wait: 50 * time.Millisecond, HandlerTimeout: 10 * time.Second,
		Retry: retry.Policy{BaseDelay: time.Second, MaxDelay: time.Second, MaxAttempts: 3}}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err != nil {
		t.Fatal(err)
	}
	stats, err := c.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	inbox.mu.Lock()
	defer inbox.mu.Unlock()
	if len(inbox.done) != 1 || inbox.expired || stats.Unrecorded != 0 {
		t.Fatalf("recorded %v, expired %v, stats %+v", inbox.done, inbox.expired, stats)
	}
}

func newTestConsumer(t *testing.T, q *memory.Queue, inbox *acceptingInbox, handler Handler) *Consumer {
	t.Helper()
	c, err := NewConsumer(q, inbox, struct {
		interfaces.DeadLetterRepository
	}{}, map[string]Handler{event.TypeImportQueued: handler},
		ConsumerConfig{
			Name: "test", Owner: "test", BatchSize: 10, Concurrency: 2,
			Visibility: 30 * time.Second, Wait: 50 * time.Millisecond, HandlerTimeout: 10 * time.Second,
			Retry: retry.Policy{BaseDelay: time.Second, MaxDelay: time.Second, MaxAttempts: 3},
		}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// A slow event must not hold back one that arrives after it while a slot is
// free: the old receive-then-wait loop made the second wait for the first.
func TestRunStartsNewEventsWhileASlowOneRuns(t *testing.T) {
	q := memory.New()
	inbox := &acceptingInbox{}
	release := make(chan struct{})
	var mu sync.Mutex
	var order []string
	c := newTestConsumer(t, q, inbox, func(ctx context.Context, env event.Envelope) (string, error) {
		var p event.ImportQueued
		if err := event.DecodePayload(env, &p); err != nil {
			return "", err
		}
		if p.ImportID == "slow" {
			select {
			case <-release:
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
		mu.Lock()
		order = append(order, p.ImportID)
		mu.Unlock()
		return "done", nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		c.Run(ctx)
		close(stopped)
	}()

	publishImport(t, q, "slow")
	time.Sleep(100 * time.Millisecond) // the slow one is now running
	publishImport(t, q, "fast")

	deadline := time.After(3 * time.Second)
	for {
		mu.Lock()
		got := append([]string(nil), order...)
		mu.Unlock()
		if len(got) == 1 {
			if got[0] != "fast" {
				t.Fatalf("finished first: %v, want fast", got)
			}
			break
		}
		select {
		case <-deadline:
			t.Fatal("the fast event waited for the slow one")
		case <-time.After(10 * time.Millisecond):
		}
	}

	close(release)
	time.Sleep(100 * time.Millisecond)
	cancel()
	<-stopped
	if len(inbox.done) != 2 {
		t.Fatalf("completed %v, want both", inbox.done)
	}
}

// Run returns only after the handlers it started have returned, and leaves
// no goroutine behind. (Events cut short by shutdown record nothing and are
// delivered again, by design.)
func TestRunWaitsForRunningEventsOnShutdownAndLeaksNothing(t *testing.T) {
	before := runtime.NumGoroutine()
	q := memory.New()
	inbox := &acceptingInbox{}
	started := make(chan struct{}, 4)
	var returned atomic.Int32
	c := newTestConsumer(t, q, inbox, func(ctx context.Context, env event.Envelope) (string, error) {
		started <- struct{}{}
		time.Sleep(150 * time.Millisecond)
		returned.Add(1)
		return "done", nil
	})
	for _, id := range []string{"a", "b"} {
		publishImport(t, q, id)
	}

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		c.Run(ctx)
		close(stopped)
	}()
	<-started
	<-started
	cancel()
	<-stopped

	if n := returned.Load(); n != 2 {
		t.Fatalf("Run returned while handlers were still running: %d of 2 had returned", n)
	}
	// Both were handed back under the claim they were taken with, and
	// neither was recorded as done.
	inbox.mu.Lock()
	released, claimed, done := append([]string(nil), inbox.released...), append([]string(nil), inbox.claimed...), len(inbox.done)
	inbox.mu.Unlock()
	slices.Sort(released)
	slices.Sort(claimed)
	if done != 0 || len(released) != 2 || !slices.Equal(released, claimed) {
		t.Fatalf("shutdown: released %v, claimed %v, completed %d", released, claimed, done)
	}
	time.Sleep(50 * time.Millisecond)
	if after := runtime.NumGoroutine(); after > before {
		t.Fatalf("goroutines: %d before, %d after Run returned", before, after)
	}
}

// Each claim is made under an owner of its own, and the outcome is written
// under that same owner: an earlier attempt of this worker that lost its
// lease cannot write over a later one.
func TestEveryClaimHasItsOwnOwner(t *testing.T) {
	q := memory.New()
	inbox := &acceptingInbox{}
	c := newTestConsumer(t, q, inbox, func(context.Context, event.Envelope) (string, error) { return "done", nil })
	for _, id := range []string{"a", "b", "c"} {
		publishImport(t, q, id)
	}
	if _, err := c.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, owner := range inbox.claimed {
		if !strings.HasPrefix(owner, "test#") || seen[owner] {
			t.Fatalf("claim owners %v", inbox.claimed)
		}
		seen[owner] = true
	}
	finished := append([]string(nil), inbox.finished...)
	claimed := append([]string(nil), inbox.claimed...)
	slices.Sort(finished)
	slices.Sort(claimed)
	if len(claimed) != 3 || !slices.Equal(finished, claimed) {
		t.Fatalf("completed as %v, claimed as %v", finished, claimed)
	}
}
