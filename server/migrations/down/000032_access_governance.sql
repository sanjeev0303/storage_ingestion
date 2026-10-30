-- Undoes 000032_access_governance.sql. API tokens stop working, alerts,
-- egress counts, classifications and the access policy are lost.
DROP TRIGGER IF EXISTS webhook_security_alert ON security_alerts;
DROP FUNCTION IF EXISTS webhook_security_alert();
DROP TABLE IF EXISTS security_alerts;
ALTER TABLE assets DROP COLUMN IF EXISTS classification;
DROP TABLE IF EXISTS egress_usage;
DROP TABLE IF EXISTS tenant_access_policy;
DROP TABLE IF EXISTS api_tokens;
ALTER TABLE download_tokens DROP COLUMN IF EXISTS revoked_by;
ALTER TABLE download_tokens DROP COLUMN IF EXISTS last_used_at;
ALTER TABLE download_tokens DROP COLUMN IF EXISTS uses;
DROP INDEX IF EXISTS ix_delivery_audit_reads;
ALTER TABLE delivery_audit_events DROP COLUMN IF EXISTS credential_id;
ALTER TABLE delivery_audit_events DROP COLUMN IF EXISTS source_ip;
