package reconciliation

import (
	"context"
	"log/slog"
	"time"

	reconciliationservice "github.com/sanjeev0303/object-storage-platform/internal/service/reconciliation"
)

// Run repeats reconciliation cycles every interval until ctx is cancelled.
// A failed cycle is logged and retried on the next tick; it never stops
// the worker.
func Run(
	ctx context.Context,
	service *reconciliationservice.Service,
	interval time.Duration,
	log *slog.Logger,
) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		stats, err := service.RunOnce(ctx)
		if err != nil && ctx.Err() == nil {
			log.Error("reconciliation cycle failed", "error", err)
		}
		if stats.Claimed > 0 || stats.Enqueued > 0 || len(stats.Findings) > 0 || len(stats.Due) > 0 {
			log.Info("reconciliation cycle",
				"dry_run", stats.DryRun,
				"enqueued", stats.Enqueued,
				"claimed", stats.Claimed,
				"succeeded", stats.Succeeded,
				"retried", stats.Retried,
				"exhausted", stats.Exhausted,
				"lease_lost", stats.LeaseLost,
				"due", len(stats.Due),
				"findings", len(stats.Findings),
				"duration_ms", stats.Duration.Milliseconds(),
			)
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
