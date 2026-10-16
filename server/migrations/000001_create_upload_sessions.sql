BEGIN;

CREATE TABLE IF NOT EXISTS upload_sessions (
        id TEXT PRIMARY KEY,
        tenant_id  TEXT NOT NULL,
        owner_id  TEXT NOT NULL,
        object_key TEXT NOT NULL,
        file_name TEXT NOT NULL,
        content_type TEXT NOT NULL,
        expected_size BIGINT NOT NULL CHECK (expected_size > 0),
        checksum_sha256  CHAR(64),
        status TEXT NOT NULL CHECK (
            status IN (
            'PENDING',
            'UPLOADING',
            'COMPLETED',
            'FAILED',
            'EXPIRED'
        )
        ),
        idempotancy_key TEXT,
        expires_at TIMESTAMPTZ NOT NULL,
        created_at TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
        updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

        CHECK (expires_at > created_at),
        CHECK(
            checksum_sha256 IS NULL
            OR checksum_sha256 ~ '^[0-9a-f]{64}$'
        )
);

--The same key may be used by different tenants or owners.
-- A tenant and owner cannot create two sessions with the same key.
CREATE UNIQUE INDEX IF NOT EXISTS
    ux_upload_sessions_idempotency
ON upload_sessions (tenant_id, owner_id, idempotancy_key)
WHERE idempotancy_key IS NOT NULL;

CREATE INDEX IF NOT EXISTS
    ix_upload_sessions_tenant_created
ON upload_sessions (tenant_id, created_at DESC);

CREATE INDEX IF NOT EXISTS
    ix_upload_sessions_expiry
ON upload_sessions (expires_at)
WHERE status IN ('PENDING', 'UPLOADING');

COMMIT;