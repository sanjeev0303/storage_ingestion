-- Each worker process writes one row while it runs and refreshes it every
-- 30 seconds, so the API can say which kinds of worker are alive instead of
-- leaving that to logs. Rows of processes that stopped are pruned by the
-- workers after a week.
CREATE TABLE IF NOT EXISTS worker_heartbeats (
    kind         TEXT NOT NULL,
    instance     TEXT NOT NULL,
    started_at   TIMESTAMPTZ NOT NULL,
    last_seen_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (kind, instance)
);
