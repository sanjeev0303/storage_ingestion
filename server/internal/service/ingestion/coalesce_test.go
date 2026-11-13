package ingestion

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// fakeBatcher answers each change with its own position, after a pause long
// enough for other callers to queue behind it.
type fakeBatcher struct {
	mu    sync.Mutex
	sizes []int
	fail  bool
	gate  chan struct{} // when set, each batch waits for it
}

func (f *fakeBatcher) TransitionMany(_ context.Context, changes []interfaces.EntryChange) ([]interfaces.EntryOutcome, error) {
	if f.gate != nil {
		<-f.gate
	} else {
		time.Sleep(2 * time.Millisecond)
	}
	f.mu.Lock()
	f.sizes = append(f.sizes, len(changes))
	f.mu.Unlock()
	if f.fail {
		return nil, errors.New("batch failed")
	}
	out := make([]interfaces.EntryOutcome, len(changes))
	for i, ch := range changes {
		out[i].Entry = model.IngestionEntry{JobID: ch.JobID, Position: ch.Position}
	}
	return out, nil
}

func countingSingle(n *atomic.Int64) func(context.Context, interfaces.EntryChange) (model.IngestionEntry, error) {
	return func(_ context.Context, ch interfaces.EntryChange) (model.IngestionEntry, error) {
		n.Add(1)
		return model.IngestionEntry{JobID: ch.JobID, Position: ch.Position}, nil
	}
}

func applyAll(t *testing.T, c *coalescer, b *fakeBatcher, single func(context.Context, interfaces.EntryChange) (model.IngestionEntry, error), n int) {
	t.Helper()
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got, err := c.apply(context.Background(), interfaces.EntryChange{JobID: "job", Position: int64(i)}, b, single, func(int) {})
			if err != nil || got.Position != int64(i) {
				t.Errorf("change %d came back as %d (%v)", i, got.Position, err)
			}
		}(i)
	}
	wg.Wait()
}

func TestCoalescerHandsEveryCallerItsOwnResultAndBatches(t *testing.T) {
	var c coalescer
	b := &fakeBatcher{}
	var singles atomic.Int64
	applyAll(t, &c, b, countingSingle(&singles), 300)

	total, largest := int(singles.Load()), 0
	for _, n := range b.sizes {
		total += n
		largest = max(largest, n)
	}
	if total != 300 {
		t.Fatalf("%d changes written, want 300", total)
	}
	if largest < 2 || largest > maxGroupCommit {
		t.Fatalf("largest batch %d: changes did not share a transaction, or one was too big", largest)
	}
	if c.running || len(c.pending) != 0 {
		t.Fatalf("left running=%v with %d pending", c.running, len(c.pending))
	}
}

func TestCoalescerFallsBackToOneAtATimeWhenABatchFails(t *testing.T) {
	var c coalescer
	b := &fakeBatcher{fail: true}
	var singles atomic.Int64
	applyAll(t, &c, b, countingSingle(&singles), 50)
	batched := 0
	for _, n := range b.sizes {
		batched += n
	}
	// Every change is written exactly once on its own, whether or not it
	// was first offered in a batch that failed.
	if singles.Load() != 50 {
		t.Fatalf("%d changes written one at a time, want 50 (offered in failed batches: %d)", singles.Load(), batched)
	}
}

func TestCoalescerCallerThatGivesUpDoesNotStallTheOthers(t *testing.T) {
	var c coalescer
	b := &fakeBatcher{}
	var singles atomic.Int64
	gate := make(chan struct{})
	var gated atomic.Bool
	// The first change is written on its own (a batch of one) and waits for
	// the gate, so it holds the lead while the next caller queues.
	single := func(ctx context.Context, ch interfaces.EntryChange) (model.IngestionEntry, error) {
		if gated.CompareAndSwap(false, true) {
			<-gate
		}
		return countingSingle(&singles)(ctx, ch)
	}

	first := make(chan error, 1)
	go func() {
		_, err := c.apply(context.Background(), interfaces.EntryChange{JobID: "job", Position: 1}, b, single, func(int) {})
		first <- err
	}()
	waitFor(t, func() bool { c.mu.Lock(); defer c.mu.Unlock(); return c.running && len(c.pending) == 0 && gated.Load() })

	// A caller queued behind it gives up, and gets its context's error.
	ctx, cancel := context.WithCancel(context.Background())
	gaveUp := make(chan error, 1)
	go func() {
		_, err := c.apply(ctx, interfaces.EntryChange{JobID: "job", Position: 2}, b, single, func(int) {})
		gaveUp <- err
	}()
	waitFor(t, func() bool { c.mu.Lock(); defer c.mu.Unlock(); return len(c.pending) == 1 })
	cancel()
	if err := <-gaveUp; !errors.Is(err, context.Canceled) {
		t.Fatalf("the caller that gave up: %v", err)
	}

	// The leader finishes, and the coalescer is free again: the change that
	// was taken back is not written, and nothing waits for a missing leader.
	close(gate)
	if err := <-first; err != nil {
		t.Fatalf("leader: %v", err)
	}
	if n := singles.Load(); n != 1 {
		t.Fatalf("%d changes written, want only the leader's", n)
	}
	applyAll(t, &c, b, single, 20)
	if c.running || len(c.pending) != 0 {
		t.Fatalf("left running=%v with %d pending", c.running, len(c.pending))
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting")
		}
		time.Sleep(time.Millisecond)
	}
}
