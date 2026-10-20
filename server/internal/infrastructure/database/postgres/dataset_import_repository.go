package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// Manifest imports. A batch of an import's members and its progress
// commit together (AddMembers with a checkpoint), so an import resumed
// after a crash starts after the last batch that committed and writes
// nothing twice.

const importColumns = `id, tenant_id, dataset_id, state, source, idempotency_key, COALESCE(manifest_key, ''),
	COALESCE(manifest_etag, ''), manifest_bytes, COALESCE(manifest_sha256, ''), duplicate_policy, lines_read,
	members_added, members_updated, members_skipped, lines_rejected, checkpoint_line, checkpoint_offset, attempts,
	cancel_requested, COALESCE(last_error, ''), created_by, created_at, updated_at, started_at, completed_at`

func scanImport(row pgx.Row) (model.ManifestImport, error) {
	var m model.ManifestImport
	var state, policy string
	err := row.Scan(&m.ID, &m.TenantID, &m.DatasetID, &state, &m.Source, &m.IdempotencyKey, &m.ManifestKey,
		&m.ManifestETag, &m.ManifestBytes, &m.ManifestSHA256, &policy, &m.LinesRead,
		&m.MembersAdded, &m.MembersUpdated, &m.MembersSkipped, &m.LinesRejected, &m.CheckpointLine, &m.CheckpointOffset,
		&m.Attempts, &m.CancelRequested, &m.LastError, &m.CreatedBy, &m.CreatedAt, &m.UpdatedAt, &m.StartedAt, &m.CompletedAt)
	m.State, m.DuplicatePolicy = model.ImportState(state), model.DuplicatePolicy(policy)
	return m, err
}

// CreateImport records an import, or returns the dataset's import with
// the same idempotency key when it describes the same manifest. An inline
// manifest is queued at once, with the event that starts it; a staged one
// waits for its upload. maxActive caps the tenant's unfinished imports.
func (r *DatasetRepository) CreateImport(ctx context.Context, imp model.ManifestImport, inline []byte, maxActive int,
	ev *model.OutboxEvent) (model.ManifestImport, bool, error) {
	tx, err := r.db.Pool.Begin(ctx)
	if err != nil {
		return model.ManifestImport{}, false, fmt.Errorf("begin import: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	if _, err := lockDraft(ctx, tx, imp.TenantID, imp.DatasetID); err != nil {
		return model.ManifestImport{}, false, err
	}
	existing, err := scanImport(tx.QueryRow(ctx, `
		SELECT `+importColumns+` FROM dataset_manifest_imports WHERE dataset_id = $1 AND idempotency_key = $2
	`, imp.DatasetID, imp.IdempotencyKey))
	if err == nil {
		if existing.Source != imp.Source || existing.DuplicatePolicy != imp.DuplicatePolicy ||
			existing.ManifestSHA256 != imp.ManifestSHA256 || existing.ManifestBytes != imp.ManifestBytes {
			return existing, false, model.ErrImportConflict
		}
		return existing, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return model.ManifestImport{}, false, fmt.Errorf("find import: %w", err)
	}

	var active int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM dataset_manifest_imports
		WHERE tenant_id = $1 AND state IN ('AWAITING_UPLOAD', 'QUEUED', 'RUNNING')
	`, imp.TenantID).Scan(&active); err != nil {
		return model.ManifestImport{}, false, fmt.Errorf("count imports: %w", err)
	}
	if maxActive > 0 && active >= maxActive {
		return model.ManifestImport{}, false, fmt.Errorf("%w: at most %d manifest imports at a time", model.ErrTooManyJobs, maxActive)
	}

	stored, err := scanImport(tx.QueryRow(ctx, `
		INSERT INTO dataset_manifest_imports (id, tenant_id, dataset_id, state, source, idempotency_key, manifest_inline,
		                                      manifest_key, manifest_bytes, manifest_sha256, duplicate_policy,
		                                      next_attempt_at, created_by, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NULLIF($8, ''), $9, NULLIF($10, ''), $11, $12, $13, $12, $12)
		RETURNING `+importColumns,
		imp.ID, imp.TenantID, imp.DatasetID, string(imp.State), imp.Source, imp.IdempotencyKey, inline,
		imp.ManifestKey, imp.ManifestBytes, imp.ManifestSHA256, string(imp.DuplicatePolicy), imp.CreatedAt, imp.CreatedBy))
	if err != nil {
		return model.ManifestImport{}, false, fmt.Errorf("create import: %w", err)
	}
	if ev != nil {
		if err := insertOutbox(ctx, tx, *ev); err != nil {
			return model.ManifestImport{}, false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		if isUniqueViolation(err) {
			// The same key, committed by a concurrent request.
			got, err := r.importByKey(ctx, imp.DatasetID, imp.IdempotencyKey)
			return got, false, err
		}
		return model.ManifestImport{}, false, fmt.Errorf("commit import: %w", err)
	}
	return stored, true, nil
}

func (r *DatasetRepository) importByKey(ctx context.Context, datasetID, key string) (model.ManifestImport, error) {
	return scanImport(r.db.Pool.QueryRow(ctx, `
		SELECT `+importColumns+` FROM dataset_manifest_imports WHERE dataset_id = $1 AND idempotency_key = $2
	`, datasetID, key))
}

// SubmitImport queues a staged import once its manifest is uploaded.
// Submitting a queued or running import again changes nothing.
func (r *DatasetRepository) SubmitImport(ctx context.Context, tenantID, importID, etag string, size int64, now time.Time,
	ev model.OutboxEvent) (model.ManifestImport, error) {
	tx, err := r.db.Pool.Begin(ctx)
	if err != nil {
		return model.ManifestImport{}, fmt.Errorf("begin submit: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	imp, err := scanImport(tx.QueryRow(ctx, `
		UPDATE dataset_manifest_imports SET state = 'QUEUED', manifest_etag = $3, manifest_bytes = $4,
		                                    next_attempt_at = $5, updated_at = $5
		WHERE tenant_id = $1 AND id = $2 AND state = 'AWAITING_UPLOAD'
		RETURNING `+importColumns, tenantID, importID, etag, size, now))
	if errors.Is(err, pgx.ErrNoRows) {
		current, err := r.GetImport(ctx, tenantID, importID)
		if err != nil {
			return current, err
		}
		if current.State == model.ImportQueued || current.State == model.ImportRunning || current.State == model.ImportCompleted {
			return current, nil
		}
		return current, model.ErrImportState
	}
	if err != nil {
		return model.ManifestImport{}, fmt.Errorf("submit import: %w", err)
	}
	if err := insertOutbox(ctx, tx, ev); err != nil {
		return model.ManifestImport{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return model.ManifestImport{}, fmt.Errorf("commit submit: %w", err)
	}
	return imp, nil
}

func (r *DatasetRepository) GetImport(ctx context.Context, tenantID, importID string) (model.ManifestImport, error) {
	imp, err := scanImport(r.db.Pool.QueryRow(ctx,
		`SELECT `+importColumns+` FROM dataset_manifest_imports WHERE tenant_id = $1 AND id = $2`, tenantID, importID))
	if errors.Is(err, pgx.ErrNoRows) {
		return imp, model.ErrImportNotFound
	}
	if err != nil {
		return imp, fmt.Errorf("get import: %w", err)
	}
	return imp, nil
}

// ListImportErrors pages an import's rejected lines by line number.
func (r *DatasetRepository) ListImportErrors(ctx context.Context, tenantID, importID string, afterLine int64, limit int) ([]model.ImportError, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT e.line, e.code, e.detail
		FROM dataset_import_errors AS e
		JOIN dataset_manifest_imports AS i ON i.id = e.import_id
		WHERE i.tenant_id = $1 AND e.import_id = $2 AND e.line > $3
		ORDER BY e.line
		LIMIT $4
	`, tenantID, importID, afterLine, limit)
	if err != nil {
		return nil, fmt.Errorf("list import errors: %w", err)
	}
	return collect(rows, func(row pgx.Row) (model.ImportError, error) {
		var e model.ImportError
		err := row.Scan(&e.Line, &e.Code, &e.Detail)
		return e, err
	})
}

// ClaimImports leases due imports to owner: queued ones, and running ones
// whose worker's lease ran out.
func (r *DatasetRepository) ClaimImports(ctx context.Context, owner, importID string, now time.Time, lease time.Duration, limit int) ([]model.ManifestImport, error) {
	rows, err := r.db.Pool.Query(ctx, `
		UPDATE dataset_manifest_imports SET state = 'RUNNING', lease_owner = $1, lease_expires_at = $2::timestamptz + $3::interval,
		                                    attempts = attempts + 1, started_at = COALESCE(started_at, $2), updated_at = $2
		WHERE id IN (
		    SELECT id FROM dataset_manifest_imports
		    WHERE state IN ('QUEUED', 'RUNNING') AND next_attempt_at <= $2
		      AND (lease_expires_at IS NULL OR lease_expires_at < $2) AND ($4 = '' OR id = $4)
		    ORDER BY next_attempt_at
		    LIMIT $5
		    FOR UPDATE SKIP LOCKED
		)
		RETURNING `+importColumns, owner, now, lease, importID, limit)
	if err != nil {
		return nil, fmt.Errorf("claim imports: %w", err)
	}
	return collect(rows, scanImport)
}

// InlineManifest returns an inline manifest's bytes from offset on.
func (r *DatasetRepository) InlineManifest(ctx context.Context, importID string, offset int64) ([]byte, error) {
	var body []byte
	err := r.db.Pool.QueryRow(ctx, `
		SELECT substring(manifest_inline FROM $2::int + 1) FROM dataset_manifest_imports WHERE id = $1
	`, importID, offset).Scan(&body)
	if err != nil {
		return nil, fmt.Errorf("read inline manifest: %w", err)
	}
	return body, nil
}

// ExtendImport renews owner's lease and says whether a cancellation was
// asked for. A lease owner no longer holds is ErrImportState.
func (r *DatasetRepository) ExtendImport(ctx context.Context, importID, owner string, now time.Time, lease time.Duration) (bool, error) {
	var cancel bool
	err := r.db.Pool.QueryRow(ctx, `
		UPDATE dataset_manifest_imports SET lease_expires_at = $3::timestamptz + $4::interval, updated_at = $3
		WHERE id = $1 AND lease_owner = $2 AND state = 'RUNNING'
		RETURNING cancel_requested
	`, importID, owner, now, lease).Scan(&cancel)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, model.ErrImportState
	}
	if err != nil {
		return false, fmt.Errorf("extend import lease: %w", err)
	}
	return cancel, nil
}

// FinishImport ends a running import. One that changed the draft is
// recorded in the lineage graph: the dataset imported from it.
func (r *DatasetRepository) FinishImport(ctx context.Context, importID, owner string, state model.ImportState, lastError string, now time.Time) error {
	tx, err := r.db.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin finish import: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	imp, err := scanImport(tx.QueryRow(ctx, `
		UPDATE dataset_manifest_imports SET state = $3, last_error = NULLIF($4, ''), lease_owner = NULL,
		                                    lease_expires_at = NULL, completed_at = $5, updated_at = $5,
		                                    manifest_inline = NULL
		WHERE id = $1 AND lease_owner = $2 AND state = 'RUNNING'
		RETURNING `+importColumns, importID, owner, string(state), lastError, now))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.ErrImportState
	}
	if err != nil {
		return fmt.Errorf("finish import: %w", err)
	}
	if imp.MembersAdded+imp.MembersUpdated > 0 {
		err := link(ctx, tx, imp.TenantID, model.LineageEdge{
			From: model.LineageRef{Kind: model.NodeDataset, RefID: imp.DatasetID},
			To:   model.LineageRef{Kind: model.NodeManifestImport, RefID: imp.ID}, Relation: model.RelImportedFrom,
			Actor: imp.CreatedBy, JobID: imp.ID,
			Details: map[string]any{"state": string(state), "manifest_sha256": imp.ManifestSHA256, "lines": imp.LinesRead,
				"added": imp.MembersAdded, "updated": imp.MembersUpdated},
		}, now, true)
		if err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit finish import: %w", err)
	}
	return nil
}

// RetryImport gives up owner's lease until next, keeping the checkpoint.
func (r *DatasetRepository) RetryImport(ctx context.Context, importID, owner string, next time.Time, lastError string, now time.Time) error {
	_, err := r.db.Pool.Exec(ctx, `
		UPDATE dataset_manifest_imports SET lease_owner = NULL, lease_expires_at = NULL, next_attempt_at = $3,
		                                    last_error = $4, updated_at = $5
		WHERE id = $1 AND lease_owner = $2 AND state = 'RUNNING'
	`, importID, owner, next, lastError, now)
	if err != nil {
		return fmt.Errorf("retry import: %w", err)
	}
	return nil
}

// CancelImport cancels an import that no worker holds at once, and asks
// the worker holding one to stop after its batch. Members a cancelled
// import already added stay in the draft.
func (r *DatasetRepository) CancelImport(ctx context.Context, tenantID, importID string, now time.Time) (model.ManifestImport, error) {
	imp, err := scanImport(r.db.Pool.QueryRow(ctx, `
		UPDATE dataset_manifest_imports SET
		    state = CASE WHEN state IN ('AWAITING_UPLOAD', 'QUEUED') OR lease_expires_at IS NULL OR lease_expires_at < $3
		                 THEN 'CANCELLED' ELSE state END,
		    completed_at = CASE WHEN state IN ('AWAITING_UPLOAD', 'QUEUED') OR lease_expires_at IS NULL OR lease_expires_at < $3
		                        THEN $3 ELSE completed_at END,
		    lease_owner = CASE WHEN lease_expires_at < $3 THEN NULL ELSE lease_owner END,
		    lease_expires_at = CASE WHEN lease_expires_at < $3 THEN NULL ELSE lease_expires_at END,
		    cancel_requested = true, updated_at = $3
		WHERE tenant_id = $1 AND id = $2 AND state IN ('AWAITING_UPLOAD', 'QUEUED', 'RUNNING')
		RETURNING `+importColumns, tenantID, importID, now))
	if errors.Is(err, pgx.ErrNoRows) {
		current, err := r.GetImport(ctx, tenantID, importID)
		if err != nil {
			return current, err
		}
		return current, model.ErrImportState
	}
	if err != nil {
		return imp, fmt.Errorf("cancel import: %w", err)
	}
	return imp, nil
}

// writeImportProgress stores a batch's rejected lines and moves the
// import's checkpoint, in the batch's transaction. Progress resets the
// attempts: they count claims that got nowhere.
func writeImportProgress(ctx context.Context, tx pgx.Tx, cp *interfaces.ImportCheckpoint, added, updated, skipped int64,
	rejected []model.ImportError, now time.Time) error {
	if len(rejected) > 0 {
		lines := make([]int64, len(rejected))
		codes := make([]string, len(rejected))
		details := make([]string, len(rejected))
		for i, e := range rejected {
			lines[i], codes[i], details[i] = e.Line, e.Code, e.Detail
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO dataset_import_errors (import_id, line, code, detail)
			SELECT $1, x.line, x.code, x.detail FROM unnest($2::bigint[], $3::text[], $4::text[]) AS x(line, code, detail)
			ON CONFLICT (import_id, line) DO NOTHING
		`, cp.ImportID, lines, codes, details); err != nil {
			return fmt.Errorf("record rejected lines: %w", err)
		}
	}
	_, err := tx.Exec(ctx, `
		UPDATE dataset_manifest_imports SET lines_read = $2, checkpoint_line = $2, checkpoint_offset = $3,
		    members_added = members_added + $4, members_updated = members_updated + $5,
		    members_skipped = members_skipped + $6, lines_rejected = lines_rejected + $7, attempts = 0, updated_at = $8
		WHERE id = $1
	`, cp.ImportID, cp.Lines, cp.Offset, added, updated, skipped, cp.RejectedCount+int64(len(rejected)-len(cp.Rejected)), now)
	if err != nil {
		return fmt.Errorf("move import checkpoint: %w", err)
	}
	return nil
}
