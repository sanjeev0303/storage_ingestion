DROP TABLE IF EXISTS publications;
DROP TRIGGER IF EXISTS webhook_upstream_source ON upstream_sources;
DROP FUNCTION IF EXISTS webhook_upstream_source();
DROP TABLE IF EXISTS upstream_sources;
DROP TABLE IF EXISTS embeddings;
DROP TABLE IF EXISTS embedding_indexes;
DROP TABLE IF EXISTS segments;
DROP TABLE IF EXISTS recording_metadata;
DELETE FROM lineage_edges WHERE from_node IN (SELECT id FROM lineage_nodes WHERE kind = 'UPSTREAM_SOURCE') OR to_node IN (SELECT id FROM lineage_nodes WHERE kind = 'UPSTREAM_SOURCE');
DELETE FROM lineage_nodes WHERE kind = 'UPSTREAM_SOURCE';
ALTER TABLE lineage_nodes DROP CONSTRAINT IF EXISTS lineage_nodes_kind_check;
ALTER TABLE lineage_nodes ADD CONSTRAINT lineage_nodes_kind_check CHECK (kind IN (
    'OBJECT_VERSION', 'DATASET', 'DATASET_VERSION', 'MANIFEST_IMPORT', 'VALIDATION_REPORT', 'TRANSFORMATION',
    'TRAINING_RUN', 'MODEL'));
