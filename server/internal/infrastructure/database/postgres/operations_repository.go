package postgres

import (
	"context"
	"fmt"
	"time"

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
	return status, nil
}
