BEGIN;

-- SAML 2.0 beside OpenID Connect (docs/sso.md, "SAML"): a connection is
-- one or the other. A SAML connection keeps its identity provider's
-- metadata, and has no client ID or secret.
ALTER TABLE sso_connections ADD COLUMN IF NOT EXISTS protocol TEXT NOT NULL DEFAULT 'OIDC'
    CHECK (protocol IN ('OIDC', 'SAML'));
ALTER TABLE sso_connections ADD COLUMN IF NOT EXISTS idp_metadata TEXT
    CHECK (idp_metadata IS NULL OR octet_length(idp_metadata) <= 262144);
ALTER TABLE sso_connections DROP CONSTRAINT IF EXISTS sso_connections_client_id_check;
ALTER TABLE sso_connections ADD CONSTRAINT sso_connections_client_id_check CHECK (length(client_id) <= 512);
ALTER TABLE sso_connections ALTER COLUMN secret_key_version DROP NOT NULL;
ALTER TABLE sso_connections ALTER COLUMN secret_nonce DROP NOT NULL;
ALTER TABLE sso_connections ALTER COLUMN secret_ciphertext DROP NOT NULL;

-- SCIM 2.0 provisioning (docs/sso.md, "SCIM"): one bearer token per
-- organization, stored only as its SHA-256.
CREATE TABLE IF NOT EXISTS scim_tokens (
    org_id       TEXT PRIMARY KEY REFERENCES organizations (id),
    token_hash   BYTEA NOT NULL UNIQUE,
    created_by   TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL,
    last_used_at TIMESTAMPTZ
);

COMMIT;
