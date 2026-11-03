-- Content-defined chunk index (docs/chunks.md): what each file's chunks are,
-- so that what chunk-level dedup would save can be measured and a new build
-- can be fetched as the difference from one already held. Bytes stay stored
-- as whole objects.

ALTER TABLE datasets ADD COLUMN IF NOT EXISTS chunking_enabled BOOLEAN NOT NULL DEFAULT false;

-- Files are keyed by content: the same bytes in two datasets are read once.
CREATE TABLE IF NOT EXISTS chunked_files (
    tenant_id  TEXT NOT NULL,
    sha256     TEXT NOT NULL,
    size_bytes BIGINT NOT NULL,
    chunks     INTEGER NOT NULL,
    params     TEXT NOT NULL,
    indexed_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, sha256)
);

CREATE TABLE IF NOT EXISTS file_chunks (
    tenant_id    TEXT NOT NULL,
    file_sha256  TEXT NOT NULL,
    ordinal      INTEGER NOT NULL,
    byte_offset  BIGINT NOT NULL,
    length       INTEGER NOT NULL,
    chunk_sha256 BYTEA NOT NULL,
    PRIMARY KEY (tenant_id, file_sha256, ordinal),
    FOREIGN KEY (tenant_id, file_sha256) REFERENCES chunked_files (tenant_id, sha256) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS ix_file_chunks_chunk ON file_chunks (tenant_id, chunk_sha256);

CREATE TABLE IF NOT EXISTS chunk_jobs (
    version_id    TEXT PRIMARY KEY,
    tenant_id     TEXT NOT NULL,
    dataset_id    TEXT NOT NULL,
    state         TEXT NOT NULL CHECK (state IN ('PENDING', 'RUNNING', 'DONE', 'FAILED')),
    files_indexed BIGINT NOT NULL DEFAULT 0,
    bytes_read    BIGINT NOT NULL DEFAULT 0,
    skipped       JSONB NOT NULL DEFAULT '[]'::jsonb,
    error         TEXT NOT NULL DEFAULT '',
    attempts      INTEGER NOT NULL DEFAULT 0,
    lease_owner   TEXT,
    lease_until   TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL,
    finished_at   TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS ix_chunk_jobs_state ON chunk_jobs (state, created_at);
