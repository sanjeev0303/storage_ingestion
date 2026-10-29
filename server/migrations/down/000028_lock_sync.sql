-- Undoes 000028_lock_sync.sql. Queued lock syncs are dropped; the retention
-- sweep still applies every lock, just later.
DROP TABLE IF EXISTS lock_sync_requests;
