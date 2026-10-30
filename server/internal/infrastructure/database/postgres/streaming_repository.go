package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// StreamingRepository reads snapshot layouts for shard streams and keeps
// training runs' verification reports and checkpoints (docs/streaming.md).
type StreamingRepository struct {
	db *DB
}

func NewStreamingRepository(db *DB) *StreamingRepository {
	return &StreamingRepository{db: db}
}

// Layout pages a version's readable members in path order after
// (afterPath, afterOrdinal), with the pack each is served from.
func (r *StreamingRepository) Layout(ctx context.Context, tenantID, versionID, afterPath string, afterOrdinal int64, limit int) ([]model.LayoutRow, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT vm.ordinal, vm.path, vm.asset_id, vm.size_bytes, COALESCE(vm.checksum_sha256, ''), COALESCE(e.pack_id, '')
		FROM dataset_version_members vm
		JOIN assets a ON a.id = vm.asset_id AND a.tenant_id = vm.tenant_id AND a.status = 'AVAILABLE'
		LEFT JOIN pack_entries e ON e.asset_id = vm.asset_id AND e.tenant_id = vm.tenant_id AND e.state = 'PUBLISHED'
		WHERE vm.tenant_id = $1 AND vm.version_id = $2
		  AND (vm.path COLLATE "C", vm.ordinal) > ($3 COLLATE "C", $4)
		ORDER BY vm.path COLLATE "C", vm.ordinal
		LIMIT $5
	`, tenantID, versionID, afterPath, afterOrdinal, limit)
	if err != nil {
		return nil, fmt.Errorf("read snapshot layout: %w", err)
	}
	defer rows.Close()
	var out []model.LayoutRow
	for rows.Next() {
		var l model.LayoutRow
		if err := rows.Scan(&l.Ordinal, &l.Path, &l.AssetID, &l.SizeBytes, &l.SHA256, &l.PackID); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// MembersAt reads a version's members by ordinal; missing ordinals are
// left out.
func (r *StreamingRepository) MembersAt(ctx context.Context, tenantID, versionID string, ordinals []int64) ([]model.LayoutRow, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT ordinal, path, asset_id, size_bytes, COALESCE(checksum_sha256, ''), ''
		FROM dataset_version_members
		WHERE tenant_id = $1 AND version_id = $2 AND ordinal = ANY($3)
	`, tenantID, versionID, ordinals)
	if err != nil {
		return nil, fmt.Errorf("read snapshot members: %w", err)
	}
	defer rows.Close()
	var out []model.LayoutRow
	for rows.Next() {
		var l model.LayoutRow
		if err := rows.Scan(&l.Ordinal, &l.Path, &l.AssetID, &l.SizeBytes, &l.SHA256, &l.PackID); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (r *StreamingRepository) PutVerification(ctx context.Context, v model.RunVerification) error {
	failures, err := json.Marshal(v.Failures)
	if err != nil {
		return err
	}
	_, err = r.db.Pool.Exec(ctx, `
		INSERT INTO run_verifications (id, tenant_id, run_id, version_id, host, sample_rate, checked, failed, failures, created_by, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
	`, v.ID, v.TenantID, v.RunID, v.VersionID, v.Host, v.SampleRate, v.Checked, v.Failed, failures, v.CreatedBy, v.CreatedAt)
	if err != nil {
		return fmt.Errorf("record run verification: %w", err)
	}
	return nil
}

func (r *StreamingRepository) Verifications(ctx context.Context, tenantID, runID string, limit int) ([]model.RunVerification, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT id, run_id, version_id, host, sample_rate, checked, failed, failures, created_by, created_at
		FROM run_verifications WHERE tenant_id = $1 AND run_id = $2
		ORDER BY created_at DESC LIMIT $3
	`, tenantID, runID, limit)
	if err != nil {
		return nil, fmt.Errorf("list run verifications: %w", err)
	}
	defer rows.Close()
	out := []model.RunVerification{}
	for rows.Next() {
		var v model.RunVerification
		var failures []byte
		if err := rows.Scan(&v.ID, &v.RunID, &v.VersionID, &v.Host, &v.SampleRate, &v.Checked, &v.Failed, &failures,
			&v.CreatedBy, &v.CreatedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(failures, &v.Failures); err != nil {
			return nil, err
		}
		v.TenantID = tenantID
		out = append(out, v)
	}
	return out, rows.Err()
}

const checkpointColumns = `id, run_id, step, version_id, written_by, COALESCE(verified_by, ''), verified_at,
	COALESCE(verify_error, ''), created_by, created_at`

func scanCheckpoint(row pgx.Row) (model.RunCheckpoint, error) {
	var c model.RunCheckpoint
	err := row.Scan(&c.ID, &c.RunID, &c.Step, &c.VersionID, &c.WrittenBy, &c.VerifiedBy, &c.VerifiedAt, &c.VerifyError,
		&c.CreatedBy, &c.CreatedAt)
	return c, err
}

func (r *StreamingRepository) PutCheckpoint(ctx context.Context, c model.RunCheckpoint) error {
	_, err := r.db.Pool.Exec(ctx, `
		INSERT INTO run_checkpoints (id, tenant_id, run_id, step, version_id, written_by, created_by, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
	`, c.ID, c.TenantID, c.RunID, c.Step, c.VersionID, c.WrittenBy, c.CreatedBy, c.CreatedAt)
	if isUniqueViolation(err) {
		return model.ErrCheckpointExists
	}
	if err != nil {
		return fmt.Errorf("record checkpoint: %w", err)
	}
	return nil
}

func (r *StreamingRepository) Checkpoint(ctx context.Context, tenantID, id string) (model.RunCheckpoint, error) {
	c, err := scanCheckpoint(r.db.Pool.QueryRow(ctx, `SELECT `+checkpointColumns+` FROM run_checkpoints WHERE tenant_id = $1 AND id = $2`,
		tenantID, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return c, model.ErrCheckpointNotFound
	}
	if err != nil {
		return c, fmt.Errorf("read checkpoint: %w", err)
	}
	c.TenantID = tenantID
	return c, nil
}

func (r *StreamingRepository) Checkpoints(ctx context.Context, tenantID, runID string, limit int) ([]model.RunCheckpoint, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT `+checkpointColumns+` FROM run_checkpoints WHERE tenant_id = $1 AND run_id = $2 ORDER BY step DESC LIMIT $3
	`, tenantID, runID, limit)
	if err != nil {
		return nil, fmt.Errorf("list checkpoints: %w", err)
	}
	defer rows.Close()
	out := []model.RunCheckpoint{}
	for rows.Next() {
		c, err := scanCheckpoint(rows)
		if err != nil {
			return nil, err
		}
		c.TenantID = tenantID
		out = append(out, c)
	}
	return out, rows.Err()
}

// VerifyCheckpoint records a check: verified when verifyError is "".
func (r *StreamingRepository) VerifyCheckpoint(ctx context.Context, tenantID, id, host, verifyError string, now time.Time) error {
	_, err := r.db.Pool.Exec(ctx, `
		UPDATE run_checkpoints SET verified_by = $3, verify_error = NULLIF($4, ''),
		       verified_at = CASE WHEN $4 = '' THEN $5::timestamptz END
		WHERE tenant_id = $1 AND id = $2
	`, tenantID, id, host, verifyError, now)
	if err != nil {
		return fmt.Errorf("record checkpoint check: %w", err)
	}
	return nil
}
