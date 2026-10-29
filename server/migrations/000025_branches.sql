BEGIN;

-- Dataset branches and merges (docs/datasets.md, "Branches"). A branch is
-- a dataset of its own whose parent is the dataset it was branched from:
-- it starts with the members of one snapshot (rows only, no bytes) and
-- every dataset feature works on it. Merges between a branch and its
-- parent are three-way, from the last version merged between them.
ALTER TABLE datasets ADD COLUMN IF NOT EXISTS parent_dataset_id TEXT REFERENCES datasets (id);
ALTER TABLE datasets ADD COLUMN IF NOT EXISTS branch_name TEXT;
ALTER TABLE datasets ADD COLUMN IF NOT EXISTS fork_version_id TEXT;
-- The common ancestor of the next merge: the fork, then the last version
-- merged in either direction.
ALTER TABLE datasets ADD COLUMN IF NOT EXISTS merge_base_version_id TEXT;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'ck_datasets_branch') THEN
        ALTER TABLE datasets ADD CONSTRAINT ck_datasets_branch
            CHECK ((parent_dataset_id IS NULL) = (branch_name IS NULL)
               AND (parent_dataset_id IS NULL) = (fork_version_id IS NULL));
    END IF;
END $$;
CREATE UNIQUE INDEX IF NOT EXISTS ux_datasets_branch ON datasets (parent_dataset_id, branch_name)
    WHERE parent_dataset_id IS NOT NULL;

-- Every merge into a dataset's draft: what was merged, from where, and
-- what it changed. Also its idempotency record.
CREATE TABLE IF NOT EXISTS dataset_merges (
    id                TEXT PRIMARY KEY,
    tenant_id         TEXT NOT NULL,
    target_dataset_id TEXT NOT NULL REFERENCES datasets (id),
    source_dataset_id TEXT NOT NULL REFERENCES datasets (id),
    source_version_id TEXT NOT NULL,
    base_version_id   TEXT NOT NULL,
    strategy          TEXT NOT NULL CHECK (strategy IN ('FAIL', 'SOURCE', 'TARGET')),
    added             BIGINT NOT NULL,
    updated           BIGINT NOT NULL,
    removed           BIGINT NOT NULL,
    conflicts         BIGINT NOT NULL,
    idempotency_key   TEXT,
    created_by        TEXT NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS ix_dataset_merges_target ON dataset_merges (target_dataset_id, created_at DESC);
CREATE UNIQUE INDEX IF NOT EXISTS ux_dataset_merges_key ON dataset_merges (target_dataset_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;

COMMIT;
