BEGIN;

-- Phase 10: managed datasets. A dataset (Phase 7's datasets row) gets a
-- lifecycle of its own beside its ingestion status, a draft membership of
-- catalog objects, immutable versions (snapshots) of exact object
-- versions, manifest imports, validation profiles, jobs, reports and
-- findings, and a lineage graph. Everything is additive: rolling back is
-- dropping the new tables and columns (docs/datasets.md).

-- The managed lifecycle. Phase 7's status stays the ingestion status.
ALTER TABLE datasets ADD COLUMN IF NOT EXISTS description TEXT NOT NULL DEFAULT ''
    CHECK (length(description) <= 2000);
ALTER TABLE datasets ADD COLUMN IF NOT EXISTS tags TEXT[] NOT NULL DEFAULT '{}'
    CHECK (cardinality(tags) <= 32);
ALTER TABLE datasets ADD COLUMN IF NOT EXISTS metadata JSONB NOT NULL DEFAULT '{}'
    CHECK (jsonb_typeof(metadata) = 'object' AND octet_length(metadata::text) <= 16384);
ALTER TABLE datasets ADD COLUMN IF NOT EXISTS lifecycle_status TEXT NOT NULL DEFAULT 'DRAFT'
    CHECK (lifecycle_status IN ('DRAFT', 'VALIDATING', 'READY', 'READY_WITH_WARNINGS', 'FAILED', 'ARCHIVED'));
-- revision changes with every change to the dataset row (If-Match);
-- members_revision with every membership change, so a snapshot knows
-- whether the draft still matches it.
ALTER TABLE datasets ADD COLUMN IF NOT EXISTS revision BIGINT NOT NULL DEFAULT 1;
ALTER TABLE datasets ADD COLUMN IF NOT EXISTS members_revision BIGINT NOT NULL DEFAULT 0;
ALTER TABLE datasets ADD COLUMN IF NOT EXISTS member_count BIGINT NOT NULL DEFAULT 0;
ALTER TABLE datasets ADD COLUMN IF NOT EXISTS next_version INTEGER NOT NULL DEFAULT 1;
ALTER TABLE datasets ADD COLUMN IF NOT EXISTS current_version_id TEXT;
ALTER TABLE datasets ADD COLUMN IF NOT EXISTS profile_id TEXT;
ALTER TABLE datasets ADD COLUMN IF NOT EXISTS profile_version INTEGER NOT NULL DEFAULT 0;
ALTER TABLE datasets ADD COLUMN IF NOT EXISTS latest_report_id TEXT;
-- A derived dataset records the snapshots it was created from and the
-- transformation, so its versions can name their inputs.
ALTER TABLE datasets ADD COLUMN IF NOT EXISTS derivation JSONB;
ALTER TABLE datasets ADD COLUMN IF NOT EXISTS updated_by TEXT;
ALTER TABLE datasets ADD COLUMN IF NOT EXISTS archived_at TIMESTAMPTZ;
-- Listing a tenant's datasets is by name (the unique (tenant_id, name)
-- index); filtered by lifecycle status:
CREATE INDEX IF NOT EXISTS ix_datasets_tenant_status ON datasets (tenant_id, lifecycle_status, name);

-- The draft membership: catalog objects, each at most once, optionally
-- pinned to one version (asset); unpinned members follow the object's
-- current version until a snapshot fixes it.
CREATE TABLE IF NOT EXISTS dataset_members (
    id          TEXT PRIMARY KEY,
    tenant_id   TEXT NOT NULL,
    dataset_id  TEXT NOT NULL,
    object_id   TEXT NOT NULL,
    asset_id    TEXT,
    label       TEXT CHECK (length(label) <= 256),
    split       TEXT CHECK (split IN ('train', 'validation', 'test')),
    metadata    JSONB NOT NULL DEFAULT '{}'
                CHECK (jsonb_typeof(metadata) = 'object' AND octet_length(metadata::text) <= 8192),
    -- Who added it: "api", "import:<id>" or "derived:<version id>".
    source      TEXT NOT NULL,
    added_by    TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS ux_dataset_members_object ON dataset_members (dataset_id, object_id);
-- Keyset pages of a dataset's members.
CREATE INDEX IF NOT EXISTS ix_dataset_members_page ON dataset_members (dataset_id, id);
-- The foreign keys to objects and assets are checked when either side
-- changes; these keep the checks from scanning.
CREATE INDEX IF NOT EXISTS ix_dataset_members_catalog ON dataset_members (object_id);
CREATE INDEX IF NOT EXISTS ix_dataset_members_asset ON dataset_members (asset_id) WHERE asset_id IS NOT NULL;

-- Immutable dataset versions. A version's member rows are written once,
-- in the transaction that creates it, and never changed.
CREATE TABLE IF NOT EXISTS dataset_versions (
    id                TEXT PRIMARY KEY,
    tenant_id         TEXT NOT NULL,
    dataset_id        TEXT NOT NULL,
    version           INTEGER NOT NULL CHECK (version > 0),
    status            TEXT NOT NULL CHECK (status IN ('FINALIZED')),
    -- The outcome of the latest finished validation of this version.
    validation_status TEXT NOT NULL DEFAULT 'NOT_VALIDATED'
                      CHECK (validation_status IN ('NOT_VALIDATED', 'READY', 'READY_WITH_WARNINGS', 'FAILED')),
    member_count      BIGINT NOT NULL,
    total_bytes       BIGINT NOT NULL,
    -- SHA-256 of the canonical manifest (docs/datasets.md).
    manifest_sha256   TEXT NOT NULL,
    manifest_format   TEXT NOT NULL,
    members_revision  BIGINT NOT NULL,
    profile_id        TEXT,
    profile_version   INTEGER NOT NULL DEFAULT 0,
    config            JSONB NOT NULL DEFAULT '{}',
    parent_version_id TEXT,
    derivation        JSONB,
    latest_report_id  TEXT,
    idempotency_key   TEXT,
    created_by        TEXT NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL,
    UNIQUE (dataset_id, version)
);
CREATE UNIQUE INDEX IF NOT EXISTS ux_dataset_versions_idempotency ON dataset_versions (dataset_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS ux_dataset_versions_id_tenant ON dataset_versions (id, tenant_id);

CREATE TABLE IF NOT EXISTS dataset_version_members (
    version_id      TEXT NOT NULL,
    tenant_id       TEXT NOT NULL,
    -- ordinal is the member's place in the canonical order (object ID).
    ordinal         BIGINT NOT NULL,
    object_id       TEXT NOT NULL,
    asset_id        TEXT NOT NULL,
    object_version  INTEGER NOT NULL,
    path            TEXT NOT NULL,
    label           TEXT,
    split           TEXT,
    size_bytes      BIGINT NOT NULL,
    content_type    TEXT NOT NULL,
    checksum_sha256 TEXT,
    -- The version's user metadata with the member's on top, and the
    -- member's own (what a derived dataset copies).
    metadata        JSONB NOT NULL DEFAULT '{}',
    member_metadata JSONB NOT NULL DEFAULT '{}',
    PRIMARY KEY (version_id, ordinal)
);
CREATE UNIQUE INDEX IF NOT EXISTS ux_dataset_version_members_object ON dataset_version_members (version_id, object_id);
-- Which snapshots hold an object version (lineage, deletion impact).
CREATE INDEX IF NOT EXISTS ix_dataset_version_members_asset ON dataset_version_members (asset_id);

-- Manifest imports: one job per manifest, read in checkpointed batches.
-- A small manifest arrives in the request and is kept here; a large one
-- is staged in object storage first.
CREATE TABLE IF NOT EXISTS dataset_manifest_imports (
    id                TEXT PRIMARY KEY,
    tenant_id         TEXT NOT NULL,
    dataset_id        TEXT NOT NULL,
    state             TEXT NOT NULL CHECK (state IN ('AWAITING_UPLOAD', 'QUEUED', 'RUNNING', 'COMPLETED', 'FAILED', 'CANCELLED')),
    source            TEXT NOT NULL CHECK (source IN ('INLINE', 'STAGED')),
    idempotency_key   TEXT NOT NULL,
    manifest_inline   BYTEA,
    manifest_key      TEXT,
    manifest_etag     TEXT,
    manifest_bytes    BIGINT NOT NULL DEFAULT 0,
    -- Declared by the client (checked) or computed from the inline body.
    manifest_sha256   TEXT,
    duplicate_policy  TEXT NOT NULL CHECK (duplicate_policy IN ('REJECT', 'SKIP', 'REPLACE')),
    lines_read        BIGINT NOT NULL DEFAULT 0,
    members_added     BIGINT NOT NULL DEFAULT 0,
    members_updated   BIGINT NOT NULL DEFAULT 0,
    members_skipped   BIGINT NOT NULL DEFAULT 0,
    lines_rejected    BIGINT NOT NULL DEFAULT 0,
    -- Where reading resumes: lines and bytes committed with their members.
    checkpoint_line   BIGINT NOT NULL DEFAULT 0,
    checkpoint_offset BIGINT NOT NULL DEFAULT 0,
    attempts          INTEGER NOT NULL DEFAULT 0,
    lease_owner       TEXT,
    lease_expires_at  TIMESTAMPTZ,
    next_attempt_at   TIMESTAMPTZ NOT NULL,
    cancel_requested  BOOLEAN NOT NULL DEFAULT false,
    last_error        TEXT,
    created_by        TEXT NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL,
    updated_at        TIMESTAMPTZ NOT NULL,
    started_at        TIMESTAMPTZ,
    completed_at      TIMESTAMPTZ
);
CREATE UNIQUE INDEX IF NOT EXISTS ux_dataset_imports_idempotency ON dataset_manifest_imports (dataset_id, idempotency_key);
CREATE INDEX IF NOT EXISTS ix_dataset_imports_due ON dataset_manifest_imports (next_attempt_at)
    WHERE state IN ('QUEUED', 'RUNNING');

-- Rejected manifest lines, by line number: a line read again after a
-- crash writes nothing new.
CREATE TABLE IF NOT EXISTS dataset_import_errors (
    import_id TEXT NOT NULL REFERENCES dataset_manifest_imports (id) ON DELETE CASCADE,
    line      BIGINT NOT NULL,
    code      TEXT NOT NULL,
    detail    TEXT NOT NULL,
    PRIMARY KEY (import_id, line)
);

-- Validation profiles: tenant-owned, versioned, immutable once written.
-- Built-in profiles live in code. A profile keeps its ID across versions.
CREATE TABLE IF NOT EXISTS validation_profiles (
    id          TEXT NOT NULL,
    version     INTEGER NOT NULL CHECK (version > 0),
    tenant_id   TEXT NOT NULL,
    name        TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 128),
    description TEXT NOT NULL DEFAULT '',
    definition  JSONB NOT NULL,
    created_by  TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (id, version)
);
CREATE UNIQUE INDEX IF NOT EXISTS ux_validation_profiles_name ON validation_profiles (tenant_id, name, version);

-- Validation jobs, run by the dataset worker in checkpointed batches.
CREATE TABLE IF NOT EXISTS validation_jobs (
    id                 TEXT PRIMARY KEY,
    tenant_id          TEXT NOT NULL,
    dataset_id         TEXT NOT NULL,
    dataset_version_id TEXT NOT NULL,
    profile_id         TEXT NOT NULL,
    profile_version    INTEGER NOT NULL,
    -- The profile as it was when the job was created: a report always
    -- says exactly which rules it applied.
    profile_snapshot   JSONB NOT NULL,
    mode               TEXT NOT NULL CHECK (mode IN ('METADATA', 'FULL', 'SAMPLED')),
    sample_size        INTEGER NOT NULL DEFAULT 0,
    readiness          BOOLEAN NOT NULL DEFAULT false,
    state              TEXT NOT NULL CHECK (state IN ('QUEUED', 'RUNNING', 'COMPLETED', 'COMPLETED_WITH_WARNINGS', 'FAILED', 'CANCELLED')),
    total_items        BIGINT NOT NULL DEFAULT 0,
    processed_items    BIGINT NOT NULL DEFAULT 0,
    passed_items       BIGINT NOT NULL DEFAULT 0,
    warning_items      BIGINT NOT NULL DEFAULT 0,
    failed_items       BIGINT NOT NULL DEFAULT 0,
    -- Members whose content checks were not run (metadata mode, outside
    -- the sample, or the bytes were unavailable).
    content_skipped    BIGINT NOT NULL DEFAULT 0,
    content_examined   BIGINT NOT NULL DEFAULT 0,
    bytes_total        BIGINT NOT NULL DEFAULT 0,
    bytes_read         BIGINT NOT NULL DEFAULT 0,
    -- Per-rule outcome counts of the member pass, merged batch by batch.
    rule_counts        JSONB NOT NULL DEFAULT '{}',
    checkpoint_ordinal BIGINT NOT NULL DEFAULT 0,
    attempts           INTEGER NOT NULL DEFAULT 0,
    max_attempts       INTEGER NOT NULL,
    lease_owner        TEXT,
    lease_expires_at   TIMESTAMPTZ,
    next_attempt_at    TIMESTAMPTZ NOT NULL,
    cancel_requested   BOOLEAN NOT NULL DEFAULT false,
    last_error         TEXT,
    report_id          TEXT,
    idempotency_key    TEXT,
    created_by         TEXT NOT NULL,
    created_at         TIMESTAMPTZ NOT NULL,
    started_at         TIMESTAMPTZ,
    completed_at       TIMESTAMPTZ,
    updated_at         TIMESTAMPTZ NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS ux_validation_jobs_idempotency ON validation_jobs (dataset_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;
CREATE INDEX IF NOT EXISTS ix_validation_jobs_due ON validation_jobs (next_attempt_at)
    WHERE state IN ('QUEUED', 'RUNNING');
CREATE INDEX IF NOT EXISTS ix_validation_jobs_dataset ON validation_jobs (dataset_id, created_at DESC, id);

CREATE TABLE IF NOT EXISTS validation_reports (
    id                 TEXT PRIMARY KEY,
    tenant_id          TEXT NOT NULL,
    job_id             TEXT NOT NULL UNIQUE,
    dataset_id         TEXT NOT NULL,
    dataset_version_id TEXT NOT NULL,
    profile_id         TEXT NOT NULL,
    profile_version    INTEGER NOT NULL,
    mode               TEXT NOT NULL,
    status             TEXT NOT NULL CHECK (status IN ('READY', 'READY_WITH_WARNINGS', 'FAILED')),
    readiness          BOOLEAN NOT NULL,
    schema_version     INTEGER NOT NULL,
    body               JSONB NOT NULL,
    summary            TEXT NOT NULL,
    started_at         TIMESTAMPTZ NOT NULL,
    completed_at       TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS ix_validation_reports_dataset ON validation_reports (dataset_id, completed_at DESC, id);

-- Findings: a stable ID per job, so a repeated batch writes nothing new.
CREATE TABLE IF NOT EXISTS validation_findings (
    job_id       TEXT NOT NULL REFERENCES validation_jobs (id),
    id           TEXT NOT NULL,
    tenant_id    TEXT NOT NULL,
    rule_id      TEXT NOT NULL,
    rule_version INTEGER NOT NULL,
    severity     TEXT NOT NULL CHECK (severity IN ('ERROR', 'WARNING', 'INFO')),
    code         TEXT NOT NULL,
    object_id    TEXT,
    asset_id     TEXT,
    message      TEXT NOT NULL,
    expected     TEXT,
    observed     TEXT,
    evidence     TEXT NOT NULL,
    remediation  TEXT,
    created_at   TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (job_id, id)
);
-- Pages of a report's findings, all or by severity.
CREATE INDEX IF NOT EXISTS ix_validation_findings_severity ON validation_findings (job_id, severity, id);

-- Lineage: nodes are references to records that live elsewhere; edges
-- are typed and unique, so writing one twice changes nothing.
CREATE TABLE IF NOT EXISTS lineage_nodes (
    id         TEXT PRIMARY KEY,
    tenant_id  TEXT NOT NULL,
    kind       TEXT NOT NULL CHECK (kind IN ('OBJECT_VERSION', 'DATASET', 'DATASET_VERSION', 'MANIFEST_IMPORT',
                                             'VALIDATION_REPORT', 'TRANSFORMATION')),
    ref_id     TEXT NOT NULL,
    -- A short, safe description: names, versions and hashes only.
    details    JSONB NOT NULL DEFAULT '{}' CHECK (octet_length(details::text) <= 4096),
    created_at TIMESTAMPTZ NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS ux_lineage_nodes_ref ON lineage_nodes (tenant_id, kind, ref_id);

CREATE TABLE IF NOT EXISTS lineage_edges (
    from_node  TEXT NOT NULL REFERENCES lineage_nodes (id),
    to_node    TEXT NOT NULL REFERENCES lineage_nodes (id),
    relation   TEXT NOT NULL CHECK (relation IN ('CONTAINS', 'DERIVED_FROM', 'IMPORTED_FROM', 'VALIDATED_BY',
                                                 'CREATED_FROM_SNAPSHOT', 'TRANSFORMED_BY')),
    tenant_id  TEXT NOT NULL,
    actor      TEXT NOT NULL,
    job_id     TEXT,
    details    JSONB NOT NULL DEFAULT '{}' CHECK (octet_length(details::text) <= 4096),
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (from_node, to_node, relation),
    CHECK (from_node <> to_node)
);
-- Upstream traversal follows edges backwards.
CREATE INDEX IF NOT EXISTS ix_lineage_edges_to ON lineage_edges (to_node, relation);

-- No reference crosses tenants.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'fk_dataset_members_dataset') THEN
        ALTER TABLE dataset_members ADD CONSTRAINT fk_dataset_members_dataset
            FOREIGN KEY (dataset_id, tenant_id) REFERENCES datasets (id, tenant_id);
        ALTER TABLE dataset_members ADD CONSTRAINT fk_dataset_members_object
            FOREIGN KEY (object_id, tenant_id) REFERENCES catalog_objects (id, tenant_id);
        ALTER TABLE dataset_members ADD CONSTRAINT fk_dataset_members_asset
            FOREIGN KEY (asset_id, tenant_id) REFERENCES assets (id, tenant_id);
        ALTER TABLE dataset_versions ADD CONSTRAINT fk_dataset_versions_dataset
            FOREIGN KEY (dataset_id, tenant_id) REFERENCES datasets (id, tenant_id);
        ALTER TABLE dataset_version_members ADD CONSTRAINT fk_dataset_version_members_version
            FOREIGN KEY (version_id, tenant_id) REFERENCES dataset_versions (id, tenant_id);
        ALTER TABLE dataset_version_members ADD CONSTRAINT fk_dataset_version_members_asset
            FOREIGN KEY (asset_id, tenant_id) REFERENCES assets (id, tenant_id);
        ALTER TABLE dataset_manifest_imports ADD CONSTRAINT fk_dataset_imports_dataset
            FOREIGN KEY (dataset_id, tenant_id) REFERENCES datasets (id, tenant_id);
        ALTER TABLE validation_jobs ADD CONSTRAINT fk_validation_jobs_version
            FOREIGN KEY (dataset_version_id, tenant_id) REFERENCES dataset_versions (id, tenant_id);
    END IF;
END $$;

COMMIT;
