BEGIN;

-- Each stage's own result on the processing job, so verification,
-- validation, scanning and promotion are separate, inspectable outcomes.
ALTER TABLE processing_jobs
ADD COLUMN IF NOT EXISTS verification_status TEXT,
ADD COLUMN IF NOT EXISTS checksum_source     TEXT,
ADD COLUMN IF NOT EXISTS validation_status   TEXT,
ADD COLUMN IF NOT EXISTS validation_reason   TEXT,
ADD COLUMN IF NOT EXISTS scan_error          TEXT,
ADD COLUMN IF NOT EXISTS promotion_status    TEXT;

ALTER TABLE processing_jobs DROP CONSTRAINT IF EXISTS processing_jobs_verification_status_check;
ALTER TABLE processing_jobs ADD CONSTRAINT processing_jobs_verification_status_check
    CHECK (verification_status IN ('PASSED', 'FAILED'));

-- DECLARED: compared with the checksum the client sent. COMPUTED: only
-- computed here, which proves nothing about the sender's original file.
ALTER TABLE processing_jobs DROP CONSTRAINT IF EXISTS processing_jobs_checksum_source_check;
ALTER TABLE processing_jobs ADD CONSTRAINT processing_jobs_checksum_source_check
    CHECK (checksum_source IN ('DECLARED', 'COMPUTED'));

ALTER TABLE processing_jobs DROP CONSTRAINT IF EXISTS processing_jobs_validation_status_check;
ALTER TABLE processing_jobs ADD CONSTRAINT processing_jobs_validation_status_check
    CHECK (validation_status IN ('PASSED', 'REJECTED'));

-- scan_result now also records incomplete attempts. Only CLEAN (or the
-- development-only NOT_SCANNED) ever allows promotion.
ALTER TABLE processing_jobs DROP CONSTRAINT IF EXISTS processing_jobs_scan_result_check;
ALTER TABLE processing_jobs ADD CONSTRAINT processing_jobs_scan_result_check
    CHECK (scan_result IN ('CLEAN', 'INFECTED', 'UNSUPPORTED', 'TIMEOUT', 'ERROR', 'NOT_SCANNED'));

ALTER TABLE processing_jobs DROP CONSTRAINT IF EXISTS processing_jobs_promotion_status_check;
ALTER TABLE processing_jobs ADD CONSTRAINT processing_jobs_promotion_status_check
    CHECK (promotion_status IN ('PENDING', 'PUBLISHED'));

-- Backlog and stuck-lease reporting.
CREATE INDEX IF NOT EXISTS ix_processing_jobs_leased
ON processing_jobs (lease_expires_at)
WHERE state = 'PENDING' AND lease_owner IS NOT NULL;

-- Per-tenant content rules. A tenant without a row uses the configured
-- default policy.
CREATE TABLE IF NOT EXISTS tenant_content_policies (
    tenant_id           TEXT PRIMARY KEY,
    allowed_types       TEXT[] NOT NULL DEFAULT '{}',
    denied_types        TEXT[] NOT NULL DEFAULT '{}',
    max_size_bytes      BIGINT CHECK (max_size_bytes IS NULL OR max_size_bytes > 0),
    unknown_type_action TEXT CHECK (unknown_type_action IN ('allow', 'reject')),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Durable quarantine. HELD: kept out of delivery until processing passes
-- (released) or an operator acts. REJECTED: permanent; the quarantined
-- object is purged after retain_until. Signatures stay internal.
CREATE TABLE IF NOT EXISTS quarantine_records (
    id             TEXT PRIMARY KEY,
    tenant_id      TEXT NOT NULL,
    asset_id       TEXT NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    job_id         TEXT NOT NULL,
    upload_id      TEXT NOT NULL,
    object_key     TEXT NOT NULL,
    category       TEXT NOT NULL CHECK (
                       category IN ('VERIFICATION_FAILED', 'VALIDATION_REJECTED', 'MALWARE_DETECTED',
                                    'SCAN_INCOMPLETE', 'PROCESSING_EXHAUSTED')
                   ),
    reason         TEXT NOT NULL,
    scan_result    TEXT,
    scan_signature TEXT,
    status         TEXT NOT NULL CHECK (status IN ('HELD', 'RELEASED', 'REJECTED', 'PURGED')),
    retain_until   TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    resolved_at    TIMESTAMPTZ
);

-- At most one open (HELD) record per asset.
CREATE UNIQUE INDEX IF NOT EXISTS ux_quarantine_records_held
ON quarantine_records (asset_id)
WHERE status = 'HELD';

CREATE INDEX IF NOT EXISTS ix_quarantine_records_tenant
ON quarantine_records (tenant_id, created_at DESC);

-- Rejected records whose object is due for purge.
CREATE INDEX IF NOT EXISTS ix_quarantine_records_purge
ON quarantine_records (retain_until)
WHERE status = 'REJECTED';

-- Append-only audit trail of processing decisions. Never holds file
-- content, URLs or credentials.
CREATE TABLE IF NOT EXISTS processing_events (
    id         TEXT PRIMARY KEY,
    tenant_id  TEXT NOT NULL,
    asset_id   TEXT NOT NULL,
    job_id     TEXT,
    event      TEXT NOT NULL,
    detail     TEXT,
    actor      TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS ix_processing_events_asset
ON processing_events (tenant_id, asset_id, created_at);

COMMIT;
