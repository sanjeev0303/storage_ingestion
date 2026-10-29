-- A retention policy or legal hold that changes queues its scope here, so
-- the cleanup worker applies S3 Object Lock to the assets it covers within
-- a cycle, ahead of the regular retention sweep that walks every asset.
-- The sweep still covers anything a lost request missed.

CREATE TABLE IF NOT EXISTS lock_sync_requests (
    id         TEXT PRIMARY KEY,
    tenant_id  TEXT NOT NULL,
    scope      TEXT NOT NULL CHECK (scope IN ('TENANT', 'DATASET', 'ASSET')),
    scope_key  TEXT NOT NULL DEFAULT '',
    -- The last asset applied, by ID; the next batch starts after it.
    position   TEXT NOT NULL DEFAULT '',
    reason     TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    done_at    TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS ix_lock_sync_pending ON lock_sync_requests (created_at, id) WHERE done_at IS NULL;
