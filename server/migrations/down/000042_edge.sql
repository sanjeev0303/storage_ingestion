DROP TRIGGER IF EXISTS webhook_edge_recording ON edge_recordings;
DROP FUNCTION IF EXISTS webhook_edge_recording();
DROP TABLE IF EXISTS edge_chunks;
DROP TABLE IF EXISTS edge_recordings;
DROP TABLE IF EXISTS edge_devices;
ALTER TABLE api_tokens DROP COLUMN IF EXISTS path_prefix;
