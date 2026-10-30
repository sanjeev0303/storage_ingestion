-- Adoption: registering objects that already sit in an organization's own
-- bucket, without listing the bucket and without uploading them again;
-- and importing objects from URL lists and STAC catalogs. Each adoption
-- job reads one source (a manifest, an S3 Inventory report, an ASC MHL,
-- a BagIt bag, a URL list or a STAC catalog) into entries, one per object,
-- and records each entry's outcome.

-- An adopted asset is published where it already is. DETACH: deleting the
-- asset removes the platform's record only, never the object. MANAGE: the
-- platform deletes the object like its own.
ALTER TABLE assets ADD COLUMN IF NOT EXISTS adoption TEXT CHECK (adoption IN ('DETACH', 'MANAGE'));
-- One live asset per adopted object.
CREATE UNIQUE INDEX IF NOT EXISTS ux_assets_adopted_key ON assets (tenant_id, storage_connection_id, object_key)
    WHERE adoption IS NOT NULL AND status NOT IN ('DELETED', 'REJECTED');

-- Digests computed when an object was read whole, beyond its SHA-256: what
-- MHL and BagIt exports reuse instead of reading it again.
CREATE TABLE IF NOT EXISTS asset_digests (
    asset_id   TEXT PRIMARY KEY REFERENCES assets (id) ON DELETE CASCADE,
    tenant_id  TEXT NOT NULL,
    digests    JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS adoption_jobs (
    id              TEXT PRIMARY KEY,
    tenant_id       TEXT NOT NULL,
    connection_id   TEXT NOT NULL,
    dataset         TEXT NOT NULL DEFAULT '',
    source          TEXT NOT NULL CHECK (source IN ('MANIFEST', 'S3_INVENTORY', 'MHL', 'BAGIT', 'URL_LIST', 'STAC')),
    -- Where the source is: an object key on the connection, or a URL for
    -- STAC; '' when it was sent inline.
    source_ref      TEXT NOT NULL DEFAULT '',
    inline_source   TEXT,
    mode            TEXT NOT NULL CHECK (mode IN ('DETACH', 'MANAGE')),
    -- Entries without an expected digest are refused unless false.
    require_digest  BOOLEAN NOT NULL DEFAULT true,
    state           TEXT NOT NULL CHECK (state IN ('QUEUED', 'READING', 'RUNNING', 'COMPLETED', 'FAILED', 'CANCELLED')),
    entries         BIGINT NOT NULL DEFAULT 0,
    adopted         BIGINT NOT NULL DEFAULT 0,
    rejected        BIGINT NOT NULL DEFAULT 0,
    failed          BIGINT NOT NULL DEFAULT 0,
    bytes           BIGINT NOT NULL DEFAULT 0,
    error           TEXT NOT NULL DEFAULT '',
    lease_owner     TEXT,
    lease_expires_at TIMESTAMPTZ,
    created_by      TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL,
    updated_at      TIMESTAMPTZ NOT NULL,
    finished_at     TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS ix_adoption_jobs_due ON adoption_jobs (updated_at) WHERE state IN ('QUEUED', 'READING', 'RUNNING');
CREATE INDEX IF NOT EXISTS ix_adoption_jobs_tenant ON adoption_jobs (tenant_id, created_at DESC);

CREATE TABLE IF NOT EXISTS adoption_entries (
    job_id        TEXT NOT NULL REFERENCES adoption_jobs (id) ON DELETE CASCADE,
    line          BIGINT NOT NULL,
    tenant_id     TEXT NOT NULL,
    -- The object on the connection (adoption), or the URL it comes from
    -- (import).
    object_key    TEXT NOT NULL DEFAULT '',
    url           TEXT NOT NULL DEFAULT '',
    path          TEXT NOT NULL,
    size_bytes    BIGINT NOT NULL DEFAULT 0,
    content_type  TEXT NOT NULL DEFAULT '',
    expected      JSONB NOT NULL DEFAULT '{}',
    metadata      JSONB,
    state         TEXT NOT NULL CHECK (state IN ('PENDING', 'ADOPTED', 'IMPORTED', 'REJECTED', 'FAILED', 'SKIPPED')),
    code          TEXT NOT NULL DEFAULT '',
    detail        TEXT NOT NULL DEFAULT '',
    asset_id      TEXT NOT NULL DEFAULT '',
    upload_id     TEXT NOT NULL DEFAULT '',
    sha256        TEXT NOT NULL DEFAULT '',
    attempts      INTEGER NOT NULL DEFAULT 0,
    updated_at    TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (job_id, line)
);
CREATE INDEX IF NOT EXISTS ix_adoption_entries_pending ON adoption_entries (job_id, line) WHERE state = 'PENDING';
