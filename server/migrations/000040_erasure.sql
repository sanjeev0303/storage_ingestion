-- Erasure, withdrawal and destroy-by-provenance (docs/erasure.md).

-- Erasure deletes at once, without the grace period, under its own reason.
ALTER TABLE deletion_operations DROP CONSTRAINT IF EXISTS deletion_operations_reason_check;
ALTER TABLE deletion_operations ADD CONSTRAINT deletion_operations_reason_check
    CHECK (reason IN ('USER_REQUEST', 'POLICY_EXPIRED', 'OPERATOR', 'ERASURE'));

-- A snapshot an erasure replaced: its successor, and why. Superseded
-- snapshots are not read for training any more.
ALTER TABLE dataset_versions
    ADD COLUMN IF NOT EXISTS superseded_by TEXT,
    ADD COLUMN IF NOT EXISTS superseded_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS superseded_reason TEXT;

-- The subject index: members and objects naming a subject_id.
CREATE INDEX IF NOT EXISTS ix_assets_subject ON assets (tenant_id, (metadata->>'subject_id'))
    WHERE metadata ? 'subject_id';
CREATE INDEX IF NOT EXISTS ix_version_members_subject ON dataset_version_members
    (tenant_id, (COALESCE(member_metadata->>'subject_id', metadata->>'subject_id')))
    WHERE member_metadata ? 'subject_id' OR metadata ? 'subject_id';
CREATE INDEX IF NOT EXISTS ix_dataset_members_subject ON dataset_members (tenant_id, (metadata->>'subject_id'))
    WHERE metadata ? 'subject_id';
CREATE INDEX IF NOT EXISTS ix_version_members_checksum ON dataset_version_members (tenant_id, checksum_sha256)
    WHERE checksum_sha256 IS NOT NULL;

CREATE TABLE IF NOT EXISTS erasure_requests (
    id                    TEXT PRIMARY KEY,
    tenant_id             TEXT NOT NULL,
    kind                  TEXT NOT NULL CHECK (kind IN ('SUBJECT_WITHDRAWAL', 'COPYRIGHT', 'PII', 'CONSENT_EXPIRED', 'OTHER')),
    state                 TEXT NOT NULL CHECK (state IN ('PLANNED', 'APPROVED', 'DELETING', 'WAITING_ON_HOLD', 'COMPLETED', 'CANCELLED', 'FAILED')),
    reason                TEXT NOT NULL CHECK (char_length(reason) BETWEEN 1 AND 2000),
    reference             TEXT NOT NULL DEFAULT '' CHECK (char_length(reference) <= 200),
    institution           TEXT NOT NULL DEFAULT '' CHECK (char_length(institution) <= 200),
    -- Subjects in clear while the request is open; only their SHA-256
    -- once it is done.
    subject_ids           JSONB NOT NULL DEFAULT '[]'::jsonb,
    subject_hashes        JSONB NOT NULL DEFAULT '[]'::jsonb,
    sha256s               JSONB NOT NULL DEFAULT '[]'::jsonb,
    object_ids            JSONB NOT NULL DEFAULT '[]'::jsonb,
    subject_key           TEXT NOT NULL DEFAULT 'subject_id',
    plan                  JSONB NOT NULL DEFAULT '{}'::jsonb,
    successors            JSONB NOT NULL DEFAULT '[]'::jsonb,
    requested_by          TEXT NOT NULL,
    approved_by           TEXT,
    approved_note         TEXT NOT NULL DEFAULT '',
    approved_at           TIMESTAMPTZ,
    executed_by           TEXT,
    executed_at           TIMESTAMPTZ,
    completed_at          TIMESTAMPTZ,
    note                  TEXT NOT NULL DEFAULT '',
    certificate_body      TEXT,
    certificate_signature TEXT,
    certificate_key_id    TEXT,
    created_at            TIMESTAMPTZ NOT NULL,
    updated_at            TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS ix_erasure_requests_tenant ON erasure_requests (tenant_id, created_at DESC);
CREATE INDEX IF NOT EXISTS ix_erasure_requests_open ON erasure_requests (updated_at)
    WHERE state IN ('DELETING', 'WAITING_ON_HOLD', 'FAILED');

-- Each file an erasure deletes, and how its deletion went.
CREATE TABLE IF NOT EXISTS erasure_items (
    erasure_id     TEXT NOT NULL REFERENCES erasure_requests (id),
    tenant_id      TEXT NOT NULL,
    asset_id       TEXT NOT NULL,
    object_id      TEXT NOT NULL DEFAULT '',
    object_version INTEGER NOT NULL DEFAULT 0,
    path           TEXT NOT NULL DEFAULT '',
    sha256         TEXT NOT NULL DEFAULT '',
    size_bytes     BIGINT NOT NULL DEFAULT 0,
    connection_id  TEXT NOT NULL DEFAULT '',
    deletion_id    TEXT NOT NULL DEFAULT '',
    state          TEXT NOT NULL CHECK (state IN ('PENDING', 'DELETING', 'HELD', 'DESTROYED', 'FAILED')),
    reason         TEXT NOT NULL DEFAULT '',
    certificate_id TEXT NOT NULL DEFAULT '',
    destroyed_at   TIMESTAMPTZ,
    updated_at     TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (erasure_id, asset_id)
);

-- Webhook: an erasure finished, with the models to retrain.
CREATE OR REPLACE FUNCTION webhook_erasure() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.state = 'COMPLETED' AND OLD.state IS DISTINCT FROM 'COMPLETED' THEN
        PERFORM queue_webhook(NEW.tenant_id, 'erasure.completed', jsonb_build_object(
            'erasure_id', NEW.id, 'kind', NEW.kind, 'institution', NEW.institution, 'reference', NEW.reference,
            'models', NEW.plan->'models', 'successors', NEW.successors));
    END IF;
    RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS webhook_erasure ON erasure_requests;
CREATE TRIGGER webhook_erasure AFTER UPDATE ON erasure_requests
    FOR EACH ROW EXECUTE FUNCTION webhook_erasure();
