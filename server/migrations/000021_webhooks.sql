BEGIN;

-- Customer webhooks (docs/webhooks.md). An organization registers HTTPS
-- endpoints and the event types it wants; the webhook worker signs and
-- delivers each event to each subscribed endpoint, with retries.
CREATE TABLE IF NOT EXISTS webhook_endpoints (
    id                TEXT PRIMARY KEY,
    tenant_id         TEXT NOT NULL,
    url               TEXT NOT NULL CHECK (length(url) BETWEEN 1 AND 2048),
    description       TEXT NOT NULL DEFAULT '' CHECK (length(description) <= 500),
    event_types       TEXT[] NOT NULL CHECK (cardinality(event_types) > 0),
    enabled           BOOLEAN NOT NULL DEFAULT true,
    -- The signing secret, sealed with the credential keyring; never stored
    -- in the clear.
    secret_key_version INTEGER NOT NULL,
    secret_nonce      BYTEA NOT NULL,
    secret_ciphertext BYTEA NOT NULL,
    created_by        TEXT NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL,
    updated_at        TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS ix_webhook_endpoints_tenant ON webhook_endpoints (tenant_id, created_at);

-- One row per event and endpoint. The payload is identifiers and states
-- only: never file content, storage keys or URLs.
CREATE TABLE IF NOT EXISTS webhook_deliveries (
    id               TEXT PRIMARY KEY,
    event_id         TEXT NOT NULL,
    endpoint_id      TEXT NOT NULL REFERENCES webhook_endpoints (id) ON DELETE CASCADE,
    tenant_id        TEXT NOT NULL,
    event_type       TEXT NOT NULL,
    payload          JSONB NOT NULL CHECK (octet_length(payload::text) <= 16384),
    status           TEXT NOT NULL CHECK (status IN ('PENDING', 'DELIVERED', 'FAILED')),
    attempts         INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at  TIMESTAMPTZ NOT NULL,
    lease_owner      TEXT,
    lease_expires_at TIMESTAMPTZ,
    last_status_code INTEGER,
    last_error       TEXT,
    last_attempt_at  TIMESTAMPTZ,
    delivered_at     TIMESTAMPTZ,
    created_at       TIMESTAMPTZ NOT NULL,
    UNIQUE (endpoint_id, event_id),
    CHECK ((lease_owner IS NULL) = (lease_expires_at IS NULL))
);

CREATE INDEX IF NOT EXISTS ix_webhook_deliveries_due ON webhook_deliveries (next_attempt_at) WHERE status = 'PENDING';
CREATE INDEX IF NOT EXISTS ix_webhook_deliveries_endpoint ON webhook_deliveries (endpoint_id, created_at DESC, id DESC);

-- queue_webhook writes one delivery per enabled endpoint of the tenant that
-- subscribed to the type. Called from the triggers below, so an event is
-- queued in the very transaction that made the change: it exists exactly
-- when the change committed, whichever code path made it.
CREATE OR REPLACE FUNCTION queue_webhook(p_tenant TEXT, p_type TEXT, p_payload JSONB) RETURNS void
LANGUAGE plpgsql AS $$
DECLARE
    event_id TEXT := replace(gen_random_uuid()::text, '-', '');
BEGIN
    INSERT INTO webhook_deliveries (id, event_id, endpoint_id, tenant_id, event_type, payload, status, next_attempt_at, created_at)
    SELECT replace(gen_random_uuid()::text, '-', ''), event_id, e.id, p_tenant, p_type, p_payload, 'PENDING', now(), now()
    FROM webhook_endpoints e
    WHERE e.tenant_id = p_tenant AND e.enabled AND p_type = ANY (e.event_types);
    IF FOUND THEN
        PERFORM pg_notify('webhook_deliveries', '');
    END IF;
END;
$$;

CREATE OR REPLACE FUNCTION webhook_asset_status() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.status = 'AVAILABLE' THEN
        PERFORM queue_webhook(NEW.tenant_id, 'object.available', jsonb_build_object(
            'asset_id', NEW.id, 'object_id', NEW.object_id, 'version', NEW.version, 'namespace', COALESCE(NEW.dataset, ''),
            'file_name', NEW.file_name, 'size_bytes', NEW.size_bytes, 'checksum_sha256', NEW.checksum_sha256));
    ELSIF NEW.status = 'REJECTED' THEN
        PERFORM queue_webhook(NEW.tenant_id, 'object.rejected', jsonb_build_object(
            'asset_id', NEW.id, 'object_id', NEW.object_id, 'namespace', COALESCE(NEW.dataset, ''),
            'file_name', NEW.file_name, 'reason', NEW.rejection_reason));
    END IF;
    RETURN NULL;
END;
$$;

DROP TRIGGER IF EXISTS webhook_asset_status ON assets;
CREATE TRIGGER webhook_asset_status
    AFTER UPDATE OF status ON assets
    FOR EACH ROW
    WHEN (NEW.status IS DISTINCT FROM OLD.status AND NEW.status IN ('AVAILABLE', 'REJECTED'))
    EXECUTE FUNCTION webhook_asset_status();

CREATE OR REPLACE FUNCTION webhook_deletion_completed() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    PERFORM queue_webhook(NEW.tenant_id, 'object.erased', jsonb_build_object(
        'asset_id', NEW.asset_id, 'deletion_id', NEW.id, 'reason', NEW.reason));
    RETURN NULL;
END;
$$;

DROP TRIGGER IF EXISTS webhook_deletion_completed ON deletion_operations;
CREATE TRIGGER webhook_deletion_completed
    AFTER UPDATE OF state ON deletion_operations
    FOR EACH ROW
    WHEN (NEW.state = 'COMPLETED' AND OLD.state IS DISTINCT FROM 'COMPLETED')
    EXECUTE FUNCTION webhook_deletion_completed();

CREATE OR REPLACE FUNCTION webhook_ingestion_finished() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    PERFORM queue_webhook(NEW.tenant_id, 'ingestion.finished', jsonb_build_object(
        'job_id', NEW.id, 'dataset_id', NEW.dataset_id, 'state', NEW.state,
        'total_entries', NEW.total_entries, 'published_entries', NEW.published_entries,
        'failed_entries', NEW.failed_entries, 'stored_bytes', NEW.stored_bytes));
    RETURN NULL;
END;
$$;

DROP TRIGGER IF EXISTS webhook_ingestion_finished ON ingestion_jobs;
CREATE TRIGGER webhook_ingestion_finished
    AFTER UPDATE OF state ON ingestion_jobs
    FOR EACH ROW
    WHEN (NEW.state IS DISTINCT FROM OLD.state AND NEW.state IN ('COMPLETED', 'PARTIALLY_COMPLETED', 'FAILED', 'CANCELLED'))
    EXECUTE FUNCTION webhook_ingestion_finished();

CREATE OR REPLACE FUNCTION webhook_snapshot_created() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    PERFORM queue_webhook(NEW.tenant_id, 'dataset.snapshot_created', jsonb_build_object(
        'dataset_id', NEW.dataset_id, 'version_id', NEW.id, 'version', NEW.version,
        'member_count', NEW.member_count, 'total_bytes', NEW.total_bytes, 'manifest_sha256', NEW.manifest_sha256));
    RETURN NULL;
END;
$$;

DROP TRIGGER IF EXISTS webhook_snapshot_created ON dataset_versions;
CREATE TRIGGER webhook_snapshot_created
    AFTER INSERT ON dataset_versions
    FOR EACH ROW EXECUTE FUNCTION webhook_snapshot_created();

CREATE OR REPLACE FUNCTION webhook_validation_finished() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    PERFORM queue_webhook(NEW.tenant_id, 'dataset.validation_finished', jsonb_build_object(
        'dataset_id', NEW.dataset_id, 'version_id', NEW.dataset_version_id, 'job_id', NEW.id,
        'profile_id', NEW.profile_id, 'state', NEW.state, 'report_id', NEW.report_id,
        'passed_items', NEW.passed_items, 'warning_items', NEW.warning_items, 'failed_items', NEW.failed_items));
    RETURN NULL;
END;
$$;

DROP TRIGGER IF EXISTS webhook_validation_finished ON validation_jobs;
CREATE TRIGGER webhook_validation_finished
    AFTER UPDATE OF state ON validation_jobs
    FOR EACH ROW
    WHEN (NEW.state IS DISTINCT FROM OLD.state AND NEW.state IN ('COMPLETED', 'COMPLETED_WITH_WARNINGS', 'FAILED', 'CANCELLED'))
    EXECUTE FUNCTION webhook_validation_finished();

COMMIT;
