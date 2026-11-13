package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// EdgeRepository keeps edge devices, their recordings and chunks
// (docs/edge.md).
type EdgeRepository struct {
	db *DB
}

func NewEdgeRepository(db *DB) *EdgeRepository {
	return &EdgeRepository{db: db}
}

const edgeDeviceColumns = `id, tenant_id, name, dataset_id, namespace, path_prefix, token_id, last_seen, pending_files, pending_bytes,
	failed_files, oldest_pending, agent_version, note, created_by, created_at, revoked_at`

func scanEdgeDevice(row pgx.Row) (model.EdgeDevice, error) {
	var d model.EdgeDevice
	err := row.Scan(&d.ID, &d.TenantID, &d.Name, &d.DatasetID, &d.Namespace, &d.PathPrefix, &d.TokenID, &d.LastSeen, &d.PendingFiles, &d.PendingBytes,
		&d.FailedFiles, &d.OldestPending, &d.AgentVersion, &d.Note, &d.CreatedBy, &d.CreatedAt, &d.RevokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return d, model.ErrDeviceNotFound
	}
	return d, err
}

func (r *EdgeRepository) AddDevice(ctx context.Context, d model.EdgeDevice) error {
	_, err := r.db.Pool.Exec(ctx, `INSERT INTO edge_devices (id, tenant_id, name, dataset_id, namespace, path_prefix, token_id, created_by, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`, d.ID, d.TenantID, d.Name, d.DatasetID, d.Namespace, d.PathPrefix, d.TokenID, d.CreatedBy, d.CreatedAt)
	if isUniqueViolation(err) {
		return fmt.Errorf("%w: a device with that name exists", model.ErrInvalidEdge)
	}
	if err != nil {
		return fmt.Errorf("add device: %w", err)
	}
	return nil
}

func (r *EdgeRepository) Device(ctx context.Context, tenantID, id string) (model.EdgeDevice, error) {
	return scanEdgeDevice(r.db.Pool.QueryRow(ctx, `SELECT `+edgeDeviceColumns+` FROM edge_devices WHERE tenant_id = $1 AND id = $2`, tenantID, id))
}

// DeviceOfToken finds the live device a credential belongs to.
func (r *EdgeRepository) DeviceOfToken(ctx context.Context, tenantID, tokenID string) (model.EdgeDevice, error) {
	return scanEdgeDevice(r.db.Pool.QueryRow(ctx, `SELECT `+edgeDeviceColumns+` FROM edge_devices
		WHERE tenant_id = $1 AND token_id = $2 AND revoked_at IS NULL`, tenantID, tokenID))
}

func (r *EdgeRepository) Devices(ctx context.Context, tenantID string) ([]model.EdgeDevice, error) {
	rows, err := r.db.Pool.Query(ctx, `SELECT `+edgeDeviceColumns+` FROM edge_devices WHERE tenant_id = $1 ORDER BY revoked_at IS NOT NULL, name`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list devices: %w", err)
	}
	defer rows.Close()
	out := []model.EdgeDevice{}
	for rows.Next() {
		d, err := scanEdgeDevice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (r *EdgeRepository) Heartbeat(ctx context.Context, d model.EdgeDevice, h model.EdgeHeartbeat) error {
	_, err := r.db.Pool.Exec(ctx, `UPDATE edge_devices SET last_seen = now(), pending_files = $3, pending_bytes = $4, failed_files = $5,
		oldest_pending = $6, agent_version = $7, note = $8 WHERE tenant_id = $1 AND id = $2`,
		d.TenantID, d.ID, h.PendingFiles, h.PendingBytes, h.FailedFiles, h.OldestPending, h.AgentVersion, h.Note)
	return err
}

func (r *EdgeRepository) RevokeDevice(ctx context.Context, tenantID, id string) error {
	_, err := r.db.Pool.Exec(ctx, `UPDATE edge_devices SET revoked_at = now() WHERE tenant_id = $1 AND id = $2 AND revoked_at IS NULL`, tenantID, id)
	return err
}

const edgeRecordingColumns = `r.id, r.tenant_id, r.device_id, r.name, r.kind, r.state, r.gap_seconds, r.missing, COALESCE(r.version_id, ''), r.created_at, r.closed_at,
	(SELECT count(*) FROM edge_chunks c WHERE c.recording_id = r.id), (SELECT COALESCE(sum(c.size_bytes), 0) FROM edge_chunks c WHERE c.recording_id = r.id)`

func scanEdgeRecording(row pgx.Row) (model.EdgeRecording, error) {
	var e model.EdgeRecording
	var missing []byte
	err := row.Scan(&e.ID, &e.TenantID, &e.DeviceID, &e.Name, &e.Kind, &e.State, &e.GapSeconds, &missing, &e.VersionID, &e.CreatedAt, &e.ClosedAt, &e.Chunks, &e.Bytes)
	if errors.Is(err, pgx.ErrNoRows) {
		return e, model.ErrDeviceNotFound
	}
	if err != nil {
		return e, err
	}
	return e, json.Unmarshal(missing, &e.Missing)
}

// OpenRecording makes a device's recording, or answers the one with that name.
func (r *EdgeRepository) OpenRecording(ctx context.Context, e model.EdgeRecording) (model.EdgeRecording, error) {
	if _, err := r.db.Pool.Exec(ctx, `INSERT INTO edge_recordings (id, tenant_id, device_id, name, kind, state, gap_seconds, created_at)
		VALUES ($1, $2, $3, $4, $5, 'OPEN', $6, $7) ON CONFLICT (device_id, name) DO NOTHING`,
		e.ID, e.TenantID, e.DeviceID, e.Name, e.Kind, e.GapSeconds, e.CreatedAt); err != nil {
		return e, fmt.Errorf("open recording: %w", err)
	}
	return scanEdgeRecording(r.db.Pool.QueryRow(ctx, `SELECT `+edgeRecordingColumns+` FROM edge_recordings r WHERE r.device_id = $1 AND r.name = $2`, e.DeviceID, e.Name))
}

func (r *EdgeRepository) Recording(ctx context.Context, tenantID, id string) (model.EdgeRecording, error) {
	return scanEdgeRecording(r.db.Pool.QueryRow(ctx, `SELECT `+edgeRecordingColumns+` FROM edge_recordings r WHERE r.tenant_id = $1 AND r.id = $2`, tenantID, id))
}

func (r *EdgeRepository) Recordings(ctx context.Context, tenantID, deviceID string, limit int) ([]model.EdgeRecording, error) {
	rows, err := r.db.Pool.Query(ctx, `SELECT `+edgeRecordingColumns+` FROM edge_recordings r
		WHERE r.tenant_id = $1 AND ($2 = '' OR r.device_id = $2) ORDER BY r.created_at DESC LIMIT $3`, tenantID, deviceID, limit)
	if err != nil {
		return nil, fmt.Errorf("list recordings: %w", err)
	}
	defer rows.Close()
	out := []model.EdgeRecording{}
	for rows.Next() {
		e, err := scanEdgeRecording(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// AddChunk records a chunk once: sent again with the same bytes it changes
// nothing, with other bytes it is refused. A COMPLETE recording takes no
// new chunk. The recording's share lock makes a close that is completing
// it wait for this chunk, or this chunk wait for the close.
func (r *EdgeRepository) AddChunk(ctx context.Context, tenantID, recordingID string, c model.EdgeChunk) error {
	return withTx(ctx, r.db, func(tx pgx.Tx) error {
		var state string
		err := tx.QueryRow(ctx, `SELECT state FROM edge_recordings WHERE id = $1 AND tenant_id = $2 FOR SHARE`, recordingID, tenantID).Scan(&state)
		if errors.Is(err, pgx.ErrNoRows) {
			return model.ErrDeviceNotFound
		}
		if err != nil {
			return fmt.Errorf("lock recording: %w", err)
		}
		if state == model.RecordingComplete {
			return model.ErrRecordingComplete
		}
		tag, err := tx.Exec(ctx, `INSERT INTO edge_chunks (recording_id, tenant_id, path, sha256, size_bytes, starts_at, ends_at, added_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8) ON CONFLICT (recording_id, path) DO NOTHING`,
			recordingID, tenantID, c.Path, c.SHA256, c.SizeBytes, c.StartsAt, c.EndsAt, c.AddedAt)
		if err != nil {
			return fmt.Errorf("add chunk: %w", err)
		}
		if tag.RowsAffected() == 1 {
			return nil
		}
		var existing string
		if err := tx.QueryRow(ctx, `SELECT sha256 FROM edge_chunks WHERE recording_id = $1 AND path = $2`, recordingID, c.Path).Scan(&existing); err != nil {
			return err
		}
		if existing != c.SHA256 {
			return model.ErrChunkDiffers
		}
		return nil
	})
}

func (r *EdgeRepository) Chunks(ctx context.Context, tenantID, recordingID string) ([]model.EdgeChunk, error) {
	rows, err := r.db.Pool.Query(ctx, `SELECT path, sha256, size_bytes, starts_at, ends_at, added_at FROM edge_chunks
		WHERE tenant_id = $1 AND recording_id = $2 ORDER BY starts_at NULLS LAST, path`, tenantID, recordingID)
	if err != nil {
		return nil, fmt.Errorf("list chunks: %w", err)
	}
	defer rows.Close()
	out := []model.EdgeChunk{}
	for rows.Next() {
		var c model.EdgeChunk
		if err := rows.Scan(&c.Path, &c.SHA256, &c.SizeBytes, &c.StartsAt, &c.EndsAt, &c.AddedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// FileAt finds the version of a path with these bytes.
func (r *EdgeRepository) FileAt(ctx context.Context, tenantID, namespace, path, sha256 string) (model.EdgeFile, bool, error) {
	var f model.EdgeFile
	err := r.db.Pool.QueryRow(ctx, `SELECT a.id, a.object_id, `+assetUsableStatus+`, COALESCE(a.blocked_reason, a.rejection_reason, '') FROM assets a
		JOIN catalog_objects o ON o.tenant_id = a.tenant_id AND o.id = a.object_id
		WHERE a.tenant_id = $1 AND a.dataset = $2 AND o.path = $3 AND a.checksum_sha256 = $4 AND a.status <> 'DELETED'
		ORDER BY (a.status = 'AVAILABLE' AND a.blocked_reason IS NULL) DESC, a.created_at DESC LIMIT 1`, tenantID, namespace, path, sha256).Scan(&f.AssetID, &f.ObjectID, &f.Status, &f.Reason)
	if errors.Is(err, pgx.ErrNoRows) {
		return f, false, nil
	}
	if err != nil {
		return f, false, fmt.Errorf("find file: %w", err)
	}
	return f, true, nil
}

// CloseRecording records how a recording ended. COMPLETE is final: a close
// that read the recording before another completed it changes nothing and
// gets ErrRecordingComplete. INCOMPLETE may be closed again. A COMPLETE
// close names how many chunks it read; when a chunk arrived since, nothing
// changes and it gets ErrRecordingChanged.
func (r *EdgeRepository) CloseRecording(ctx context.Context, e model.EdgeRecording, chunks int) error {
	missing, err := json.Marshal(e.Missing)
	if err != nil {
		return err
	}
	return withTx(ctx, r.db, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE edge_recordings SET state = $3, missing = $4, version_id = NULLIF($5, ''), closed_at = $6
			WHERE tenant_id = $1 AND id = $2 AND state <> 'COMPLETE'`,
			e.TenantID, e.ID, e.State, missing, e.VersionID, e.ClosedAt)
		if err != nil {
			return fmt.Errorf("close recording: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return model.ErrRecordingComplete
		}
		if e.State != model.RecordingComplete {
			return nil
		}
		// Counted under the row lock the update took: a chunk added from
		// here on waits for this close and then finds the recording COMPLETE.
		var count int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM edge_chunks WHERE tenant_id = $1 AND recording_id = $2`, e.TenantID, e.ID).Scan(&count); err != nil {
			return fmt.Errorf("count chunks: %w", err)
		}
		if count != chunks {
			return model.ErrRecordingChanged
		}
		return nil
	})
}

// QueueBackfill sends the webhook for a closed gap.
func (r *EdgeRepository) QueueBackfill(ctx context.Context, tenantID string, e model.EdgeRecording, c model.EdgeChunk, gaps int) error {
	payload, err := json.Marshal(map[string]any{"recording_id": e.ID, "device_id": e.DeviceID, "name": e.Name, "path": c.Path, "starts_at": c.StartsAt, "ends_at": c.EndsAt, "gaps_left": gaps})
	if err != nil {
		return err
	}
	_, err = r.db.Pool.Exec(ctx, `SELECT queue_webhook($1, 'edge.backfill_closed', $2::jsonb)`, tenantID, string(payload))
	return err
}
