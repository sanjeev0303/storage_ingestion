-- Chunk storage and delta uploads (docs/chunks.md): a file's bytes may be kept
-- as chunks stored once, and a new file may be built from what is already held.

ALTER TABLE datasets ADD COLUMN IF NOT EXISTS chunk_storage_enabled BOOLEAN NOT NULL DEFAULT false;

-- One chunk's object. A chunk is kept while a reference to it exists; the row
-- is locked when a reference is added and before it is deleted.
CREATE TABLE IF NOT EXISTS stored_chunks (
    tenant_id     TEXT NOT NULL,
    connection_id TEXT NOT NULL,
    chunk_sha256  TEXT NOT NULL,
    length        INTEGER NOT NULL,
    object_key    TEXT NOT NULL,
    state         TEXT NOT NULL CHECK (state IN ('LIVE', 'DELETING')),
    written_at    TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, connection_id, chunk_sha256)
);

CREATE TABLE IF NOT EXISTS chunked_assets (
    asset_id          TEXT PRIMARY KEY,
    tenant_id         TEXT NOT NULL,
    connection_id     TEXT NOT NULL,
    file_sha256       TEXT NOT NULL,
    -- SKIPPED: looked at, and it would not save enough; looked at again later.
    -- WRITING -> PUBLISHED, read from chunks -> REHYDRATING -> RELEASED.
    state             TEXT NOT NULL CHECK (state IN ('SKIPPED', 'WRITING', 'PUBLISHED', 'REHYDRATING', 'RELEASED')),
    reason            TEXT NOT NULL DEFAULT '',
    source_key        TEXT NOT NULL DEFAULT '',
    source_etag       TEXT NOT NULL DEFAULT '',
    source_retired_at TIMESTAMPTZ,
    checked_at        TIMESTAMPTZ NOT NULL,
    published_at      TIMESTAMPTZ,
    released_at       TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS ix_chunked_assets_state ON chunked_assets (state, checked_at);
CREATE INDEX IF NOT EXISTS ix_chunked_assets_file ON chunked_assets (tenant_id, file_sha256);

-- Which asset needs which chunk. A chunk with no row here may be deleted.
CREATE TABLE IF NOT EXISTS chunk_refs (
    tenant_id     TEXT NOT NULL,
    connection_id TEXT NOT NULL,
    chunk_sha256  TEXT NOT NULL,
    asset_id      TEXT NOT NULL,
    PRIMARY KEY (tenant_id, connection_id, chunk_sha256, asset_id)
);
CREATE INDEX IF NOT EXISTS ix_chunk_refs_asset ON chunk_refs (asset_id);

CREATE TABLE IF NOT EXISTS delta_uploads (
    id              TEXT PRIMARY KEY,
    tenant_id       TEXT NOT NULL,
    owner_id        TEXT NOT NULL,
    dataset_id      TEXT NOT NULL,
    namespace       TEXT NOT NULL,
    base_version_id TEXT NOT NULL,
    path            TEXT NOT NULL,
    size_bytes      BIGINT NOT NULL,
    sha256          TEXT NOT NULL,
    content_type    TEXT NOT NULL,
    connection_id   TEXT NOT NULL,
    state           TEXT NOT NULL CHECK (state IN ('OPEN', 'QUEUED', 'ASSEMBLING', 'DONE', 'FAILED', 'EXPIRED')),
    reused_bytes    BIGINT NOT NULL DEFAULT 0,
    sent_bytes      BIGINT NOT NULL DEFAULT 0,
    upload_id       TEXT NOT NULL DEFAULT '',
    error           TEXT NOT NULL DEFAULT '',
    attempts        INTEGER NOT NULL DEFAULT 0,
    lease_owner     TEXT,
    lease_until     TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL,
    expires_at      TIMESTAMPTZ NOT NULL,
    finished_at     TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS ix_delta_uploads_state ON delta_uploads (state, created_at);

CREATE TABLE IF NOT EXISTS delta_upload_chunks (
    upload_id       TEXT NOT NULL REFERENCES delta_uploads (id) ON DELETE CASCADE,
    ordinal         INTEGER NOT NULL,
    byte_offset     BIGINT NOT NULL,
    length          INTEGER NOT NULL,
    sha256          TEXT NOT NULL,
    -- Where the bytes come from: a file the base snapshot holds, or, with no
    -- source, the chunk the client sent.
    source_asset    TEXT,
    source_offset   BIGINT,
    received        BOOLEAN NOT NULL DEFAULT false,
    PRIMARY KEY (upload_id, ordinal)
);
