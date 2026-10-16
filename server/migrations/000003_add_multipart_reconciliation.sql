BEGIN;

-- New lifecycle states for multipart reliability:
--   COMPLETING  completion requested, provider outcome possibly unknown
--   VERIFIED    stored bytes passed SHA-256 verification (promotable)
--   ABORTING    abort/expiry requested, provider not yet confirmed
-- 000001 declared the status CHECK inline, so PostgreSQL named it
-- upload_sessions_status_check.
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
        'FAILED',
        'EXPIRED'
    )
);

-- Why a session failed, expired or is being aborted. Sanitized codes only.
ALTER TABLE upload_sessions
ADD COLUMN IF NOT EXISTS failure_reason TEXT;

-- Sessions the reconciliation sweeper scans for.
CREATE INDEX IF NOT EXISTS ix_upload_sessions_reconcile
ON upload_sessions (status, updated_at)
WHERE status IN ('PENDING', 'UPLOADING', 'COMPLETING', 'COMPLETED', 'ABORTING');

-- Durable recovery work. Never holds presigned URLs, credentials or bodies.
CREATE TABLE IF NOT EXISTS reconciliation_tasks (
    id                TEXT PRIMARY KEY,
    tenant_id         TEXT NOT NULL,
    upload_id         TEXT NOT NULL REFERENCES upload_sessions(id) ON DELETE CASCADE,
    operation         TEXT NOT NULL CHECK (
                          operation IN ('INITIATE', 'COMPLETE', 'ABORT', 'EXPIRE', 'VERIFY')
                      ),
    state             TEXT NOT NULL DEFAULT 'PENDING' CHECK (
                          state IN ('PENDING', 'SUCCEEDED', 'EXHAUSTED')
                      ),
    storage_provider  TEXT NOT NULL,
    object_key        TEXT NOT NULL,
    storage_upload_id TEXT,
    attempt_count     INTEGER NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    max_attempts      INTEGER NOT NULL CHECK (max_attempts > 0),
    last_attempt_at   TIMESTAMPTZ,
    next_attempt_at   TIMESTAMPTZ NOT NULL,
    last_error        TEXT,
    lease_owner       TEXT,
    lease_expires_at  TIMESTAMPTZ,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CHECK ((lease_owner IS NULL) = (lease_expires_at IS NULL))
);

-- At most one open task per upload: concurrent sweepers cannot duplicate it.
CREATE UNIQUE INDEX IF NOT EXISTS ux_reconciliation_tasks_open_upload
ON reconciliation_tasks (upload_id)
WHERE state = 'PENDING';

-- Due work, in the order workers claim it.
CREATE INDEX IF NOT EXISTS ix_reconciliation_tasks_due
ON reconciliation_tasks (next_attempt_at)
WHERE state = 'PENDING';

-- Exhausted work an operator must look at.
CREATE INDEX IF NOT EXISTS ix_reconciliation_tasks_exhausted
ON reconciliation_tasks (tenant_id, updated_at DESC)
WHERE state = 'EXHAUSTED';

COMMIT;
