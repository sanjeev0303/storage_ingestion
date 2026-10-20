// Package dataset runs the dataset worker loop: due manifest imports and
// validations (queued ones, retries, and jobs whose worker stopped), the
// release of datasets left VALIDATING, and the job gauges.
package dataset

import (
	"context"
	"log/slog"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/platform/retry"
	datasetservice "github.com/sanjeev0303/object-storage-platform/internal/service/dataset"
)

// Stats counts what one round of sweeps handled.
type Stats struct {
	Imports     int   `json:"imports"`
	Validations int   `json:"validations"`
	Released    int64 `json:"datasets_released"`
}

// RunOnce runs every sweep once, each job type with up to slots jobs. A
// failing sweep is logged and the others still run.
func RunOnce(ctx context.Context, svc *datasetservice.Service, owner string, slots int, log *slog.Logger) Stats {
	var stats Stats
	var err error
	if stats.Imports, err = svc.RunDueImports(ctx, owner, slots); err != nil && ctx.Err() == nil {
		log.Error("dataset import sweep failed", "error", retry.Sanitize(err))
	}
	if stats.Validations, err = svc.RunDueValidations(ctx, owner, slots); err != nil && ctx.Err() == nil {
		log.Error("dataset validation sweep failed", "error", retry.Sanitize(err))
	}
	if stats.Released, err = svc.ReleaseStuck(ctx); err != nil && ctx.Err() == nil {
		log.Error("releasing stuck datasets failed", "error", retry.Sanitize(err))
	}
	svc.Gauges(ctx)
	return stats
}

// Run repeats the sweeps every interval until ctx ends. A round that
// found work runs again at once, so a backlog drains without waiting.
func Run(ctx context.Context, svc *datasetservice.Service, owner string, slots int, interval time.Duration, log *slog.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		stats := RunOnce(ctx, svc, owner, slots, log)
		if stats != (Stats{}) {
			log.Info("dataset sweeps", "imports", stats.Imports, "validations", stats.Validations, "datasets_released", stats.Released)
			if ctx.Err() == nil && stats.Imports+stats.Validations > 0 {
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
