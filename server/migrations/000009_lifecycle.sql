BEGIN;

-- Datasets group assets for retention. Optional, chosen at upload time and
-- copied to the asset.
ALTER TABLE upload_sessions ADD COLUMN IF NOT EXISTS dataset TEXT;
ALTER TABLE assets ADD COLUMN IF NOT EXISTS dataset TEXT;

-- SOFT_DELETED: deletion requested. Never delivered, still restorable until
-- the deletion workflow starts removing its objects.
ALTER TABLE assets DROP CONSTRAINT IF EXISTS assets_status_check;
ALTER TABLE assets ADD CONSTRAINT assets_status_check CHECK (
    status IN ('QUARANTINED', 'PROCESSING', 'AVAILABLE', 'REJECTED', 'SOFT_DELETED', 'DELETED')
);

-- Retention evaluation and the missing-object check page through assets by
-- ID, optionally within one tenant.
CREATE INDEX IF NOT EXISTS ix_assets_tenant_id ON assets (tenant_id, id);

-- Retention policies are append-only: every change is a new version, and
-- the version in effect at a moment is the latest one whose effective_from
-- has passed. A NULL setting inherits from the broader scope.
CREATE TABLE IF NOT EXISTS retention_policies (
    id                           TEXT PRIMARY KEY,
    tenant_id                    TEXT NOT NULL,
    scope                        TEXT NOT NULL CHECK (scope IN ('TENANT', 'DATASET', 'ASSET')),
    scope_key                    TEXT NOT NULL DEFAULT '',
    version                      INTEGER NOT NULL CHECK (version > 0),
    min_retention_seconds        BIGINT CHECK (min_retention_seconds IS NULL OR min_retention_seconds >= 0),
    max_age_seconds              BIGINT CHECK (max_age_seconds IS NULL OR max_age_seconds >= 0),
    deletion_grace_seconds       BIGINT CHECK (deletion_grace_seconds IS NULL OR deletion_grace_seconds >= 0),
    quarantine_retention_seconds BIGINT CHECK (quarantine_retention_seconds IS NULL OR quarantine_retention_seconds > 0),
    effective_from               TIMESTAMPTZ NOT NULL,
    created_by                   TEXT NOT NULL,
    reason                       TEXT NOT NULL,
    created_at                   TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    UNIQUE (tenant_id, scope, scope_key, version),
    CHECK ((scope = 'TENANT') = (scope_key = ''))
);

CREATE INDEX IF NOT EXISTS ix_retention_policies_lookup
ON retention_policies (tenant_id, scope, scope_key, effective_from DESC, version DESC);

-- Legal holds stop every permanent deletion in their scope until released.
CREATE TABLE IF NOT EXISTS legal_holds (
    id          TEXT PRIMARY KEY,
    tenant_id   TEXT NOT NULL,
    scope       TEXT NOT NULL CHECK (scope IN ('TENANT', 'DATASET', 'ASSET')),
    scope_key   TEXT NOT NULL DEFAULT '',
    reason      TEXT NOT NULL,
    placed_by   TEXT NOT NULL,
    placed_at   TIMESTAMPTZ NOT NULL,
    released_by TEXT,
    released_at TIMESTAMPTZ,

    CHECK ((scope = 'TENANT') = (scope_key = '')),
    CHECK ((released_at IS NULL) = (released_by IS NULL))
);

CREATE INDEX IF NOT EXISTS ix_legal_holds_active
ON legal_holds (tenant_id, scope, scope_key)
WHERE released_at IS NULL;

-- The durable deletion workflow, one row per request. A worker claims a
-- row with a lease; every state change checks the lease, so two workers
-- never delete the same asset at once.
CREATE TABLE IF NOT EXISTS deletion_operations (
    id                 TEXT PRIMARY KEY,
    tenant_id          TEXT NOT NULL,
    asset_id           TEXT NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    state              TEXT NOT NULL CHECK (
                           state IN ('SCHEDULED', 'PROTECTED', 'IN_PROGRESS', 'RETRY_WAIT',
                                     'STORAGE_DELETED', 'COMPLETED', 'FAILED', 'CANCELLED')
                       ),
    reason             TEXT NOT NULL CHECK (reason IN ('USER_REQUEST', 'POLICY_EXPIRED', 'OPERATOR')),
    requested_by       TEXT NOT NULL,
    previous_status    TEXT NOT NULL,
    policy_detail      TEXT,
    purge_after        TIMESTAMPTZ NOT NULL,
    protected_reason   TEXT,
    attempt_count      INTEGER NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    max_attempts       INTEGER NOT NULL CHECK (max_attempts > 0),
    next_attempt_at    TIMESTAMPTZ NOT NULL,
    last_error         TEXT,
    lease_owner        TEXT,
    lease_expires_at   TIMESTAMPTZ,
    storage_deleted_at TIMESTAMPTZ,
    completed_at       TIMESTAMPTZ,
    created_at         TIMESTAMPTZ NOT NULL,
    updated_at         TIMESTAMPTZ NOT NULL,

    CHECK ((lease_owner IS NULL) = (lease_expires_at IS NULL))
);

-- At most one live deletion per asset: repeated requests and concurrent
-- schedulers cannot create a second one.
CREATE UNIQUE INDEX IF NOT EXISTS ux_deletion_operations_live
ON deletion_operations (asset_id)
WHERE state NOT IN ('COMPLETED', 'FAILED', 'CANCELLED');

CREATE INDEX IF NOT EXISTS ix_deletion_operations_due
ON deletion_operations (next_attempt_at)
WHERE state NOT IN ('COMPLETED', 'FAILED', 'CANCELLED');

CREATE INDEX IF NOT EXISTS ix_deletion_operations_tenant
ON deletion_operations (tenant_id, asset_id, created_at DESC);

-- Append-only audit of lifecycle decisions. Never holds URLs, credentials
-- or content. tenant_id is NULL only for platform-wide events.
CREATE TABLE IF NOT EXISTS lifecycle_events (
    id           TEXT PRIMARY KEY,
    tenant_id    TEXT,
    asset_id     TEXT,
    operation_id TEXT,
    event        TEXT NOT NULL,
    outcome      TEXT,
    reason       TEXT,
    actor        TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS ix_lifecycle_events_asset
ON lifecycle_events (tenant_id, asset_id, created_at);

CREATE INDEX IF NOT EXISTS ix_lifecycle_events_created
ON lifecycle_events (created_at);

-- Inventory scans: one bucket prefix, listed page by page. The cursor is
-- the provider's continuation token, so a restarted worker resumes where
-- the scan stopped.
CREATE TABLE IF NOT EXISTS inventory_scans (
    id                TEXT PRIMARY KEY,
    bucket            TEXT NOT NULL,
    prefix            TEXT NOT NULL,
    status            TEXT NOT NULL CHECK (status IN ('RUNNING', 'COMPLETED', 'FAILED')),
    cursor            TEXT,
    pages             INTEGER NOT NULL DEFAULT 0,
    objects_seen      BIGINT NOT NULL DEFAULT 0,
    candidates_marked INTEGER NOT NULL DEFAULT 0,
    errors            INTEGER NOT NULL DEFAULT 0,
    last_error        TEXT,
    lease_owner       TEXT,
    lease_expires_at  TIMESTAMPTZ,
    started_at        TIMESTAMPTZ NOT NULL,
    updated_at        TIMESTAMPTZ NOT NULL,
    completed_at      TIMESTAMPTZ,

    CHECK ((lease_owner IS NULL) = (lease_expires_at IS NULL))
);

-- One running scan per bucket prefix.
CREATE UNIQUE INDEX IF NOT EXISTS ux_inventory_scans_running
ON inventory_scans (bucket, prefix)
WHERE status = 'RUNNING';

CREATE INDEX IF NOT EXISTS ix_inventory_scans_recent
ON inventory_scans (bucket, prefix, started_at DESC);

-- Mark-and-sweep: an object seen without an owner is marked; it is swept
-- only after repeated sightings in completed scans and a safety interval,
-- and only if a fresh check still finds no owner.
CREATE TABLE IF NOT EXISTS orphan_candidates (
    id            TEXT PRIMARY KEY,
    bucket        TEXT NOT NULL,
    object_key    TEXT NOT NULL,
    etag          TEXT NOT NULL,
    size_bytes    BIGINT NOT NULL,
    category      TEXT NOT NULL CHECK (
                      category IN ('NO_UPLOAD_SESSION', 'NO_ASSET', 'DEAD_PROMOTION_COPY',
                                   'LEFTOVER_QUARANTINE_SOURCE', 'EXPIRED_FAILED_UPLOAD',
                                   'DELETED_ASSET_OBJECT')
                  ),
    tenant_id     TEXT NOT NULL,
    reason        TEXT NOT NULL,
    status        TEXT NOT NULL CHECK (status IN ('MARKED', 'DELETED', 'CLEARED', 'FAILED')),
    observations  INTEGER NOT NULL DEFAULT 1 CHECK (observations > 0),
    first_seen_at TIMESTAMPTZ NOT NULL,
    last_seen_at  TIMESTAMPTZ NOT NULL,
    last_scan_id  TEXT NOT NULL,
    attempt_count INTEGER NOT NULL DEFAULT 0,
    last_error    TEXT,
    resolved_at   TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL,
    updated_at    TIMESTAMPTZ NOT NULL
);

-- One open candidate per stored object.
CREATE UNIQUE INDEX IF NOT EXISTS ux_orphan_candidates_open
ON orphan_candidates (bucket, object_key)
WHERE status IN ('MARKED', 'FAILED');

CREATE INDEX IF NOT EXISTS ix_orphan_candidates_due
ON orphan_candidates (first_seen_at)
WHERE status IN ('MARKED', 'FAILED');

CREATE INDEX IF NOT EXISTS ix_orphan_candidates_resolved
ON orphan_candidates (resolved_at)
WHERE status IN ('DELETED', 'CLEARED');

-- Where resumable database scans (retention evaluation, missing-object
-- checks) continue.
CREATE TABLE IF NOT EXISTS lifecycle_cursors (
    name       TEXT PRIMARY KEY,
    position   TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);

-- One row per compaction rule run, for monitoring.
CREATE TABLE IF NOT EXISTS compaction_runs (
    id            TEXT PRIMARY KEY,
    rule          TEXT NOT NULL,
    status        TEXT NOT NULL CHECK (status IN ('COMPLETED', 'FAILED', 'CANCELLED')),
    rows_affected BIGINT NOT NULL,
    batches       INTEGER NOT NULL,
    cutoff        TIMESTAMPTZ NOT NULL,
    started_at    TIMESTAMPTZ NOT NULL,
    finished_at   TIMESTAMPTZ NOT NULL,
    last_error    TEXT
);

CREATE INDEX IF NOT EXISTS ix_compaction_runs_rule
ON compaction_runs (rule, started_at DESC);

-- Compaction deletes in bounded, ordered batches; these keep each batch an
-- index range scan instead of a table scan.
CREATE INDEX IF NOT EXISTS ix_reconciliation_tasks_succeeded
ON reconciliation_tasks (updated_at, id)
WHERE state = 'SUCCEEDED';

CREATE INDEX IF NOT EXISTS ix_reconciliation_findings_created
ON reconciliation_findings (created_at, id);

CREATE INDEX IF NOT EXISTS ix_download_tokens_expiry
ON download_tokens (expires_at, id);

CREATE INDEX IF NOT EXISTS ix_delivery_audit_created
ON delivery_audit_events (created_at, id);

CREATE INDEX IF NOT EXISTS ix_processing_events_created
ON processing_events (created_at, id);

CREATE INDEX IF NOT EXISTS ix_upload_sessions_idempotency_age
ON upload_sessions (created_at, id)
WHERE idempotancy_key IS NOT NULL;

CREATE INDEX IF NOT EXISTS ix_upload_sessions_verified_age
ON upload_sessions (updated_at, id)
WHERE status = 'VERIFIED';

COMMIT;
