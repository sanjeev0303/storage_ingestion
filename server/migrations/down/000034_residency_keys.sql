ALTER TABLE datasets DROP COLUMN IF EXISTS require_customer_key, DROP COLUMN IF EXISTS residency;
ALTER TABLE connection_posture DROP COLUMN IF EXISTS encryption_key, DROP COLUMN IF EXISTS location_verified,
    DROP COLUMN IF EXISTS location;
ALTER TABLE storage_connections DROP COLUMN IF EXISTS encryption_key;
