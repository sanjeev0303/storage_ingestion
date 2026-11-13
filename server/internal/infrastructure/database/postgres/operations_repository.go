package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// OperationsStatus counts one tenant's background work in one statement,
// so every number is from the same snapshot.
func (db *DB) OperationsStatus(ctx context.Context, tenantID string, now time.Time) (model.OperationsStatus, error) {
	status := model.OperationsStatus{At: now}
	var oldestSeconds float64
	err := db.Pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM processing_jobs WHERE tenant_id = $1 AND state = 'PENDING'),
		  (SELECT count(*) FROM processing_jobs WHERE tenant_id = $1 AND state = 'EXHAUSTED'),
		  (SELECT COALESCE(EXTRACT(EPOCH FROM ($2 - min(created_at))), 0) FROM processing_jobs WHERE tenant_id = $1 AND state = 'PENDING'),
		  (SELECT count(*) FROM ingestion_jobs WHERE tenant_id = $1 AND state IN ('PENDING_VALIDATION', 'VALIDATING', 'READY', 'RUNNING', 'CANCELLING')),
		  (SELECT count(*) FROM ingestion_entries e JOIN ingestion_jobs j ON j.id = e.job_id
		     WHERE j.tenant_id = $1 AND j.state IN ('RUNNING', 'CANCELLING') AND e.state IN ('PENDING', 'FAILED_RETRYABLE')),
		  (SELECT count(*) FROM ingestion_entries e JOIN ingestion_jobs j ON j.id = e.job_id
		     WHERE j.tenant_id = $1 AND j.state IN ('RUNNING', 'CANCELLING') AND e.state IN ('SCHEDULED', 'UPLOADING', 'UPLOADED', 'PROCESSING')),
		  (SELECT count(*) FROM dataset_manifest_imports WHERE tenant_id = $1 AND state IN ('AWAITING_UPLOAD', 'QUEUED', 'RUNNING')),
		  (SELECT count(*) FROM validation_jobs WHERE tenant_id = $1 AND state = 'QUEUED'),
		  (SELECT count(*) FROM validation_jobs WHERE tenant_id = $1 AND state = 'RUNNING'),
		  (SELECT count(*) FROM deletion_operations WHERE tenant_id = $1 AND state IN ('SCHEDULED', 'IN_PROGRESS', 'RETRY_WAIT', 'STORAGE_DELETED')),
		  (SELECT count(*) FROM assets WHERE tenant_id = $1 AND status = 'AVAILABLE' AND shared_from IS NOT NULL),
		  (SELECT COALESCE(sum(size_bytes), 0) FROM assets WHERE tenant_id = $1 AND status = 'AVAILABLE' AND shared_from IS NOT NULL)
	`, tenantID, now).Scan(
		&status.ProcessingWaiting, &status.ProcessingHeld, &oldestSeconds,
		&status.IngestionJobsActive, &status.EntriesWaiting, &status.EntriesInFlight,
		&status.ImportsActive, &status.ValidationsQueued, &status.ValidationsRunning, &status.DeletionsScheduled,
		&status.SharedAssets, &status.SharedBytes,
	)
	if err != nil {
		return model.OperationsStatus{}, fmt.Errorf("operations status: %w", err)
	}
	status.OldestProcessingWait = time.Duration(oldestSeconds * float64(time.Second))

	status.Workers, err = db.workerHeartbeats(ctx, now)
	if err != nil {
		return model.OperationsStatus{}, err
	}
	return status, nil
}

// workerHeartbeats reads the heartbeats of the last day, newest first.
// Older rows are processes that stopped long ago.
func (db *DB) workerHeartbeats(ctx context.Context, now time.Time) ([]model.WorkerHeartbeat, error) {
	rows, err := db.Pool.Query(ctx, `
		SELECT kind, instance, started_at, last_seen_at
		FROM worker_heartbeats
		WHERE last_seen_at > $1
		ORDER BY kind, last_seen_at DESC
	`, now.Add(-24*time.Hour))
	if err != nil {
		return nil, fmt.Errorf("worker heartbeats: %w", err)
	}
	return collect(rows, func(row pgx.Row) (model.WorkerHeartbeat, error) {
		var h model.WorkerHeartbeat
		err := row.Scan(&h.Kind, &h.Instance, &h.StartedAt, &h.LastSeenAt)
		return h, err
	})
}

// BeatWorker records that a worker process is alive.
func (db *DB) BeatWorker(ctx context.Context, kind, instance string, startedAt, now time.Time) error {
	_, err := db.Pool.Exec(ctx, `
		INSERT INTO worker_heartbeats (kind, instance, started_at, last_seen_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (kind, instance) DO UPDATE SET last_seen_at = EXCLUDED.last_seen_at
	`, kind, instance, startedAt, now)
	if err != nil {
		return fmt.Errorf("worker heartbeat: %w", err)
	}
	return nil
}

// PruneWorkerHeartbeats deletes the rows of processes silent since before.
func (db *DB) PruneWorkerHeartbeats(ctx context.Context, before time.Time) error {
	if _, err := db.Pool.Exec(ctx, `DELETE FROM worker_heartbeats WHERE last_seen_at < $1`, before); err != nil {
		return fmt.Errorf("prune worker heartbeats: %w", err)
	}
	return nil
}
