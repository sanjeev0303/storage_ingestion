package postgres

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// DeltaUploadRepository keeps delta uploads (docs/chunks.md).
type DeltaUploadRepository struct {
	db *DB
}

func NewDeltaUploadRepository(db *DB) *DeltaUploadRepository {
	return &DeltaUploadRepository{db: db}
}

// ChunkSource is where a chunk's bytes are in a file the base snapshot holds.
type DeltaSource struct {
	AssetID string
	Offset  int64
}

// ReuseSources finds, for the given chunk hashes, a place each is held in the
// base snapshot, and only there: being able to read that snapshot is what lets
// a client name its chunks, so a hash from elsewhere gets nothing.
func (r *DeltaUploadRepository) ReuseSources(ctx context.Context, tenantID, versionID string, hashes [][]byte) (map[string]DeltaSource, error) {
	rows, err := r.db.Pool.Query(ctx, `
		WITH files AS (
		    SELECT DISTINCT ON (m.checksum_sha256) m.checksum_sha256 AS sha, m.asset_id
		    FROM dataset_version_members m JOIN assets a ON a.id = m.asset_id AND a.tenant_id = m.tenant_id AND a.status = 'AVAILABLE' AND a.blocked_reason IS NULL
		    WHERE m.tenant_id = $1 AND m.version_id = $2 AND m.checksum_sha256 IS NOT NULL
		)
		SELECT DISTINCT ON (f.chunk_sha256) f.chunk_sha256, files.asset_id, f.byte_offset
		FROM files JOIN file_chunks f ON f.tenant_id = $1 AND f.file_sha256 = files.sha
		WHERE f.chunk_sha256 = ANY($3)`, tenantID, versionID, hashes)
	if err != nil {
		return nil, fmt.Errorf("find reusable chunks: %w", err)
	}
	defer rows.Close()
	out := map[string]DeltaSource{}
	for rows.Next() {
		var raw []byte
		var s DeltaSource
		if err := rows.Scan(&raw, &s.AssetID, &s.Offset); err != nil {
			return nil, err
		}
		out[hex.EncodeToString(raw)] = s
	}
	return out, rows.Err()
}

// Create records a delta upload and its chunks whole or not at all.
func (r *DeltaUploadRepository) Create(ctx context.Context, d model.DeltaUpload, chunks []model.DeltaChunk) error {
	tx, err := r.db.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `INSERT INTO delta_uploads (id, tenant_id, owner_id, dataset_id, namespace, base_version_id, path, size_bytes, sha256, content_type, connection_id, state, reused_bytes, sent_bytes, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, 'OPEN', $12, $13, $14, $15)`,
		d.ID, d.TenantID, d.OwnerID, d.DatasetID, d.Namespace, d.BaseVersionID, d.Path, d.SizeBytes, d.SHA256, d.ContentType, d.ConnectionID, d.ReusedBytes, d.SentBytes, d.CreatedAt, d.ExpiresAt); err != nil {
		return fmt.Errorf("create delta upload: %w", err)
	}
	rows := make([][]any, len(chunks))
	for i, c := range chunks {
		var asset *string
		var offset *int64
		if c.SourceAsset != "" {
			asset, offset = &chunks[i].SourceAsset, &chunks[i].SourceOffset
		}
		rows[i] = []any{d.ID, c.Ordinal, c.Offset, c.Length, c.SHA256, asset, offset}
	}
	if _, err := tx.CopyFrom(ctx, pgx.Identifier{"delta_upload_chunks"}, []string{"upload_id", "ordinal", "byte_offset", "length", "sha256", "source_asset", "source_offset"}, pgx.CopyFromRows(rows)); err != nil {
		return fmt.Errorf("record delta chunks: %w", err)
	}
	return tx.Commit(ctx)
}

const deltaColumns = `id, tenant_id, owner_id, dataset_id, namespace, base_version_id, path, size_bytes, sha256, content_type, connection_id, state, reused_bytes, sent_bytes, upload_id, error, attempts, created_at, expires_at, finished_at`

func scanDelta(row pgx.Row) (model.DeltaUpload, error) {
	var d model.DeltaUpload
	err := row.Scan(&d.ID, &d.TenantID, &d.OwnerID, &d.DatasetID, &d.Namespace, &d.BaseVersionID, &d.Path, &d.SizeBytes, &d.SHA256, &d.ContentType, &d.ConnectionID, &d.State,
		&d.ReusedBytes, &d.SentBytes, &d.UploadID, &d.Error, &d.Attempts, &d.CreatedAt, &d.ExpiresAt, &d.FinishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return d, model.ErrDeltaNotFound
	}
	return d, err
}

func (r *DeltaUploadRepository) Get(ctx context.Context, tenantID, id string) (model.DeltaUpload, error) {
	return scanDelta(r.db.Pool.QueryRow(ctx, `SELECT `+deltaColumns+` FROM delta_uploads WHERE tenant_id = $1 AND id = $2`, tenantID, id))
}

// Chunk reads one chunk of an upload.
func (r *DeltaUploadRepository) Chunk(ctx context.Context, id string, ordinal int) (model.DeltaChunk, error) {
	var c model.DeltaChunk
	var asset *string
	var offset *int64
	err := r.db.Pool.QueryRow(ctx, `SELECT ordinal, byte_offset, length, sha256, source_asset, source_offset, received FROM delta_upload_chunks WHERE upload_id = $1 AND ordinal = $2`, id, ordinal).
		Scan(&c.Ordinal, &c.Offset, &c.Length, &c.SHA256, &asset, &offset, &c.Received)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, model.ErrDeltaNotFound
	}
	if asset != nil {
		c.SourceAsset, c.SourceOffset = *asset, *offset
	}
	return c, err
}

// Chunks reads all of an upload's chunks in order.
func (r *DeltaUploadRepository) Chunks(ctx context.Context, id string) ([]model.DeltaChunk, error) {
	rows, err := r.db.Pool.Query(ctx, `SELECT ordinal, byte_offset, length, sha256, COALESCE(source_asset, ''), COALESCE(source_offset, 0), received FROM delta_upload_chunks WHERE upload_id = $1 ORDER BY ordinal`, id)
	if err != nil {
		return nil, fmt.Errorf("read delta chunks: %w", err)
	}
	defer rows.Close()
	out := []model.DeltaChunk{}
	for rows.Next() {
		var c model.DeltaChunk
		if err := rows.Scan(&c.Ordinal, &c.Offset, &c.Length, &c.SHA256, &c.SourceAsset, &c.SourceOffset, &c.Received); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (r *DeltaUploadRepository) MarkReceived(ctx context.Context, id string, ordinal int) error {
	_, err := r.db.Pool.Exec(ctx, `UPDATE delta_upload_chunks SET received = true WHERE upload_id = $1 AND ordinal = $2`, id, ordinal)
	return err
}

// MissingChunks lists what the client still has to send.
func (r *DeltaUploadRepository) MissingChunks(ctx context.Context, id string) ([]int, error) {
	rows, err := r.db.Pool.Query(ctx, `SELECT ordinal FROM delta_upload_chunks WHERE upload_id = $1 AND source_asset IS NULL AND NOT received ORDER BY ordinal`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []int{}
	for rows.Next() {
		var n int
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// Queue hands a complete upload to the worker, and says whether it was
// still open to hand over.
func (r *DeltaUploadRepository) Queue(ctx context.Context, id string) (bool, error) {
	tag, err := r.db.Pool.Exec(ctx, `UPDATE delta_uploads SET state = 'QUEUED' WHERE id = $1 AND state = 'OPEN'`, id)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// Claim takes the oldest queued upload, or one whose worker has gone.
func (r *DeltaUploadRepository) Claim(ctx context.Context, owner string, lease time.Duration, maxAttempts int) (model.DeltaUpload, bool, error) {
	if _, err := r.db.Pool.Exec(ctx, `UPDATE delta_uploads SET state = 'FAILED', finished_at = now(), lease_owner = NULL, error = 'gave up after ' || attempts || ' attempts'
		WHERE state = 'ASSEMBLING' AND lease_until < now() AND attempts >= $1`, maxAttempts); err != nil {
		return model.DeltaUpload{}, false, fmt.Errorf("fail stuck delta uploads: %w", err)
	}
	d, err := scanDelta(r.db.Pool.QueryRow(ctx, `UPDATE delta_uploads SET state = 'ASSEMBLING', lease_owner = $1, lease_until = now() + $2::interval, attempts = attempts + 1
		WHERE id = (SELECT id FROM delta_uploads WHERE state = 'QUEUED' OR (state = 'ASSEMBLING' AND lease_until < now()) ORDER BY created_at LIMIT 1 FOR UPDATE SKIP LOCKED)
		RETURNING `+deltaColumns, owner, fmt.Sprintf("%d seconds", int(lease.Seconds()))))
	if errors.Is(err, model.ErrDeltaNotFound) {
		return d, false, nil
	}
	if err != nil {
		return d, false, fmt.Errorf("claim delta upload: %w", err)
	}
	return d, true, nil
}

func (r *DeltaUploadRepository) Extend(ctx context.Context, id, owner string, lease time.Duration) error {
	_, err := r.db.Pool.Exec(ctx, `UPDATE delta_uploads SET lease_until = now() + $3::interval WHERE id = $1 AND lease_owner = $2 AND state = 'ASSEMBLING'`,
		id, owner, fmt.Sprintf("%d seconds", int(lease.Seconds())))
	return err
}

// Finish records the outcome of owner's assembly, and says whether owner
// still held the upload: a worker whose lease ran out writes nothing.
func (r *DeltaUploadRepository) Finish(ctx context.Context, id, owner, state, uploadID, failure string, now time.Time) (bool, error) {
	tag, err := r.db.Pool.Exec(ctx, `UPDATE delta_uploads SET state = $3, upload_id = $4, error = $5, finished_at = $6, lease_owner = NULL
		WHERE id = $1 AND state = 'ASSEMBLING' AND lease_owner = $2`, id, owner, state, uploadID, failure, now)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// Expire ends an upload nobody finished, and says whether it did: one
// completed since the sweep read it is left to the worker.
func (r *DeltaUploadRepository) Expire(ctx context.Context, id string, now time.Time) (bool, error) {
	tag, err := r.db.Pool.Exec(ctx, `UPDATE delta_uploads SET state = 'EXPIRED', error = 'nobody finished it in time', finished_at = $2
		WHERE id = $1 AND state = 'OPEN' AND expires_at <= $2`, id, now)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// Expired finds uploads nobody finished, and those whose chunks are ready to go.
func (r *DeltaUploadRepository) Expired(ctx context.Context, now time.Time, limit int) ([]model.DeltaUpload, error) {
	rows, err := r.db.Pool.Query(ctx, `SELECT `+deltaColumns+` FROM delta_uploads WHERE state = 'OPEN' AND expires_at <= $1 ORDER BY expires_at LIMIT $2`, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.DeltaUpload{}
	for rows.Next() {
		d, err := scanDelta(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
