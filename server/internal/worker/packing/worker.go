// Package packing runs the pack worker loop: plan packs, build the due
// ones, and retire what packing no longer needs.
package packing

import (
	"context"
	"log/slog"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/platform/retry"
	packingservice "github.com/sanjeev0303/object-storage-platform/internal/service/packing"
)

// Run repeats a packing cycle every interval until ctx ends. A cycle that
// built a pack runs again at once, so a backlog drains without waiting.
func Run(ctx context.Context, svc *packingservice.Service, interval time.Duration, log *slog.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		stats, err := svc.RunOnce(ctx)
		if err != nil && ctx.Err() == nil {
			log.Error("packing cycle failed", "error", retry.Sanitize(err))
		}
		if stats != (packingservice.CycleStats{}) {
			log.Info("packing cycle", "planned", stats.Planned, "built", stats.Built, "published", stats.Published,
				"failed", stats.Failed, "retried", stats.Retried, "sources_retired", stats.SourcesRetired,
				"packs_retired", stats.PacksRetired, "rewrites", stats.Rewrites, "packs_removed", stats.PacksRemoved,
				"orphans_reported", stats.OrphansReported)
			if ctx.Err() == nil && stats.Built > 0 {
				continue
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
