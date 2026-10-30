-- Evidence and attestation (docs/evidence.md): audit chain anchors kept
-- outside the database, scheduled audit reviews with sign-off, lawful
-- basis and provenance recorded before snapshots and runs, and model
-- training summaries.

CREATE TABLE IF NOT EXISTS evidence_settings (
    tenant_id            TEXT PRIMARY KEY,
    -- Where chain anchors are written (a locked bucket is best); none
    -- keeps them in the database only.
    anchor_connection_id TEXT REFERENCES storage_connections (id),
    anchor_every_minutes INTEGER NOT NULL DEFAULT 60 CHECK (anchor_every_minutes BETWEEN 5 AND 1440),
    anchor_lock_days     INTEGER NOT NULL DEFAULT 2555 CHECK (anchor_lock_days BETWEEN 0 AND 36500),
    -- Exception reviews: how often, and what counts as after hours.
    review_every_days    INTEGER NOT NULL DEFAULT 7 CHECK (review_every_days BETWEEN 0 AND 365),
    work_start_hour      INTEGER NOT NULL DEFAULT 8 CHECK (work_start_hour BETWEEN 0 AND 23),
    work_end_hour        INTEGER NOT NULL DEFAULT 19 CHECK (work_end_hour BETWEEN 1 AND 24),
    timezone             TEXT NOT NULL DEFAULT 'UTC',
    bulk_read_threshold  INTEGER NOT NULL DEFAULT 200 CHECK (bulk_read_threshold BETWEEN 1 AND 1000000),
    -- Snapshots need a lawful basis, licence and source first; runs need
    -- them recorded before the snapshots they read.
    require_lawful_basis BOOLEAN NOT NULL DEFAULT false,
    updated_by           TEXT NOT NULL,
    updated_at           TIMESTAMPTZ NOT NULL
);

-- A chain head, signed and written where the database cannot rewrite it.
CREATE TABLE IF NOT EXISTS audit_anchors (
    id             TEXT PRIMARY KEY,
    chain          TEXT NOT NULL,
    seq            BIGINT NOT NULL,
    hash           BYTEA NOT NULL,
    -- The signed document, exactly, and its signature.
    body           TEXT NOT NULL,
    signature      TEXT NOT NULL DEFAULT '',
    key_id         TEXT NOT NULL DEFAULT '',
    connection_id  TEXT NOT NULL DEFAULT '',
    object_key     TEXT NOT NULL DEFAULT '',
    object_version TEXT NOT NULL DEFAULT '',
    locked_until   TIMESTAMPTZ,
    -- An RFC 3161 timestamp token over the body's SHA-256, when a TSA is set.
    tsa_token      BYTEA,
    created_at     TIMESTAMPTZ NOT NULL,
    UNIQUE (chain, seq)
);
CREATE INDEX IF NOT EXISTS ix_audit_anchors_chain ON audit_anchors (chain, created_at DESC);

-- What a reviewer must look at for a period, and their sign-off.
CREATE TABLE IF NOT EXISTS audit_reviews (
    id            TEXT PRIMARY KEY,
    tenant_id     TEXT NOT NULL,
    period_start  TIMESTAMPTZ NOT NULL,
    period_end    TIMESTAMPTZ NOT NULL,
    findings      JSONB NOT NULL,
    counts        JSONB NOT NULL,
    report_sha256 TEXT NOT NULL,
    state         TEXT NOT NULL CHECK (state IN ('OPEN', 'SIGNED')),
    signed_by     TEXT,
    signed_at     TIMESTAMPTZ,
    note          TEXT NOT NULL DEFAULT '' CHECK (char_length(note) <= 4000),
    created_by    TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS ix_audit_reviews_tenant ON audit_reviews (tenant_id, period_end DESC);

ALTER TABLE datasets
    ADD COLUMN IF NOT EXISTS lawful_basis TEXT CHECK (char_length(lawful_basis) <= 500),
    ADD COLUMN IF NOT EXISTS license TEXT CHECK (char_length(license) <= 500),
    ADD COLUMN IF NOT EXISTS source TEXT CHECK (char_length(source) <= 2000),
    ADD COLUMN IF NOT EXISTS source_kind TEXT CHECK (source_kind IN ('PUBLIC_DATASET', 'LICENSED_PRIVATE', 'WEB_CRAWLED', 'USER_DATA', 'SYNTHETIC', 'OTHER')),
    ADD COLUMN IF NOT EXISTS provenance_recorded_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS provenance_recorded_by TEXT;

ALTER TABLE models
    ADD COLUMN IF NOT EXISTS summary_published_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS summary_reminded_at TIMESTAMPTZ;

ALTER TABLE security_alerts DROP CONSTRAINT IF EXISTS security_alerts_kind_check;
ALTER TABLE security_alerts ADD CONSTRAINT security_alerts_kind_check CHECK (kind IN ('READ_SPIKE', 'MASS_LINKS',
    'NEW_NETWORK', 'EGRESS_BUDGET', 'STORAGE_POSTURE', 'LIFECYCLE_CHANGED', 'OUT_OF_BAND_WRITE', 'MASS_DELETION', 'SCRUB_MISMATCH',
    'COST_BUDGET', 'AUDIT_CHAIN_BROKEN'));

-- Webhook: a review is ready for its reviewer.
CREATE OR REPLACE FUNCTION webhook_audit_review() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    PERFORM queue_webhook(NEW.tenant_id, 'audit.review_due', jsonb_build_object(
        'review_id', NEW.id, 'period_start', NEW.period_start, 'period_end', NEW.period_end, 'counts', NEW.counts));
    RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS webhook_audit_review ON audit_reviews;
CREATE TRIGGER webhook_audit_review AFTER INSERT ON audit_reviews
    FOR EACH ROW EXECUTE FUNCTION webhook_audit_review();
