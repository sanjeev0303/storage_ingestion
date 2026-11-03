package postgres

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// extentCache keeps files' chunk lists. A list is named by the file's hash and the
// connection, and cannot change while the file is served from it, so only whether
// the asset is still chunked is asked each time: a read of a few bytes of a large
// file does not load a thousand rows a gigabyte.
type extentCache struct {
	mu      sync.Mutex
	lists   map[string][]model.ChunkExtent
	extents int
}

// ponytail: emptied whole when full, not least-recently-used. Fine while the files
// being read fit; a real LRU if reads turn out to cycle through more than this.
const maxCachedExtents = 2_000_000

func (c *extentCache) get(key string) ([]model.ChunkExtent, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	list, ok := c.lists[key]
	return list, ok
}

func (c *extentCache) put(key string, list []model.ChunkExtent) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.lists == nil || c.extents+len(list) > maxCachedExtents {
		c.lists, c.extents = map[string][]model.ChunkExtent{}, 0
	}
	c.lists[key] = list
	c.extents += len(list)
}

// ChunkedExtents says where a chunked asset's bytes are. It is on the pack
// repository because everything that reads a stored object already asks that
// one where the bytes are.
func (r *PackRepository) ChunkedExtents(ctx context.Context, tenantID, assetID string) ([]model.ChunkExtent, bool, error) {
	var connection, sha string
	var size int64
	err := r.db.Pool.QueryRow(ctx, `SELECT c.connection_id, c.file_sha256, a.size_bytes FROM chunked_assets c JOIN assets a ON a.id = c.asset_id
		WHERE c.tenant_id = $1 AND c.asset_id = $2 AND c.state IN ('PUBLISHED', 'REHYDRATING')`, tenantID, assetID).Scan(&connection, &sha, &size)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("find chunked asset: %w", err)
	}
	cacheKey := tenantID + "/" + connection + "/" + sha
	if list, ok := r.extents.get(cacheKey); ok {
		return list, true, nil
	}
	rows, err := r.db.Pool.Query(ctx, `SELECT f.byte_offset, f.length, encode(f.chunk_sha256, 'hex'), COALESCE(s.object_key, '')
		FROM file_chunks f
		LEFT JOIN stored_chunks s ON s.tenant_id = f.tenant_id AND s.connection_id = $3 AND s.chunk_sha256 = encode(f.chunk_sha256, 'hex') AND s.written_at IS NOT NULL
		WHERE f.tenant_id = $1 AND f.file_sha256 = $2 ORDER BY f.ordinal`, tenantID, sha, connection)
	if err != nil {
		return nil, false, fmt.Errorf("read chunk list: %w", err)
	}
	defer rows.Close()
	out := []model.ChunkExtent{}
	var at int64
	for rows.Next() {
		var e model.ChunkExtent
		if err := rows.Scan(&e.Offset, &e.Length, &e.SHA256, &e.Key); err != nil {
			return nil, false, err
		}
		if e.Key == "" || e.Offset != at {
			return nil, false, fmt.Errorf("%w: asset %s has a gap at byte %d", model.ErrChunkCorrupt, assetID, at)
		}
		at += int64(e.Length)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if at != size {
		return nil, false, fmt.Errorf("%w: asset %s has %d bytes of chunks for %d bytes", model.ErrChunkCorrupt, assetID, at, size)
	}
	r.extents.put(cacheKey, out)
	return out, true, nil
}

// RequestRehydrate asks for a chunked asset to become one object again. It
// reports whether the asset was chunked.
func (r *PackRepository) RequestRehydrate(ctx context.Context, tenantID, assetID string) (bool, error) {
	var state string
	err := r.db.Pool.QueryRow(ctx, `UPDATE chunked_assets SET state = CASE state WHEN 'PUBLISHED' THEN 'REHYDRATING' ELSE state END, checked_at = now()
		WHERE tenant_id = $1 AND asset_id = $2 AND state IN ('PUBLISHED', 'REHYDRATING') RETURNING state`, tenantID, assetID).Scan(&state)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// releaseChunks takes a deleted asset off its chunks, and reports whether any
// chunk only it needed still has its object. Such a chunk goes on the chunk
// store's next pass, and until then the asset's storage is not called deleted.
func (r *PackRepository) releaseChunks(ctx context.Context, tenantID, assetID string, now time.Time) (bool, error) {
	tx, err := r.db.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `UPDATE chunked_assets SET state = 'RELEASED', released_at = $3 WHERE tenant_id = $1 AND asset_id = $2 AND state <> 'RELEASED'`,
		tenantID, assetID, now); err != nil {
		return false, fmt.Errorf("release chunked asset: %w", err)
	}
	rows, err := tx.Query(ctx, `DELETE FROM chunk_refs WHERE tenant_id = $1 AND asset_id = $2 RETURNING connection_id, chunk_sha256`, tenantID, assetID)
	if err != nil {
		return false, fmt.Errorf("release chunks: %w", err)
	}
	type ref struct{ connection, sha string }
	var refs []ref
	for rows.Next() {
		var x ref
		if err := rows.Scan(&x.connection, &x.sha); err != nil {
			rows.Close()
			return false, err
		}
		refs = append(refs, x)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	// Refs already taken off on an earlier try leave nothing to look at here,
	// so what this asset released is remembered by the chunk's own state.
	var waiting bool
	err = r.db.Pool.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM file_chunks f
		JOIN chunked_assets c ON c.tenant_id = f.tenant_id AND c.file_sha256 = f.file_sha256
		JOIN stored_chunks s ON s.tenant_id = f.tenant_id AND s.connection_id = c.connection_id AND s.chunk_sha256 = encode(f.chunk_sha256, 'hex')
		WHERE c.tenant_id = $1 AND c.asset_id = $2
		  AND NOT EXISTS (SELECT 1 FROM chunk_refs x WHERE x.tenant_id = s.tenant_id AND x.connection_id = s.connection_id AND x.chunk_sha256 = s.chunk_sha256))`,
		tenantID, assetID).Scan(&waiting)
	if err != nil {
		return false, fmt.Errorf("find chunks waiting: %w", err)
	}
	return waiting, nil
}

// ChunkStorageRepository keeps what the chunk store does (docs/chunks.md).
type ChunkStorageRepository struct {
	db *DB
}

func NewChunkStorageRepository(db *DB) *ChunkStorageRepository {
	return &ChunkStorageRepository{db: db}
}

// ChunkCandidate is an asset the chunk store may take.
type ChunkCandidate struct {
	TenantID     string
	AssetID      string
	ConnectionID string
	ObjectKey    string
	ETag         string
	VersionID    string
	SHA256       string
	SizeBytes    int64
}

// Candidates finds assets of datasets with chunk storage on that have been
// indexed. Left out: objects that are not simply this asset's own (adopted,
// shared with an identical upload, packed), anything not on the standard tier,
// and anything under a hold or lock, whose object must stay as it is.
func (r *ChunkStorageRepository) Candidates(ctx context.Context, skippedBefore time.Time, limit int) ([]ChunkCandidate, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT a.tenant_id, a.id, COALESCE(a.storage_connection_id, 'platform'), a.object_key, a.storage_etag, COALESCE(a.storage_version_id, ''), a.checksum_sha256, a.size_bytes
		FROM datasets d
		JOIN assets a ON a.tenant_id = d.tenant_id AND a.dataset = d.name
		JOIN chunked_files f ON f.tenant_id = a.tenant_id AND f.sha256 = a.checksum_sha256
		WHERE d.chunk_storage_enabled AND a.status = 'AVAILABLE' AND a.adoption IS NULL AND a.storage_tier = 'STANDARD'
		  AND f.chunks > 1 AND COALESCE(a.storage_etag, '') <> ''
		  AND COALESCE(a.shared_from, '') = ''
		  AND NOT EXISTS (SELECT 1 FROM assets s WHERE s.shared_from = a.id AND s.status <> 'DELETED')
		  AND NOT EXISTS (SELECT 1 FROM pack_entries e WHERE e.asset_id = a.id AND e.state IN ('RESERVED', 'PUBLISHED'))
		  AND NOT EXISTS (SELECT 1 FROM object_locks l WHERE l.asset_id = a.id AND (l.legal_hold OR l.retain_until > now()))
		  AND NOT EXISTS (SELECT 1 FROM legal_holds h WHERE h.tenant_id = a.tenant_id AND h.released_at IS NULL
		        AND (h.scope = 'TENANT' OR (h.scope = 'DATASET' AND h.scope_key = a.dataset) OR (h.scope = 'ASSET' AND h.scope_key = a.id)))
		  AND NOT EXISTS (SELECT 1 FROM retention_policies p WHERE p.tenant_id = a.tenant_id AND COALESCE(p.min_retention_seconds, 0) > 0
		        AND (p.scope = 'TENANT' OR (p.scope = 'DATASET' AND p.scope_key IN (d.name, d.id))))
		  AND NOT EXISTS (SELECT 1 FROM chunked_assets c WHERE c.asset_id = a.id AND NOT (c.state = 'WRITING' OR (c.state = 'SKIPPED' AND c.checked_at < $1)))
		ORDER BY a.id LIMIT $2`, skippedBefore, limit)
	if err != nil {
		return nil, fmt.Errorf("list chunk candidates: %w", err)
	}
	defer rows.Close()
	out := []ChunkCandidate{}
	for rows.Next() {
		var c ChunkCandidate
		if err := rows.Scan(&c.TenantID, &c.AssetID, &c.ConnectionID, &c.ObjectKey, &c.ETag, &c.VersionID, &c.SHA256, &c.SizeBytes); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SharedBytes counts the bytes of a file in chunks another file also holds.
func (r *ChunkStorageRepository) SharedBytes(ctx context.Context, tenantID, sha string) (int64, error) {
	var n int64
	err := r.db.Pool.QueryRow(ctx, `SELECT COALESCE(sum(f.length), 0)::bigint FROM file_chunks f
		WHERE f.tenant_id = $1 AND f.file_sha256 = $2 AND EXISTS (
		    SELECT 1 FROM file_chunks o WHERE o.tenant_id = f.tenant_id AND o.chunk_sha256 = f.chunk_sha256 AND o.file_sha256 <> f.file_sha256)`, tenantID, sha).Scan(&n)
	return n, err
}

func (r *ChunkStorageRepository) mark(ctx context.Context, c ChunkCandidate, state, reason string, now time.Time) error {
	_, err := r.db.Pool.Exec(ctx, `INSERT INTO chunked_assets (asset_id, tenant_id, connection_id, file_sha256, state, reason, source_key, source_etag, checked_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (asset_id) DO UPDATE SET state = $5, reason = $6, checked_at = $9`,
		c.AssetID, c.TenantID, c.ConnectionID, c.SHA256, state, reason, c.ObjectKey, c.ETag, now)
	return err
}

// Skip records that an asset was looked at and left whole.
func (r *ChunkStorageRepository) Skip(ctx context.Context, c ChunkCandidate, reason string, now time.Time) error {
	return r.mark(ctx, c, "SKIPPED", reason, now)
}

// StartWriting records that chunks are being written for an asset.
func (r *ChunkStorageRepository) StartWriting(ctx context.Context, c ChunkCandidate, now time.Time) error {
	return r.mark(ctx, c, "WRITING", "", now)
}

// FileChunks answers a file's chunks in order.
func (r *ChunkStorageRepository) FileChunks(ctx context.Context, tenantID, sha string) ([]model.ChunkExtent, error) {
	rows, err := r.db.Pool.Query(ctx, `SELECT byte_offset, length, encode(chunk_sha256, 'hex') FROM file_chunks WHERE tenant_id = $1 AND file_sha256 = $2 ORDER BY ordinal`, tenantID, sha)
	if err != nil {
		return nil, fmt.Errorf("read file chunks: %w", err)
	}
	defer rows.Close()
	out := []model.ChunkExtent{}
	for rows.Next() {
		var e model.ChunkExtent
		if err := rows.Scan(&e.Offset, &e.Length, &e.SHA256); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ErrChunkBusy: the chunk's object is being deleted; try on a later pass.
var ErrChunkBusy = errors.New("the chunk is being deleted")

// AddRef makes asset a user of a chunk, and says whether its object still has
// to be written. The row stays locked until the reference is in, so the chunk
// cannot be judged unused in between.
func (r *ChunkStorageRepository) AddRef(ctx context.Context, c ChunkCandidate, sha, key string, length int, now time.Time) (bool, error) {
	tx, err := r.db.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var state string
	var written *time.Time
	err = tx.QueryRow(ctx, `INSERT INTO stored_chunks (tenant_id, connection_id, chunk_sha256, length, object_key, state, created_at)
		VALUES ($1, $2, $3, $4, $5, 'LIVE', $6)
		ON CONFLICT (tenant_id, connection_id, chunk_sha256) DO UPDATE SET length = stored_chunks.length
		RETURNING state, written_at`, c.TenantID, c.ConnectionID, sha, length, key, now).Scan(&state, &written)
	if err != nil {
		return false, fmt.Errorf("add chunk: %w", err)
	}
	if state != "LIVE" {
		return false, ErrChunkBusy
	}
	if _, err := tx.Exec(ctx, `INSERT INTO chunk_refs (tenant_id, connection_id, chunk_sha256, asset_id) VALUES ($1, $2, $3, $4) ON CONFLICT DO NOTHING`,
		c.TenantID, c.ConnectionID, sha, c.AssetID); err != nil {
		return false, fmt.Errorf("add chunk reference: %w", err)
	}
	return written == nil, tx.Commit(ctx)
}

func (r *ChunkStorageRepository) MarkWritten(ctx context.Context, c ChunkCandidate, sha string, now time.Time) error {
	_, err := r.db.Pool.Exec(ctx, `UPDATE stored_chunks SET written_at = $4 WHERE tenant_id = $1 AND connection_id = $2 AND chunk_sha256 = $3`,
		c.TenantID, c.ConnectionID, sha, now)
	return err
}

// Publish makes readers use the chunks, only if the asset is still the object
// that was read: anything else and the chunks are not this asset any more.
func (r *ChunkStorageRepository) Publish(ctx context.Context, c ChunkCandidate, now time.Time) (bool, error) {
	tag, err := r.db.Pool.Exec(ctx, `UPDATE chunked_assets SET state = 'PUBLISHED', published_at = $3, checked_at = $3, reason = ''
		WHERE tenant_id = $1 AND asset_id = $2 AND state = 'WRITING' AND EXISTS (
		    SELECT 1 FROM assets a WHERE a.id = $2 AND a.tenant_id = $1 AND a.status = 'AVAILABLE' AND a.storage_etag = $4 AND a.object_key = $5)`,
		c.TenantID, c.AssetID, now, c.ETag, c.ObjectKey)
	if err != nil {
		return false, fmt.Errorf("publish chunks: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// Abandon takes an asset off its chunks after a conversion that cannot finish.
func (r *ChunkStorageRepository) Abandon(ctx context.Context, c ChunkCandidate, reason string, now time.Time) error {
	if _, err := r.db.Pool.Exec(ctx, `DELETE FROM chunk_refs WHERE tenant_id = $1 AND asset_id = $2`, c.TenantID, c.AssetID); err != nil {
		return err
	}
	return r.mark(ctx, c, "SKIPPED", reason, now)
}

// ChunkSource is a published chunked asset whose original object may go.
type ChunkSource struct {
	ChunkCandidate
	SourceKey   string
	Retired     bool
	ContentType string
}

// SourcesToRetire finds originals past their grace period. The asset must still
// be the object that was chunked; a hold since then keeps the original.
func (r *ChunkStorageRepository) SourcesToRetire(ctx context.Context, before time.Time, limit int) ([]ChunkSource, error) {
	rows, err := r.db.Pool.Query(ctx, `SELECT c.tenant_id, c.asset_id, c.connection_id, c.source_key
		FROM chunked_assets c JOIN assets a ON a.id = c.asset_id AND a.tenant_id = c.tenant_id
		WHERE c.state = 'PUBLISHED' AND c.source_retired_at IS NULL AND c.published_at <= $1
		  AND a.status = 'AVAILABLE' AND a.storage_etag = c.source_etag AND a.object_key = c.source_key
		  AND NOT EXISTS (SELECT 1 FROM object_locks l WHERE l.asset_id = a.id AND (l.legal_hold OR l.retain_until > now()))
		  AND NOT EXISTS (SELECT 1 FROM legal_holds h WHERE h.tenant_id = a.tenant_id AND h.released_at IS NULL
		        AND (h.scope = 'TENANT' OR (h.scope = 'DATASET' AND h.scope_key = a.dataset) OR (h.scope = 'ASSET' AND h.scope_key = a.id)))
		ORDER BY c.published_at LIMIT $2`, before, limit)
	if err != nil {
		return nil, fmt.Errorf("list chunk sources: %w", err)
	}
	defer rows.Close()
	out := []ChunkSource{}
	for rows.Next() {
		var s ChunkSource
		if err := rows.Scan(&s.TenantID, &s.AssetID, &s.ConnectionID, &s.SourceKey); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (r *ChunkStorageRepository) MarkSourceRetired(ctx context.Context, tenantID, assetID string, now time.Time) error {
	_, err := r.db.Pool.Exec(ctx, `UPDATE chunked_assets SET source_retired_at = $3 WHERE tenant_id = $1 AND asset_id = $2`, tenantID, assetID, now)
	return err
}

// ToRehydrate finds assets asked to become one object again.
func (r *ChunkStorageRepository) ToRehydrate(ctx context.Context, limit int) ([]ChunkSource, error) {
	rows, err := r.db.Pool.Query(ctx, `SELECT c.tenant_id, c.asset_id, c.connection_id, c.source_key, c.source_retired_at IS NOT NULL,
		a.object_key, a.size_bytes, COALESCE(a.checksum_sha256, ''), COALESCE(a.content_type, 'application/octet-stream')
		FROM chunked_assets c JOIN assets a ON a.id = c.asset_id AND a.tenant_id = c.tenant_id
		WHERE c.state = 'REHYDRATING' AND a.status = 'AVAILABLE' ORDER BY c.checked_at LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("list rehydrations: %w", err)
	}
	defer rows.Close()
	out := []ChunkSource{}
	for rows.Next() {
		var s ChunkSource
		if err := rows.Scan(&s.TenantID, &s.AssetID, &s.ConnectionID, &s.SourceKey, &s.Retired, &s.ObjectKey, &s.SizeBytes, &s.SHA256, &s.ContentType); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Rehydrated points the asset at its object again, if one was written, and
// takes it off its chunks, all or none. An asset rehydrated is not chunked again.
func (r *ChunkStorageRepository) Rehydrated(ctx context.Context, s ChunkSource, wrote bool, etag, versionID string, now time.Time) error {
	tx, err := r.db.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if wrote {
		if _, err := tx.Exec(ctx, `UPDATE assets SET storage_etag = $3, storage_version_id = NULLIF($4, ''), updated_at = $5 WHERE tenant_id = $1 AND id = $2`,
			s.TenantID, s.AssetID, etag, versionID, now); err != nil {
			return fmt.Errorf("point the asset at its object: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE chunked_assets SET state = 'RELEASED', released_at = $3 WHERE tenant_id = $1 AND asset_id = $2`, s.TenantID, s.AssetID, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM chunk_refs WHERE tenant_id = $1 AND asset_id = $2`, s.TenantID, s.AssetID); err != nil {
		return err
	}
	// A lock may have been waiting for this: now there is one object to put it on.
	if _, err := tx.Exec(ctx, `INSERT INTO lock_sync_requests (id, tenant_id, scope, scope_key, reason, created_at, updated_at)
		VALUES ($1, $2, 'ASSET', $3, 'kept as one object again', $4, $4)`, "rehydrated-"+s.AssetID+"-"+now.Format("20060102150405.000"), s.TenantID, s.AssetID, now); err != nil {
		return fmt.Errorf("queue the lock: %w", err)
	}
	return tx.Commit(ctx)
}

// UnusedChunk is a chunk's object nothing needs.
type UnusedChunk struct {
	TenantID, ConnectionID, SHA256, ObjectKey string
}

// UnusedChunks finds chunks with no references, older than before so that a
// conversion that has just added one is left alone.
func (r *ChunkStorageRepository) UnusedChunks(ctx context.Context, before time.Time, limit int) ([]UnusedChunk, error) {
	rows, err := r.db.Pool.Query(ctx, `SELECT tenant_id, connection_id, chunk_sha256, object_key FROM stored_chunks s
		WHERE (state = 'DELETING' OR created_at <= $1)
		  AND NOT EXISTS (SELECT 1 FROM chunk_refs x WHERE x.tenant_id = s.tenant_id AND x.connection_id = s.connection_id AND x.chunk_sha256 = s.chunk_sha256)
		LIMIT $2`, before, limit)
	if err != nil {
		return nil, fmt.Errorf("list unused chunks: %w", err)
	}
	defer rows.Close()
	out := []UnusedChunk{}
	for rows.Next() {
		var c UnusedChunk
		if err := rows.Scan(&c.TenantID, &c.ConnectionID, &c.SHA256, &c.ObjectKey); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ClaimUnused marks a chunk as going, after locking its row and looking again
// in a statement of its own: AddRef holds the same lock until its reference is
// in, so one of the two sees the other.
func (r *ChunkStorageRepository) ClaimUnused(ctx context.Context, c UnusedChunk) (bool, error) {
	tx, err := r.db.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var state string
	err = tx.QueryRow(ctx, `SELECT state FROM stored_chunks WHERE tenant_id = $1 AND connection_id = $2 AND chunk_sha256 = $3 FOR UPDATE`,
		c.TenantID, c.ConnectionID, c.SHA256).Scan(&state)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var used bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM chunk_refs WHERE tenant_id = $1 AND connection_id = $2 AND chunk_sha256 = $3)`,
		c.TenantID, c.ConnectionID, c.SHA256).Scan(&used); err != nil {
		return false, err
	}
	if used {
		return false, nil
	}
	if _, err := tx.Exec(ctx, `UPDATE stored_chunks SET state = 'DELETING' WHERE tenant_id = $1 AND connection_id = $2 AND chunk_sha256 = $3`,
		c.TenantID, c.ConnectionID, c.SHA256); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

func (r *ChunkStorageRepository) ForgetChunk(ctx context.Context, c UnusedChunk) error {
	_, err := r.db.Pool.Exec(ctx, `DELETE FROM stored_chunks WHERE tenant_id = $1 AND connection_id = $2 AND chunk_sha256 = $3 AND state = 'DELETING'`,
		c.TenantID, c.ConnectionID, c.SHA256)
	return err
}

// SetEnabled turns chunk storage on or off for a dataset. On also turns the
// index on, which it needs. Off stops new conversions and leaves what is
// chunked alone: rehydrating is a separate request.
func (r *ChunkStorageRepository) SetEnabled(ctx context.Context, tenantID, datasetID string, on bool) error {
	_, err := r.db.Pool.Exec(ctx, `UPDATE datasets SET chunk_storage_enabled = $3, chunking_enabled = chunking_enabled OR $3 WHERE tenant_id = $1 AND id = $2`, tenantID, datasetID, on)
	return err
}

// Stats counts what chunk storage holds for a dataset.
func (r *ChunkStorageRepository) Stats(ctx context.Context, tenantID, datasetID string) (model.ChunkStorageStats, error) {
	var s model.ChunkStorageStats
	err := r.db.Pool.QueryRow(ctx, `
		WITH ds AS (SELECT name, chunk_storage_enabled FROM datasets WHERE tenant_id = $1 AND id = $2),
		held AS (
		    SELECT a.id, a.size_bytes, c.source_retired_at, c.state
		    FROM ds JOIN assets a ON a.tenant_id = $1 AND a.dataset = ds.name
		    JOIN chunked_assets c ON c.asset_id = a.id AND c.state IN ('PUBLISHED', 'REHYDRATING')
		), chunks AS (
		    SELECT DISTINCT s.chunk_sha256, s.length FROM held
		    JOIN chunk_refs x ON x.asset_id = held.id
		    JOIN stored_chunks s ON s.tenant_id = x.tenant_id AND s.connection_id = x.connection_id AND s.chunk_sha256 = x.chunk_sha256
		)
		SELECT COALESCE((SELECT chunk_storage_enabled FROM ds), false),
		       (SELECT count(*) FROM held),
		       (SELECT COALESCE(sum(size_bytes), 0)::bigint FROM held),
		       (SELECT COALESCE(sum(size_bytes), 0)::bigint FROM held WHERE source_retired_at IS NOT NULL),
		       (SELECT COALESCE(sum(length), 0)::bigint FROM chunks),
		       (SELECT count(*) FROM chunks),
		       (SELECT count(*) FROM held WHERE state = 'REHYDRATING')`, tenantID, datasetID).Scan(
		&s.Enabled, &s.Files, &s.LogicalBytes, &s.RetiredSourceBytes, &s.ChunkBytes, &s.ChunkObjects, &s.Rehydrating)
	return s, err
}

// RehydrateDataset asks for every chunked asset of a dataset to become one
// object again, and stops new conversions of it.
func (r *ChunkStorageRepository) RehydrateDataset(ctx context.Context, tenantID, datasetID string, now time.Time) (int64, error) {
	if err := r.SetEnabled(ctx, tenantID, datasetID, false); err != nil {
		return 0, err
	}
	tag, err := r.db.Pool.Exec(ctx, `UPDATE chunked_assets c SET state = 'REHYDRATING', checked_at = $3
		FROM assets a JOIN datasets d ON d.tenant_id = a.tenant_id AND d.name = a.dataset
		WHERE a.id = c.asset_id AND c.state = 'PUBLISHED' AND c.tenant_id = $1 AND d.id = $2`, tenantID, datasetID, now)
	if err != nil {
		return 0, fmt.Errorf("request rehydration: %w", err)
	}
	return tag.RowsAffected(), nil
}

// BlockingPolicy says whether a dataset is under a retention policy or hold,
// whose objects should stay exactly where the provider's lock can reach them.
func (r *ChunkStorageRepository) BlockingPolicy(ctx context.Context, tenantID, datasetID string) (string, error) {
	var name string
	if err := r.db.Pool.QueryRow(ctx, `SELECT name FROM datasets WHERE tenant_id = $1 AND id = $2`, tenantID, datasetID).Scan(&name); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", model.ErrDatasetNotFound
		}
		return "", err
	}
	var held bool
	if err := r.db.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM legal_holds WHERE tenant_id = $1 AND released_at IS NULL
		AND (scope = 'TENANT' OR (scope = 'DATASET' AND scope_key IN ($2, $3))))`, tenantID, name, datasetID).Scan(&held); err != nil {
		return "", err
	}
	if held {
		return "a legal hold covers this dataset", nil
	}
	var retained bool
	if err := r.db.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM retention_policies WHERE tenant_id = $1
		AND (scope = 'TENANT' OR (scope = 'DATASET' AND scope_key IN ($2, $3))) AND COALESCE(min_retention_seconds, 0) > 0)`, tenantID, name, datasetID).Scan(&retained); err != nil {
		return "", err
	}
	if retained {
		return "a retention policy covers this dataset", nil
	}
	return "", nil
}
