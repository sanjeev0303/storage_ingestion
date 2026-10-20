BEGIN;

-- ABORTED: a client abort confirmed at the provider. Until now an abort
-- ended as FAILED, which also meant "unrecoverable failure".
ALTER TABLE upload_sessions
DROP CONSTRAINT IF EXISTS upload_sessions_status_check;

ALTER TABLE upload_sessions
ADD CONSTRAINT upload_sessions_status_check CHECK (
    status IN (
        'PENDING',
        'UPLOADING',
        'COMPLETING',
        'COMPLETED',
        'VERIFIED',
        'ABORTING',
        'ABORTED',
        'FAILED',
        'EXPIRED'
    )
);

-- Earlier client aborts were recorded as FAILED with this reason.
UPDATE upload_sessions
SET status = 'ABORTED'
WHERE status = 'FAILED' AND failure_reason = 'aborted_by_client';

-- When completion, and abort or expiry, were confirmed. Nullable: older
-- rows simply have no timestamp.
ALTER TABLE upload_sessions
ADD COLUMN IF NOT EXISTS completed_at TIMESTAMPTZ,
ADD COLUMN IF NOT EXISTS aborted_at TIMESTAMPTZ;

-- Stuck-upload reporting and terminal-session cleanup scan by these.
CREATE INDEX IF NOT EXISTS ix_upload_sessions_terminal
ON upload_sessions (updated_at)
WHERE status IN ('ABORTED', 'FAILED', 'EXPIRED');

-- The upload status a reconciliation task left behind (e.g. COMPLETED).
ALTER TABLE reconciliation_tasks
ADD COLUMN IF NOT EXISTS outcome TEXT;

-- Discrepancies found by the scans that are not tied to a live task:
-- provider sessions or objects without an owner, stale part rows. Records
-- the action taken and its result. Never holds URLs or credentials.
CREATE TABLE IF NOT EXISTS reconciliation_findings (
    id                TEXT PRIMARY KEY,
    kind              TEXT NOT NULL CHECK (
                          kind IN ('ORPHAN_MULTIPART_SESSION', 'ORPHAN_OBJECT', 'STALE_PARTS')
                      ),
    tenant_id         TEXT,
    upload_id         TEXT,
    object_key        TEXT,
    storage_upload_id TEXT,
    action            TEXT NOT NULL CHECK (action IN ('ABORT_SESSION', 'DELETE_OBJECT', 'DELETE_PARTS')),
    outcome           TEXT NOT NULL CHECK (outcome IN ('DONE', 'FAILED')),
    reason            TEXT NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS ix_reconciliation_findings_recent
ON reconciliation_findings (created_at DESC);

CREATE INDEX IF NOT EXISTS ix_reconciliation_findings_failed
ON reconciliation_findings (created_at DESC)
WHERE outcome = 'FAILED';

COMMIT;
