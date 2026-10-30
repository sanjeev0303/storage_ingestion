-- Retention and hold engine: COMPLIANCE locks, retention counted from a
-- date in the asset's metadata, holds by query (they cover future objects
-- too), proof of when locks took effect, two-person approvals, governance
-- profiles (GxP vault, SEC 17a-4 audit trail) and signed disposition
-- certificates.

-- A policy's Object Lock mode. COMPLIANCE cannot be shortened or removed
-- by anyone, the bucket's root account included.
ALTER TABLE retention_policies ADD COLUMN IF NOT EXISTS lock_mode TEXT CHECK (lock_mode IN ('GOVERNANCE', 'COMPLIANCE'));
-- Retention counts from this metadata date (trade_date, study_end), never
-- earlier than the asset's creation.
ALTER TABLE retention_policies ADD COLUMN IF NOT EXISTS retain_from_key TEXT;
ALTER TABLE retention_policies ADD COLUMN IF NOT EXISTS template TEXT;

ALTER TABLE object_locks ADD COLUMN IF NOT EXISTS mode TEXT;
-- The last read-back of the lock from the provider: MATCH, or what differs.
ALTER TABLE object_locks ADD COLUMN IF NOT EXISTS verified_at TIMESTAMPTZ;
ALTER TABLE object_locks ADD COLUMN IF NOT EXISTS verify_result TEXT;
CREATE INDEX IF NOT EXISTS ix_object_locks_verify ON object_locks (verified_at NULLS FIRST);

-- Holds by query: a filter evaluated whenever an asset is checked, so
-- objects created after the hold are covered too. scope_key names the
-- matter for QUERY holds.
ALTER TABLE legal_holds DROP CONSTRAINT IF EXISTS legal_holds_scope_check;
ALTER TABLE legal_holds ADD CONSTRAINT legal_holds_scope_check CHECK (scope IN ('TENANT', 'DATASET', 'ASSET', 'QUERY'));
ALTER TABLE legal_holds ADD COLUMN IF NOT EXISTS query JSONB;
ALTER TABLE legal_holds ADD COLUMN IF NOT EXISTS matter TEXT NOT NULL DEFAULT '';
ALTER TABLE legal_holds ADD COLUMN IF NOT EXISTS custodians TEXT[] NOT NULL DEFAULT '{}';
ALTER TABLE legal_holds DROP CONSTRAINT IF EXISTS legal_holds_query_check;
ALTER TABLE legal_holds ADD CONSTRAINT legal_holds_query_check CHECK ((scope = 'QUERY') = (query IS NOT NULL));

ALTER TABLE lock_sync_requests DROP CONSTRAINT IF EXISTS lock_sync_requests_scope_check;
ALTER TABLE lock_sync_requests ADD CONSTRAINT lock_sync_requests_scope_check CHECK (scope IN ('TENANT', 'DATASET', 'ASSET', 'QUERY'));

-- How an organization governs its records. A profile sets the rules below;
-- STANDARD leaves them to the settings.
CREATE TABLE IF NOT EXISTS tenant_governance (
    tenant_id      TEXT PRIMARY KEY,
    profile        TEXT NOT NULL DEFAULT 'STANDARD' CHECK (profile IN ('STANDARD', 'GXP_VAULT', 'SEC_17A4_AUDIT_TRAIL')),
    -- Shortening retention, releasing a hold, bulk deletes and weakening
    -- these settings need a second member's approval.
    two_person     BOOLEAN NOT NULL DEFAULT false,
    -- Every asset deletion by a member needs approval (GxP).
    approve_deletes BOOLEAN NOT NULL DEFAULT false,
    -- No policy may keep records shorter than this (17a-4: six years).
    retention_floor_seconds BIGINT NOT NULL DEFAULT 0 CHECK (retention_floor_seconds >= 0),
    updated_by     TEXT NOT NULL,
    updated_at     TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS governance_approvals (
    id            TEXT PRIMARY KEY,
    tenant_id     TEXT NOT NULL,
    kind          TEXT NOT NULL CHECK (kind IN ('RETENTION_CHANGE', 'HOLD_RELEASE', 'ASSET_DELETE', 'GOVERNANCE_CHANGE')),
    summary       TEXT NOT NULL,
    payload       JSONB NOT NULL,
    state         TEXT NOT NULL CHECK (state IN ('PENDING', 'EXECUTED', 'REJECTED', 'EXPIRED', 'FAILED')),
    reason        TEXT NOT NULL,
    requested_by  TEXT NOT NULL,
    requested_at  TIMESTAMPTZ NOT NULL,
    expires_at    TIMESTAMPTZ NOT NULL,
    decided_by    TEXT,
    decided_at    TIMESTAMPTZ,
    decision_note TEXT NOT NULL DEFAULT '',
    result        TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS ix_governance_approvals_pending ON governance_approvals (tenant_id, requested_at DESC) WHERE state = 'PENDING';

-- What was destroyed, when, under which policy, signed so it can be
-- checked without trusting the platform's database.
CREATE TABLE IF NOT EXISTS disposition_certificates (
    id           TEXT PRIMARY KEY,
    tenant_id    TEXT NOT NULL,
    deletion_id  TEXT NOT NULL UNIQUE,
    asset_id     TEXT NOT NULL,
    -- The exact bytes signed, kept as text: JSONB would reorder them.
    body         TEXT NOT NULL,
    body_sha256  TEXT NOT NULL,
    key_id       TEXT NOT NULL DEFAULT '',
    signature    TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS ix_disposition_certificates_asset ON disposition_certificates (tenant_id, asset_id);

-- A deletion held back by a hold, a retention period or a lock is a
-- webhook event, when it starts and whenever its reason changes.
CREATE OR REPLACE FUNCTION webhook_deletion_blocked() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    PERFORM queue_webhook(NEW.tenant_id, 'object.deletion_blocked', jsonb_build_object(
        'asset_id', NEW.asset_id, 'deletion_id', NEW.id, 'reason', COALESCE(NEW.protected_reason, '')));
    RETURN NULL;
END;
$$;

DROP TRIGGER IF EXISTS webhook_deletion_blocked ON deletion_operations;
CREATE TRIGGER webhook_deletion_blocked
    AFTER UPDATE OF state, protected_reason ON deletion_operations
    FOR EACH ROW
    WHEN (NEW.state = 'PROTECTED' AND (OLD.state IS DISTINCT FROM NEW.state OR OLD.protected_reason IS DISTINCT FROM NEW.protected_reason))
    EXECUTE FUNCTION webhook_deletion_blocked();
