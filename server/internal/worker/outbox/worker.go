// Package outbox runs the outbox worker: the publisher, and event
// reconciliation on its own interval.
package outbox

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/service/events"
)

// Run publishes and reconciles until ctx is cancelled. The two run side by
// side, so a slow reconciliation pass (a queue call waiting for its
// timeout, say) never holds up publishing. A signal on wake (an event was
// just committed) starts publishing at once; nil leaves only the interval.
func Run(ctx context.Context, publisher *events.Publisher, reconciler *events.Reconciler, interval, reconcileEvery time.Duration, wake <-chan struct{}, log *slog.Logger) {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		reconcile(ctx, reconciler, reconcileEvery, log)
	}()
	publish(ctx, publisher, interval, wake, log)
	wg.Wait()
}

// publish sends batch after batch while events are due, then pauses until
// woken or for interval. A failed batch is logged and tried again on the
// next tick.
func publish(ctx context.Context, publisher *events.Publisher, interval time.Duration, wake <-chan struct{}, log *slog.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		for ctx.Err() == nil {
			stats, err := publisher.RunOnce(ctx)
			if err != nil {
				if ctx.Err() == nil {
					log.Error("outbox batch failed", "error", err)
				}
				break
			}
			if stats.Claimed > 0 {
				log.Info("outbox batch", "claimed", stats.Claimed, "published", stats.Published,
					"retried", stats.Retried, "dead_lettered", stats.DeadLettered,
					"released", stats.Released, "unrecorded", stats.Unrecorded)
			}
			// Nothing due, or the queue is throttling: wait for the tick.
			if stats.Claimed == 0 || stats.Released > 0 {
				break
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-wake:
		}
	}
}

// reconcile runs a reconciliation pass every interval. A failed pass is
// logged and tried again on the next tick.
func reconcile(ctx context.Context, reconciler *events.Reconciler, interval time.Duration, log *slog.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		report, err := reconciler.RunOnce(ctx)
		if err != nil && ctx.Err() == nil {
			log.Error("event reconciliation failed", "error", err)
		}
		if ctx.Err() == nil {
			log.Info("event reconciliation", "findings", len(report.Findings),
				"outbox_pending", report.Outbox.Pending, "outbox_dead", report.Outbox.Dead,
				"dead_letters_open", report.DeadLetters.Open)
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
