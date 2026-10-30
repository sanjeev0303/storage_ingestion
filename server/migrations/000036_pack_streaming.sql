-- Pack-aware streaming (docs/streaming.md): each pack entry names the pack
-- it is copied from, so a repack can gather a dataset's entries from many
-- packs; an organization's target pack size; repack requests; and a
-- training run's verification reports and checkpoints.

ALTER TABLE pack_entries
    ADD COLUMN IF NOT EXISTS source_pack_id TEXT,
    ADD COLUMN IF NOT EXISTS source_offset BIGINT CHECK (source_offset >= 0);

-- A rewrite's entries were copied from the pack it rewrites.
UPDATE pack_entries e SET source_pack_id = p.rewrite_of, source_offset = o.byte_offset
FROM packs p, pack_entries o
WHERE e.pack_id = p.id AND p.rewrite_of IS NOT NULL AND e.source_pack_id IS NULL
  AND o.pack_id = p.rewrite_of AND o.asset_id = e.asset_id;

CREATE INDEX IF NOT EXISTS ix_pack_entries_source_pack ON pack_entries (source_pack_id) WHERE state = 'RESERVED';

CREATE TABLE IF NOT EXISTS tenant_packing (
    tenant_id         TEXT PRIMARY KEY,
    target_pack_bytes BIGINT NOT NULL CHECK (target_pack_bytes BETWEEN 1048576 AND 10737418240),
    updated_by        TEXT NOT NULL,
    updated_at        TIMESTAMPTZ NOT NULL
);

-- A repack gathers a dataset's packed entries, in path order, into new
-- packs of the target size. The cursor is the last path planned.
CREATE TABLE IF NOT EXISTS repack_requests (
    id              TEXT PRIMARY KEY,
    tenant_id       TEXT NOT NULL,
    dataset_id      TEXT NOT NULL,
    state           TEXT NOT NULL CHECK (state IN ('PENDING', 'DONE', 'CANCELLED')),
    cursor_key      TEXT COLLATE "C" NOT NULL DEFAULT '',
    cursor_asset    TEXT NOT NULL DEFAULT '',
    packs_planned   INTEGER NOT NULL DEFAULT 0,
    entries_planned INTEGER NOT NULL DEFAULT 0,
    requested_by    TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL,
    updated_at      TIMESTAMPTZ NOT NULL,
    finished_at     TIMESTAMPTZ
);
CREATE UNIQUE INDEX IF NOT EXISTS ux_repack_requests_active ON repack_requests (dataset_id) WHERE state = 'PENDING';
CREATE INDEX IF NOT EXISTS ix_repack_requests_dataset ON repack_requests (tenant_id, dataset_id, created_at DESC);

-- What a training run's data loader found when it re-hashed what it read.
CREATE TABLE IF NOT EXISTS run_verifications (
    id          TEXT PRIMARY KEY,
    tenant_id   TEXT NOT NULL,
    run_id      TEXT NOT NULL,
    version_id  TEXT NOT NULL DEFAULT '',
    host        TEXT NOT NULL CHECK (char_length(host) BETWEEN 1 AND 255),
    sample_rate DOUBLE PRECISION NOT NULL CHECK (sample_rate > 0 AND sample_rate <= 1),
    checked     BIGINT NOT NULL CHECK (checked >= 0),
    failed      BIGINT NOT NULL CHECK (failed >= 0),
    -- The first failures, each {path, reason}.
    failures    JSONB NOT NULL DEFAULT '[]' CHECK (jsonb_typeof(failures) = 'array' AND octet_length(failures::text) <= 65536),
    created_by  TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL,
    FOREIGN KEY (run_id, tenant_id) REFERENCES training_runs (id, tenant_id)
);
CREATE INDEX IF NOT EXISTS ix_run_verifications_run ON run_verifications (tenant_id, run_id, created_at DESC);

-- A checkpoint is a snapshot of the files a run saved at a step. It is
-- verified when a host other than the one that wrote it re-reads every
-- file against its SHA-256.
CREATE TABLE IF NOT EXISTS run_checkpoints (
    id            TEXT PRIMARY KEY,
    tenant_id     TEXT NOT NULL,
    run_id        TEXT NOT NULL,
    step          BIGINT NOT NULL CHECK (step >= 0),
    version_id    TEXT NOT NULL,
    written_by    TEXT NOT NULL CHECK (char_length(written_by) BETWEEN 1 AND 255),
    verified_by   TEXT,
    verified_at   TIMESTAMPTZ,
    verify_error  TEXT,
    created_by    TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL,
    UNIQUE (run_id, step),
    FOREIGN KEY (run_id, tenant_id) REFERENCES training_runs (id, tenant_id),
    FOREIGN KEY (version_id, tenant_id) REFERENCES dataset_versions (id, tenant_id)
);
CREATE INDEX IF NOT EXISTS ix_run_checkpoints_run ON run_checkpoints (tenant_id, run_id, step DESC);
