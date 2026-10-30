-- Residency pinning and customer-managed keys (docs/residency.md): the key
-- every write to a connection is encrypted with, where each connection's
-- bucket really is, and the rules a dataset sets for where its data lives.

ALTER TABLE storage_connections ADD COLUMN IF NOT EXISTS encryption_key TEXT;

ALTER TABLE connection_posture
    ADD COLUMN IF NOT EXISTS location TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS location_verified BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS encryption_key TEXT NOT NULL DEFAULT '';

ALTER TABLE datasets
    ADD COLUMN IF NOT EXISTS residency TEXT[] NOT NULL DEFAULT '{}',
    ADD COLUMN IF NOT EXISTS require_customer_key BOOLEAN NOT NULL DEFAULT false;
