DROP TABLE IF EXISTS run_checkpoints;
DROP TABLE IF EXISTS run_verifications;
DROP TABLE IF EXISTS repack_requests;
DROP TABLE IF EXISTS tenant_packing;
DROP INDEX IF EXISTS ix_pack_entries_source_pack;
ALTER TABLE pack_entries DROP COLUMN IF EXISTS source_offset, DROP COLUMN IF EXISTS source_pack_id;
