BEGIN;

-- Wake the outbox publisher as soon as an event is committed, instead of on
-- its next poll. PostgreSQL delivers a NOTIFY only when the transaction
-- commits, and folds identical ones from one transaction into one, so a
-- statement-level trigger costs one notification per committing transaction
-- however many events it wrote. The payload is empty: the publisher only
-- needs to know there is something to claim. Polling stays as the fallback
-- (for retries that come due later, and while the listener reconnects).
CREATE OR REPLACE FUNCTION outbox_events_notify() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    PERFORM pg_notify('outbox_events', '');
    RETURN NULL;
END;
$$;

DROP TRIGGER IF EXISTS outbox_events_notify ON outbox_events;
CREATE TRIGGER outbox_events_notify
    AFTER INSERT ON outbox_events
    FOR EACH STATEMENT EXECUTE FUNCTION outbox_events_notify();

COMMIT;
