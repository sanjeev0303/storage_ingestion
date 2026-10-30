DROP TRIGGER IF EXISTS webhook_copy_job ON copy_jobs;
DROP FUNCTION IF EXISTS webhook_copy_job();
DROP TRIGGER IF EXISTS webhook_scrub_result ON scrub_results;
DROP FUNCTION IF EXISTS webhook_scrub_result();

DELETE FROM security_alerts WHERE kind IN ('MASS_DELETION', 'SCRUB_MISMATCH');
ALTER TABLE security_alerts DROP CONSTRAINT IF EXISTS security_alerts_kind_check;
ALTER TABLE security_alerts ADD CONSTRAINT security_alerts_kind_check CHECK (kind IN ('READ_SPIKE', 'MASS_LINKS',
    'NEW_NETWORK', 'EGRESS_BUDGET', 'STORAGE_POSTURE', 'LIFECYCLE_CHANGED', 'OUT_OF_BAND_WRITE'));

DROP TABLE IF EXISTS tenant_export_schedule;
DROP TABLE IF EXISTS scrub_results;
DROP TABLE IF EXISTS copy_items;
DROP TABLE IF EXISTS copy_jobs;
DROP TRIGGER IF EXISTS tr_datasets_replica_tenant ON datasets;
ALTER TABLE datasets DROP COLUMN IF EXISTS clean_version_id, DROP COLUMN IF EXISTS scrub_days,
    DROP COLUMN IF EXISTS vault_retention_days, DROP COLUMN IF EXISTS escrow, DROP COLUMN IF EXISTS replica_connection_id;
DROP TABLE IF EXISTS replica_failures;
DROP TABLE IF EXISTS asset_replicas;

CREATE OR REPLACE FUNCTION storage_binding_tenant_check() RETURNS trigger AS $$
DECLARE
    column_name   TEXT;
    connection_id TEXT;
    previous_id   TEXT;
    owner         TEXT;
    state         TEXT;
    owning_tenant TEXT;
BEGIN
    IF TG_TABLE_NAME = 'organizations' THEN
        owning_tenant := NEW.id;
    ELSE
        owning_tenant := NEW.tenant_id;
    END IF;
    FOREACH column_name IN ARRAY TG_ARGV LOOP
        EXECUTE format('SELECT ($1).%I', column_name) USING NEW INTO connection_id;
        IF TG_OP = 'UPDATE' THEN
            EXECUTE format('SELECT ($1).%I', column_name) USING OLD INTO previous_id;
            IF previous_id IS NOT DISTINCT FROM connection_id THEN
                CONTINUE;
            END IF;
            IF TG_TABLE_NAME IN ('upload_sessions', 'assets') THEN
                RAISE EXCEPTION 'the storage binding of % % cannot change', TG_TABLE_NAME, NEW.id
                    USING ERRCODE = 'integrity_constraint_violation', CONSTRAINT = 'storage_binding_immutable';
            END IF;
        END IF;
        IF connection_id IS NULL OR connection_id = 'platform' THEN
            CONTINUE;
        END IF;
        SELECT tenant_id, status INTO owner, state FROM storage_connections WHERE id = connection_id;
        IF owner IS DISTINCT FROM owning_tenant THEN
            RAISE EXCEPTION 'storage connection % does not belong to tenant %', connection_id, owning_tenant
                USING ERRCODE = 'foreign_key_violation', CONSTRAINT = 'storage_connection_tenant';
        END IF;
        IF state = 'DELETED' OR (state <> 'ACTIVE' AND TG_TABLE_NAME <> 'assets') THEN
            RAISE EXCEPTION 'storage connection % is %', connection_id, state
                USING ERRCODE = 'foreign_key_violation', CONSTRAINT = 'storage_connection_active';
        END IF;
    END LOOP;
    RETURN NEW;
END $$ LANGUAGE plpgsql;
