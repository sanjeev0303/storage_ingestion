-- Undoes 000030_governance.sql. Lost: QUERY holds (deleted, so their
-- assets are no longer held), approvals, governance settings, disposition
-- certificates, lock modes and verification results, and metadata-based
-- retention (policies count from creation again).
DROP TRIGGER IF EXISTS webhook_deletion_blocked ON deletion_operations;
DROP FUNCTION IF EXISTS webhook_deletion_blocked();
DROP TABLE IF EXISTS disposition_certificates;
DROP TABLE IF EXISTS governance_approvals;
DROP TABLE IF EXISTS tenant_governance;

DELETE FROM lock_sync_requests WHERE scope = 'QUERY';
ALTER TABLE lock_sync_requests DROP CONSTRAINT IF EXISTS lock_sync_requests_scope_check;
ALTER TABLE lock_sync_requests ADD CONSTRAINT lock_sync_requests_scope_check CHECK (scope IN ('TENANT', 'DATASET', 'ASSET'));

DELETE FROM legal_holds WHERE scope = 'QUERY';
ALTER TABLE legal_holds DROP CONSTRAINT IF EXISTS legal_holds_query_check;
ALTER TABLE legal_holds DROP COLUMN IF EXISTS custodians;
ALTER TABLE legal_holds DROP COLUMN IF EXISTS matter;
ALTER TABLE legal_holds DROP COLUMN IF EXISTS query;
ALTER TABLE legal_holds DROP CONSTRAINT IF EXISTS legal_holds_scope_check;
ALTER TABLE legal_holds ADD CONSTRAINT legal_holds_scope_check CHECK (scope IN ('TENANT', 'DATASET', 'ASSET'));

DROP INDEX IF EXISTS ix_object_locks_verify;
ALTER TABLE object_locks DROP COLUMN IF EXISTS verify_result;
ALTER TABLE object_locks DROP COLUMN IF EXISTS verified_at;
ALTER TABLE object_locks DROP COLUMN IF EXISTS mode;

ALTER TABLE retention_policies DROP COLUMN IF EXISTS template;
ALTER TABLE retention_policies DROP COLUMN IF EXISTS retain_from_key;
ALTER TABLE retention_policies DROP COLUMN IF EXISTS lock_mode;
