-- Content checks: what the quarantine gate's checks found in an upload's
-- bytes beyond its type (active content, unsafe model files, secrets,
-- broken formats, an external engine's verdict, malware found by a later
-- re-scan). A finding either flags the asset, which is still published, or
-- rejects it. Each tenant may make a finding stricter or looser than its
-- default action, within what the platform allows.

CREATE TABLE IF NOT EXISTS content_findings (
    id         TEXT PRIMARY KEY,
    tenant_id  TEXT NOT NULL,
    asset_id   TEXT NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    job_id     TEXT NOT NULL DEFAULT '',
    -- The check that found it: a validator's name, or "rescan".
    source     TEXT NOT NULL,
    code       TEXT NOT NULL,
    action     TEXT NOT NULL CHECK (action IN ('flag', 'reject')),
    detail     TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS ix_content_findings_asset ON content_findings (tenant_id, asset_id);
CREATE INDEX IF NOT EXISTS ix_content_findings_code ON content_findings (tenant_id, code, created_at DESC);

-- Per-code overrides: {"secret.aws_access_key": "reject", ...}.
ALTER TABLE tenant_content_policies ADD COLUMN IF NOT EXISTS finding_actions JSONB NOT NULL DEFAULT '{}';

-- When an asset's clean verdict was last confirmed by a re-scan with newer
-- signatures.
ALTER TABLE processing_jobs ADD COLUMN IF NOT EXISTS rescanned_at TIMESTAMPTZ;
-- Whether the content checks ran on the upload: FULL, PARTIAL (a check
-- read only part of it, or skipped it for its size) or NULL (not run).
ALTER TABLE processing_jobs ADD COLUMN IF NOT EXISTS content_checks TEXT CHECK (content_checks IN ('FULL', 'PARTIAL'));

-- A published asset that a later check found unsafe (malware on a re-scan)
-- is blocked: it keeps its lifecycle state, so its catalog version and
-- usage stay as they were, but no download is served until it is deleted.
ALTER TABLE assets ADD COLUMN IF NOT EXISTS blocked_reason TEXT;
ALTER TABLE assets ADD COLUMN IF NOT EXISTS blocked_at TIMESTAMPTZ;

-- Re-scans pick published assets whose clean verdict is from older
-- signatures.
CREATE INDEX IF NOT EXISTS ix_processing_jobs_clean_version
    ON processing_jobs (scanner, scanner_version) WHERE scan_result = 'CLEAN';

-- A new finding is a webhook event, queued in the transaction that wrote it.
CREATE OR REPLACE FUNCTION webhook_content_finding() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    PERFORM queue_webhook(NEW.tenant_id, 'object.flagged', jsonb_build_object(
        'asset_id', NEW.asset_id, 'source', NEW.source, 'code', NEW.code, 'action', NEW.action));
    RETURN NULL;
END;
$$;

DROP TRIGGER IF EXISTS webhook_content_finding ON content_findings;
CREATE TRIGGER webhook_content_finding
    AFTER INSERT ON content_findings
    FOR EACH ROW
    EXECUTE FUNCTION webhook_content_finding();
