-- Cancelling a job reads its waiting entries in position order. Without this
-- index that walks the primary key over every entry already cancelled (R5:
-- 1.4 s a batch with 600,000 of 1,000,000 cancelled); with it each batch
-- reads only the waiting entries it changes.
CREATE INDEX IF NOT EXISTS ix_ingestion_entries_waiting ON ingestion_entries (job_id, position)
WHERE state IN ('PENDING', 'FAILED_RETRYABLE');
