-- Undoes 000029_content_checks.sql. Content findings and the tenants'
-- per-code overrides are lost; assets keep their status.
DROP TRIGGER IF EXISTS webhook_content_finding ON content_findings;
DROP FUNCTION IF EXISTS webhook_content_finding();
DROP INDEX IF EXISTS ix_processing_jobs_clean_version;
ALTER TABLE processing_jobs DROP COLUMN IF EXISTS rescanned_at;
ALTER TABLE processing_jobs DROP COLUMN IF EXISTS content_checks;
ALTER TABLE assets DROP COLUMN IF EXISTS blocked_reason;
ALTER TABLE assets DROP COLUMN IF EXISTS blocked_at;
ALTER TABLE tenant_content_policies DROP COLUMN IF EXISTS finding_actions;
DROP TABLE IF EXISTS content_findings;
