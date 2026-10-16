BEGIN;

-- Store the provider-generated multipart upload ID.
ALTER TABLE upload_sessions
ADD COLUMN IF NOT EXISTS storage_upload_id TEXT;

-- Persist part metadata so upload progress survives API restarts.
CREATE TABLE IF NOT EXISTS upload_parts (
    tenant_id TEXT NOT NULL,
    upload_id  TEXT NOT NULL REFERENCES upload_sessions(id) ON DELETE CASCADE,
    part_number INTEGER NOT NULL CHECK (part_number BETWEEN 1 AND 10000),
    etag TEXT,
    size_bytes BIGINT CHECK (size_bytes IS NULL OR size_bytes > 0),
    checksum_sha256 CHAR(64),
    status          TEXT NOT NULL DEFAULT 'PENDING'
                    CHECK (status IN ('PENDING', 'UPLOADED')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    PRIMARY KEY (upload_id, part_number)
);

CREATE INDEX IF NOT EXISTS ix_upload_parts_tenant_upload
ON upload_parts (tenant_id, upload_id);

COMMIT;