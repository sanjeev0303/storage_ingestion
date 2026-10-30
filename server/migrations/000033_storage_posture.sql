-- Storage posture and connection health (docs/storage-posture.md): the
-- posture of each connection's bucket, an explicit checksum mode, the
-- bucket owner to pin, a staged second credential with rollback, objects
-- changed outside the platform, and datasets that hold regulated data.

ALTER TABLE storage_connections
    ADD COLUMN IF NOT EXISTS checksum_mode TEXT NOT NULL DEFAULT 'AUTO'
        CHECK (checksum_mode IN ('AUTO', 'WHEN_SUPPORTED', 'WHEN_REQUIRED')),
    ADD COLUMN IF NOT EXISTS expected_owner TEXT,
    -- A second credential, tested and waiting to be promoted.
    ADD COLUMN IF NOT EXISTS staged_credential_version INTEGER,
    ADD COLUMN IF NOT EXISTS staged_key_version INTEGER,
    ADD COLUMN IF NOT EXISTS staged_nonce BYTEA,
    ADD COLUMN IF NOT EXISTS staged_ciphertext BYTEA,
    ADD COLUMN IF NOT EXISTS staged_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS staged_by TEXT,
    -- The credential a promotion replaced, kept for a rollback until
    -- previous_until.
    ADD COLUMN IF NOT EXISTS previous_credential_version INTEGER,
    ADD COLUMN IF NOT EXISTS previous_key_version INTEGER,
    ADD COLUMN IF NOT EXISTS previous_nonce BYTEA,
    ADD COLUMN IF NOT EXISTS previous_ciphertext BYTEA,
    ADD COLUMN IF NOT EXISTS previous_until TIMESTAMPTZ;

ALTER TABLE storage_connections DROP CONSTRAINT IF EXISTS ck_staged_credential;
ALTER TABLE storage_connections ADD CONSTRAINT ck_staged_credential
    CHECK ((staged_ciphertext IS NULL) = (staged_nonce IS NULL) AND (previous_ciphertext IS NULL) = (previous_nonce IS NULL));

CREATE TABLE IF NOT EXISTS connection_posture (
    connection_id  TEXT PRIMARY KEY REFERENCES storage_connections (id),
    tenant_id      TEXT NOT NULL,
    checked_at     TIMESTAMPTZ NOT NULL,
    worst          TEXT NOT NULL CHECK (worst IN ('PASS', 'WARN', 'FAIL')),
    phi_safe       BOOLEAN NOT NULL,
    checks         JSONB NOT NULL,
    lifecycle_hash TEXT NOT NULL DEFAULT '',
    owner_account  TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS ix_connection_posture_checked ON connection_posture (checked_at);

-- Objects the platform holds that changed, or appeared, outside it.
CREATE TABLE IF NOT EXISTS storage_drift_findings (
    id            TEXT PRIMARY KEY,
    tenant_id     TEXT NOT NULL,
    connection_id TEXT NOT NULL,
    asset_id      TEXT NOT NULL DEFAULT '',
    object_key    TEXT NOT NULL,
    kind          TEXT NOT NULL CHECK (kind IN ('OVERWRITTEN', 'DELETED', 'NEW_VERSION', 'UNREADABLE', 'UNKNOWN_OBJECT')),
    detail        TEXT NOT NULL,
    detected_at   TIMESTAMPTZ NOT NULL,
    resolved_at   TIMESTAMPTZ,
    resolved_by   TEXT
);
CREATE UNIQUE INDEX IF NOT EXISTS ux_storage_drift_open
    ON storage_drift_findings (tenant_id, connection_id, object_key, kind) WHERE resolved_at IS NULL;
CREATE INDEX IF NOT EXISTS ix_storage_drift_asset
    ON storage_drift_findings (tenant_id, asset_id) WHERE resolved_at IS NULL;

-- Where each connection's drift scan is: the next recorded object to
-- check, and the next page of its listing.
CREATE TABLE IF NOT EXISTS connection_drift_scans (
    connection_id   TEXT PRIMARY KEY REFERENCES storage_connections (id),
    tenant_id       TEXT NOT NULL,
    object_cursor   TEXT NOT NULL DEFAULT '',
    listing_cursor  TEXT NOT NULL DEFAULT '',
    objects_checked BIGINT NOT NULL DEFAULT 0,
    last_run_at     TIMESTAMPTZ NOT NULL
);

ALTER TABLE datasets ADD COLUMN IF NOT EXISTS data_class TEXT CHECK (data_class IN ('PHI', 'PCI', 'PII'));

ALTER TABLE security_alerts DROP CONSTRAINT IF EXISTS security_alerts_kind_check;
ALTER TABLE security_alerts ADD CONSTRAINT security_alerts_kind_check CHECK (kind IN ('READ_SPIKE', 'MASS_LINKS',
    'NEW_NETWORK', 'EGRESS_BUDGET', 'STORAGE_POSTURE', 'LIFECYCLE_CHANGED', 'OUT_OF_BAND_WRITE'));
