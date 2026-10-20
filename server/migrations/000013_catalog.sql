BEGIN;

-- Phase 9: the object catalog. A logical object is a tenant's named file:
-- a normalized path in a dataset's namespace ('' is the tenant's root
-- namespace). Its versions are assets: one per upload, each under its own
-- server-generated storage key, numbered when published. Everything here
-- is additive: existing assets keep working and have no object until the
-- catalog reconciliation backfills them.
CREATE TABLE IF NOT EXISTS catalog_objects (
    id               TEXT PRIMARY KEY,
    tenant_id        TEXT NOT NULL,
    dataset          TEXT COLLATE "C" NOT NULL DEFAULT '',
    -- path is the normal form as written; path_key the case- and
    -- Unicode-normalization-insensitive form, unique per namespace. "C"
    -- (bytewise) collation makes listing order independent of the
    -- server's locale and prefix matches index range scans.
    path             TEXT NOT NULL CHECK (length(path) BETWEEN 1 AND 1024),
    path_key         TEXT COLLATE "C" NOT NULL CHECK (octet_length(path_key) BETWEEN 1 AND 2048),
    versioning       TEXT NOT NULL DEFAULT 'ENABLED' CHECK (versioning IN ('ENABLED', 'DISABLED')),
    -- revision is the optimistic-concurrency token of client-visible
    -- changes.
    revision         BIGINT NOT NULL DEFAULT 1,
    metadata         JSONB NOT NULL DEFAULT '{}'
                     CHECK (jsonb_typeof(metadata) = 'object' AND octet_length(metadata::text) <= 16384),
    next_version     INTEGER NOT NULL DEFAULT 1 CHECK (next_version > 0),
    -- The current version and its system metadata, copied when it became
    -- current, so a listing reads one row per object.
    current_asset_id TEXT,
    current_version  INTEGER,
    size_bytes       BIGINT,
    content_type     TEXT,
    checksum_sha256  TEXT,
    published_at     TIMESTAMPTZ,
    deleted_at       TIMESTAMPTZ,
    created_by       TEXT NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL,
    updated_at       TIMESTAMPTZ NOT NULL,
    CHECK ((current_asset_id IS NULL) = (current_version IS NULL))
);

-- Composite foreign keys reference (id, tenant_id).
CREATE UNIQUE INDEX IF NOT EXISTS ux_catalog_objects_id_tenant ON catalog_objects (id, tenant_id);
-- One object per path in a namespace. Objects are never removed, so a
-- path keeps its object (and its version history) for good; also the
-- path lookup and the listing that includes unpublished objects.
CREATE UNIQUE INDEX IF NOT EXISTS ux_catalog_objects_path ON catalog_objects (tenant_id, dataset, path_key);
-- The default listing: published objects in path order, prefix ranges.
CREATE INDEX IF NOT EXISTS ix_catalog_objects_listed ON catalog_objects (tenant_id, dataset, path_key)
    WHERE current_asset_id IS NOT NULL;
-- The object an asset is current for (deletion moves it, the purge
-- checks it); an asset is the current version of one object at most.
CREATE UNIQUE INDEX IF NOT EXISTS ux_catalog_objects_current ON catalog_objects (current_asset_id)
    WHERE current_asset_id IS NOT NULL;
-- Listing by last change, and modification-time ranges.
CREATE INDEX IF NOT EXISTS ix_catalog_objects_updated ON catalog_objects (tenant_id, updated_at, id)
    WHERE current_asset_id IS NOT NULL;
-- Exact user-metadata filters (metadata @> '{"k":"v"}').
CREATE INDEX IF NOT EXISTS ix_catalog_objects_metadata ON catalog_objects USING gin (metadata jsonb_path_ops)
    WHERE current_asset_id IS NOT NULL;

-- Assets are the versions.
ALTER TABLE assets ADD COLUMN IF NOT EXISTS object_id TEXT;
ALTER TABLE assets ADD COLUMN IF NOT EXISTS version INTEGER CHECK (version > 0);
ALTER TABLE assets ADD COLUMN IF NOT EXISTS metadata JSONB
    CHECK (metadata IS NULL OR octet_length(metadata::text) <= 16384);
-- One version number per object; the version history, newest first.
CREATE UNIQUE INDEX IF NOT EXISTS ux_assets_object_version ON assets (object_id, version) WHERE version IS NOT NULL;
-- Every asset of an object, published or not (deletions, in-flight
-- uploads, the foreign key's checks).
CREATE INDEX IF NOT EXISTS ix_assets_object ON assets (object_id) WHERE object_id IS NOT NULL;
-- Published assets not yet in the catalog: the backfill's work list.
CREATE INDEX IF NOT EXISTS ix_assets_uncatalogued ON assets (tenant_id, id) WHERE object_id IS NULL AND status = 'AVAILABLE';

-- An upload names the path it publishes to; the object is claimed when
-- the upload starts.
ALTER TABLE upload_sessions ADD COLUMN IF NOT EXISTS path TEXT;
ALTER TABLE upload_sessions ADD COLUMN IF NOT EXISTS object_id TEXT;
ALTER TABLE upload_sessions ADD COLUMN IF NOT EXISTS create_only BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE upload_sessions ADD COLUMN IF NOT EXISTS metadata JSONB
    CHECK (metadata IS NULL OR octet_length(metadata::text) <= 16384);
CREATE INDEX IF NOT EXISTS ix_upload_sessions_object ON upload_sessions (object_id) WHERE object_id IS NOT NULL;

-- No reference may cross tenants: an asset of another tenant's object, an
-- object whose current version is another tenant's asset.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'fk_assets_object') THEN
        ALTER TABLE assets ADD CONSTRAINT fk_assets_object
            FOREIGN KEY (object_id, tenant_id) REFERENCES catalog_objects (id, tenant_id);
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'fk_upload_sessions_object') THEN
        ALTER TABLE upload_sessions ADD CONSTRAINT fk_upload_sessions_object
            FOREIGN KEY (object_id, tenant_id) REFERENCES catalog_objects (id, tenant_id);
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'fk_catalog_objects_current') THEN
        ALTER TABLE catalog_objects ADD CONSTRAINT fk_catalog_objects_current
            FOREIGN KEY (current_asset_id, tenant_id) REFERENCES assets (id, tenant_id);
    END IF;
END $$;

COMMIT;
