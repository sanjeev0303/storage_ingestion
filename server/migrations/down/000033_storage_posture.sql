DELETE FROM security_alerts WHERE kind IN ('STORAGE_POSTURE', 'LIFECYCLE_CHANGED', 'OUT_OF_BAND_WRITE');
ALTER TABLE security_alerts DROP CONSTRAINT IF EXISTS security_alerts_kind_check;
ALTER TABLE security_alerts ADD CONSTRAINT security_alerts_kind_check
    CHECK (kind IN ('READ_SPIKE', 'MASS_LINKS', 'NEW_NETWORK', 'EGRESS_BUDGET'));

ALTER TABLE datasets DROP COLUMN IF EXISTS data_class;
DROP TABLE IF EXISTS connection_drift_scans;
DROP TABLE IF EXISTS storage_drift_findings;
DROP TABLE IF EXISTS connection_posture;

ALTER TABLE storage_connections DROP CONSTRAINT IF EXISTS ck_staged_credential;
ALTER TABLE storage_connections
    DROP COLUMN IF EXISTS previous_until,
    DROP COLUMN IF EXISTS previous_ciphertext,
    DROP COLUMN IF EXISTS previous_nonce,
    DROP COLUMN IF EXISTS previous_key_version,
    DROP COLUMN IF EXISTS previous_credential_version,
    DROP COLUMN IF EXISTS staged_by,
    DROP COLUMN IF EXISTS staged_at,
    DROP COLUMN IF EXISTS staged_ciphertext,
    DROP COLUMN IF EXISTS staged_nonce,
    DROP COLUMN IF EXISTS staged_key_version,
    DROP COLUMN IF EXISTS staged_credential_version,
    DROP COLUMN IF EXISTS expected_owner,
    DROP COLUMN IF EXISTS checksum_mode;
