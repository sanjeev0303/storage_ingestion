DROP TABLE IF EXISTS delta_upload_chunks;
DROP TABLE IF EXISTS delta_uploads;
DROP TABLE IF EXISTS chunk_refs;
DROP TABLE IF EXISTS chunked_assets;
DROP TABLE IF EXISTS stored_chunks;
ALTER TABLE datasets DROP COLUMN IF EXISTS chunk_storage_enabled;
