-- Training runs and the models they produce, linked into lineage: a run
-- TRAINED_ON the dataset versions it read, a model PRODUCED_BY its run.
-- Runs come from the SDK or from MLflow and Weights & Biases integrations;
-- (source, external_id) makes recording the same run again idempotent.

CREATE TABLE IF NOT EXISTS training_runs (
    id          TEXT PRIMARY KEY,
    tenant_id   TEXT NOT NULL,
    name        TEXT NOT NULL CHECK (char_length(name) BETWEEN 1 AND 200),
    source      TEXT NOT NULL CHECK (source IN ('SDK', 'MLFLOW', 'WANDB', 'OTHER')),
    external_id TEXT NOT NULL DEFAULT '' CHECK (char_length(external_id) <= 256),
    url         TEXT NOT NULL DEFAULT '' CHECK (char_length(url) <= 2048),
    status      TEXT NOT NULL CHECK (status IN ('RUNNING', 'FINISHED', 'FAILED')),
    params      JSONB NOT NULL DEFAULT '{}' CHECK (octet_length(params::text) <= 8192),
    metrics     JSONB NOT NULL DEFAULT '{}' CHECK (octet_length(metrics::text) <= 8192),
    created_by  TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL,
    finished_at TIMESTAMPTZ,
    UNIQUE (id, tenant_id)
);
CREATE UNIQUE INDEX IF NOT EXISTS ux_training_runs_external ON training_runs (tenant_id, source, external_id) WHERE external_id <> '';
CREATE INDEX IF NOT EXISTS ix_training_runs_recent ON training_runs (tenant_id, created_at DESC, id DESC);

-- The dataset versions a run trained on.
CREATE TABLE IF NOT EXISTS training_run_inputs (
    run_id     TEXT NOT NULL,
    version_id TEXT NOT NULL,
    tenant_id  TEXT NOT NULL,
    PRIMARY KEY (run_id, version_id),
    FOREIGN KEY (run_id, tenant_id) REFERENCES training_runs (id, tenant_id),
    FOREIGN KEY (version_id, tenant_id) REFERENCES dataset_versions (id, tenant_id)
);
CREATE INDEX IF NOT EXISTS ix_training_run_inputs_version ON training_run_inputs (tenant_id, version_id);

CREATE TABLE IF NOT EXISTS models (
    id         TEXT PRIMARY KEY,
    tenant_id  TEXT NOT NULL,
    run_id     TEXT NOT NULL,
    name       TEXT NOT NULL CHECK (char_length(name) BETWEEN 1 AND 200),
    version    TEXT NOT NULL CHECK (char_length(version) BETWEEN 1 AND 64),
    uri        TEXT NOT NULL DEFAULT '' CHECK (char_length(uri) <= 2048),
    created_by TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    UNIQUE (tenant_id, name, version),
    FOREIGN KEY (run_id, tenant_id) REFERENCES training_runs (id, tenant_id)
);
CREATE INDEX IF NOT EXISTS ix_models_run ON models (tenant_id, run_id);

ALTER TABLE lineage_nodes DROP CONSTRAINT IF EXISTS lineage_nodes_kind_check;
ALTER TABLE lineage_nodes ADD CONSTRAINT lineage_nodes_kind_check CHECK (kind IN (
    'OBJECT_VERSION', 'DATASET', 'DATASET_VERSION', 'MANIFEST_IMPORT', 'VALIDATION_REPORT', 'TRANSFORMATION',
    'TRAINING_RUN', 'MODEL'));
ALTER TABLE lineage_edges DROP CONSTRAINT IF EXISTS lineage_edges_relation_check;
ALTER TABLE lineage_edges ADD CONSTRAINT lineage_edges_relation_check CHECK (relation IN (
    'CONTAINS', 'DERIVED_FROM', 'IMPORTED_FROM', 'VALIDATED_BY', 'CREATED_FROM_SNAPSHOT', 'TRANSFORMED_BY',
    'TRAINED_ON', 'PRODUCED_BY'));
