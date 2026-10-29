BEGIN;

-- Malware verdict reuse. A tenant that uploads bytes it has already had
-- scanned CLEAN skips the scan, when the earlier verdict came from the same
-- scanner with the same engine and signature version. scanner_version is
-- written only for a CLEAN scan whose scanned bytes matched the declared
-- SHA-256, so a row that has one is evidence about exactly those bytes.
-- scan_reused_from names the asset whose scan was reused.
ALTER TABLE processing_jobs ADD COLUMN IF NOT EXISTS scanner_version TEXT;
ALTER TABLE processing_jobs ADD COLUMN IF NOT EXISTS scan_reused_from TEXT;

-- The lookup: a tenant's assets with given bytes.
CREATE INDEX IF NOT EXISTS ix_assets_tenant_checksum
ON assets (tenant_id, checksum_sha256, size_bytes)
WHERE checksum_sha256 IS NOT NULL;

COMMIT;
