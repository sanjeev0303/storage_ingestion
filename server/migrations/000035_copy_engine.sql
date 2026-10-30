-- The copy engine (docs/copy-engine.md): replicas at a second connection,
-- migration, restore, export bundles and restore drills between
-- connections, scrubs that repair from a replica, clean points and escrow.

CREATE TABLE IF NOT EXISTS asset_replicas (
    tenant_id       TEXT NOT NULL,
    asset_id        TEXT NOT NULL,
    connection_id   TEXT NOT NULL REFERENCES storage_connections (id),
    object_key      TEXT NOT NULL,
    version_id      TEXT NOT NULL DEFAULT '',
    etag            TEXT NOT NULL DEFAULT '',
    size_bytes      BIGINT NOT NULL,
    checksum_sha256 TEXT NOT NULL,
    lock_mode       TEXT NOT NULL DEFAULT '',
    locked_until    TIMESTAMPTZ,
    replicated_at   TIMESTAMPTZ NOT NULL,
    verified_at     TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, asset_id, connection_id)
);
CREATE INDEX IF NOT EXISTS ix_asset_replicas_connection ON asset_replicas (connection_id);

-- Replicas that could not be made: the scan tries them again after a while.
CREATE TABLE IF NOT EXISTS replica_failures (
    tenant_id     TEXT NOT NULL,
    asset_id      TEXT NOT NULL,
    connection_id TEXT NOT NULL,
    error         TEXT NOT NULL,
    attempts      INTEGER NOT NULL DEFAULT 1,
    retry_after   TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, asset_id, connection_id)
);

ALTER TABLE datasets
    ADD COLUMN IF NOT EXISTS replica_connection_id TEXT REFERENCES storage_connections (id),
    ADD COLUMN IF NOT EXISTS escrow BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS vault_retention_days INTEGER NOT NULL DEFAULT 0 CHECK (vault_retention_days BETWEEN 0 AND 36500),
    ADD COLUMN IF NOT EXISTS scrub_days INTEGER NOT NULL DEFAULT 0 CHECK (scrub_days BETWEEN 0 AND 3650),
    ADD COLUMN IF NOT EXISTS clean_version_id TEXT;

DROP TRIGGER IF EXISTS tr_datasets_replica_tenant ON datasets;
CREATE TRIGGER tr_datasets_replica_tenant BEFORE INSERT OR UPDATE OF replica_connection_id
    ON datasets FOR EACH ROW EXECUTE FUNCTION storage_binding_tenant_check('replica_connection_id');

CREATE TABLE IF NOT EXISTS copy_jobs (
    id                   TEXT PRIMARY KEY,
    tenant_id            TEXT NOT NULL,
    kind                 TEXT NOT NULL CHECK (kind IN ('MIGRATE', 'RESTORE', 'EXPORT', 'DRILL')),
    dataset_id           TEXT NOT NULL DEFAULT '',
    version_id           TEXT NOT NULL DEFAULT '',
    source_connection_id TEXT NOT NULL DEFAULT '',
    target_connection_id TEXT NOT NULL DEFAULT '',
    target_prefix        TEXT NOT NULL DEFAULT '',
    options              JSONB NOT NULL DEFAULT '{}',
    state                TEXT NOT NULL CHECK (state IN ('QUEUED', 'RUNNING', 'COMPLETED', 'FAILED', 'CANCELLED')),
    planned              BOOLEAN NOT NULL DEFAULT false,
    total_items          BIGINT NOT NULL DEFAULT 0,
    done_items           BIGINT NOT NULL DEFAULT 0,
    failed_items         BIGINT NOT NULL DEFAULT 0,
    total_bytes          BIGINT NOT NULL DEFAULT 0,
    done_bytes           BIGINT NOT NULL DEFAULT 0,
    report               JSONB NOT NULL DEFAULT '{}',
    error                TEXT NOT NULL DEFAULT '',
    idempotency_key      TEXT,
    created_by           TEXT NOT NULL,
    created_at           TIMESTAMPTZ NOT NULL,
    updated_at           TIMESTAMPTZ NOT NULL,
    started_at           TIMESTAMPTZ,
    finished_at          TIMESTAMPTZ,
    lease_owner          TEXT,
    lease_expires_at     TIMESTAMPTZ
);
CREATE UNIQUE INDEX IF NOT EXISTS ux_copy_jobs_idempotency ON copy_jobs (tenant_id, idempotency_key) WHERE idempotency_key IS NOT NULL;
CREATE INDEX IF NOT EXISTS ix_copy_jobs_active ON copy_jobs (state, updated_at) WHERE state IN ('QUEUED', 'RUNNING');
CREATE INDEX IF NOT EXISTS ix_copy_jobs_tenant ON copy_jobs (tenant_id, created_at DESC);

CREATE TABLE IF NOT EXISTS copy_items (
    job_id            TEXT NOT NULL REFERENCES copy_jobs (id) ON DELETE CASCADE,
    ordinal           INTEGER NOT NULL,
    tenant_id         TEXT NOT NULL,
    asset_id          TEXT NOT NULL DEFAULT '',
    path              TEXT NOT NULL DEFAULT '',
    target_key        TEXT NOT NULL,
    size_bytes        BIGINT NOT NULL DEFAULT 0,
    checksum_sha256   TEXT NOT NULL DEFAULT '',
    state             TEXT NOT NULL CHECK (state IN ('PENDING', 'DONE', 'FAILED')),
    target_version_id TEXT NOT NULL DEFAULT '',
    target_etag       TEXT NOT NULL DEFAULT '',
    error             TEXT NOT NULL DEFAULT '',
    duration_ms       BIGINT NOT NULL DEFAULT 0,
    done_at           TIMESTAMPTZ,
    PRIMARY KEY (job_id, ordinal)
);
CREATE INDEX IF NOT EXISTS ix_copy_items_pending ON copy_items (job_id, ordinal) WHERE state = 'PENDING';

-- The last scrub of each asset: its bytes re-read and re-hashed.
CREATE TABLE IF NOT EXISTS scrub_results (
    tenant_id  TEXT NOT NULL,
    asset_id   TEXT NOT NULL,
    checked_at TIMESTAMPTZ NOT NULL,
    ok         BOOLEAN NOT NULL,
    detail     TEXT NOT NULL DEFAULT '',
    repaired   BOOLEAN NOT NULL DEFAULT false,
    PRIMARY KEY (tenant_id, asset_id)
);
CREATE INDEX IF NOT EXISTS ix_scrub_results_failed ON scrub_results (tenant_id, checked_at DESC) WHERE NOT ok;

-- An organization's scheduled export-everything bundle.
CREATE TABLE IF NOT EXISTS tenant_export_schedule (
    tenant_id      TEXT PRIMARY KEY,
    connection_id  TEXT NOT NULL REFERENCES storage_connections (id),
    interval_hours INTEGER NOT NULL CHECK (interval_hours BETWEEN 1 AND 8760),
    last_job_id    TEXT,
    last_run_at    TIMESTAMPTZ,
    updated_by     TEXT NOT NULL,
    updated_at     TIMESTAMPTZ NOT NULL
);

ALTER TABLE security_alerts DROP CONSTRAINT IF EXISTS security_alerts_kind_check;
ALTER TABLE security_alerts ADD CONSTRAINT security_alerts_kind_check CHECK (kind IN ('READ_SPIKE', 'MASS_LINKS',
    'NEW_NETWORK', 'EGRESS_BUDGET', 'STORAGE_POSTURE', 'LIFECYCLE_CHANGED', 'OUT_OF_BAND_WRITE', 'MASS_DELETION', 'SCRUB_MISMATCH'));

-- Webhooks: a scrub that found an object damaged or missing, and a copy job
-- that ended.
CREATE OR REPLACE FUNCTION webhook_scrub_result() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NOT NEW.ok THEN
        PERFORM queue_webhook(NEW.tenant_id, 'object.scrub_failed', jsonb_build_object(
            'asset_id', NEW.asset_id, 'detail', NEW.detail, 'repaired', NEW.repaired, 'checked_at', NEW.checked_at));
    END IF;
    RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS webhook_scrub_result ON scrub_results;
CREATE TRIGGER webhook_scrub_result AFTER INSERT OR UPDATE ON scrub_results
    FOR EACH ROW EXECUTE FUNCTION webhook_scrub_result();

CREATE OR REPLACE FUNCTION webhook_copy_job() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.state IN ('COMPLETED', 'FAILED') AND OLD.state IS DISTINCT FROM NEW.state THEN
        PERFORM queue_webhook(NEW.tenant_id, 'copy.job_finished', jsonb_build_object(
            'job_id', NEW.id, 'kind', NEW.kind, 'state', NEW.state, 'dataset_id', NEW.dataset_id,
            'done_items', NEW.done_items, 'failed_items', NEW.failed_items, 'error', NEW.error));
    END IF;
    RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS webhook_copy_job ON copy_jobs;
CREATE TRIGGER webhook_copy_job AFTER UPDATE OF state ON copy_jobs
    FOR EACH ROW EXECUTE FUNCTION webhook_copy_job();

-- An asset's storage binding stays fixed, except inside a migration's switch.
CREATE OR REPLACE FUNCTION storage_binding_tenant_check() RETURNS trigger AS $$
DECLARE
    column_name   TEXT;
    connection_id TEXT;
    previous_id   TEXT;
    owner         TEXT;
    state         TEXT;
    owning_tenant TEXT;
BEGIN
    IF TG_TABLE_NAME = 'organizations' THEN
        owning_tenant := NEW.id;
    ELSE
        owning_tenant := NEW.tenant_id;
    END IF;
    FOREACH column_name IN ARRAY TG_ARGV LOOP
        EXECUTE format('SELECT ($1).%I', column_name) USING NEW INTO connection_id;
        IF TG_OP = 'UPDATE' THEN
            EXECUTE format('SELECT ($1).%I', column_name) USING OLD INTO previous_id;
            IF previous_id IS NOT DISTINCT FROM connection_id THEN
                CONTINUE;
            END IF;
            -- Only a verified migration (docs/copy-engine.md) moves assets, in a
            -- transaction that says so.
            IF TG_TABLE_NAME IN ('upload_sessions', 'assets')
               AND NOT (TG_TABLE_NAME = 'assets' AND coalesce(current_setting('osp.migrating_assets', true), '') = 'on') THEN
                RAISE EXCEPTION 'the storage binding of % % cannot change', TG_TABLE_NAME, NEW.id
                    USING ERRCODE = 'integrity_constraint_violation', CONSTRAINT = 'storage_binding_immutable';
            END IF;
        END IF;
        IF connection_id IS NULL OR connection_id = 'platform' THEN
            CONTINUE;
        END IF;
        SELECT tenant_id, status INTO owner, state FROM storage_connections WHERE id = connection_id;
        IF owner IS DISTINCT FROM owning_tenant THEN
            RAISE EXCEPTION 'storage connection % does not belong to tenant %', connection_id, owning_tenant
                USING ERRCODE = 'foreign_key_violation', CONSTRAINT = 'storage_connection_tenant';
        END IF;
        IF state = 'DELETED' OR (state <> 'ACTIVE' AND TG_TABLE_NAME <> 'assets') THEN
            RAISE EXCEPTION 'storage connection % is %', connection_id, state
                USING ERRCODE = 'foreign_key_violation', CONSTRAINT = 'storage_connection_active';
        END IF;
    END LOOP;
    RETURN NEW;
END $$ LANGUAGE plpgsql;
