package concurrency

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestLimiterNeverRunsMoreThanItsSlots(t *testing.T) {
	l := NewLimiter(3)
	var running, peak atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = l.Do(context.Background(), func(context.Context) error {
				n := running.Add(1)
				for {
					p := peak.Load()
					if n <= p || peak.CompareAndSwap(p, n) {
						break
					}
				}
				time.Sleep(time.Millisecond)
				running.Add(-1)
				return nil
			})
		}()
	}
	wg.Wait()
	if p := peak.Load(); p != 3 {
		t.Fatalf("expected at most 3 at once and reaching 3, peak %d", p)
	}
}

func TestLimiterGivesUpWhenTheContextEnds(t *testing.T) {
	l := NewLimiter(1)
	hold := make(chan struct{})
	go func() { _ = l.Do(context.Background(), func(context.Context) error { <-hold; return nil }) }()
	time.Sleep(10 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := l.Do(ctx, func(context.Context) error { t.Fatal("must not run"); return nil })
	close(hold)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected the deadline, got %v", err)
	}
}
