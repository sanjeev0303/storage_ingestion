BEGIN;

-- Datasets: a tenant's named collection of ingested files. The name is the
-- dataset label uploads and assets already carry, so retention policies
-- written for it apply to ingested files too.
CREATE TABLE IF NOT EXISTS datasets (
    id                TEXT PRIMARY KEY,
    tenant_id         TEXT NOT NULL,
    name              TEXT NOT NULL,
    completion_policy TEXT NOT NULL CHECK (completion_policy IN ('ALL', 'REQUIRED', 'PARTIAL')),
    status            TEXT NOT NULL CHECK (status IN ('OPEN', 'INGESTING', 'READY', 'PARTIALLY_READY', 'INCOMPLETE')),
    max_bytes         BIGINT NOT NULL DEFAULT 0 CHECK (max_bytes >= 0),
    reserved_bytes    BIGINT NOT NULL DEFAULT 0 CHECK (reserved_bytes >= 0),
    stored_bytes      BIGINT NOT NULL DEFAULT 0 CHECK (stored_bytes >= 0),
    created_by        TEXT NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL,
    updated_at        TIMESTAMPTZ NOT NULL,
    ready_at          TIMESTAMPTZ
);

CREATE UNIQUE INDEX IF NOT EXISTS ux_datasets_tenant_name ON datasets (tenant_id, name);
CREATE INDEX IF NOT EXISTS ix_datasets_tenant ON datasets (tenant_id, created_at DESC, id);

-- Ingestion jobs: one manifest's ingestion into a dataset. Entry counters
-- are kept in the same transaction as every entry change; reconciliation
-- recounts them from ingestion_entries.
CREATE TABLE IF NOT EXISTS ingestion_jobs (
    id                  TEXT PRIMARY KEY,
    tenant_id           TEXT NOT NULL,
    dataset_id          TEXT NOT NULL REFERENCES datasets (id),
    created_by          TEXT NOT NULL,
    idempotency_key     TEXT NOT NULL,
    state               TEXT NOT NULL CHECK (state IN (
                            'PENDING_VALIDATION', 'VALIDATING', 'READY', 'RUNNING',
                            'PARTIALLY_COMPLETED', 'COMPLETED', 'FAILED', 'CANCELLING', 'CANCELLED')),
    max_concurrency     INTEGER NOT NULL DEFAULT 0 CHECK (max_concurrency >= 0),

    manifest_source     TEXT CHECK (manifest_source IN ('INLINE', 'STAGED')),
    manifest_object_key TEXT,
    manifest_etag       TEXT,
    manifest_bytes      BIGINT NOT NULL DEFAULT 0,
    manifest_sha256     TEXT,
    manifest_complete   BOOLEAN NOT NULL DEFAULT FALSE,
    -- Reading checkpoint: the last line stored and the byte offset after it.
    parsed_lines        BIGINT NOT NULL DEFAULT 0 CHECK (parsed_lines >= 0),
    parsed_bytes        BIGINT NOT NULL DEFAULT 0 CHECK (parsed_bytes >= 0),
    -- Lease of the worker reading a staged manifest.
    lease_owner         TEXT,
    lease_expires_at    TIMESTAMPTZ,

    total_entries       BIGINT NOT NULL DEFAULT 0 CHECK (total_entries >= 0),
    invalid_entries     BIGINT NOT NULL DEFAULT 0 CHECK (invalid_entries >= 0),
    pending_entries     BIGINT NOT NULL DEFAULT 0 CHECK (pending_entries >= 0),
    scheduled_entries   BIGINT NOT NULL DEFAULT 0 CHECK (scheduled_entries >= 0),
    uploading_entries   BIGINT NOT NULL DEFAULT 0 CHECK (uploading_entries >= 0),
    uploaded_entries    BIGINT NOT NULL DEFAULT 0 CHECK (uploaded_entries >= 0),
    processing_entries  BIGINT NOT NULL DEFAULT 0 CHECK (processing_entries >= 0),
    published_entries   BIGINT NOT NULL DEFAULT 0 CHECK (published_entries >= 0),
    retryable_entries   BIGINT NOT NULL DEFAULT 0 CHECK (retryable_entries >= 0),
    failed_entries      BIGINT NOT NULL DEFAULT 0 CHECK (failed_entries >= 0),
    cancelled_entries   BIGINT NOT NULL DEFAULT 0 CHECK (cancelled_entries >= 0),
    required_failed     BIGINT NOT NULL DEFAULT 0 CHECK (required_failed >= 0),
    expected_bytes      BIGINT NOT NULL DEFAULT 0 CHECK (expected_bytes >= 0),
    stored_bytes        BIGINT NOT NULL DEFAULT 0 CHECK (stored_bytes >= 0),

    error               TEXT,
    created_at          TIMESTAMPTZ NOT NULL,
    updated_at          TIMESTAMPTZ NOT NULL,
    finalized_at        TIMESTAMPTZ,
    completed_at        TIMESTAMPTZ,
    cancelled_at        TIMESTAMPTZ,

    CHECK ((lease_owner IS NULL) = (lease_expires_at IS NULL))
);

CREATE UNIQUE INDEX IF NOT EXISTS ux_ingestion_jobs_idempotency ON ingestion_jobs (tenant_id, idempotency_key);
CREATE INDEX IF NOT EXISTS ix_ingestion_jobs_tenant ON ingestion_jobs (tenant_id, created_at DESC, id);
CREATE INDEX IF NOT EXISTS ix_ingestion_jobs_dataset ON ingestion_jobs (dataset_id);
-- Worker sweeps: jobs with work in progress.
CREATE INDEX IF NOT EXISTS ix_ingestion_jobs_active ON ingestion_jobs (state, updated_at)
WHERE state IN ('VALIDATING', 'READY', 'RUNNING', 'CANCELLING');

-- Manifest entries: one row per manifest line, identified by (job,
-- position). Paths are logical metadata, never storage keys: an entry's
-- object is stored under its upload session's quarantine key.
CREATE TABLE IF NOT EXISTS ingestion_entries (
    job_id            TEXT NOT NULL REFERENCES ingestion_jobs (id) ON DELETE CASCADE,
    position          BIGINT NOT NULL CHECK (position > 0),
    tenant_id         TEXT NOT NULL,
    dataset_id        TEXT NOT NULL,
    path              TEXT,
    path_key          TEXT,
    size_bytes        BIGINT NOT NULL DEFAULT 0 CHECK (size_bytes >= 0),
    checksum_sha256   TEXT,
    content_type      TEXT,
    metadata          JSONB CHECK (metadata IS NULL OR octet_length(metadata::text) <= 16384),
    required          BOOLEAN NOT NULL DEFAULT TRUE,
    strategy          TEXT CHECK (strategy IN ('SINGLE', 'MULTIPART')),
    state             TEXT NOT NULL CHECK (state IN (
                          'INVALID', 'PENDING', 'SCHEDULED', 'UPLOADING', 'UPLOADED',
                          'PROCESSING', 'PUBLISHED', 'FAILED_RETRYABLE', 'FAILED', 'CANCELLED')),
    upload_id         TEXT,
    upload_generation INTEGER NOT NULL DEFAULT 0 CHECK (upload_generation >= 0),
    asset_id          TEXT,
    attempts          INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    manual_retries    INTEGER NOT NULL DEFAULT 0 CHECK (manual_retries >= 0),
    next_attempt_at   TIMESTAMPTZ,
    lease_expires_at  TIMESTAMPTZ,
    reserved_bytes    BIGINT NOT NULL DEFAULT 0 CHECK (reserved_bytes >= 0),
    stored_bytes      BIGINT NOT NULL DEFAULT 0 CHECK (stored_bytes >= 0),
    error_code        TEXT,
    error_detail      TEXT,
    created_at        TIMESTAMPTZ NOT NULL,
    updated_at        TIMESTAMPTZ NOT NULL,

    PRIMARY KEY (job_id, position),
    CHECK (state = 'INVALID' OR (path IS NOT NULL AND path_key IS NOT NULL AND strategy IS NOT NULL))
);

-- Duplicate logical paths are rejected per dataset: one live entry per
-- path key. An invalid, failed or cancelled entry frees its path.
CREATE UNIQUE INDEX IF NOT EXISTS ux_ingestion_entries_path ON ingestion_entries (dataset_id, path_key)
WHERE state NOT IN ('INVALID', 'FAILED', 'CANCELLED');
-- One entry per upload session, and the outcome lookup by upload.
CREATE UNIQUE INDEX IF NOT EXISTS ux_ingestion_entries_upload ON ingestion_entries (upload_id)
WHERE upload_id IS NOT NULL;
-- Claims and keyset pagination by state within a job.
CREATE INDEX IF NOT EXISTS ix_ingestion_entries_state ON ingestion_entries (job_id, state, position);
-- Worker sweeps: expired upload leases, due retries, outcomes to check.
CREATE INDEX IF NOT EXISTS ix_ingestion_entries_lease ON ingestion_entries (lease_expires_at)
WHERE state IN ('SCHEDULED', 'UPLOADING');
CREATE INDEX IF NOT EXISTS ix_ingestion_entries_retry ON ingestion_entries (next_attempt_at)
WHERE state = 'FAILED_RETRYABLE';
CREATE INDEX IF NOT EXISTS ix_ingestion_entries_awaiting ON ingestion_entries (updated_at)
WHERE state IN ('UPLOADED', 'PROCESSING');

-- Per-tenant usage, changed only by conditional updates so concurrent jobs
-- cannot push a tenant past its quota. Reserved bytes are for uploads in
-- flight (declared sizes); stored bytes are published (verified sizes).
CREATE TABLE IF NOT EXISTS ingestion_tenant_usage (
    tenant_id           TEXT PRIMARY KEY,
    reserved_bytes      BIGINT NOT NULL DEFAULT 0 CHECK (reserved_bytes >= 0),
    stored_bytes        BIGINT NOT NULL DEFAULT 0 CHECK (stored_bytes >= 0),
    active_jobs         INTEGER NOT NULL DEFAULT 0 CHECK (active_jobs >= 0),
    active_uploads      INTEGER NOT NULL DEFAULT 0 CHECK (active_uploads >= 0),
    active_multipart    INTEGER NOT NULL DEFAULT 0 CHECK (active_multipart >= 0),
    pending_entries     BIGINT NOT NULL DEFAULT 0 CHECK (pending_entries >= 0),
    awaiting_processing BIGINT NOT NULL DEFAULT 0 CHECK (awaiting_processing >= 0),
    updated_at          TIMESTAMPTZ NOT NULL
);

-- Per-tenant overrides of the configured quota defaults.
CREATE TABLE IF NOT EXISTS ingestion_tenant_quotas (
    tenant_id           TEXT PRIMARY KEY,
    max_storage_bytes   BIGINT CHECK (max_storage_bytes IS NULL OR max_storage_bytes > 0),
    max_active_jobs     INTEGER CHECK (max_active_jobs IS NULL OR max_active_jobs > 0),
    max_active_uploads  INTEGER CHECK (max_active_uploads IS NULL OR max_active_uploads > 0),
    max_pending_entries BIGINT CHECK (max_pending_entries IS NULL OR max_pending_entries > 0),
    updated_at          TIMESTAMPTZ NOT NULL
);

-- The lock every claim takes, so the global upload-slot limit holds across
-- tenants. Slots in use are counted from the entries in flight
-- (ix_ingestion_entries_lease), never kept as a counter: finishing an upload
-- does not touch this row.
CREATE TABLE IF NOT EXISTS ingestion_capacity (
    name       TEXT PRIMARY KEY,
    updated_at TIMESTAMPTZ NOT NULL
);

INSERT INTO ingestion_capacity (name, updated_at)
VALUES ('upload_slots', NOW())
ON CONFLICT (name) DO NOTHING;

COMMIT;
