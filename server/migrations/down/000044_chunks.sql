DROP TABLE IF EXISTS chunk_jobs;
DROP TABLE IF EXISTS file_chunks;
DROP TABLE IF EXISTS chunked_files;
ALTER TABLE datasets DROP COLUMN IF EXISTS chunking_enabled;
