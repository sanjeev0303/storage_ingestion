BEGIN;

-- Accounts. An account is PENDING until its owner sets a password through
-- an activation token, ACTIVE afterwards, and DEACTIVATED by an
-- administrator (its sessions are revoked with it). Emails are unique
-- without regard to case. Password hashes are Argon2id PHC strings and
-- never leave the repository layer.
CREATE TABLE IF NOT EXISTS users (
    id                  TEXT PRIMARY KEY,
    email               TEXT NOT NULL CHECK (length(email) BETWEEN 3 AND 254),
    display_name        TEXT NOT NULL DEFAULT '',
    password_hash       TEXT,
    status              TEXT NOT NULL CHECK (status IN ('PENDING', 'ACTIVE', 'DEACTIVATED')),
    -- Platform roles are separate from organization roles: a platform
    -- administrator manages organizations and users, not their data.
    platform_role       TEXT CHECK (platform_role IN ('ADMIN', 'AUDITOR')),
    password_changed_at TIMESTAMPTZ,
    last_login_at       TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL,
    updated_at          TIMESTAMPTZ NOT NULL,
    CHECK (status <> 'ACTIVE' OR password_hash IS NOT NULL)
);

CREATE UNIQUE INDEX IF NOT EXISTS ux_users_email ON users (lower(email));

-- One row per login. Tokens are opaque random strings; only their SHA-256
-- is stored. The session holds the current access token; refresh tokens
-- are kept so a replayed one can be recognised and its session revoked.
CREATE TABLE IF NOT EXISTS auth_sessions (
    id                TEXT PRIMARY KEY,
    user_id           TEXT NOT NULL REFERENCES users (id),
    access_hash       BYTEA NOT NULL,
    access_expires_at TIMESTAMPTZ NOT NULL,
    -- expires_at ends the session whatever refreshes happen.
    expires_at        TIMESTAMPTZ NOT NULL,
    source_ip         TEXT,
    user_agent        TEXT,
    created_at        TIMESTAMPTZ NOT NULL,
    last_used_at      TIMESTAMPTZ NOT NULL,
    revoked_at        TIMESTAMPTZ,
    revoke_reason     TEXT
);

CREATE UNIQUE INDEX IF NOT EXISTS ux_auth_sessions_access ON auth_sessions (access_hash);
CREATE INDEX IF NOT EXISTS ix_auth_sessions_user ON auth_sessions (user_id) WHERE revoked_at IS NULL;

CREATE TABLE IF NOT EXISTS auth_refresh_tokens (
    token_hash BYTEA PRIMARY KEY,
    session_id TEXT NOT NULL REFERENCES auth_sessions (id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL,
    used_at    TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS ix_auth_refresh_session ON auth_refresh_tokens (session_id);

-- Single-use activation and password-reset tokens, hashed like the others.
CREATE TABLE IF NOT EXISTS auth_credential_tokens (
    token_hash BYTEA PRIMARY KEY,
    user_id    TEXT NOT NULL REFERENCES users (id),
    purpose    TEXT NOT NULL CHECK (purpose IN ('ACTIVATE', 'RESET')),
    created_by TEXT,
    created_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    used_at    TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS ix_auth_credential_user ON auth_credential_tokens (user_id) WHERE used_at IS NULL;

-- Organizations are tenants: an organization's ID is the tenant_id every
-- tenant-owned table carries. An organization is never physically
-- deleted; DELETED is reachable only once it owns nothing that still has
-- to be kept.
CREATE TABLE IF NOT EXISTS organizations (
    id             TEXT PRIMARY KEY,
    name           TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 200),
    slug           TEXT NOT NULL,
    status         TEXT NOT NULL CHECK (status IN ('ACTIVE', 'SUSPENDED', 'DEACTIVATED', 'DELETED')),
    settings       JSONB NOT NULL DEFAULT '{}',
    created_by     TEXT NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL,
    updated_at     TIMESTAMPTZ NOT NULL,
    deactivated_at TIMESTAMPTZ
);

CREATE UNIQUE INDEX IF NOT EXISTS ux_organizations_slug ON organizations (slug);

-- Custom roles of one organization. The built-in roles (owner, admin,
-- developer, auditor) are defined in code and never stored.
CREATE TABLE IF NOT EXISTS org_roles (
    id          TEXT PRIMARY KEY,
    org_id      TEXT NOT NULL REFERENCES organizations (id),
    name        TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 64),
    description TEXT NOT NULL DEFAULT '',
    permissions TEXT[] NOT NULL,
    created_by  TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL,
    deleted_at  TIMESTAMPTZ
);

CREATE UNIQUE INDEX IF NOT EXISTS ux_org_roles_name ON org_roles (org_id, lower(name)) WHERE deleted_at IS NULL;

CREATE TABLE IF NOT EXISTS memberships (
    org_id     TEXT NOT NULL REFERENCES organizations (id),
    user_id    TEXT NOT NULL REFERENCES users (id),
    role_id    TEXT NOT NULL,
    status     TEXT NOT NULL CHECK (status IN ('ACTIVE', 'SUSPENDED')),
    created_by TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (org_id, user_id)
);

CREATE INDEX IF NOT EXISTS ix_memberships_user ON memberships (user_id);

CREATE TABLE IF NOT EXISTS invitations (
    id          TEXT PRIMARY KEY,
    org_id      TEXT NOT NULL REFERENCES organizations (id),
    email       TEXT NOT NULL,
    role_id     TEXT NOT NULL,
    token_hash  BYTEA NOT NULL,
    status      TEXT NOT NULL CHECK (status IN ('PENDING', 'ACCEPTED', 'REVOKED')),
    created_by  TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL,
    expires_at  TIMESTAMPTZ NOT NULL,
    accepted_at TIMESTAMPTZ,
    accepted_by TEXT
);

CREATE UNIQUE INDEX IF NOT EXISTS ux_invitations_token ON invitations (token_hash);
CREATE UNIQUE INDEX IF NOT EXISTS ux_invitations_pending ON invitations (org_id, lower(email)) WHERE status = 'PENDING';
CREATE INDEX IF NOT EXISTS ix_invitations_org ON invitations (org_id, created_at DESC, id);

-- Security audit log. Each chain (a tenant, or '_platform') is a hash
-- chain: hash = SHA-256(prev_hash || the row's content), with the chain's
-- head kept in audit_chain_heads. The triggers below make the table
-- append-only for every ordinary statement.
CREATE TABLE IF NOT EXISTS audit_events (
    id             TEXT PRIMARY KEY,
    chain          TEXT NOT NULL,
    seq            BIGINT NOT NULL CHECK (seq > 0),
    tenant_id      TEXT,
    actor_type     TEXT NOT NULL CHECK (actor_type IN ('USER', 'SERVICE', 'ANONYMOUS')),
    actor_id       TEXT NOT NULL,
    action         TEXT NOT NULL,
    resource_type  TEXT NOT NULL,
    resource_id    TEXT NOT NULL DEFAULT '',
    outcome        TEXT NOT NULL CHECK (outcome IN ('SUCCESS', 'DENIED', 'FAILED')),
    reason         TEXT NOT NULL DEFAULT '',
    correlation_id TEXT NOT NULL DEFAULT '',
    source_ip      TEXT NOT NULL DEFAULT '',
    details        JSONB NOT NULL DEFAULT '{}' CHECK (octet_length(details::text) <= 16384),
    created_at     TIMESTAMPTZ NOT NULL,
    prev_hash      BYTEA,
    hash           BYTEA NOT NULL,
    UNIQUE (chain, seq)
);

CREATE INDEX IF NOT EXISTS ix_audit_events_tenant ON audit_events (tenant_id, seq DESC);
CREATE INDEX IF NOT EXISTS ix_audit_events_created ON audit_events (created_at);

CREATE TABLE IF NOT EXISTS audit_chain_heads (
    chain      TEXT PRIMARY KEY,
    seq        BIGINT NOT NULL,
    hash       BYTEA NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);

-- Written when retention purges the start of a chain, so verification can
-- start from the last purged row instead of from the beginning.
CREATE TABLE IF NOT EXISTS audit_checkpoints (
    chain      TEXT NOT NULL,
    seq        BIGINT NOT NULL,
    hash       BYTEA NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (chain, seq)
);

CREATE OR REPLACE FUNCTION audit_events_append_only() RETURNS trigger AS $$
BEGIN
    -- Retention purge is the one deletion allowed, and only by a
    -- transaction that says so.
    IF TG_OP = 'DELETE' AND current_setting('storage.audit_purge', true) = 'on' THEN
        RETURN OLD;
    END IF;
    RAISE EXCEPTION 'audit_events is append-only' USING ERRCODE = 'insufficient_privilege';
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS audit_events_append_only ON audit_events;
CREATE TRIGGER audit_events_append_only
BEFORE UPDATE OR DELETE ON audit_events
FOR EACH ROW EXECUTE FUNCTION audit_events_append_only();

DROP TRIGGER IF EXISTS audit_events_no_truncate ON audit_events;
CREATE TRIGGER audit_events_no_truncate
BEFORE TRUNCATE ON audit_events
FOR EACH STATEMENT EXECUTE FUNCTION audit_events_append_only();

-- Quotas: one policy table for every per-tenant limit. Phase 7's
-- ingestion quota table becomes it; NULL columns fall back to the
-- configured defaults.
ALTER TABLE IF EXISTS ingestion_tenant_quotas RENAME TO tenant_quotas;

ALTER TABLE tenant_quotas
    ADD COLUMN IF NOT EXISTS max_assets              BIGINT  CHECK (max_assets IS NULL OR max_assets > 0),
    ADD COLUMN IF NOT EXISTS max_object_bytes        BIGINT  CHECK (max_object_bytes IS NULL OR max_object_bytes > 0),
    ADD COLUMN IF NOT EXISTS max_active_multipart    INTEGER CHECK (max_active_multipart IS NULL OR max_active_multipart > 0),
    ADD COLUMN IF NOT EXISTS max_pending_processing  INTEGER CHECK (max_pending_processing IS NULL OR max_pending_processing > 0),
    ADD COLUMN IF NOT EXISTS max_manifest_entries    BIGINT  CHECK (max_manifest_entries IS NULL OR max_manifest_entries > 0),
    ADD COLUMN IF NOT EXISTS max_manifest_bytes      BIGINT  CHECK (max_manifest_bytes IS NULL OR max_manifest_bytes > 0),
    ADD COLUMN IF NOT EXISTS max_requests_per_minute INTEGER CHECK (max_requests_per_minute IS NULL OR max_requests_per_minute > 0);

-- Storage usage per tenant, changed in the same transaction as the
-- upload session or asset change it reflects. Reconciliation recomputes
-- it from the sessions and assets, which are authoritative.
CREATE TABLE IF NOT EXISTS tenant_usage (
    tenant_id              TEXT PRIMARY KEY,
    -- Declared bytes held by open reservations (one per upload session).
    reserved_bytes         BIGINT NOT NULL DEFAULT 0 CHECK (reserved_bytes >= 0),
    reserved_uploads       BIGINT NOT NULL DEFAULT 0 CHECK (reserved_uploads >= 0),
    -- Verified bytes and count of AVAILABLE assets.
    stored_bytes           BIGINT NOT NULL DEFAULT 0 CHECK (stored_bytes >= 0),
    asset_count            BIGINT NOT NULL DEFAULT 0 CHECK (asset_count >= 0),
    -- Soft-deleted assets whose objects are still kept.
    pending_deletion_bytes BIGINT NOT NULL DEFAULT 0 CHECK (pending_deletion_bytes >= 0),
    updated_at             TIMESTAMPTZ NOT NULL
);

-- The reservation of an upload session lives on the session itself, so
-- every status change can settle it in the same statement's transaction.
ALTER TABLE upload_sessions
    ADD COLUMN IF NOT EXISTS quota_state    TEXT CHECK (quota_state IN ('RESERVED', 'CONSUMED', 'RELEASED')),
    ADD COLUMN IF NOT EXISTS reserved_bytes BIGINT NOT NULL DEFAULT 0 CHECK (reserved_bytes >= 0),
    -- Known when the session is created, before the provider upload is.
    ADD COLUMN IF NOT EXISTS multipart      BOOLEAN NOT NULL DEFAULT false;

-- Counted under the tenant's usage lock when a reservation is made.
CREATE INDEX IF NOT EXISTS ix_upload_sessions_active ON upload_sessions (tenant_id)
WHERE status IN ('PENDING', 'UPLOADING', 'COMPLETING', 'ABORTING');
CREATE INDEX IF NOT EXISTS ix_upload_sessions_reserved ON upload_sessions (expires_at)
WHERE quota_state = 'RESERVED';
CREATE INDEX IF NOT EXISTS ix_processing_jobs_pending_tenant ON processing_jobs (tenant_id)
WHERE state = 'PENDING';

-- Composite keys: a child row cannot reference another tenant's parent,
-- whatever the application does.
CREATE UNIQUE INDEX IF NOT EXISTS ux_upload_sessions_id_tenant ON upload_sessions (id, tenant_id);
CREATE UNIQUE INDEX IF NOT EXISTS ux_assets_id_tenant ON assets (id, tenant_id);
CREATE UNIQUE INDEX IF NOT EXISTS ux_datasets_id_tenant ON datasets (id, tenant_id);
CREATE UNIQUE INDEX IF NOT EXISTS ux_ingestion_jobs_id_tenant ON ingestion_jobs (id, tenant_id);

DO $$
DECLARE
    fk RECORD;
BEGIN
    FOR fk IN
        SELECT * FROM (VALUES
            ('assets', 'fk_assets_upload_tenant', '(upload_id, tenant_id)', 'upload_sessions (id, tenant_id)', 'CASCADE'),
            ('processing_jobs', 'fk_processing_jobs_asset_tenant', '(asset_id, tenant_id)', 'assets (id, tenant_id)', 'CASCADE'),
            ('quarantine_records', 'fk_quarantine_asset_tenant', '(asset_id, tenant_id)', 'assets (id, tenant_id)', 'CASCADE'),
            ('download_tokens', 'fk_download_tokens_asset_tenant', '(asset_id, tenant_id)', 'assets (id, tenant_id)', 'CASCADE'),
            ('deletion_operations', 'fk_deletion_asset_tenant', '(asset_id, tenant_id)', 'assets (id, tenant_id)', 'CASCADE'),
            ('ingestion_jobs', 'fk_ingestion_jobs_dataset_tenant', '(dataset_id, tenant_id)', 'datasets (id, tenant_id)', 'NO ACTION'),
            ('ingestion_entries', 'fk_ingestion_entries_job_tenant', '(job_id, tenant_id)', 'ingestion_jobs (id, tenant_id)', 'CASCADE')
        ) AS t(tbl, name, cols, target, on_delete)
    LOOP
        IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = fk.name) THEN
            EXECUTE format('ALTER TABLE %I ADD CONSTRAINT %I FOREIGN KEY %s REFERENCES %s ON DELETE %s',
                fk.tbl, fk.name, fk.cols, fk.target, fk.on_delete);
        END IF;
    END LOOP;
END;
$$;

COMMIT;
