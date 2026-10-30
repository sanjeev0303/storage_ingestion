DROP TRIGGER IF EXISTS webhook_audit_review ON audit_reviews;
DROP FUNCTION IF EXISTS webhook_audit_review();

DELETE FROM security_alerts WHERE kind = 'AUDIT_CHAIN_BROKEN';
ALTER TABLE security_alerts DROP CONSTRAINT IF EXISTS security_alerts_kind_check;
ALTER TABLE security_alerts ADD CONSTRAINT security_alerts_kind_check CHECK (kind IN ('READ_SPIKE', 'MASS_LINKS',
    'NEW_NETWORK', 'EGRESS_BUDGET', 'STORAGE_POSTURE', 'LIFECYCLE_CHANGED', 'OUT_OF_BAND_WRITE', 'MASS_DELETION', 'SCRUB_MISMATCH',
    'COST_BUDGET'));

ALTER TABLE models DROP COLUMN IF EXISTS summary_reminded_at, DROP COLUMN IF EXISTS summary_published_at;
ALTER TABLE datasets DROP COLUMN IF EXISTS provenance_recorded_by, DROP COLUMN IF EXISTS provenance_recorded_at,
    DROP COLUMN IF EXISTS source_kind, DROP COLUMN IF EXISTS source, DROP COLUMN IF EXISTS license, DROP COLUMN IF EXISTS lawful_basis;
DROP TABLE IF EXISTS audit_reviews;
DROP TABLE IF EXISTS audit_anchors;
DROP TABLE IF EXISTS evidence_settings;
