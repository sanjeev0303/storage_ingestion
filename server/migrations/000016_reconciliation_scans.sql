BEGIN;

-- Phase 12: the reconciliation worker's listing scans (provider multipart
-- sessions, and the staging objects of aborted or expired uploads) run on
-- every storage connection that holds uploads, and keep their progress in
-- inventory_scans beside the cleanup worker's object inventories of the
-- same prefixes. kind tells the scans apart; every scan before this
-- migration was an object inventory.
ALTER TABLE inventory_scans ADD COLUMN IF NOT EXISTS kind TEXT NOT NULL DEFAULT 'OBJECTS'
    CHECK (kind IN ('OBJECTS', 'UPLOAD_SESSIONS', 'UPLOAD_OBJECTS'));
DROP INDEX IF EXISTS ux_inventory_scans_running;
CREATE UNIQUE INDEX IF NOT EXISTS ux_inventory_scans_running
    ON inventory_scans (storage_connection_id, bucket, prefix, kind) WHERE status = 'RUNNING';

-- A finding names the connection it was found on: a tenant's bucket or
-- the platform's.
ALTER TABLE reconciliation_findings ADD COLUMN IF NOT EXISTS storage_connection_id TEXT NOT NULL DEFAULT 'platform'
    REFERENCES storage_connections (id);

COMMIT;
