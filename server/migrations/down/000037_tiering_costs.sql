DROP TRIGGER IF EXISTS webhook_archive_restore ON archive_restores;
DROP FUNCTION IF EXISTS webhook_archive_restore();

DELETE FROM security_alerts WHERE kind = 'COST_BUDGET';
ALTER TABLE security_alerts DROP CONSTRAINT IF EXISTS security_alerts_kind_check;
ALTER TABLE security_alerts ADD CONSTRAINT security_alerts_kind_check CHECK (kind IN ('READ_SPIKE', 'MASS_LINKS',
    'NEW_NETWORK', 'EGRESS_BUDGET', 'STORAGE_POSTURE', 'LIFECYCLE_CHANGED', 'OUT_OF_BAND_WRITE', 'MASS_DELETION', 'SCRUB_MISMATCH'));

ALTER TABLE dataset_versions DROP COLUMN IF EXISTS expired_by, DROP COLUMN IF EXISTS expired_at;
DROP TABLE IF EXISTS cost_budgets;
DROP TABLE IF EXISTS archive_restores;
DROP TABLE IF EXISTS tier_moves;
DROP TABLE IF EXISTS tier_rules;
DROP INDEX IF EXISTS ix_assets_tier;
ALTER TABLE packs DROP COLUMN IF EXISTS readable_until, DROP COLUMN IF EXISTS storage_tier;
ALTER TABLE assets DROP COLUMN IF EXISTS readable_until, DROP COLUMN IF EXISTS tiered_at, DROP COLUMN IF EXISTS storage_tier;
