-- Edge and instrument ingest: devices with upload-only credentials, runs and
-- recordings that grow chunk by chunk, heartbeats (docs/edge.md).

-- A token confined to a path prefix inside its dataset.
ALTER TABLE api_tokens ADD COLUMN IF NOT EXISTS path_prefix TEXT NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS edge_devices (
    id            TEXT PRIMARY KEY,
    tenant_id     TEXT NOT NULL,
    name          TEXT NOT NULL CHECK (char_length(name) BETWEEN 1 AND 100),
    dataset_id    TEXT NOT NULL,
    namespace     TEXT NOT NULL,
    path_prefix   TEXT NOT NULL,
    token_id      TEXT NOT NULL,
    last_seen     TIMESTAMPTZ,
    pending_files BIGINT NOT NULL DEFAULT 0,
    pending_bytes BIGINT NOT NULL DEFAULT 0,
    failed_files  BIGINT NOT NULL DEFAULT 0,
    oldest_pending TIMESTAMPTZ,
    agent_version TEXT NOT NULL DEFAULT '',
    note          TEXT NOT NULL DEFAULT '',
    created_by    TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL,
    revoked_at    TIMESTAMPTZ,
    UNIQUE (tenant_id, name),
    FOREIGN KEY (dataset_id, tenant_id) REFERENCES datasets (id, tenant_id)
);
CREATE INDEX IF NOT EXISTS ix_edge_devices_token ON edge_devices (token_id);

-- A run (an instrument's folder, complete when its marker appears) or a
-- recording (rotating chunks that grow into one logical file).
CREATE TABLE IF NOT EXISTS edge_recordings (
    id             TEXT PRIMARY KEY,
    tenant_id      TEXT NOT NULL,
    device_id      TEXT NOT NULL REFERENCES edge_devices (id),
    name           TEXT NOT NULL CHECK (char_length(name) BETWEEN 1 AND 300),
    kind           TEXT NOT NULL CHECK (kind IN ('RUN', 'RECORDING')),
    state          TEXT NOT NULL CHECK (state IN ('OPEN', 'COMPLETE', 'INCOMPLETE')),
    gap_seconds    INTEGER NOT NULL DEFAULT 5 CHECK (gap_seconds >= 0),
    missing        JSONB NOT NULL DEFAULT '[]'::jsonb,
    version_id     TEXT,
    created_at     TIMESTAMPTZ NOT NULL,
    closed_at      TIMESTAMPTZ,
    UNIQUE (device_id, name)
);
CREATE INDEX IF NOT EXISTS ix_edge_recordings_tenant ON edge_recordings (tenant_id, created_at DESC);

CREATE TABLE IF NOT EXISTS edge_chunks (
    recording_id TEXT NOT NULL REFERENCES edge_recordings (id),
    tenant_id    TEXT NOT NULL,
    path         TEXT NOT NULL,
    sha256       TEXT NOT NULL,
    size_bytes   BIGINT NOT NULL,
    starts_at    TIMESTAMPTZ,
    ends_at      TIMESTAMPTZ,
    added_at     TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (recording_id, path)
);

CREATE OR REPLACE FUNCTION webhook_edge_recording() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.state <> 'OPEN' AND NEW.state IS DISTINCT FROM OLD.state THEN
        PERFORM queue_webhook(NEW.tenant_id, CASE NEW.state WHEN 'COMPLETE' THEN 'edge.run_complete' ELSE 'edge.run_incomplete' END,
            jsonb_build_object('recording_id', NEW.id, 'device_id', NEW.device_id, 'name', NEW.name, 'kind', NEW.kind,
                'version_id', NEW.version_id, 'missing', NEW.missing));
    END IF;
    RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS webhook_edge_recording ON edge_recordings;
CREATE TRIGGER webhook_edge_recording AFTER UPDATE ON edge_recordings
    FOR EACH ROW EXECUTE FUNCTION webhook_edge_recording();
