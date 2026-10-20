// Package compaction runs metadata compaction on a schedule.
package compaction

import (
	"context"
	"log/slog"
	"time"

	compactionservice "github.com/sanjeev0303/object-storage-platform/internal/service/compaction"
)

// Run compacts every interval until ctx is cancelled. A failed run is
// logged and the next one continues where it stopped.
func Run(ctx context.Context, service *compactionservice.Service, interval time.Duration, log *slog.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		results, err := service.RunOnce(ctx)
		if err != nil && ctx.Err() == nil {
			log.Error("compaction run failed", "error", err)
		}
		var rows int64
		for _, r := range results {
			rows += r.Rows
		}
		log.Info("compaction run", "rules", len(results), "rows", rows)

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
