// Package heartbeat records that a worker process is alive, so
// GET /api/v1/operations/status can report each kind of worker.
package heartbeat

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// Run writes the process's heartbeat now and every model.WorkerBeatInterval
// until ctx ends. A failed write is logged and the next beat tries again;
// it never stops the worker. An empty instance means host and process ID.
func Run(ctx context.Context, db *postgres.DB, kind, instance string, log *slog.Logger) {
	if instance == "" {
		host, _ := os.Hostname()
		instance = fmt.Sprintf("%s-%d", host, os.Getpid())
	}
	started := time.Now().UTC()
	if err := db.PruneWorkerHeartbeats(ctx, started.Add(-7*24*time.Hour)); err != nil && ctx.Err() == nil {
		log.Warn("pruning worker heartbeats failed", "error", err)
	}

	ticker := time.NewTicker(model.WorkerBeatInterval)
	defer ticker.Stop()
	for {
		if err := db.BeatWorker(ctx, kind, instance, started, time.Now().UTC()); err != nil && ctx.Err() == nil {
			log.Warn("worker heartbeat failed", "kind", kind, "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
