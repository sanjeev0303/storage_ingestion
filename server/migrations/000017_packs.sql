BEGIN;

-- Phase 13: small-file packing. A pack is one physical object holding the
-- bytes of many published asset versions of one tenant, on one storage
-- connection, plus the job that builds it. Assets keep their identity:
-- a pack entry only says where an asset version's bytes are read.
CREATE TABLE IF NOT EXISTS packs (
    id                    TEXT PRIMARY KEY,
    tenant_id             TEXT NOT NULL,
    storage_connection_id TEXT NOT NULL REFERENCES storage_connections (id),
    bucket                TEXT NOT NULL,
    object_key            TEXT NOT NULL,
    manifest_key          TEXT NOT NULL,
    format                TEXT NOT NULL,
    format_version        INTEGER NOT NULL,
    state                 TEXT NOT NULL CHECK (state IN ('PLANNED', 'WRITING', 'VERIFYING', 'PUBLISHED', 'RETIRING', 'DELETED', 'FAILED')),
    entry_count           INTEGER NOT NULL CHECK (entry_count > 0),
    total_bytes           BIGINT NOT NULL CHECK (total_bytes >= 0),
    sha256                TEXT,
    manifest_sha256       TEXT,
    storage_etag          TEXT,
    storage_version_id    TEXT,
    -- The pack whose live entries this one takes over (a rewrite).
    rewrite_of            TEXT REFERENCES packs (id),
    attempts              INTEGER NOT NULL DEFAULT 0,
    max_attempts          INTEGER NOT NULL CHECK (max_attempts > 0),
    next_attempt_at       TIMESTAMPTZ NOT NULL,
    lease_owner           TEXT,
    lease_expires_at      TIMESTAMPTZ,
    last_error            TEXT,
    created_at            TIMESTAMPTZ NOT NULL,
    updated_at            TIMESTAMPTZ NOT NULL,
    published_at          TIMESTAMPTZ,
    retired_at            TIMESTAMPTZ,
    deleted_at            TIMESTAMPTZ,

    UNIQUE (id, tenant_id),
    UNIQUE (storage_connection_id, bucket, object_key),
    CHECK ((lease_owner IS NULL) = (lease_expires_at IS NULL)),
    -- Nothing is published unverified.
    CHECK (state NOT IN ('PUBLISHED', 'RETIRING') OR (sha256 IS NOT NULL AND manifest_sha256 IS NOT NULL AND storage_etag IS NOT NULL))
);

-- Jobs to run, and packs to retire or clean up.
CREATE INDEX IF NOT EXISTS ix_packs_due ON packs (next_attempt_at)
WHERE state IN ('PLANNED', 'WRITING', 'VERIFYING');
CREATE INDEX IF NOT EXISTS ix_packs_tenant_state ON packs (tenant_id, state);
CREATE INDEX IF NOT EXISTS ix_packs_cleanup ON packs (updated_at)
WHERE state IN ('RETIRING', 'FAILED');

CREATE TABLE IF NOT EXISTS pack_entries (
    pack_id           TEXT NOT NULL,
    tenant_id         TEXT NOT NULL,
    ordinal           INTEGER NOT NULL CHECK (ordinal >= 0),
    asset_id          TEXT NOT NULL,
    object_id         TEXT,
    version           INTEGER NOT NULL DEFAULT 0,
    byte_offset       BIGINT NOT NULL CHECK (byte_offset >= 0),
    stored_length     BIGINT NOT NULL CHECK (stored_length >= 0),
    size_bytes        BIGINT NOT NULL CHECK (size_bytes >= 0),
    checksum_sha256   TEXT NOT NULL,
    -- Where the bytes were copied from: the asset's own object, or an
    -- entry of the pack this one rewrites.
    source_key        TEXT NOT NULL,
    source_etag       TEXT,
    source_version_id TEXT,
    state             TEXT NOT NULL CHECK (state IN ('RESERVED', 'PUBLISHED', 'DELETED', 'MOVED', 'RELEASED', 'EXCLUDED')),
    source_retired_at TIMESTAMPTZ,
    deleted_at        TIMESTAMPTZ,
    created_at        TIMESTAMPTZ NOT NULL,
    updated_at        TIMESTAMPTZ NOT NULL,

    PRIMARY KEY (pack_id, ordinal),
    UNIQUE (pack_id, asset_id),
    FOREIGN KEY (pack_id, tenant_id) REFERENCES packs (id, tenant_id),
    FOREIGN KEY (asset_id, tenant_id) REFERENCES assets (id, tenant_id),
    CHECK (byte_offset <= 9223372036854775807 - stored_length)
);

-- An asset version is served from at most one pack, and reserved by at
-- most one pack being built (a rewrite reserves an asset that is still
-- published in the pack it rewrites).
CREATE UNIQUE INDEX IF NOT EXISTS ux_pack_entries_published ON pack_entries (asset_id) WHERE state = 'PUBLISHED';
CREATE UNIQUE INDEX IF NOT EXISTS ux_pack_entries_reserved ON pack_entries (asset_id) WHERE state = 'RESERVED';
-- Every pack holding an asset's bytes, whatever the entry's state.
CREATE INDEX IF NOT EXISTS ix_pack_entries_asset ON pack_entries (asset_id);
-- Sources to retire, and deleted entries waiting for their pack to go.
CREATE INDEX IF NOT EXISTS ix_pack_entries_sources ON pack_entries (pack_id) WHERE state = 'PUBLISHED' AND source_retired_at IS NULL;
CREATE INDEX IF NOT EXISTS ix_pack_entries_deleted ON pack_entries (pack_id) WHERE state = 'DELETED';

-- An entry's identity, place and checksum are fixed when it is planned:
-- only its state and timestamps change, so a published entry can never be
-- pointed at other bytes.
CREATE OR REPLACE FUNCTION pack_entry_immutable() RETURNS trigger AS $$
BEGIN
    IF NEW.pack_id <> OLD.pack_id OR NEW.tenant_id <> OLD.tenant_id OR NEW.ordinal <> OLD.ordinal
       OR NEW.asset_id <> OLD.asset_id OR NEW.byte_offset <> OLD.byte_offset
       OR NEW.stored_length <> OLD.stored_length OR NEW.size_bytes <> OLD.size_bytes
       OR NEW.checksum_sha256 <> OLD.checksum_sha256 THEN
        RAISE EXCEPTION 'a pack entry''s identity and byte range cannot change'
            USING ERRCODE = 'integrity_constraint_violation', CONSTRAINT = 'pack_entry_immutable';
    END IF;
    RETURN NEW;
END
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS tr_pack_entries_immutable ON pack_entries;
CREATE TRIGGER tr_pack_entries_immutable BEFORE UPDATE ON pack_entries
    FOR EACH ROW EXECUTE FUNCTION pack_entry_immutable();

-- The packing scan pages through each tenant's published assets in ID
-- order, and steps from tenant to tenant.
CREATE INDEX IF NOT EXISTS ix_assets_pack_scan ON assets (tenant_id, id) WHERE status = 'AVAILABLE';

COMMIT;
