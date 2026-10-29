BEGIN;

-- Single sign-on with OpenID Connect (docs/sso.md): one identity provider
-- per organization.
CREATE TABLE IF NOT EXISTS sso_connections (
    org_id             TEXT PRIMARY KEY REFERENCES organizations (id),
    issuer             TEXT NOT NULL CHECK (length(issuer) BETWEEN 1 AND 2048),
    client_id          TEXT NOT NULL CHECK (length(client_id) BETWEEN 1 AND 512),
    -- The client secret, sealed with the credential keyring.
    secret_key_version INTEGER NOT NULL,
    secret_nonce       BYTEA NOT NULL,
    secret_ciphertext  BYTEA NOT NULL,
    -- Only verified emails in these domains may sign in.
    allowed_domains    TEXT[] NOT NULL CHECK (cardinality(allowed_domains) > 0),
    -- The role given to people who sign in for the first time; empty means
    -- only existing members may sign in.
    default_role       TEXT NOT NULL DEFAULT '',
    enabled            BOOLEAN NOT NULL DEFAULT true,
    updated_by         TEXT NOT NULL,
    created_at         TIMESTAMPTZ NOT NULL,
    updated_at         TIMESTAMPTZ NOT NULL
);

-- A sign-in in progress: single use, short-lived. Only the state's hash is
-- kept.
CREATE TABLE IF NOT EXISTS sso_login_states (
    state_hash   BYTEA PRIMARY KEY,
    org_id       TEXT NOT NULL,
    nonce        TEXT NOT NULL,
    verifier     TEXT NOT NULL,
    redirect_uri TEXT NOT NULL,
    expires_at   TIMESTAMPTZ NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS ix_sso_login_states_expiry ON sso_login_states (expires_at);

-- Which account a provider's subject is. Linked on its first sign-in, so a
-- later change of email at the provider cannot move it to another account.
CREATE TABLE IF NOT EXISTS sso_identities (
    issuer     TEXT NOT NULL,
    subject    TEXT NOT NULL,
    user_id    TEXT NOT NULL REFERENCES users (id),
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (issuer, subject),
    UNIQUE (issuer, user_id)
);

-- An account made by a first SSO sign-in has no password: it is active
-- without one, and can only sign in through its provider.
ALTER TABLE users ADD COLUMN IF NOT EXISTS sso_only BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE users DROP CONSTRAINT IF EXISTS users_check;
ALTER TABLE users ADD CONSTRAINT users_check CHECK (status <> 'ACTIVE' OR password_hash IS NOT NULL OR sso_only);

COMMIT;
