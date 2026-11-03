package postgres

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// ChunkRepository keeps the chunk index (docs/chunks.md).
type ChunkRepository struct {
	db *DB
}

func NewChunkRepository(db *DB) *ChunkRepository {
	return &ChunkRepository{db: db}
}

func (r *ChunkRepository) SetEnabled(ctx context.Context, tenantID, datasetID string, on bool) error {
	_, err := r.db.Pool.Exec(ctx, `UPDATE datasets SET chunking_enabled = $3 WHERE tenant_id = $1 AND id = $2`, tenantID, datasetID, on)
	return err
}

func (r *ChunkRepository) Enabled(ctx context.Context, tenantID, datasetID string) (bool, error) {
	var on bool
	err := r.db.Pool.QueryRow(ctx, `SELECT chunking_enabled FROM datasets WHERE tenant_id = $1 AND id = $2`, tenantID, datasetID).Scan(&on)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, model.ErrDatasetNotFound
	}
	return on, err
}

// QueueNew queues the versions of datasets with chunking on that have no job.
func (r *ChunkRepository) QueueNew(ctx context.Context, now time.Time) (int64, error) {
	tag, err := r.db.Pool.Exec(ctx, `INSERT INTO chunk_jobs (version_id, tenant_id, dataset_id, state, created_at)
		SELECT v.id, v.tenant_id, v.dataset_id, 'PENDING', $1::timestamptz
		FROM dataset_versions v JOIN datasets d ON d.id = v.dataset_id AND d.tenant_id = v.tenant_id
		WHERE d.chunking_enabled AND v.expired_at IS NULL
		  AND NOT EXISTS (SELECT 1 FROM chunk_jobs j WHERE j.version_id = v.id)
		LIMIT 100
		ON CONFLICT (version_id) DO NOTHING`, now)
	if err != nil {
		return 0, fmt.Errorf("queue chunk jobs: %w", err)
	}
	return tag.RowsAffected(), nil
}

// Requeue asks for a version to be gone over again.
func (r *ChunkRepository) Requeue(ctx context.Context, tenantID, datasetID, versionID string, now time.Time) error {
	_, err := r.db.Pool.Exec(ctx, `INSERT INTO chunk_jobs (version_id, tenant_id, dataset_id, state, created_at) VALUES ($1, $2, $3, 'PENDING', $4)
		ON CONFLICT (version_id) DO UPDATE SET state = 'PENDING', attempts = 0, error = '', created_at = $4, finished_at = NULL
		WHERE chunk_jobs.state <> 'RUNNING'`, versionID, tenantID, datasetID, now)
	return err
}

const chunkJobColumns = `version_id, tenant_id, dataset_id, state, files_indexed, bytes_read, skipped, error, created_at, finished_at`

func scanChunkJob(row pgx.Row) (model.ChunkJob, error) {
	var j model.ChunkJob
	var skipped []byte
	err := row.Scan(&j.VersionID, &j.TenantID, &j.DatasetID, &j.State, &j.FilesIndexed, &j.BytesRead, &skipped, &j.Error, &j.CreatedAt, &j.FinishedAt)
	if err != nil {
		return j, err
	}
	return j, json.Unmarshal(skipped, &j.Skipped)
}

func (r *ChunkRepository) Job(ctx context.Context, tenantID, versionID string) (model.ChunkJob, bool, error) {
	j, err := scanChunkJob(r.db.Pool.QueryRow(ctx, `SELECT `+chunkJobColumns+` FROM chunk_jobs WHERE tenant_id = $1 AND version_id = $2`, tenantID, versionID))
	if errors.Is(err, pgx.ErrNoRows) {
		return j, false, nil
	}
	return j, err == nil, err
}

// Claim takes the oldest job, or one whose worker has gone.
func (r *ChunkRepository) Claim(ctx context.Context, owner string, lease time.Duration, maxAttempts int) (model.ChunkJob, bool, error) {
	if _, err := r.db.Pool.Exec(ctx, `UPDATE chunk_jobs SET state = 'FAILED', finished_at = now(), lease_owner = NULL,
		error = 'gave up after ' || attempts || ' attempts'
		WHERE state = 'RUNNING' AND lease_until < now() AND attempts >= $1`, maxAttempts); err != nil {
		return model.ChunkJob{}, false, fmt.Errorf("fail stuck chunk jobs: %w", err)
	}
	j, err := scanChunkJob(r.db.Pool.QueryRow(ctx, `UPDATE chunk_jobs SET state = 'RUNNING', lease_owner = $1,
		lease_until = now() + $2::interval, attempts = attempts + 1
		WHERE version_id = (SELECT version_id FROM chunk_jobs
		    WHERE state = 'PENDING' OR (state = 'RUNNING' AND lease_until < now())
		    ORDER BY created_at LIMIT 1 FOR UPDATE SKIP LOCKED)
		RETURNING `+chunkJobColumns, owner, fmt.Sprintf("%d seconds", int(lease.Seconds()))))
	if errors.Is(err, pgx.ErrNoRows) {
		return j, false, nil
	}
	if err != nil {
		return j, false, fmt.Errorf("claim chunk job: %w", err)
	}
	return j, true, nil
}

func (r *ChunkRepository) Extend(ctx context.Context, versionID, owner string, lease time.Duration) error {
	_, err := r.db.Pool.Exec(ctx, `UPDATE chunk_jobs SET lease_until = now() + $3::interval WHERE version_id = $1 AND lease_owner = $2 AND state = 'RUNNING'`,
		versionID, owner, fmt.Sprintf("%d seconds", int(lease.Seconds())))
	return err
}

// Finish records the outcome, if owner still holds the lease.
func (r *ChunkRepository) Finish(ctx context.Context, owner string, j model.ChunkJob) error {
	skipped, err := json.Marshal(j.Skipped)
	if err != nil {
		return err
	}
	_, err = r.db.Pool.Exec(ctx, `UPDATE chunk_jobs SET state = 'DONE', files_indexed = $3, bytes_read = $4, skipped = $5, error = '', finished_at = now(), lease_owner = NULL
		WHERE version_id = $1 AND lease_owner = $2`, j.VersionID, owner, j.FilesIndexed, j.BytesRead, skipped)
	return err
}

// HasFile tells whether these bytes were indexed already.
func (r *ChunkRepository) HasFile(ctx context.Context, tenantID, sha256 string) (bool, error) {
	var found bool
	err := r.db.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM chunked_files WHERE tenant_id = $1 AND sha256 = $2)`, tenantID, sha256).Scan(&found)
	return found, err
}

// PutFile records a file's chunks whole or not at all.
func (r *ChunkRepository) PutFile(ctx context.Context, tenantID, sha256 string, size int64, params string, chunks []model.RawChunk, now time.Time) error {
	tx, err := r.db.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	tag, err := tx.Exec(ctx, `INSERT INTO chunked_files (tenant_id, sha256, size_bytes, chunks, params, indexed_at) VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (tenant_id, sha256) DO NOTHING`, tenantID, sha256, size, len(chunks), params, now)
	if err != nil {
		return fmt.Errorf("put chunked file: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil // another worker got there
	}
	rows := make([][]any, len(chunks))
	for i, c := range chunks {
		rows[i] = []any{tenantID, sha256, i, c.Offset, c.Length, c.SHA256}
	}
	if _, err := tx.CopyFrom(ctx, pgx.Identifier{"file_chunks"}, []string{"tenant_id", "file_sha256", "ordinal", "byte_offset", "length", "chunk_sha256"}, pgx.CopyFromRows(rows)); err != nil {
		return fmt.Errorf("put chunks: %w", err)
	}
	return tx.Commit(ctx)
}

// Manifest answers one member's chunks, a page at a time, and the file's hash.
// found is false when the file has not been indexed.
func (r *ChunkRepository) Manifest(ctx context.Context, tenantID, versionID, path string, after, limit int) (string, []model.FileChunk, bool, error) {
	var sha string
	err := r.db.Pool.QueryRow(ctx, `SELECT COALESCE(checksum_sha256, '') FROM dataset_version_members WHERE tenant_id = $1 AND version_id = $2 AND path = $3`,
		tenantID, versionID, path).Scan(&sha)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil, false, model.ErrVersionNotFound
	}
	if err != nil {
		return "", nil, false, err
	}
	rows, err := r.db.Pool.Query(ctx, `SELECT ordinal, byte_offset, length, chunk_sha256 FROM file_chunks
		WHERE tenant_id = $1 AND file_sha256 = $2 AND ordinal >= $3 ORDER BY ordinal LIMIT $4`, tenantID, sha, after, limit)
	if err != nil {
		return sha, nil, false, fmt.Errorf("read manifest: %w", err)
	}
	defer rows.Close()
	out := []model.FileChunk{}
	for rows.Next() {
		var c model.FileChunk
		var raw []byte
		if err := rows.Scan(&c.Ordinal, &c.Offset, &c.Length, &raw); err != nil {
			return sha, nil, false, err
		}
		c.SHA256 = hex.EncodeToString(raw)
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return sha, nil, false, err
	}
	found := len(out) > 0
	if !found {
		if found, err = r.HasFile(ctx, tenantID, sha); err != nil {
			return sha, nil, false, err
		}
	}
	return sha, out, found, nil
}

// Savings counts what a dataset's unexpired versions hold.
//
// ponytail: computed on request, a few seconds per million chunks. Store it
// with the job if datasets get much larger than that.
func (r *ChunkRepository) Savings(ctx context.Context, tenantID, datasetID string) (model.ChunkSavings, error) {
	var s model.ChunkSavings
	err := r.db.Pool.QueryRow(ctx, `
		WITH members AS (
		    SELECT m.checksum_sha256 AS sha, m.size_bytes
		    FROM dataset_version_members m JOIN dataset_versions v ON v.id = m.version_id
		    WHERE m.tenant_id = $1 AND v.dataset_id = $2 AND v.expired_at IS NULL
		), distinct_files AS (
		    SELECT DISTINCT ON (COALESCE(sha, '')) sha, size_bytes FROM members
		), indexed AS (
		    SELECT d.sha, d.size_bytes FROM distinct_files d JOIN chunked_files c ON c.tenant_id = $1 AND c.sha256 = d.sha
		), distinct_chunks AS (
		    SELECT DISTINCT ON (f.chunk_sha256) f.length
		    FROM indexed i JOIN file_chunks f ON f.tenant_id = $1 AND f.file_sha256 = i.sha
		)
		SELECT (SELECT COALESCE(sum(size_bytes), 0)::bigint FROM members),
		       (SELECT COALESCE(sum(size_bytes), 0)::bigint FROM distinct_files),
		       (SELECT count(*) FROM indexed),
		       (SELECT COALESCE(sum(size_bytes), 0)::bigint FROM indexed),
		       (SELECT COALESCE(sum(length), 0)::bigint FROM distinct_chunks),
		       (SELECT count(*) FROM distinct_chunks),
		       (SELECT count(*) FROM distinct_files) - (SELECT count(*) FROM indexed)`, tenantID, datasetID).Scan(
		&s.LogicalBytes, &s.UniqueFileBytes, &s.IndexedFiles, &s.IndexedFileBytes, &s.UniqueChunkBytes, &s.UniqueChunks, &s.NotIndexedFiles)
	if err != nil {
		return s, fmt.Errorf("chunk savings: %w", err)
	}
	s.NotIndexedBytes = s.UniqueFileBytes - s.IndexedFileBytes
	s.WouldSaveBytes = s.IndexedFileBytes - s.UniqueChunkBytes
	return s, nil
}

// Delta counts what moving from one version to another needs.
func (r *ChunkRepository) Delta(ctx context.Context, tenantID, fromID, toID string, largest int) (model.ChunkDelta, error) {
	d := model.ChunkDelta{FromVersionID: fromID, ToVersionID: toID, LargestChanged: []model.DeltaFile{}}
	const changed = `
		WITH old AS (
		    SELECT DISTINCT checksum_sha256 AS sha FROM dataset_version_members WHERE tenant_id = $1 AND version_id = $2 AND checksum_sha256 IS NOT NULL
		), old_chunks AS (
		    SELECT DISTINCT f.chunk_sha256 FROM old JOIN file_chunks f ON f.tenant_id = $1 AND f.file_sha256 = old.sha
		), changed AS (
		    SELECT m.path, m.size_bytes, m.checksum_sha256 AS sha,
		           EXISTS (SELECT 1 FROM chunked_files c WHERE c.tenant_id = $1 AND c.sha256 = m.checksum_sha256) AS indexed,
		           COALESCE((SELECT sum(f.length) FROM file_chunks f JOIN old_chunks o ON o.chunk_sha256 = f.chunk_sha256
		                     WHERE f.tenant_id = $1 AND f.file_sha256 = m.checksum_sha256), 0)::bigint AS reused
		    FROM dataset_version_members m
		    WHERE m.tenant_id = $1 AND m.version_id = $3
		      AND (m.checksum_sha256 IS NULL OR m.checksum_sha256 NOT IN (SELECT sha FROM old))
		)`
	err := r.db.Pool.QueryRow(ctx, changed+`
		SELECT count(*), COALESCE(sum(size_bytes), 0)::bigint, COALESCE(sum(reused), 0)::bigint, count(*) FILTER (WHERE NOT indexed) FROM changed`,
		tenantID, fromID, toID).Scan(&d.ChangedFiles, &d.ChangedBytes, &d.ReusedBytes, &d.NotIndexed)
	if err != nil {
		return d, fmt.Errorf("delta: %w", err)
	}
	var files int64
	if err := r.db.Pool.QueryRow(ctx, `SELECT count(*), COALESCE(sum(size_bytes), 0)::bigint FROM dataset_version_members WHERE tenant_id = $1 AND version_id = $2`,
		tenantID, toID).Scan(&files, &d.TotalBytes); err != nil {
		return d, err
	}
	d.UnchangedFiles, d.UnchangedBytes = files-d.ChangedFiles, d.TotalBytes-d.ChangedBytes
	d.FetchBytes = d.ChangedBytes - d.ReusedBytes
	rows, err := r.db.Pool.Query(ctx, changed+`SELECT path, size_bytes, reused, indexed FROM changed ORDER BY size_bytes DESC, path LIMIT $4`, tenantID, fromID, toID, largest)
	if err != nil {
		return d, fmt.Errorf("delta files: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var f model.DeltaFile
		if err := rows.Scan(&f.Path, &f.SizeBytes, &f.ReusedBytes, &f.Indexed); err != nil {
			return d, err
		}
		d.LargestChanged = append(d.LargestChanged, f)
	}
	return d, rows.Err()
}

// Prune forgets the chunks of files no available object holds any more: after
// an erasure, even the hashes of its pieces should go.
func (r *ChunkRepository) Prune(ctx context.Context) (int64, error) {
	tag, err := r.db.Pool.Exec(ctx, `DELETE FROM chunked_files c WHERE NOT EXISTS (
		SELECT 1 FROM assets a WHERE a.tenant_id = c.tenant_id AND a.checksum_sha256 = c.sha256 AND a.status <> 'DELETED')
		AND NOT EXISTS (SELECT 1 FROM chunked_assets x WHERE x.tenant_id = c.tenant_id AND x.file_sha256 = c.sha256 AND x.state IN ('WRITING', 'PUBLISHED', 'REHYDRATING'))`)
	if err != nil {
		return 0, fmt.Errorf("prune chunks: %w", err)
	}
	return tag.RowsAffected(), nil
}
