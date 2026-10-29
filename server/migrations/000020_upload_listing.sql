BEGIN;

-- GET /api/v1/uploads pages a tenant's upload sessions newest first.
CREATE INDEX IF NOT EXISTS ix_upload_sessions_listing
ON upload_sessions (tenant_id, created_at DESC, id DESC);

COMMIT;
