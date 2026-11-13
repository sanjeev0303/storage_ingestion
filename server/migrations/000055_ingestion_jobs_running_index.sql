-- Every claim counts its tenant's jobs that still have work, under the one
-- lock all claims share, so its cost is paid by every tenant. Without this
-- index that count walks either every job the tenant ever ran
-- (ix_ingestion_jobs_tenant) or every tenant's active jobs
-- (ix_ingestion_jobs_active). Uploads and processing already have their own
-- partial index for the same kind of count (ix_upload_sessions_active,
-- ix_processing_jobs_pending_tenant).
CREATE INDEX IF NOT EXISTS ix_ingestion_jobs_tenant_running ON ingestion_jobs (tenant_id)
WHERE state IN ('READY', 'RUNNING');
