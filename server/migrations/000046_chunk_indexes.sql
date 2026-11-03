-- Chunk storage asks for a dataset's available files every pass, and the upstream
-- watch for new URL imports every few minutes. Without these both read everything.
CREATE INDEX IF NOT EXISTS ix_assets_tenant_dataset ON assets (tenant_id, dataset) WHERE status = 'AVAILABLE';
CREATE INDEX IF NOT EXISTS ix_adoption_entries_imported_url ON adoption_entries (tenant_id, url) WHERE url <> '' AND state = 'IMPORTED';
CREATE INDEX IF NOT EXISTS ix_stored_chunks_unused ON stored_chunks (created_at);
