-- Access governance: scoped API tokens (also used as vendor workspaces),
-- a share-link and egress policy per organization, egress accounting,
-- sensitivity classification of assets, and security alerts from read
-- anomalies.

-- Where each read came from and with which credential, for baselines.
ALTER TABLE delivery_audit_events ADD COLUMN IF NOT EXISTS source_ip TEXT;
ALTER TABLE delivery_audit_events ADD COLUMN IF NOT EXISTS credential_id TEXT;
CREATE INDEX IF NOT EXISTS ix_delivery_audit_reads ON delivery_audit_events (tenant_id, principal_id, created_at DESC)
    WHERE outcome = 'ALLOWED';

ALTER TABLE download_tokens ADD COLUMN IF NOT EXISTS uses INTEGER NOT NULL DEFAULT 0;
ALTER TABLE download_tokens ADD COLUMN IF NOT EXISTS last_used_at TIMESTAMPTZ;
ALTER TABLE download_tokens ADD COLUMN IF NOT EXISTS revoked_by TEXT;

-- API tokens: a member's credential for scripts and vendors, never more
-- than the member's role allows, optionally confined to datasets and
-- networks, always expiring.
CREATE TABLE IF NOT EXISTS api_tokens (
    id           TEXT PRIMARY KEY,
    tenant_id    TEXT NOT NULL,
    user_id      TEXT NOT NULL,
    name         TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 100),
    token_hash   TEXT NOT NULL UNIQUE,
    hint         TEXT NOT NULL,
    permissions  BIGINT NOT NULL,
    datasets     TEXT[] NOT NULL DEFAULT '{}',
    networks     TEXT[] NOT NULL DEFAULT '{}',
    allow_links  BOOLEAN NOT NULL DEFAULT false,
    expires_at   TIMESTAMPTZ NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL,
    last_used_at TIMESTAMPTZ,
    last_used_ip TEXT,
    revoked_at   TIMESTAMPTZ,
    revoked_by   TEXT,
    CHECK (expires_at > created_at)
);
CREATE INDEX IF NOT EXISTS ix_api_tokens_tenant ON api_tokens (tenant_id, created_at DESC);

CREATE TABLE IF NOT EXISTS tenant_access_policy (
    tenant_id                TEXT PRIMARY KEY,
    links_enabled            BOOLEAN NOT NULL DEFAULT true,
    max_link_ttl_seconds     BIGINT NOT NULL DEFAULT 0 CHECK (max_link_ttl_seconds >= 0),
    egress_monthly_bytes     BIGINT NOT NULL DEFAULT 0 CHECK (egress_monthly_bytes >= 0),
    egress_daily_member_bytes BIGINT NOT NULL DEFAULT 0 CHECK (egress_daily_member_bytes >= 0),
    egress_action            TEXT NOT NULL DEFAULT 'WARN' CHECK (egress_action IN ('WARN', 'BLOCK')),
    mass_link_threshold      INTEGER NOT NULL DEFAULT 300 CHECK (mass_link_threshold > 0),
    anomaly_action           TEXT NOT NULL DEFAULT 'ALERT' CHECK (anomaly_action IN ('ALERT', 'REVOKE_LINKS')),
    updated_by               TEXT NOT NULL,
    updated_at               TIMESTAMPTZ NOT NULL
);

-- Bytes handed out per member and day: the size of each object a link was
-- issued for, and the bytes a proxied download served.
CREATE TABLE IF NOT EXISTS egress_usage (
    tenant_id    TEXT NOT NULL,
    day          DATE NOT NULL,
    principal_id TEXT NOT NULL,
    bytes        BIGINT NOT NULL DEFAULT 0,
    reads        BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (tenant_id, day, principal_id)
);

ALTER TABLE assets ADD COLUMN IF NOT EXISTS classification TEXT CHECK (classification IN ('PUBLIC', 'INTERNAL', 'CONFIDENTIAL', 'RESTRICTED'));

CREATE TABLE IF NOT EXISTS security_alerts (
    id              TEXT PRIMARY KEY,
    tenant_id       TEXT NOT NULL,
    principal_id    TEXT NOT NULL DEFAULT '',
    kind            TEXT NOT NULL CHECK (kind IN ('READ_SPIKE', 'MASS_LINKS', 'NEW_NETWORK', 'EGRESS_BUDGET')),
    detail          TEXT NOT NULL,
    metrics         JSONB NOT NULL DEFAULT '{}',
    action_taken    TEXT NOT NULL DEFAULT '',
    state           TEXT NOT NULL DEFAULT 'OPEN' CHECK (state IN ('OPEN', 'ACKNOWLEDGED')),
    dedup_key       TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL,
    acknowledged_by TEXT,
    acknowledged_at TIMESTAMPTZ,
    UNIQUE (tenant_id, dedup_key)
);
CREATE INDEX IF NOT EXISTS ix_security_alerts_open ON security_alerts (tenant_id, created_at DESC);

CREATE OR REPLACE FUNCTION webhook_security_alert() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    PERFORM queue_webhook(NEW.tenant_id, 'security.alert', jsonb_build_object(
        'alert_id', NEW.id, 'kind', NEW.kind, 'principal_id', NEW.principal_id, 'detail', NEW.detail,
        'action_taken', NEW.action_taken));
    RETURN NULL;
END;
$$;

DROP TRIGGER IF EXISTS webhook_security_alert ON security_alerts;
CREATE TRIGGER webhook_security_alert AFTER INSERT ON security_alerts
    FOR EACH ROW EXECUTE FUNCTION webhook_security_alert();
