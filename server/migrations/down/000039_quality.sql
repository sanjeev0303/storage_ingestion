DROP TRIGGER IF EXISTS webhook_leakage ON leakage_checks;
DROP FUNCTION IF EXISTS webhook_leakage();
DROP TRIGGER IF EXISTS webhook_version_drift ON version_profiles;
DROP FUNCTION IF EXISTS webhook_version_drift();
DROP TABLE IF EXISTS leakage_checks;
ALTER TABLE training_run_inputs DROP COLUMN IF EXISTS role;
DROP INDEX IF EXISTS ux_dataset_versions_release;
ALTER TABLE dataset_versions DROP COLUMN IF EXISTS released_by, DROP COLUMN IF EXISTS released_at,
    DROP COLUMN IF EXISTS release_notes, DROP COLUMN IF EXISTS release_label;
DROP TRIGGER IF EXISTS queue_version_profile ON dataset_versions;
DROP FUNCTION IF EXISTS queue_version_profile();
DROP TABLE IF EXISTS version_profiles;
DROP TABLE IF EXISTS dataset_quality;
