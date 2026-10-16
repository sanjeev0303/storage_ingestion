package processing

import (
	"context"
	"log/slog"
	"time"

	processingservice "github.com/sanjeev0303/object-storage-platform/internal/service/processing"
)

// Run repeats processing cycles every interval until ctx is cancelled. A
// failed cycle is logged and retried on the next tick.
func Run(
	ctx context.Context,
	service *processingservice.Service,
	interval time.Duration,
	log *slog.Logger,
) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		stats, err := service.RunOnce(ctx)
		if err != nil && ctx.Err() == nil {
			log.Error("processing cycle failed", "error", err)
		}
		if stats.Claimed > 0 || stats.Enqueued > 0 {
			log.Info("processing cycle",
				"enqueued", stats.Enqueued,
				"claimed", stats.Claimed,
				"promoted", stats.Succeeded,
				"rejected_or_cancelled", stats.Failed,
				"retried", stats.Retried,
				"exhausted", stats.Exhausted,
				"lease_lost", stats.LeaseLost,
			)
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
