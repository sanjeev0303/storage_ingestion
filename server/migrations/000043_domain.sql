-- Domain metadata and publishing (docs/domain.md): what recordings hold,
-- segments inside them, embedding indexes people bring, the upstream copies
-- imports came from, and snapshots published with a landing page and a DOI.

CREATE TABLE IF NOT EXISTS recording_metadata (
    version_id    TEXT NOT NULL,
    tenant_id     TEXT NOT NULL,
    asset_id      TEXT NOT NULL,
    path          TEXT NOT NULL,
    kind          TEXT NOT NULL CHECK (kind IN ('MCAP', 'ROSBAG2')),
    start_ns      BIGINT NOT NULL,
    end_ns        BIGINT NOT NULL,
    message_count BIGINT NOT NULL,
    topics        JSONB NOT NULL DEFAULT '[]'::jsonb,
    PRIMARY KEY (version_id, asset_id)
);
CREATE INDEX IF NOT EXISTS ix_recording_metadata_time ON recording_metadata (tenant_id, version_id, start_ns);

-- A time range inside a file of a snapshot. It adds to the snapshot and does
-- not change its manifest.
CREATE TABLE IF NOT EXISTS segments (
    id         TEXT PRIMARY KEY,
    tenant_id  TEXT NOT NULL,
    dataset_id TEXT NOT NULL,
    version_id TEXT NOT NULL,
    asset_id   TEXT NOT NULL,
    path       TEXT NOT NULL,
    start_ns   BIGINT NOT NULL,
    end_ns     BIGINT NOT NULL CHECK (end_ns >= start_ns),
    label      TEXT NOT NULL DEFAULT '' CHECK (char_length(label) <= 200),
    note       TEXT NOT NULL DEFAULT '' CHECK (char_length(note) <= 2000),
    attributes JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_by TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS ix_segments_version ON segments (tenant_id, version_id, path, start_ns);
CREATE INDEX IF NOT EXISTS ix_segments_label ON segments (tenant_id, version_id, label);

CREATE TABLE IF NOT EXISTS embedding_indexes (
    id         TEXT PRIMARY KEY,
    tenant_id  TEXT NOT NULL,
    dataset_id TEXT NOT NULL,
    version_id TEXT NOT NULL,
    name       TEXT NOT NULL CHECK (char_length(name) BETWEEN 1 AND 100),
    model      TEXT NOT NULL DEFAULT '' CHECK (char_length(model) <= 200),
    dimensions INTEGER NOT NULL CHECK (dimensions BETWEEN 1 AND 4096),
    metric     TEXT NOT NULL CHECK (metric IN ('COSINE', 'DOT', 'EUCLIDEAN')),
    created_by TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    UNIQUE (version_id, name)
);

-- A vector is little-endian float32s. segment_id is '' for a whole file.
CREATE TABLE IF NOT EXISTS embeddings (
    index_id   TEXT NOT NULL REFERENCES embedding_indexes (id) ON DELETE CASCADE,
    tenant_id  TEXT NOT NULL,
    path       TEXT NOT NULL,
    segment_id TEXT NOT NULL DEFAULT '',
    vector     BYTEA NOT NULL,
    PRIMARY KEY (index_id, path, segment_id)
);

-- The copy an import came from, watched for changing or going away.
CREATE TABLE IF NOT EXISTS upstream_sources (
    tenant_id     TEXT NOT NULL,
    url           TEXT NOT NULL,
    asset_id      TEXT NOT NULL,
    path          TEXT NOT NULL,
    sha256        TEXT NOT NULL DEFAULT '',
    size_bytes    BIGINT NOT NULL DEFAULT 0,
    state         TEXT NOT NULL CHECK (state IN ('WATCHING', 'CHANGED', 'GONE')),
    etag          TEXT NOT NULL DEFAULT '',
    last_modified TEXT NOT NULL DEFAULT '',
    note          TEXT NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL,
    checked_at    TIMESTAMPTZ,
    changed_at    TIMESTAMPTZ,
    next_check_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, url)
);
CREATE INDEX IF NOT EXISTS ix_upstream_due ON upstream_sources (next_check_at);
CREATE INDEX IF NOT EXISTS ix_upstream_asset ON upstream_sources (tenant_id, asset_id);

CREATE OR REPLACE FUNCTION webhook_upstream_source() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.state <> 'WATCHING' AND NEW.state IS DISTINCT FROM OLD.state THEN
        PERFORM queue_webhook(NEW.tenant_id, CASE NEW.state WHEN 'GONE' THEN 'upstream.gone' ELSE 'upstream.changed' END,
            jsonb_build_object('url', NEW.url, 'path', NEW.path, 'asset_id', NEW.asset_id, 'sha256', NEW.sha256, 'note', NEW.note));
    END IF;
    RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS webhook_upstream_source ON upstream_sources;
CREATE TRIGGER webhook_upstream_source AFTER UPDATE ON upstream_sources
    FOR EACH ROW EXECUTE FUNCTION webhook_upstream_source();

-- A snapshot made citable. The landing page outlives a withdrawal: a DOI must
-- keep resolving to something that says what happened.
CREATE TABLE IF NOT EXISTS publications (
    id               TEXT PRIMARY KEY,
    tenant_id        TEXT NOT NULL,
    dataset_id       TEXT NOT NULL,
    version_id       TEXT NOT NULL UNIQUE,
    slug             TEXT NOT NULL UNIQUE,
    title            TEXT NOT NULL CHECK (char_length(title) BETWEEN 1 AND 500),
    description      TEXT NOT NULL DEFAULT '' CHECK (char_length(description) <= 10000),
    creators         JSONB NOT NULL,
    publisher        TEXT NOT NULL CHECK (char_length(publisher) BETWEEN 1 AND 500),
    publication_year INTEGER NOT NULL,
    license          TEXT NOT NULL CHECK (char_length(license) BETWEEN 1 AND 500),
    version_number   INTEGER NOT NULL,
    manifest_sha256  TEXT NOT NULL,
    member_count     BIGINT NOT NULL,
    total_bytes      BIGINT NOT NULL,
    doi              TEXT NOT NULL DEFAULT '',
    doi_error        TEXT NOT NULL DEFAULT '',
    withdrawn_at     TIMESTAMPTZ,
    withdrawn_reason TEXT NOT NULL DEFAULT '' CHECK (char_length(withdrawn_reason) <= 2000),
    created_by       TEXT NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS ix_publications_dataset ON publications (tenant_id, dataset_id, created_at DESC);

-- Upstream sources are lineage nodes.
ALTER TABLE lineage_nodes DROP CONSTRAINT IF EXISTS lineage_nodes_kind_check;
ALTER TABLE lineage_nodes ADD CONSTRAINT lineage_nodes_kind_check CHECK (kind IN (
    'OBJECT_VERSION', 'DATASET', 'DATASET_VERSION', 'MANIFEST_IMPORT', 'VALIDATION_REPORT', 'TRANSFORMATION',
    'TRAINING_RUN', 'MODEL', 'UPSTREAM_SOURCE'));
