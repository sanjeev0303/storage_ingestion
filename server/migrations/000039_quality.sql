-- Data quality: column profiles per snapshot, drift against the parent,
-- leakage between training and evaluation sets, and release labels
-- (docs/quality.md).

-- Per dataset: whether snapshots are profiled, the drift thresholds, and
-- how a member names its subject (patient, user, scene) for leakage checks.
CREATE TABLE IF NOT EXISTS dataset_quality (
    dataset_id        TEXT PRIMARY KEY,
    tenant_id         TEXT NOT NULL,
    profile_snapshots BOOLEAN NOT NULL DEFAULT true,
    max_psi           DOUBLE PRECISION NOT NULL DEFAULT 0.2 CHECK (max_psi > 0 AND max_psi <= 10),
    max_null_change   DOUBLE PRECISION NOT NULL DEFAULT 0.1 CHECK (max_null_change > 0 AND max_null_change <= 1),
    max_row_change    DOUBLE PRECISION NOT NULL DEFAULT 0.5 CHECK (max_row_change > 0 AND max_row_change <= 100),
    subject_key       TEXT NOT NULL DEFAULT '' CHECK (char_length(subject_key) <= 128),
    subject_pattern   TEXT NOT NULL DEFAULT '' CHECK (char_length(subject_pattern) <= 500),
    updated_by        TEXT NOT NULL DEFAULT '',
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (dataset_id, tenant_id) REFERENCES datasets (id, tenant_id)
);

-- One column profile per version, made by the dataset worker.
CREATE TABLE IF NOT EXISTS version_profiles (
    version_id          TEXT PRIMARY KEY,
    tenant_id           TEXT NOT NULL,
    dataset_id          TEXT NOT NULL,
    state               TEXT NOT NULL CHECK (state IN ('PENDING', 'RUNNING', 'DONE', 'FAILED')),
    tables              JSONB NOT NULL DEFAULT '[]'::jsonb,
    skipped             JSONB NOT NULL DEFAULT '[]'::jsonb,
    files_profiled      INTEGER NOT NULL DEFAULT 0,
    bytes_read          BIGINT NOT NULL DEFAULT 0,
    other_files         BIGINT NOT NULL DEFAULT 0,
    baseline_version_id TEXT,
    drift               JSONB,
    drifted             BOOLEAN NOT NULL DEFAULT false,
    error               TEXT NOT NULL DEFAULT '',
    attempts            INTEGER NOT NULL DEFAULT 0,
    lease_owner         TEXT,
    lease_until         TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at         TIMESTAMPTZ,
    FOREIGN KEY (version_id, tenant_id) REFERENCES dataset_versions (id, tenant_id)
);
CREATE INDEX IF NOT EXISTS ix_version_profiles_todo ON version_profiles (created_at) WHERE state IN ('PENDING', 'RUNNING');

-- Every new version is queued for profiling unless its dataset turned it
-- off, whichever path made it (snapshot, merge, derivation).
CREATE OR REPLACE FUNCTION queue_version_profile() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF COALESCE((SELECT profile_snapshots FROM dataset_quality WHERE dataset_id = NEW.dataset_id), true) THEN
        INSERT INTO version_profiles (version_id, tenant_id, dataset_id, state)
        VALUES (NEW.id, NEW.tenant_id, NEW.dataset_id, 'PENDING') ON CONFLICT DO NOTHING;
    END IF;
    RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS queue_version_profile ON dataset_versions;
CREATE TRIGGER queue_version_profile AFTER INSERT ON dataset_versions
    FOR EACH ROW EXECUTE FUNCTION queue_version_profile();

-- Release labels: DR1, 2026.10, v3. One per version, unique in a dataset.
ALTER TABLE dataset_versions
    ADD COLUMN IF NOT EXISTS release_label TEXT CHECK (release_label ~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,39}$'),
    ADD COLUMN IF NOT EXISTS release_notes TEXT CHECK (char_length(release_notes) <= 4000),
    ADD COLUMN IF NOT EXISTS released_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS released_by TEXT;
CREATE UNIQUE INDEX IF NOT EXISTS ux_dataset_versions_release ON dataset_versions (dataset_id, release_label)
    WHERE release_label IS NOT NULL;

-- What a run used each version for.
ALTER TABLE training_run_inputs
    ADD COLUMN IF NOT EXISTS role TEXT CHECK (role IN ('TRAIN', 'VALIDATION', 'TEST'));

-- Overlap between a training version and an evaluation version.
CREATE TABLE IF NOT EXISTS leakage_checks (
    id               TEXT PRIMARY KEY,
    tenant_id        TEXT NOT NULL,
    run_id           TEXT,
    train_version_id TEXT NOT NULL,
    eval_version_id  TEXT NOT NULL,
    method           TEXT NOT NULL CHECK (method IN ('EXACT_HASH', 'SUBJECT', 'NGRAM')),
    state            TEXT NOT NULL CHECK (state IN ('CLEAN', 'LEAKED', 'NOT_CHECKED')),
    overlap          BIGINT NOT NULL DEFAULT 0,
    eval_members     BIGINT NOT NULL DEFAULT 0,
    examples         JSONB NOT NULL DEFAULT '[]'::jsonb,
    detail           TEXT NOT NULL DEFAULT '' CHECK (char_length(detail) <= 2000),
    parameters       JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_by       TEXT NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (train_version_id, tenant_id) REFERENCES dataset_versions (id, tenant_id),
    FOREIGN KEY (eval_version_id, tenant_id) REFERENCES dataset_versions (id, tenant_id)
);
CREATE INDEX IF NOT EXISTS ix_leakage_checks_run ON leakage_checks (tenant_id, run_id, created_at DESC);
CREATE INDEX IF NOT EXISTS ix_leakage_checks_versions ON leakage_checks (tenant_id, eval_version_id, train_version_id);

-- Webhooks: drift past a threshold, and leakage found.
CREATE OR REPLACE FUNCTION webhook_version_drift() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.state = 'DONE' AND NEW.drifted AND (OLD.state IS DISTINCT FROM 'DONE' OR NOT OLD.drifted) THEN
        PERFORM queue_webhook(NEW.tenant_id, 'dataset.drift_detected', jsonb_build_object(
            'dataset_id', NEW.dataset_id, 'version_id', NEW.version_id, 'baseline_version_id', NEW.baseline_version_id,
            'breaches', NEW.drift->'breaches'));
    END IF;
    RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS webhook_version_drift ON version_profiles;
CREATE TRIGGER webhook_version_drift AFTER UPDATE ON version_profiles
    FOR EACH ROW EXECUTE FUNCTION webhook_version_drift();

CREATE OR REPLACE FUNCTION webhook_leakage() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.state = 'LEAKED' THEN
        PERFORM queue_webhook(NEW.tenant_id, 'training_run.leakage_detected', jsonb_build_object(
            'check_id', NEW.id, 'run_id', NEW.run_id, 'train_version_id', NEW.train_version_id,
            'eval_version_id', NEW.eval_version_id, 'method', NEW.method, 'overlap', NEW.overlap,
            'eval_members', NEW.eval_members));
    END IF;
    RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS webhook_leakage ON leakage_checks;
CREATE TRIGGER webhook_leakage AFTER INSERT ON leakage_checks
    FOR EACH ROW EXECUTE FUNCTION webhook_leakage();
