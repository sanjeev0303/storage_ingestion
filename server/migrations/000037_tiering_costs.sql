-- Lifecycle tiering and costs (docs/tiering.md): each asset's storage
-- tier, tier rules that skip pinned and held objects, restores of archived
-- objects, cost budgets, and snapshot expiry.

ALTER TABLE assets
    ADD COLUMN IF NOT EXISTS storage_tier TEXT NOT NULL DEFAULT 'STANDARD'
        CHECK (storage_tier IN ('STANDARD', 'INFREQUENT', 'ARCHIVE', 'DEEP_ARCHIVE')),
    ADD COLUMN IF NOT EXISTS tiered_at TIMESTAMPTZ,
    -- An archived asset is readable until then, once restored.
    ADD COLUMN IF NOT EXISTS readable_until TIMESTAMPTZ;
CREATE INDEX IF NOT EXISTS ix_assets_tier ON assets (tenant_id, storage_tier) WHERE storage_tier <> 'STANDARD';

CREATE TABLE IF NOT EXISTS tier_rules (
    id           TEXT PRIMARY KEY,
    tenant_id    TEXT NOT NULL,
    -- A rule for one dataset's members, or (empty) for every asset.
    dataset_id   TEXT NOT NULL DEFAULT '',
    name         TEXT NOT NULL CHECK (char_length(name) BETWEEN 1 AND 100),
    tier         TEXT NOT NULL CHECK (tier IN ('INFREQUENT', 'ARCHIVE', 'DEEP_ARCHIVE')),
    min_age_days INTEGER NOT NULL CHECK (min_age_days BETWEEN 0 AND 36500),
    unread_days  INTEGER NOT NULL CHECK (unread_days BETWEEN 0 AND 36500),
    enabled      BOOLEAN NOT NULL DEFAULT true,
    created_by   TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL,
    updated_at   TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS ix_tier_rules_tenant ON tier_rules (tenant_id) WHERE enabled;

-- Every move between tiers, and every asset a rule passed over and why.
CREATE TABLE IF NOT EXISTS tier_moves (
    id         TEXT PRIMARY KEY,
    tenant_id  TEXT NOT NULL,
    asset_id   TEXT NOT NULL,
    rule_id    TEXT NOT NULL DEFAULT '',
    from_tier  TEXT NOT NULL,
    to_tier    TEXT NOT NULL,
    size_bytes BIGINT NOT NULL,
    state      TEXT NOT NULL CHECK (state IN ('MOVED', 'FAILED')),
    reason     TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS ix_tier_moves_tenant ON tier_moves (tenant_id, created_at DESC);
CREATE INDEX IF NOT EXISTS ix_tier_moves_failed ON tier_moves (asset_id, created_at DESC) WHERE state = 'FAILED';

-- A temporary readable copy of an archived asset.
CREATE TABLE IF NOT EXISTS archive_restores (
    id           TEXT PRIMARY KEY,
    tenant_id    TEXT NOT NULL,
    asset_id     TEXT NOT NULL,
    -- The snapshot it was asked for, when it was.
    version_id   TEXT NOT NULL DEFAULT '',
    state        TEXT NOT NULL CHECK (state IN ('PENDING', 'RESTORING', 'READY', 'FAILED', 'EXPIRED')),
    days         INTEGER NOT NULL CHECK (days BETWEEN 1 AND 365),
    speed        TEXT NOT NULL CHECK (speed IN ('FAST', 'STANDARD', 'BULK')),
    error        TEXT NOT NULL DEFAULT '',
    requested_by TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL,
    updated_at   TIMESTAMPTZ NOT NULL,
    ready_at     TIMESTAMPTZ,
    readable_until TIMESTAMPTZ
);
CREATE UNIQUE INDEX IF NOT EXISTS ux_archive_restores_open ON archive_restores (asset_id) WHERE state IN ('PENDING', 'RESTORING');
CREATE INDEX IF NOT EXISTS ix_archive_restores_tenant ON archive_restores (tenant_id, created_at DESC);
CREATE INDEX IF NOT EXISTS ix_archive_restores_version ON archive_restores (tenant_id, version_id) WHERE version_id <> '';

-- An organization's monthly cost budget: estimated spend (storage,
-- requests and egress at list prices) and reads.
CREATE TABLE IF NOT EXISTS cost_budgets (
    tenant_id        TEXT PRIMARY KEY,
    monthly_usd      NUMERIC(14, 2) NOT NULL DEFAULT 0 CHECK (monthly_usd >= 0),
    monthly_requests BIGINT NOT NULL DEFAULT 0 CHECK (monthly_requests >= 0),
    action           TEXT NOT NULL CHECK (action IN ('ALERT', 'BLOCK')),
    updated_by       TEXT NOT NULL,
    updated_at       TIMESTAMPTZ NOT NULL
);

-- Snapshots can expire: the record stays (number, manifest hash, who and
-- when), its member list goes, and objects only it held become deletable.
ALTER TABLE dataset_versions ADD COLUMN IF NOT EXISTS expired_at TIMESTAMPTZ;
ALTER TABLE dataset_versions ADD COLUMN IF NOT EXISTS expired_by TEXT;

ALTER TABLE security_alerts DROP CONSTRAINT IF EXISTS security_alerts_kind_check;
ALTER TABLE security_alerts ADD CONSTRAINT security_alerts_kind_check CHECK (kind IN ('READ_SPIKE', 'MASS_LINKS',
    'NEW_NETWORK', 'EGRESS_BUDGET', 'STORAGE_POSTURE', 'LIFECYCLE_CHANGED', 'OUT_OF_BAND_WRITE', 'MASS_DELETION', 'SCRUB_MISMATCH',
    'COST_BUDGET'));

-- Webhook: an archived object is readable again, or its restore failed.
CREATE OR REPLACE FUNCTION webhook_archive_restore() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.state IN ('READY', 'FAILED') AND OLD.state IS DISTINCT FROM NEW.state THEN
        PERFORM queue_webhook(NEW.tenant_id, 'object.restored', jsonb_build_object(
            'restore_id', NEW.id, 'asset_id', NEW.asset_id, 'version_id', NEW.version_id, 'state', NEW.state,
            'readable_until', NEW.readable_until, 'error', NEW.error));
    END IF;
    RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS webhook_archive_restore ON archive_restores;
CREATE TRIGGER webhook_archive_restore AFTER UPDATE OF state ON archive_restores
    FOR EACH ROW EXECUTE FUNCTION webhook_archive_restore();

-- A pack moves between tiers as one object, once every file in it may.
ALTER TABLE packs
    ADD COLUMN IF NOT EXISTS storage_tier TEXT NOT NULL DEFAULT 'STANDARD'
        CHECK (storage_tier IN ('STANDARD', 'INFREQUENT', 'ARCHIVE', 'DEEP_ARCHIVE')),
    ADD COLUMN IF NOT EXISTS readable_until TIMESTAMPTZ;
