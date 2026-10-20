BEGIN;

-- Transactional outbox. A business change that needs asynchronous work
-- writes its event here in the same transaction; the outbox worker
-- publishes committed rows to the queue. Payloads hold identifiers only:
-- never file content, URLs or credentials.
CREATE TABLE IF NOT EXISTS outbox_events (
    id                 TEXT PRIMARY KEY,
    event_type         TEXT NOT NULL,
    schema_version     INTEGER NOT NULL CHECK (schema_version > 0),
    aggregate_type     TEXT NOT NULL,
    aggregate_id       TEXT NOT NULL,
    aggregate_sequence BIGINT NOT NULL CHECK (aggregate_sequence > 0),
    tenant_id          TEXT,
    payload            JSONB NOT NULL CHECK (octet_length(payload::text) <= 65536),
    correlation_id     TEXT,
    causation_id       TEXT,
    destination        TEXT NOT NULL,
    status             TEXT NOT NULL CHECK (status IN ('PENDING', 'PUBLISHED', 'DEAD')),
    attempts           INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at    TIMESTAMPTZ NOT NULL,
    last_attempt_at    TIMESTAMPTZ,
    last_error         TEXT,
    lease_owner        TEXT,
    lease_expires_at   TIMESTAMPTZ,
    published_at       TIMESTAMPTZ,
    -- How many times reconciliation sent it again after publication.
    republished        INTEGER NOT NULL DEFAULT 0,
    created_at         TIMESTAMPTZ NOT NULL,
    updated_at         TIMESTAMPTZ NOT NULL,

    CHECK ((lease_owner IS NULL) = (lease_expires_at IS NULL)),
    CHECK (status <> 'PUBLISHED' OR published_at IS NOT NULL)
);

-- Events of one aggregate are numbered; publication follows the numbers.
CREATE UNIQUE INDEX IF NOT EXISTS ux_outbox_aggregate_sequence
ON outbox_events (aggregate_type, aggregate_id, aggregate_sequence);

CREATE INDEX IF NOT EXISTS ix_outbox_due
ON outbox_events (next_attempt_at)
WHERE status = 'PENDING';

CREATE INDEX IF NOT EXISTS ix_outbox_published
ON outbox_events (published_at, id)
WHERE status = 'PUBLISHED';

CREATE INDEX IF NOT EXISTS ix_outbox_created
ON outbox_events (created_at, id);

CREATE INDEX IF NOT EXISTS ix_outbox_dead
ON outbox_events (updated_at)
WHERE status = 'DEAD';

-- Consumer inbox: one row per (consumer, event). It deduplicates
-- deliveries and leases processing so a duplicate delivered while the
-- first is still running waits instead of running twice.
CREATE TABLE IF NOT EXISTS event_inbox (
    consumer           TEXT NOT NULL,
    event_id           TEXT NOT NULL,
    event_type         TEXT NOT NULL,
    schema_version     INTEGER NOT NULL,
    tenant_id          TEXT,
    aggregate_type     TEXT NOT NULL,
    aggregate_id       TEXT NOT NULL,
    aggregate_sequence BIGINT NOT NULL,
    correlation_id     TEXT,
    status             TEXT NOT NULL CHECK (status IN ('PROCESSING', 'COMPLETED', 'SKIPPED', 'FAILED', 'DEAD')),
    outcome            TEXT,
    attempts           INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    lease_owner        TEXT,
    lease_expires_at   TIMESTAMPTZ,
    next_attempt_at    TIMESTAMPTZ,
    last_error         TEXT,
    first_seen_at      TIMESTAMPTZ NOT NULL,
    last_attempt_at    TIMESTAMPTZ,
    completed_at       TIMESTAMPTZ,
    updated_at         TIMESTAMPTZ NOT NULL,

    PRIMARY KEY (consumer, event_id),
    CHECK ((lease_owner IS NULL) = (lease_expires_at IS NULL))
);

-- Stale-event detection: the newest event a consumer finished per aggregate.
CREATE INDEX IF NOT EXISTS ix_inbox_aggregate
ON event_inbox (consumer, aggregate_type, aggregate_id, aggregate_sequence DESC)
WHERE status IN ('COMPLETED', 'SKIPPED');

-- Reconciliation: unfinished records nobody has touched for a while.
CREATE INDEX IF NOT EXISTS ix_inbox_unfinished
ON event_inbox (updated_at)
WHERE status IN ('PROCESSING', 'FAILED');

CREATE INDEX IF NOT EXISTS ix_inbox_finished
ON event_inbox (completed_at, event_id)
WHERE status IN ('COMPLETED', 'SKIPPED');

-- Dead letters: the single operational record of events that could not be
-- published or consumed, whatever stopped them (the publisher, a consumer,
-- or the queue's own redrive to its dead-letter queue, which is drained
-- into this table).
CREATE TABLE IF NOT EXISTS dead_letters (
    id               TEXT PRIMARY KEY,
    event_id         TEXT NOT NULL,
    source           TEXT NOT NULL CHECK (source IN ('PUBLISHER', 'CONSUMER', 'NATIVE_DLQ')),
    consumer         TEXT NOT NULL DEFAULT '',
    event_type       TEXT,
    schema_version   INTEGER,
    tenant_id        TEXT,
    aggregate_type   TEXT,
    aggregate_id     TEXT,
    correlation_id   TEXT,
    failure_category TEXT NOT NULL CHECK (
                         failure_category IN ('PERMANENT', 'EXHAUSTED', 'MALFORMED', 'UNKNOWN_TYPE',
                                              'UNSUPPORTED_VERSION', 'REDRIVE', 'UNCONSUMED')
                     ),
    last_error       TEXT NOT NULL,
    attempts         INTEGER NOT NULL,
    event_created_at TIMESTAMPTZ,
    first_failed_at  TIMESTAMPTZ NOT NULL,
    last_failed_at   TIMESTAMPTZ NOT NULL,
    dead_lettered_at TIMESTAMPTZ NOT NULL,
    -- For a message that could not be read: its digest, never its content.
    message_digest   TEXT,
    status           TEXT NOT NULL CHECK (status IN ('OPEN', 'RETRIED', 'REPLAYED', 'RESOLVED', 'DISCARDED')),
    resolution_note  TEXT,
    resolved_by      TEXT,
    resolved_at      TIMESTAMPTZ,
    replay_event_id  TEXT,
    -- Set by reconciliation when the business state moved on since.
    source_changed   TEXT,
    updated_at       TIMESTAMPTZ NOT NULL
);

-- One open dead letter per event and consumer.
CREATE UNIQUE INDEX IF NOT EXISTS ux_dead_letters_open
ON dead_letters (event_id, consumer)
WHERE status = 'OPEN';

CREATE INDEX IF NOT EXISTS ix_dead_letters_open
ON dead_letters (dead_lettered_at)
WHERE status = 'OPEN';

CREATE INDEX IF NOT EXISTS ix_dead_letters_closed
ON dead_letters (resolved_at, id)
WHERE status <> 'OPEN';

-- What event reconciliation found and did.
CREATE TABLE IF NOT EXISTS event_discrepancies (
    id             TEXT PRIMARY KEY,
    kind           TEXT NOT NULL,
    event_id       TEXT,
    consumer       TEXT,
    tenant_id      TEXT,
    aggregate_type TEXT,
    aggregate_id   TEXT,
    detail         TEXT NOT NULL,
    action         TEXT NOT NULL,
    outcome        TEXT NOT NULL CHECK (outcome IN ('REPAIRED', 'REPORTED', 'FAILED')),
    created_at     TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS ix_event_discrepancies_created
ON event_discrepancies (created_at, id);

COMMIT;
