BEGIN;

-- What the platform last applied to an asset's stored version on a bucket
-- with S3 Object Lock (docs/lifecycle.md, "Object Lock"): the provider
-- itself then refuses to delete it early, whoever holds the bucket's keys.
CREATE TABLE IF NOT EXISTS object_locks (
    asset_id     TEXT PRIMARY KEY REFERENCES assets (id) ON DELETE CASCADE,
    tenant_id    TEXT NOT NULL,
    retain_until TIMESTAMPTZ,
    legal_hold   BOOLEAN NOT NULL DEFAULT false,
    applied_at   TIMESTAMPTZ NOT NULL
);

COMMIT;
