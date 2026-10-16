BEGIN;

-- Append-only record of delivery decisions. Never holds issued URLs,
-- credentials or file content; reason is a sanitized code.
CREATE TABLE IF NOT EXISTS delivery_audit_events (
    id           TEXT PRIMARY KEY,
    tenant_id    TEXT NOT NULL,
    principal_id TEXT NOT NULL,
    asset_id     TEXT NOT NULL,
    operation    TEXT NOT NULL CHECK (operation IN ('METADATA', 'DOWNLOAD_URL', 'PROXY_DOWNLOAD')),
    outcome      TEXT NOT NULL CHECK (outcome IN ('ALLOWED', 'DENIED', 'FAILED')),
    reason       TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Per-asset history, and per-tenant review of denials.
CREATE INDEX IF NOT EXISTS ix_delivery_audit_asset
ON delivery_audit_events (tenant_id, asset_id, created_at DESC);

CREATE INDEX IF NOT EXISTS ix_delivery_audit_denied
ON delivery_audit_events (tenant_id, created_at DESC)
WHERE outcome <> 'ALLOWED';

COMMIT;
