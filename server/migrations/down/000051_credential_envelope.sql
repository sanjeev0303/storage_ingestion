-- Dropping the wrapped keys would make every enveloped credential
-- unreadable, so this refuses while any exists. Re-enter (rotate) those
-- credentials under the older release instead, or restore a backup.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM storage_connections
               WHERE credential_wrapped_key IS NOT NULL OR staged_wrapped_key IS NOT NULL OR previous_wrapped_key IS NOT NULL) THEN
        RAISE EXCEPTION 'storage credentials are sealed in envelopes; rolling back 000051 would lose them';
    END IF;
END $$;
ALTER TABLE storage_connections
    DROP COLUMN IF EXISTS previous_wrapped_key,
    DROP COLUMN IF EXISTS staged_wrapped_key,
    DROP COLUMN IF EXISTS credential_wrapped_key;
