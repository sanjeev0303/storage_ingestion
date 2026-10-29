BEGIN;

-- Content deduplication (docs/content-security.md, "Storing identical files once"): an upload
-- whose verified bytes equal an object the organization already stores on
-- the same connection is published pointing at that object instead of a
-- copy. shared_from names the asset whose stored object it uses.
ALTER TABLE assets ADD COLUMN IF NOT EXISTS shared_from TEXT;

-- Deletion and packing ask, before removing bytes, whether another live
-- asset still points at the key.
CREATE INDEX IF NOT EXISTS ix_assets_object_key ON assets (object_key);

COMMIT;
