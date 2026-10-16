BEGIN;

-- One asset per completed upload. It starts QUARANTINED and becomes
-- AVAILABLE only through promotion, after verification and scanning.
CREATE TABLE IF NOT EXISTS assets (
    id               TEXT PRIMARY KEY,
    tenant_id        TEXT NOT NULL,
    owner_id         TEXT NOT NULL,
    upload_id        TEXT NOT NULL UNIQUE REFERENCES upload_sessions(id) ON DELETE CASCADE,
    object_key       TEXT NOT NULL,
    file_name        TEXT NOT NULL,
    content_type     TEXT NOT NULL,
    size_bytes       BIGINT NOT NULL CHECK (size_bytes > 0),
    checksum_sha256  CHAR(64),
    status           TEXT NOT NULL CHECK (
                         status IN ('QUARANTINED', 'PROCESSING', 'AVAILABLE', 'REJECTED', 'DELETED')
                     ),
    rejection_reason TEXT,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CHECK (status <> 'REJECTED' OR rejection_reason IS NOT NULL),
    -- Quarantine objects never become deliverable in place.
    CHECK (status <> 'AVAILABLE' OR object_key NOT LIKE 'quarantine/%')
);

CREATE INDEX IF NOT EXISTS ix_assets_tenant_status
ON assets (tenant_id, status);

-- The durable processing queue: exactly one job per asset, holding the
-- result of every stage it has passed. Never holds presigned URLs,
-- credentials or file content.
CREATE TABLE IF NOT EXISTS processing_jobs (
    id                    TEXT PRIMARY KEY,
    tenant_id             TEXT NOT NULL,
    asset_id              TEXT NOT NULL UNIQUE REFERENCES assets(id) ON DELETE CASCADE,
    upload_id             TEXT NOT NULL,
    stage                 TEXT NOT NULL CHECK (stage IN ('VERIFY', 'SCAN', 'PROMOTE', 'CLEANUP')),
    state                 TEXT NOT NULL DEFAULT 'PENDING' CHECK (
                              state IN ('PENDING', 'SUCCEEDED', 'FAILED', 'EXHAUSTED')
                          ),
    attempt_count         INTEGER NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    max_attempts          INTEGER NOT NULL CHECK (max_attempts > 0),
    next_attempt_at       TIMESTAMPTZ NOT NULL,
    last_error            TEXT,
    lease_owner           TEXT,
    lease_expires_at      TIMESTAMPTZ,

    checksum_algorithm    TEXT,
    checksum_verified_at  TIMESTAMPTZ,
    detected_content_type TEXT,
    scanner               TEXT,
    scan_result           TEXT CHECK (scan_result IN ('CLEAN', 'INFECTED', 'NOT_SCANNED')),
    scan_signature        TEXT,
    scanned_at            TIMESTAMPTZ,
    promotion_key         TEXT,
    promoted_at           TIMESTAMPTZ,

    started_at            TIMESTAMPTZ,
    completed_at          TIMESTAMPTZ,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CHECK ((lease_owner IS NULL) = (lease_expires_at IS NULL))
);

CREATE INDEX IF NOT EXISTS ix_processing_jobs_due
ON processing_jobs (next_attempt_at)
WHERE state = 'PENDING';

-- Jobs an operator must look at.
CREATE INDEX IF NOT EXISTS ix_processing_jobs_exhausted
ON processing_jobs (tenant_id, updated_at DESC)
WHERE state = 'EXHAUSTED';

COMMIT;
