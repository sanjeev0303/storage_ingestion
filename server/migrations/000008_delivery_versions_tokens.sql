BEGIN;

-- The exact stored object an AVAILABLE asset was published as. Delivery
-- reads conditionally on these, so bytes from a replaced object are never
-- served under the asset's metadata. Assets published before this
-- migration have NULLs and are checked against a fresh HEAD instead.
ALTER TABLE assets
ADD COLUMN IF NOT EXISTS storage_etag       TEXT,
ADD COLUMN IF NOT EXISTS storage_version_id TEXT,
ADD COLUMN IF NOT EXISTS published_at       TIMESTAMPTZ;

-- Request-level transfer details. Never URLs, tokens or content.
ALTER TABLE delivery_audit_events
ADD COLUMN IF NOT EXISTS request_id TEXT,
ADD COLUMN IF NOT EXISTS byte_range TEXT,
ADD COLUMN IF NOT EXISTS bytes_sent BIGINT;

ALTER TABLE delivery_audit_events DROP CONSTRAINT IF EXISTS delivery_audit_events_operation_check;
ALTER TABLE delivery_audit_events ADD CONSTRAINT delivery_audit_events_operation_check
    CHECK (operation IN ('METADATA', 'DOWNLOAD_URL', 'PROXY_DOWNLOAD', 'TOKEN_DOWNLOAD', 'TOKEN_REVOKE'));

-- Revocable download links. Only the SHA-256 of the token is stored, so a
-- database read does not yield a usable link.
CREATE TABLE IF NOT EXISTS download_tokens (
    id           TEXT PRIMARY KEY,
    token_hash   TEXT NOT NULL UNIQUE,
    tenant_id    TEXT NOT NULL,
    asset_id     TEXT NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    principal_id TEXT NOT NULL,
    filename     TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at   TIMESTAMPTZ NOT NULL,
    revoked_at   TIMESTAMPTZ,
    CHECK (expires_at > created_at)
);

-- Revoking every live link for an asset.
CREATE INDEX IF NOT EXISTS ix_download_tokens_asset
ON download_tokens (tenant_id, asset_id)
WHERE revoked_at IS NULL;

COMMIT;
