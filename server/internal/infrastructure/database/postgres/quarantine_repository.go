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

// QuarantineRepository stores quarantine records.
type QuarantineRepository struct {
	db *DB
}

var _ interfaces.QuarantineRepository = (*QuarantineRepository)(nil)

func NewQuarantineRepository(db *DB) *QuarantineRepository {
	return &QuarantineRepository{db: db}
}

const quarantineColumns = `
	id,
	tenant_id,
	asset_id,
	job_id,
	upload_id,
	object_key,
	category,
	reason,
	COALESCE(scan_result, ''),
	COALESCE(scan_signature, ''),
	status,
	retain_until,
	created_at,
	updated_at,
	resolved_at
`

func scanQuarantine(row pgx.Row) (model.QuarantineRecord, error) {
	var record model.QuarantineRecord
	err := row.Scan(
		&record.ID,
		&record.TenantID,
		&record.AssetID,
		&record.JobID,
		&record.UploadID,
		&record.ObjectKey,
		&record.Category,
		&record.Reason,
		&record.ScanResult,
		&record.ScanSignature,
		&record.Status,
		&record.RetainUntil,
		&record.CreatedAt,
		&record.UpdatedAt,
		&record.ResolvedAt,
	)
	return record, err
}

// Hold opens the asset's HELD record, or refreshes the open one with the
// latest category and reason. The partial unique index keeps it single.
func (r *QuarantineRepository) Hold(ctx context.Context, record model.QuarantineRecord) (model.QuarantineRecord, error) {
	held, err := scanQuarantine(r.db.Pool.QueryRow(ctx, `
		INSERT INTO quarantine_records (
			id, tenant_id, asset_id, job_id, upload_id, object_key, category, reason,
			scan_result, scan_signature, status, created_at, updated_at
		)
		VALUES (gen_random_uuid()::text, $1, $2, $3, $4, $5, $6, $7, NULLIF($8, ''), NULLIF($9, ''), 'HELD', $10, $10)
		ON CONFLICT (asset_id) WHERE status = 'HELD'
		DO UPDATE SET
			category = EXCLUDED.category,
			reason = EXCLUDED.reason,
			scan_result = EXCLUDED.scan_result,
			scan_signature = EXCLUDED.scan_signature,
			updated_at = EXCLUDED.updated_at
		RETURNING `+quarantineColumns,
		record.TenantID, record.AssetID, record.JobID, record.UploadID, record.ObjectKey,
		record.Category, record.Reason, record.ScanResult, record.ScanSignature, record.CreatedAt))
	if err != nil {
		return model.QuarantineRecord{}, fmt.Errorf("hold asset in quarantine: %w", err)
	}
	return held, nil
}

// Reject closes an open HELD record as REJECTED, or inserts a REJECTED
// record. An asset is rejected once, so a repeated call adds nothing.
func (r *QuarantineRepository) Reject(ctx context.Context, record model.QuarantineRecord) error {
	_, err := r.db.Pool.Exec(ctx, `
		WITH closed AS (
			UPDATE quarantine_records
			SET status = 'REJECTED',
			    category = $6,
			    reason = $7,
			    scan_result = NULLIF($8, ''),
			    scan_signature = NULLIF($9, ''),
			    retain_until = $10,
			    updated_at = $11,
			    resolved_at = $11
			WHERE tenant_id = $1 AND asset_id = $2 AND status = 'HELD'
			RETURNING id
		)
		INSERT INTO quarantine_records (
			id, tenant_id, asset_id, job_id, upload_id, object_key, category, reason,
			scan_result, scan_signature, status, retain_until, created_at, updated_at, resolved_at
		)
		SELECT gen_random_uuid()::text, $1, $2, $3, $4, $5, $6, $7, NULLIF($8, ''), NULLIF($9, ''),
		       'REJECTED', $10, $11, $11, $11
		WHERE NOT EXISTS (SELECT 1 FROM closed)
		  AND NOT EXISTS (
			SELECT 1 FROM quarantine_records
			WHERE tenant_id = $1 AND asset_id = $2 AND status IN ('REJECTED', 'PURGED')
		  )
	`, record.TenantID, record.AssetID, record.JobID, record.UploadID, record.ObjectKey,
		record.Category, record.Reason, record.ScanResult, record.ScanSignature, record.RetainUntil, record.CreatedAt)
	if err != nil {
		return fmt.Errorf("reject asset in quarantine: %w", err)
	}
	return nil
}

func (r *QuarantineRepository) Release(ctx context.Context, tenantID, assetID string, at time.Time) (bool, error) {
	tag, err := r.db.Pool.Exec(ctx, `
		UPDATE quarantine_records
		SET status = 'RELEASED', updated_at = $3, resolved_at = $3
		WHERE tenant_id = $1 AND asset_id = $2 AND status = 'HELD'
	`, tenantID, assetID, at)
	if err != nil {
		return false, fmt.Errorf("release quarantine hold: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

func (r *QuarantineRepository) DuePurge(ctx context.Context, now time.Time, limit int) ([]model.QuarantineRecord, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT `+quarantineColumns+`
		FROM quarantine_records
		WHERE status = 'REJECTED' AND retain_until <= $1
		ORDER BY retain_until
		LIMIT $2
	`, now, limit)
	if err != nil {
		return nil, fmt.Errorf("list quarantine records due for purge: %w", err)
	}
	return collectQuarantine(rows)
}

func (r *QuarantineRepository) MarkPurged(ctx context.Context, id string, at time.Time) error {
	_, err := r.db.Pool.Exec(ctx, `
		UPDATE quarantine_records
		SET status = 'PURGED', updated_at = $2
		WHERE id = $1 AND status = 'REJECTED'
	`, id, at)
	if err != nil {
		return fmt.Errorf("mark quarantine record purged: %w", err)
	}
	return nil
}

func (r *QuarantineRepository) ListForAsset(ctx context.Context, tenantID, assetID string) ([]model.QuarantineRecord, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT `+quarantineColumns+`
		FROM quarantine_records
		WHERE tenant_id = $1 AND asset_id = $2
		ORDER BY created_at DESC
	`, tenantID, assetID)
	if err != nil {
		return nil, fmt.Errorf("list quarantine records: %w", err)
	}
	return collectQuarantine(rows)
}

func collectQuarantine(rows pgx.Rows) ([]model.QuarantineRecord, error) {
	defer rows.Close()
	var records []model.QuarantineRecord
	for rows.Next() {
		record, err := scanQuarantine(rows)
		if err != nil {
			return nil, fmt.Errorf("scan quarantine record: %w", err)
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

// ProcessingEventRepository appends processing audit events.
type ProcessingEventRepository struct {
	db *DB
}

var _ interfaces.ProcessingEventRepository = (*ProcessingEventRepository)(nil)

func NewProcessingEventRepository(db *DB) *ProcessingEventRepository {
	return &ProcessingEventRepository{db: db}
}

func (r *ProcessingEventRepository) Record(ctx context.Context, event model.ProcessingEvent) error {
	_, err := r.db.Pool.Exec(ctx, `
		INSERT INTO processing_events (id, tenant_id, asset_id, job_id, event, detail, actor, created_at)
		VALUES (gen_random_uuid()::text, $1, $2, NULLIF($3, ''), $4, NULLIF($5, ''), $6, $7)
	`, event.TenantID, event.AssetID, event.JobID, event.Event, event.Detail, event.Actor, event.CreatedAt)
	if err != nil {
		return fmt.Errorf("record processing event: %w", err)
	}
	return nil
}

func (r *ProcessingEventRepository) ListForAsset(ctx context.Context, tenantID, assetID string) ([]model.ProcessingEvent, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT id, tenant_id, asset_id, COALESCE(job_id, ''), event, COALESCE(detail, ''), actor, created_at
		FROM processing_events
		WHERE tenant_id = $1 AND asset_id = $2
		ORDER BY created_at, id
	`, tenantID, assetID)
	if err != nil {
		return nil, fmt.Errorf("list processing events: %w", err)
	}
	defer rows.Close()

	var events []model.ProcessingEvent
	for rows.Next() {
		var e model.ProcessingEvent
		if err := rows.Scan(&e.ID, &e.TenantID, &e.AssetID, &e.JobID, &e.Event, &e.Detail, &e.Actor, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan processing event: %w", err)
		}
		events = append(events, e)
	}
	return events, rows.Err()
}

// ContentPolicyRepository reads tenant content policies.
type ContentPolicyRepository struct {
	db *DB
}

var _ interfaces.ContentPolicyRepository = (*ContentPolicyRepository)(nil)

func NewContentPolicyRepository(db *DB) *ContentPolicyRepository {
	return &ContentPolicyRepository{db: db}
}

func (r *ContentPolicyRepository) Get(ctx context.Context, tenantID string) (model.ContentPolicy, bool, error) {
	var policy model.ContentPolicy
	var maxSize *int64
	err := r.db.Pool.QueryRow(ctx, `
		SELECT allowed_types, denied_types, max_size_bytes, COALESCE(unknown_type_action, '')
		FROM tenant_content_policies
		WHERE tenant_id = $1
	`, tenantID).Scan(&policy.AllowedTypes, &policy.DeniedTypes, &maxSize, &policy.UnknownTypeAction)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.ContentPolicy{}, false, nil
	}
	if err != nil {
		return model.ContentPolicy{}, false, fmt.Errorf("get content policy: %w", err)
	}
	if maxSize != nil {
		policy.MaxSizeBytes = *maxSize
	}
	return policy, true, nil
}
