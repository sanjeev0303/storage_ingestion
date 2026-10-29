-- Undoes 000027_training_runs.sql. Recorded training runs and models, and
-- their lineage, are deleted: export them first if they matter.
DELETE FROM lineage_edges WHERE relation IN ('TRAINED_ON', 'PRODUCED_BY');
DELETE FROM lineage_nodes WHERE kind IN ('TRAINING_RUN', 'MODEL');
DROP TABLE IF EXISTS models;
DROP TABLE IF EXISTS training_run_inputs;
DROP TABLE IF EXISTS training_runs;

ALTER TABLE lineage_nodes DROP CONSTRAINT IF EXISTS lineage_nodes_kind_check;
ALTER TABLE lineage_nodes ADD CONSTRAINT lineage_nodes_kind_check CHECK (kind IN (
    'OBJECT_VERSION', 'DATASET', 'DATASET_VERSION', 'MANIFEST_IMPORT', 'VALIDATION_REPORT', 'TRANSFORMATION'));
ALTER TABLE lineage_edges DROP CONSTRAINT IF EXISTS lineage_edges_relation_check;
ALTER TABLE lineage_edges ADD CONSTRAINT lineage_edges_relation_check CHECK (relation IN (
    'CONTAINS', 'DERIVED_FROM', 'IMPORTED_FROM', 'VALIDATED_BY', 'CREATED_FROM_SNAPSHOT', 'TRANSFORMED_BY'));
