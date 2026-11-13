-- Envelope encryption for storage credentials (docs/storage-connections.md).
-- Each credential is sealed with its own random data key; the data key,
-- sealed by the master key named in *_key_version, is kept next to it. A
-- NULL wrapped key is a credential sealed by the master key directly, as
-- before this migration: it still opens, and is moved to an envelope when
-- it is next written or by `credential-admin reseal`.
ALTER TABLE storage_connections
    ADD COLUMN IF NOT EXISTS credential_wrapped_key BYTEA,
    ADD COLUMN IF NOT EXISTS staged_wrapped_key BYTEA,
    ADD COLUMN IF NOT EXISTS previous_wrapped_key BYTEA;
