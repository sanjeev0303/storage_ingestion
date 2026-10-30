DROP TRIGGER IF EXISTS webhook_erasure ON erasure_requests;
DROP FUNCTION IF EXISTS webhook_erasure();
DROP TABLE IF EXISTS erasure_items;
DROP TABLE IF EXISTS erasure_requests;
DROP INDEX IF EXISTS ix_version_members_checksum;
DROP INDEX IF EXISTS ix_dataset_members_subject;
DROP INDEX IF EXISTS ix_version_members_subject;
DROP INDEX IF EXISTS ix_assets_subject;
ALTER TABLE dataset_versions DROP COLUMN IF EXISTS superseded_reason, DROP COLUMN IF EXISTS superseded_at,
    DROP COLUMN IF EXISTS superseded_by;
UPDATE deletion_operations SET reason = 'OPERATOR' WHERE reason = 'ERASURE';
ALTER TABLE deletion_operations DROP CONSTRAINT IF EXISTS deletion_operations_reason_check;
ALTER TABLE deletion_operations ADD CONSTRAINT deletion_operations_reason_check
    CHECK (reason IN ('USER_REQUEST', 'POLICY_EXPIRED', 'OPERATOR'));
