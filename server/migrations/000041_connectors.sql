-- Ecosystem connectors: chat shares, Git LFS locks, imported collections and
-- the DICOM index behind the DICOMweb gateway (docs/connectors.md).

CREATE TABLE IF NOT EXISTS chat_integrations (
    id              TEXT PRIMARY KEY,
    tenant_id       TEXT NOT NULL,
    kind            TEXT NOT NULL CHECK (kind IN ('SLACK', 'TEAMS')),
    name            TEXT NOT NULL CHECK (char_length(name) BETWEEN 1 AND 100),
    -- The incoming webhook URL is a credential: sealed, with its host kept to show.
    url_host        TEXT NOT NULL,
    url_key_version INTEGER NOT NULL,
    url_nonce       BYTEA NOT NULL,
    url_cipher      BYTEA NOT NULL,
    created_by      TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS ix_chat_integrations_tenant ON chat_integrations (tenant_id, created_at DESC);

-- What a chat message links to: a page that needs a signed-in member, never
-- the bytes, so a chat export holds only links that stop working.
CREATE TABLE IF NOT EXISTS chat_shares (
    id             TEXT PRIMARY KEY,
    tenant_id      TEXT NOT NULL,
    asset_id       TEXT NOT NULL,
    integration_id TEXT NOT NULL REFERENCES chat_integrations (id) ON DELETE CASCADE,
    note           TEXT NOT NULL DEFAULT '' CHECK (char_length(note) <= 500),
    expires_at     TIMESTAMPTZ NOT NULL,
    revoked_at     TIMESTAMPTZ,
    opens          BIGINT NOT NULL DEFAULT 0,
    created_by     TEXT NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL,
    FOREIGN KEY (asset_id, tenant_id) REFERENCES assets (id, tenant_id)
);
CREATE INDEX IF NOT EXISTS ix_chat_shares_tenant ON chat_shares (tenant_id, created_at DESC);

CREATE TABLE IF NOT EXISTS lfs_locks (
    id        TEXT PRIMARY KEY,
    tenant_id TEXT NOT NULL,
    dataset   TEXT NOT NULL,
    path      TEXT NOT NULL CHECK (char_length(path) BETWEEN 1 AND 1024),
    owner_id  TEXT NOT NULL,
    locked_at TIMESTAMPTZ NOT NULL,
    UNIQUE (tenant_id, dataset, path)
);

-- A comms-archive or forensic export brought in: what it said it held, and
-- what the import found.
CREATE TABLE IF NOT EXISTS collection_imports (
    id          TEXT PRIMARY KEY,
    tenant_id   TEXT NOT NULL,
    dataset_id  TEXT NOT NULL,
    format      TEXT NOT NULL CHECK (format IN ('COMMS_JSONL', 'RSMF', 'UFDR', 'MANIFEST')),
    source      TEXT NOT NULL DEFAULT '' CHECK (char_length(source) <= 500),
    export_sha256 TEXT NOT NULL DEFAULT '',
    items       BIGINT NOT NULL,
    verified    BIGINT NOT NULL,
    mismatches  JSONB NOT NULL DEFAULT '[]'::jsonb,
    retention   TEXT NOT NULL DEFAULT '',
    created_by  TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL,
    FOREIGN KEY (dataset_id, tenant_id) REFERENCES datasets (id, tenant_id)
);
CREATE INDEX IF NOT EXISTS ix_collection_imports_dataset ON collection_imports (tenant_id, dataset_id, created_at DESC);

-- Each DICOM instance of a version, from its header.
CREATE TABLE IF NOT EXISTS dicom_instances (
    version_id  TEXT NOT NULL,
    tenant_id   TEXT NOT NULL,
    sop_uid     TEXT NOT NULL,
    study_uid   TEXT NOT NULL,
    series_uid  TEXT NOT NULL,
    modality    TEXT NOT NULL DEFAULT '',
    patient_id  TEXT NOT NULL DEFAULT '',
    study_date  TEXT NOT NULL DEFAULT '',
    asset_id    TEXT NOT NULL,
    path        TEXT NOT NULL,
    tags        JSONB NOT NULL,
    PRIMARY KEY (version_id, sop_uid),
    FOREIGN KEY (version_id, tenant_id) REFERENCES dataset_versions (id, tenant_id)
);
CREATE INDEX IF NOT EXISTS ix_dicom_study ON dicom_instances (version_id, study_uid, series_uid);
