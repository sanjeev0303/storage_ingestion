BEGIN;

-- Phase 11: storage connections. An organization's bytes live on the
-- platform-managed connection or on a connection to its own storage.
-- Every upload and object version records the connection it was written
-- to; existing records are bound to the platform connection, which is
-- what they were always on. No credential is ever stored in plaintext:
-- only AES-256-GCM ciphertext bound to tenant, connection, provider and
-- credential version (docs/storage-connections.md).

CREATE TABLE IF NOT EXISTS storage_connections (
    id                      TEXT PRIMARY KEY,
    -- NULL only for the platform connection, which every tenant shares.
    tenant_id               TEXT,
    name                    TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 128),
    provider                TEXT NOT NULL CHECK (provider IN ('PLATFORM_MANAGED', 'AWS_S3', 'S3_COMPATIBLE', 'CLOUDFLARE_R2',
                                                              'GOOGLE_CLOUD_STORAGE', 'AZURE_BLOB')),
    status                  TEXT NOT NULL CHECK (status IN ('PENDING', 'ACTIVE', 'DISABLED', 'DELETED')),
    auth_method             TEXT NOT NULL CHECK (auth_method IN ('PLATFORM', 'ACCESS_KEY', 'ASSUME_ROLE', 'SERVICE_ACCOUNT_KEY',
                                                                 'ACCOUNT_KEY', 'SAS_TOKEN')),
    bucket                  TEXT NOT NULL DEFAULT '',
    region                  TEXT NOT NULL DEFAULT '',
    endpoint                TEXT NOT NULL DEFAULT '',
    account_id              TEXT NOT NULL DEFAULT '',
    force_path_style        BOOLEAN NOT NULL DEFAULT false,
    role_arn                TEXT NOT NULL DEFAULT '',
    external_id             TEXT NOT NULL DEFAULT '',
    staging_prefix          TEXT NOT NULL DEFAULT 'quarantine',
    ready_prefix            TEXT NOT NULL DEFAULT 'assets',
    staging_mode            TEXT NOT NULL DEFAULT 'CONNECTION' CHECK (staging_mode IN ('CONNECTION', 'PLATFORM')),
    -- Sealed credentials: version 0 means none stored.
    credential_version      INTEGER NOT NULL DEFAULT 0 CHECK (credential_version >= 0),
    credential_key_version  INTEGER,
    credential_nonce        BYTEA,
    credential_ciphertext   BYTEA,
    credential_rotated_at   TIMESTAMPTZ,
    capabilities            JSONB,
    capabilities_checked_at TIMESTAMPTZ,
    last_tested_at          TIMESTAMPTZ,
    last_test_ok_at         TIMESTAMPTZ,
    last_error_category     TEXT,
    revision                BIGINT NOT NULL DEFAULT 1,
    created_by              TEXT NOT NULL,
    updated_by              TEXT,
    created_at              TIMESTAMPTZ NOT NULL,
    updated_at              TIMESTAMPTZ NOT NULL,
    disabled_at             TIMESTAMPTZ,
    deleted_at              TIMESTAMPTZ,
    CHECK ((tenant_id IS NULL) = (id = 'platform')),
    CHECK ((credential_ciphertext IS NULL) = (credential_nonce IS NULL)),
    CHECK ((credential_ciphertext IS NULL) = (credential_version = 0)),
    CHECK (staging_prefix <> ready_prefix)
);
-- Names are unique per tenant among connections that are not deleted.
CREATE UNIQUE INDEX IF NOT EXISTS ux_storage_connections_name
    ON storage_connections (tenant_id, lower(name)) WHERE status <> 'DELETED';
CREATE INDEX IF NOT EXISTS ix_storage_connections_tenant ON storage_connections (tenant_id, created_at, id);

INSERT INTO storage_connections (id, tenant_id, name, provider, status, auth_method, created_by, created_at, updated_at)
VALUES ('platform', NULL, 'Platform-managed storage', 'PLATFORM_MANAGED', 'ACTIVE', 'PLATFORM', 'migration', now(), now())
ON CONFLICT (id) DO NOTHING;

-- An organization's default, and an optional dataset-level choice
-- (NULL: the organization's default at upload time).
ALTER TABLE organizations ADD COLUMN IF NOT EXISTS default_storage_connection_id TEXT NOT NULL DEFAULT 'platform'
    REFERENCES storage_connections (id);
ALTER TABLE datasets ADD COLUMN IF NOT EXISTS storage_connection_id TEXT REFERENCES storage_connections (id);

-- The immutable storage binding. storage_connection_id is where an
-- object's published bytes live; staging_connection_id where its upload
-- landed (the same, unless the connection stages on the platform). The
-- constant default binds every existing row to the platform connection
-- without rewriting the tables.
ALTER TABLE upload_sessions ADD COLUMN IF NOT EXISTS storage_connection_id TEXT NOT NULL DEFAULT 'platform'
    REFERENCES storage_connections (id);
ALTER TABLE upload_sessions ADD COLUMN IF NOT EXISTS staging_connection_id TEXT NOT NULL DEFAULT 'platform'
    REFERENCES storage_connections (id);
ALTER TABLE assets ADD COLUMN IF NOT EXISTS storage_connection_id TEXT NOT NULL DEFAULT 'platform'
    REFERENCES storage_connections (id);
ALTER TABLE assets ADD COLUMN IF NOT EXISTS staging_connection_id TEXT NOT NULL DEFAULT 'platform'
    REFERENCES storage_connections (id);
ALTER TABLE processing_jobs ADD COLUMN IF NOT EXISTS storage_connection_id TEXT NOT NULL DEFAULT 'platform'
    REFERENCES storage_connections (id);
-- The current version's connection, copied with its other fields.
ALTER TABLE catalog_objects ADD COLUMN IF NOT EXISTS storage_connection_id TEXT REFERENCES storage_connections (id);

-- What still depends on a connection, for deletion checks.
CREATE INDEX IF NOT EXISTS ix_upload_sessions_connection ON upload_sessions (storage_connection_id)
    WHERE storage_connection_id <> 'platform';
CREATE INDEX IF NOT EXISTS ix_upload_sessions_staging_connection ON upload_sessions (staging_connection_id)
    WHERE staging_connection_id <> 'platform';
CREATE INDEX IF NOT EXISTS ix_assets_connection ON assets (storage_connection_id)
    WHERE storage_connection_id <> 'platform';
CREATE INDEX IF NOT EXISTS ix_assets_staging_connection ON assets (staging_connection_id)
    WHERE staging_connection_id <> 'platform';

-- Inventory scans and orphan candidates are per connection: the same
-- bucket name on two connections is two different buckets.
ALTER TABLE inventory_scans ADD COLUMN IF NOT EXISTS storage_connection_id TEXT NOT NULL DEFAULT 'platform'
    REFERENCES storage_connections (id);
ALTER TABLE orphan_candidates ADD COLUMN IF NOT EXISTS storage_connection_id TEXT NOT NULL DEFAULT 'platform'
    REFERENCES storage_connections (id);
DROP INDEX IF EXISTS ux_inventory_scans_running;
CREATE UNIQUE INDEX IF NOT EXISTS ux_inventory_scans_running
    ON inventory_scans (storage_connection_id, bucket, prefix) WHERE status = 'RUNNING';
DROP INDEX IF EXISTS ux_orphan_candidates_open;
CREATE UNIQUE INDEX IF NOT EXISTS ux_orphan_candidates_open
    ON orphan_candidates (storage_connection_id, bucket, object_key) WHERE status IN ('MARKED', 'FAILED');

-- A record may be bound only to the platform connection or to a
-- connection of its own tenant, never to a deleted one. New work (an
-- upload session, a dataset's choice, an organization's default) needs an
-- active connection; an asset may still be written to a disabled one,
-- which drains the uploads it already took. Upload and asset bindings
-- never change once written. The trigger arguments name the columns.
CREATE OR REPLACE FUNCTION storage_binding_tenant_check() RETURNS trigger AS $$
DECLARE
    column_name   TEXT;
    connection_id TEXT;
    previous_id   TEXT;
    owner         TEXT;
    state         TEXT;
    owning_tenant TEXT;
BEGIN
    IF TG_TABLE_NAME = 'organizations' THEN
        owning_tenant := NEW.id;
    ELSE
        owning_tenant := NEW.tenant_id;
    END IF;
    FOREACH column_name IN ARRAY TG_ARGV LOOP
        EXECUTE format('SELECT ($1).%I', column_name) USING NEW INTO connection_id;
        IF TG_OP = 'UPDATE' THEN
            EXECUTE format('SELECT ($1).%I', column_name) USING OLD INTO previous_id;
            IF previous_id IS NOT DISTINCT FROM connection_id THEN
                CONTINUE;
            END IF;
            IF TG_TABLE_NAME IN ('upload_sessions', 'assets') THEN
                RAISE EXCEPTION 'the storage binding of % % cannot change', TG_TABLE_NAME, NEW.id
                    USING ERRCODE = 'integrity_constraint_violation', CONSTRAINT = 'storage_binding_immutable';
            END IF;
        END IF;
        IF connection_id IS NULL OR connection_id = 'platform' THEN
            CONTINUE;
        END IF;
        SELECT tenant_id, status INTO owner, state FROM storage_connections WHERE id = connection_id;
        IF owner IS DISTINCT FROM owning_tenant THEN
            RAISE EXCEPTION 'storage connection % does not belong to tenant %', connection_id, owning_tenant
                USING ERRCODE = 'foreign_key_violation', CONSTRAINT = 'storage_connection_tenant';
        END IF;
        IF state = 'DELETED' OR (state <> 'ACTIVE' AND TG_TABLE_NAME <> 'assets') THEN
            RAISE EXCEPTION 'storage connection % is %', connection_id, state
                USING ERRCODE = 'foreign_key_violation', CONSTRAINT = 'storage_connection_active';
        END IF;
    END LOOP;
    RETURN NEW;
END $$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS tr_upload_sessions_storage_tenant ON upload_sessions;
CREATE TRIGGER tr_upload_sessions_storage_tenant BEFORE INSERT OR UPDATE OF storage_connection_id, staging_connection_id
    ON upload_sessions FOR EACH ROW EXECUTE FUNCTION storage_binding_tenant_check('storage_connection_id', 'staging_connection_id');
DROP TRIGGER IF EXISTS tr_assets_storage_tenant ON assets;
CREATE TRIGGER tr_assets_storage_tenant BEFORE INSERT OR UPDATE OF storage_connection_id, staging_connection_id
    ON assets FOR EACH ROW EXECUTE FUNCTION storage_binding_tenant_check('storage_connection_id', 'staging_connection_id');
DROP TRIGGER IF EXISTS tr_datasets_storage_tenant ON datasets;
CREATE TRIGGER tr_datasets_storage_tenant BEFORE INSERT OR UPDATE OF storage_connection_id
    ON datasets FOR EACH ROW EXECUTE FUNCTION storage_binding_tenant_check('storage_connection_id');
DROP TRIGGER IF EXISTS tr_organizations_storage_tenant ON organizations;
CREATE TRIGGER tr_organizations_storage_tenant BEFORE INSERT OR UPDATE OF default_storage_connection_id
    ON organizations FOR EACH ROW EXECUTE FUNCTION storage_binding_tenant_check('default_storage_connection_id');

COMMIT;
