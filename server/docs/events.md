# Event pipeline: outbox, queue, inbox and dead letters (local runbook)

A committed business change that needs asynchronous work writes its event to the outbox **in the same PostgreSQL transaction**. The outbox worker publishes committed events to SQS, or to ElasticMQ locally. Consumers in the processing and cleanup workers take each event through an inbox, run its handler through the owning service, and acknowledge the message only after the outcome is recorded. Events that cannot be published or consumed end up in one table, `dead_letters`, where an operator retries, replays, resolves or discards them.

Delivery is **at least once**, not exactly once. An event can reach a consumer twice, for example after a publisher crash or a lost acknowledgement. Each repeat is harmless for two reasons:

- the inbox records every event a consumer has handled;
- every handler goes through the domain's conditional state changes: job and deletion leases, and compare-and-set status updates.

| Piece | Where | Role |
|---|---|---|
| `outbox_events` | migration `000010_events.sql` | events, written with their business change |
| `outbox-worker` | `cmd/outbox-worker` | publisher + event reconciliation |
| `processing-worker` | `cmd/processing-worker` | consumes the `processing` queue (and keeps its poll) |
| `cleanup-worker` | `cmd/cleanup-worker` | consumes the `lifecycle` queue (and keeps its cycles) |
| `event_inbox` | migration 000010 | one row per consumer and event: deduplication, leases, attempts |
| `dead_letters`, `event_discrepancies` | migration 000010 | dead letters; what reconciliation found and did |
| `event-admin` | `cmd/event-admin` | operator CLI: outbox, inbox, dead letters, discrepancies |
| SQS adapter | `internal/infrastructure/queue/sqs` | FIFO queues with a dead-letter queue each |
| memory queue | `internal/infrastructure/queue/memory` | the same behaviour in process, with failure injection, for tests only |

`EVENT_QUEUE=sqs` turns publishing and consuming on. Events are written to the outbox either way. Without a queue, the existing polls still do all the work. They stay running as a safety net when the queue is on.

## Asynchronous workflows

| Workflow | Source of truth | Trigger | Owner | Retry | Idempotency | Terminal failure |
|---|---|---|---|---|---|---|
| Upload completion → processing | `upload_sessions` COMPLETED, then `processing_jobs` | `upload.completed`, committed in the transaction that completes the upload. API completion and reconciliation recovery both go through it. The processing poll is the safety net | processing-worker: `processing` consumer → `Service.Intake` | delivery: the inbox budget. Work: the job's own budget | one asset and one job per upload (unique keys); the job lease; the inbox | the job ends EXHAUSTED with a HELD quarantine record (operator requeue); an undeliverable event goes to the dead letters |
| Verification, validation, malware scan, promotion | the `processing_jobs` stage | stages of one job, each saved under the job's lease before the next starts | processing-worker | job attempts with backoff and jitter (`PROCESSING_*`) | every stage is idempotent and resumes at its recorded stage | EXHAUSTED → HELD; REJECTED → quarantine record |
| Multipart recovery | `upload_sessions` + `reconciliation_tasks` | tasks derived from session state | reconciliation-worker | task attempts | compare-and-set transitions, provider inspection | EXHAUSTED task (visible) |
| Retention evaluation and deletion | `deletion_operations` | `asset.deletion_scheduled`, committed with the soft delete (owner, operator and retention expiry alike). The deletion schedule claims due operations too | cleanup-worker: `lifecycle` consumer → `RunDeletion`; deletion cycle | operation attempts (`LIFECYCLE_*`) | operation lease and conditional state changes; storage deletes are idempotent | FAILED; `lifecycle-admin requeue` |
| Legal hold release | `legal_holds` | `legal_hold.released`, committed with the release | cleanup-worker: `ResumeAfterHold` | the inbox budget | it only makes PROTECTED operations due; every blocker, other holds included, is checked again before anything is deleted | none needed |
| Quarantine purge, orphan scans and sweeps, missing-object checks, compaction | their own tables | time | processing-, cleanup- and compaction-worker | per pass | conditional updates, observation rules | recorded per run |
| Outbox publication | `outbox_events` | a committed row | outbox-worker | `OUTBOX_*` budget | lease; SQS deduplication by event ID and publication generation | DEAD + dead letter |
| Event consumption | `event_inbox` | a queue delivery | the consumers | `EVENT_*` budget | the inbox, keyed by (consumer, event) | DEAD + dead letter |

Events exist only where a worker needs them.

- **Between processing stages:** none. The stages run in one worker on one durable job, so an event per stage would only duplicate the job row.
- **After promotion:** none. Nothing downstream is asynchronous: delivery is synchronous, and CDN publication is out of scope.
- **Time-driven work** (purges, scans, compaction, multipart expiry): no business change to attach an event to. Their tables are the work items.
- **Bulk ingestion** (`docs/ingestion.md`): two events. `ingestion.manifest_staged` starts reading a staged manifest; `upload.processed`, written by processing in the transaction that ends a job when an ingestion entry uses the upload, records the entry's outcome. Claims, completions and sweeps change durable rows directly and need no event.
- **Managed datasets** (`docs/datasets.md`): two events on the `datasets` queue, consumed by `dataset-worker`. `dataset.import_queued` is committed when a manifest import is queued (inline at once, staged on submission); `dataset.validation_queued` is committed with a validation job. The handlers claim the job with a lease and continue from its checkpoint, so a repeated delivery finds nothing due; the worker's poll is the safety net.

## Envelope and schema versions

Every message is one JSON envelope (`internal/dto/event`):

| Field | Meaning |
|---|---|
| `id` | the stable event ID: deduplication key everywhere |
| `type`, `schema_version` | what the payload is, and which version of it |
| `occurred_at` | when the business change committed |
| `aggregate_type`, `aggregate_id`, `aggregate_sequence` | the record the event is about, and its number among that record's events |
| `tenant_id` | the owning tenant (required for every current type) |
| `correlation_id` | the request ID of the API call that caused it, carried on into handlers |
| `causation_id` | for an operator replay: the event it replays |
| `payload` | identifiers only: never file content, signed URLs or credentials |

| Type | Version | Aggregate | Queue | Payload | Handler |
|---|---|---|---|---|---|
| `upload.completed` | 1 | `upload` | `processing` | `upload_id` | `processing.Service.Intake` |
| `asset.deletion_scheduled` | 1 | `asset` | `lifecycle` | `operation_id` | `asset.Service.RunDeletion` |
| `legal_hold.released` | 1 | `legal_hold` | `lifecycle` | `hold_id` | `asset.Service.ResumeAfterHold` |
| `ingestion.manifest_staged` | 1 | `ingestion_job` | `ingestion` | `job_id` | `ingestion.Service.ReadStagedManifest` |
| `upload.processed` | 1 | `upload` | `ingestion` | `upload_id` (the outcome is read from the asset) | `ingestion.Service.OnUploadProcessed` |

Compatibility rules:

- A change old consumers can read keeps the version. A new optional field is such a change: unknown JSON fields are ignored.
- Removing, renaming or retyping a field, or changing its meaning, needs a new version. Consumers learn the new version first. Producers switch to it only after every consumer is deployed.
- A message that cannot be handled is dead-lettered and acknowledged, never treated as success:
  - Unreadable JSON, missing fields or an incomplete payload → `MALFORMED`. When even the event ID is unreadable, only a SHA-256 digest of the message is kept, never its content.
  - A type the consumer has no handler for → `UNKNOWN_TYPE`.
  - A version it cannot read → `UNSUPPORTED_VERSION`.
  - The second and third stay in the outbox, so they can be retried once a consumer that understands them is deployed.

## Outbox: transactions and publication

Events commit together with:

- the upload's move to COMPLETED (`UploadRepository.TransitionStatus`);
- the soft delete (`DeletionRepository.SoftDelete`);
- the hold release (`LegalHoldRepository.Release`).

If the event cannot be written, the business change rolls back too. If the business change is refused, no event is written.

Each event takes the next `aggregate_sequence` of its aggregate. Every writer first takes a transaction-scoped advisory lock on the aggregate, so two writers cannot both number an event `MAX+1`. This matters because some writers (operator replays, reconciliation) lock no business row; without it, a collision would fail the other call, which can be a user's request. The unique index `(aggregate_type, aggregate_id, aggregate_sequence)` remains as the backstop.

The publisher loops as follows:

1. **Claim** up to `OUTBOX_BATCH_SIZE` due PENDING events under a lease (`FOR UPDATE SKIP LOCKED`). An event is claimable only once no earlier event of its aggregate is still PENDING. An expired lease, meaning a publisher that stopped mid-way, is claimable again, and that abandoned attempt counts. No transaction is open while the queue is called.
2. **Send**, at most `OUTBOX_CONCURRENCY` at a time, each within `OUTBOX_PUBLISH_TIMEOUT`.
   - The group key is `aggregate_type:aggregate_id`, which is the FIFO message group.
   - The dedup key is `event_id-republished`. The publisher retries with the same generation; operator retries and reconciliation republish with a new one.
   - A send starts only while it can finish, and its result be recorded, before the batch's lease runs out: the claim time plus `OUTBOX_LEASE` minus `OUTBOX_PUBLISH_TIMEOUT` minus 10s. A slow queue can make a batch outlast its lease; the events it cannot reach in time are given back unsent instead of being sent while another publisher may already hold them. So `OUTBOX_LEASE` must be longer than `OUTBOX_PUBLISH_TIMEOUT` plus 10s.
   - An event whose budget was used up by attempts abandoned by stopped publishers is dead-lettered without another send.
3. **Record** the result:
   - Accepted → PUBLISHED.
   - A retryable failure → the attempt is counted and the event is retried after a backoff with jitter.
   - A permanent failure, or the last attempt → DEAD, plus a dead letter in the same transaction.
   - Throttling (`ErrQueueThrottled`) → that event and the rest of the batch are given back after a backoff, without spending an attempt. The queue's load is not the event's fault, and counting it would dead-letter events under sustained throttling.
   - Shutdown → unsent events are given back.

**The critical window:** if the queue accepted an event but the publisher stopped before recording it, the lease runs out and the event is sent again. Within five minutes SQS drops the repeat (same dedup key). After that, the consumer's inbox drops it.

## Consumers: inbox and idempotency

A batch can hold several events of one aggregate: SQS FIFO returns a group's consecutive messages in one receive. The consumer runs different groups side by side, at most `EVENT_CONCURRENCY` at a time, and each group's events one after another, in order. If one of them does not finish (it is retried, busy or interrupted), the rest of its group are handed back untried and counted as deferred, so a later event can never be handled before an earlier one. The queue holds the group while the unfinished event is hidden. A message that waited for its turn has its visibility renewed before it is handled.

For each message the consumer does the following:

1. **Decode** and validate the envelope. If it cannot be handled → dead letter (see above).
2. **Claim** the event in the inbox: insert `(consumer, event_id)`, or take it over when its retry is due or its last lease expired. Possible answers:
   - `ClaimDone` (already handled) or `ClaimDead` (already dead-lettered) → acknowledge the duplicate.
   - `ClaimBusy` (another worker holds it now, or its retry is not due) → hand the message back until then. A concurrent duplicate can never run the handler twice.
3. **Check order:** if this consumer already handled a later event of the same aggregate, the event is stale. Mark it SKIPPED and acknowledge.
4. **Run the handler** while a heartbeat renews both the inbox lease and the message's visibility, bounded by `EVENT_HANDLER_TIMEOUT`.
5. **Record the outcome first**, acknowledge afterwards:
   - success → COMPLETED;
   - `model.ErrStaleEvent` → SKIPPED;
   - a retryable error → FAILED, and the message comes back after the backoff;
   - a permanent error or the last attempt → DEAD plus a dead letter, then acknowledge.
   - On shutdown nothing is written: the lease and the visibility run out and the event comes back. This includes a claim or order check cut short by shutdown.

Inbox completion is not in the same transaction as the business change, which happens in the owning service. So a consumer that crashes between the two runs the handler again. That is safe because handlers rely on the services' own idempotency:

- `Intake` finds the asset and job that already exist, and only claims a job that is due and free;
- `RunDeletion` claims only a live, due operation;
- every deletion step checks its lease and state;
- `ResumeAfterHold` only moves `next_attempt_at`.

Completing or skipping an event also resolves any open dead letter for it, for example one escalated while it looked lost.

## Retries: one budget per layer

| Layer | Budget | Backoff | When it runs out |
|---|---|---|---|
| Publisher | `OUTBOX_MAX_ATTEMPTS` (10) | `OUTBOX_BACKOFF_BASE`..`MAX` (2s..5m), jitter into [d/2, d] | outbox DEAD + dead letter `PUBLISHER/EXHAUSTED` |
| Consumer | `EVENT_MAX_ATTEMPTS` (8), counted in the inbox | `EVENT_BACKOFF_BASE`..`MAX` (5s..15m), as the message's visibility delay, rounded up to whole seconds so it never comes back before its inbox retry is due | inbox DEAD + dead letter `CONSUMER/EXHAUSTED` |
| SQS redrive | `SQS_MAX_RECEIVE_COUNT` (20, must exceed `EVENT_MAX_ATTEMPTS`) | n/a | SQS moves the message to `<prefix><queue>-dlq.fifo`. Reconciliation drains it into `dead_letters` (`NATIVE_DLQ/REDRIVE`) |
| Business work | the job's or deletion's own attempts | unchanged | HELD quarantine / FAILED deletion, as before |

The redrive is only a backstop, for consumers that crash before recording anything. The consumer's own budget runs out first.

**Retryable:** network errors and timeouts, throttling, provider 5xx errors, an unavailable database, a dependency that is down, and deployment problems that someone fixes: a missing queue, and access, credential and encryption-key errors. Throttling spends no attempt on the publisher (see above); the others count against the budget. Failing an access error at once would turn one bad IAM change or key rotation into a dead letter for every event of the outage.

**Permanent:**

- undecodable envelopes, unknown types and unsupported versions;
- invalid requests to the queue (an invalid parameter or message content);
- oversize messages;
- a payload naming a record that does not exist;
- anything a handler marks `retry.Permanent`.

A stale event is neither: it is skipped. Every stored error goes through `retry.Sanitize`. That function keeps one line of at most 500 characters, and it cuts URL query strings and signature parameters.

## Dead letters

`dead_letters` is the only dead-letter record. SQS's own dead-letter queues are drained into it by reconciliation, so the two cannot disagree.

Each record holds:

- the event ID, type, version, tenant, aggregate and correlation ID;
- the source (`PUBLISHER`, `CONSUMER` or `NATIVE_DLQ`) and the consumer;
- the failure category;
- the sanitized last error;
- attempts and the first/last failure times;
- the dead-letter time;
- the resolution: status, note, actor and time.

The payload is not copied: it stays in the outbox, referenced by event ID, and `event-admin` never prints it.

| Operation | Command | What it does |
|---|---|---|
| List | `event-admin dead-letters -status OPEN` | newest first |
| Inspect | `event-admin inspect -id ID` | the dead letter, the event's metadata, the inbox record, and whether a resend would be refused |
| Retry | `event-admin retry -id ID -reason "..."` | same identity. Checks the business state first. Then, in one transaction: outbox → PENDING with a new generation, inbox DEAD → FAILED with a fresh budget, dead letter → RETRIED |
| Replay | `event-admin replay -id ID -reason "..."` | a new event ID with `causation_id` = the original, next in its aggregate's order; dead letter → REPLAYED with the new ID |
| Resolve / discard | `event-admin resolve`/`discard -id ID -reason "..."` | closes it with the operator's reason |

**Stale replays are refused.** Before a retry or replay, the business state is checked:

- `upload.completed` must still be needed: the upload COMPLETED or VERIFIED and its processing not ended;
- `asset.deletion_scheduled` needs its deletion still live, so not cancelled by a restore, finished or failed;
- `legal_hold.released` needs the hold released.

Otherwise the command fails with the reason, and the operator resolves or discards. Reconciliation also marks open dead letters whose state moved on (`source_changed`), and never resolves them by itself.

## Ordering

Order matters per aggregate only, never globally. It is protected four ways:

- **Outbox:** each aggregate's events are numbered, and an event is not published while an earlier one of its aggregate is still pending. A DEAD event does not block the ones after it.
- **SQS FIFO:** a message group is the aggregate, so its messages are delivered in order, and none while an earlier one is in flight. A message waiting for its retry holds its group. One receive can return several of a group's messages; the consumer runs them one at a time and hands back the rest when one does not finish.
- **Inbox:** an event older than one the consumer already handled for the same aggregate is skipped as stale. This catches anything that still arrives out of order, such as republications or operator retries.
- **Domain state:** handlers check the current state, so a delayed deletion for a restored asset finds its operation CANCELLED and does nothing.

## Reconciliation

The outbox worker runs it every `EVENT_RECONCILE_INTERVAL`, in bounded batches (`EVENT_RECONCILE_BATCH`), without holding a transaction across queue calls. `outbox-worker -once -dry-run` reports what it would do and writes nothing.

| Finding | Detection | Action |
|---|---|---|
| `native_dead_letter` | messages in a queue's SQS dead-letter queue | dead letter `NATIVE_DLQ/REDRIVE`, inbox → DEAD, then delete the message; already handled → just delete (its acknowledgement was lost) |
| `unconsumed` | published more than `EVENT_STUCK_AFTER` ago, within `EVENT_RECONCILE_WINDOW`, never received by its consumer, and with no dead letter other than a RETRIED one (an operator's resolve, discard or replay sticks) | republish (new generation) up to `EVENT_MAX_REPUBLISH` times, then dead letter `UNCONSUMED` |
| `inbox_stalled` | PROCESSING with an expired lease, or FAILED long past its retry, untouched for `EVENT_STUCK_AFTER` | inbox → FAILED and due; republish the event. Past `EVENT_MAX_REPUBLISH`: dead letter `UNCONSUMED` instead |
| `missing_event` | a COMPLETED/VERIFIED upload with neither an asset nor an `upload.completed` event; such uploads are looked at before those whose event is on its way | emit the event, once even when two reconcilers find it (checked under the aggregate lock) |
| `inconsistent` | `upload.completed` handled but the upload has no asset | report only |
| `dead_letter_source_changed` | an open dead letter whose business state moved on | mark it for an operator |

A republication acts only on the publication generation the reconciler read, so two reconcilers, or one working from an older read, send an event again at most once.

Expired publisher leases and events pending past `EVENT_STUCK_AFTER` need no repair here, because the next claim takes them. They are reported in the gauges and the log. Repairs and reports go to `event_discrepancies` (`event-admin discrepancies`).

## Database (migration `000010_events.sql`)

| Table | Keys and indexes |
|---|---|
| `outbox_events` | PK `id`; unique `(aggregate_type, aggregate_id, aggregate_sequence)`; `next_attempt_at` WHERE PENDING (claims); `(published_at, id)` WHERE PUBLISHED; `(created_at, id)`; `updated_at` WHERE DEAD. Checks: lease owner and expiry set together, PUBLISHED has `published_at`, payload ≤ 64 KiB |
| `event_inbox` | PK `(consumer, event_id)`; `(consumer, aggregate_type, aggregate_id, aggregate_sequence DESC)` WHERE COMPLETED/SKIPPED (stale check); `updated_at` WHERE PROCESSING/FAILED (stalled records); `(completed_at, event_id)` WHERE finished |
| `dead_letters` | PK `id`; unique `(event_id, consumer)` WHERE OPEN (one open dead letter per event and consumer; a repeat updates it); `dead_lettered_at` WHERE OPEN; `(resolved_at, id)` WHERE closed |
| `event_discrepancies` | PK `id`; `(created_at, id)` |

Compaction (`compaction-worker`) removes:

- published outbox events past the operational retention, except each aggregate's latest (its numbering continues from it) and any an open dead letter refers to;
- finished inbox records past the operational retention, which must stay above SQS's 14-day message retention;
- closed dead letters and discrepancies past the audit retention.

## Metrics

Exposed through expvar at `/debug/vars` with `METRICS_ADDR`, one name per worker: `events` for the outbox worker, `processing` and `lifecycle` for the consumers.

| Area | Names |
|---|---|
| Outbox (gauges, refreshed each reconciliation) | `outbox_pending`, `outbox_oldest_pending_seconds`, `outbox_expired_leases`, `outbox_dead` |
| Publication | `outbox_published`, `outbox_publish_ms_total` (÷ attempts = latency), `outbox_publish_failures`, `outbox_publish_retries`, `outbox_publish_throttled`, `outbox_publish_unrecorded`, `outbox_dead_lettered` |
| Queue backlog (gauges) | `queue_<queue>_waiting`, `_in_flight`, `_delayed`, `_dead_letters` (SQS exposes no message age through its API; that is a CloudWatch metric) |
| Consumption | `events_received`, `events_completed`, `events_handler_ms_total`, `events_retried`, `events_skipped`, `events_stale`, `events_duplicates`, `events_concurrent_duplicates` (idempotency conflicts), `events_dead_lettered`, `events_rejected`, `events_ack_failures` |
| Dead letters (gauges) | `dead_letters_open`, `dead_letters_oldest_seconds` |
| Reconciliation | `reconcile_<finding>` |
| Lifecycle | `deletions_expedited` |

Log lines carry `event_id`, `event_type`, `tenant_id`, `aggregate` and `correlation_id`, and never payloads, URLs or credentials.

## Running it locally

```powershell
docker compose up -d postgres minio clamav elasticmq    # ElasticMQ: SQS API on 127.0.0.1:9324, UI on 9325
.\scripts\migrate.ps1                                  # applies 000010_events.sql
.\scripts\migrate.ps1 -Database object_storage_test    # for the integration tests
```

```bash
go build -o bin/ ./cmd/...
# ElasticMQ accepts any credentials; the queues are created at startup.
export EVENT_QUEUE=sqs SQS_ENDPOINT=http://127.0.0.1:9324 SQS_ACCESS_KEY_ID=local SQS_SECRET_ACCESS_KEY=local SQS_CREATE_QUEUES=true

./bin/api.exe &
./bin/outbox-worker.exe &                         # publisher + reconciliation
SCANNER=clamav ./bin/processing-worker.exe &      # processing poll + processing consumer
./bin/cleanup-worker.exe &                        # lifecycle cycles + lifecycle consumer
```

| Step | Command |
|---|---|
| Trigger a business event | complete an upload through the API (`docs/multipart-reliability.md`). Or delete an asset with no grace period: `lifecycle-admin policy-set ... -grace 0s`, then `curl -X DELETE localhost:8080/api/v1/assets/$A` |
| Publish a test event | `./bin/event-admin.exe emit -type upload.completed -tenant local-tenant -aggregate-type upload -aggregate-id $UPLOAD -payload '{"upload_id":"'$UPLOAD'"}'` (`APP_ENV=development` only) |
| Inspect outbox and inbox | `./bin/event-admin.exe outbox -status PENDING`, `./bin/event-admin.exe inbox -consumer processing`, `./bin/event-admin.exe stats` |
| Transient failure | `docker pause object-storage-elasticmq`: publication retries with backoff (`outbox` shows the attempts and the last error); `docker unpause object-storage-elasticmq` |
| Permanent failure | `event-admin emit -type upload.completed ... -aggregate-id missing -payload '{"upload_id":"missing"}'` → dead letter `PERMANENT`; `emit -type smoke.unknown -queue processing ...` → `UNKNOWN_TYPE`; `-version 9` → `UNSUPPORTED_VERSION` |
| Inspect dead letters | `./bin/event-admin.exe dead-letters -status OPEN`, `./bin/event-admin.exe inspect -id $DL` |
| Replay safely | `./bin/event-admin.exe retry -id $DL -reason "scanner fixed"` (refused if stale), `replay -id $DL -reason "..."`, `discard -id $DL -reason "..."` |
| Publisher crash | stop `outbox-worker` with `kill -9`; events wait in PENDING (`event-admin stats`); start it again and they are published. A lease held at the crash expires after `OUTBOX_LEASE` |
| Consumer crash | stop `processing-worker` with `kill -9`; the message waits in SQS (or, if it was in flight, comes back after `EVENT_VISIBILITY`); start it again |
| Reconciliation | `./bin/outbox-worker.exe -once -dry-run` (report only), `./bin/outbox-worker.exe -once` (publish one batch and repair), `./bin/event-admin.exe discrepancies` |
| One batch of events | `./bin/processing-worker.exe -consume-once`, `./bin/cleanup-worker.exe -consume-once` |

Tests:

```bash
go test ./...                                               # unit tests; integration tests skip without the variables below
TEST_DATABASE_URL=... TEST_S3_BUCKET=object-storage-it TEST_S3_CREATE_BUCKET=true \
TEST_CLAMAV_ADDRESS=127.0.0.1:3310 TEST_SQS_ENDPOINT=http://127.0.0.1:9324 \
go test ./tests/integration/
```

Use disposable resources for failure experiments. The Phase 6 smoke run used database `object_storage_smoke`, bucket `object-storage-smoke` and queues under a prefix of their own.

## Limitations

- At least once, never exactly once. Handlers can run more than once, and correctness rests on the services' own idempotency, because the inbox's completion is a separate transaction from the business change.
- A DEAD event does not block later events of its aggregate. If an operator retries it later, the consumer's stale check skips it once a newer event of that aggregate has been handled.
- The `unconsumed` and `missing_event` checks look back `EVENT_RECONCILE_WINDOW` (72h). Anything older is left to the polls, which still do all the work.
- FIFO queues allow 300 messages per second each without batching. The group is the aggregate, so throughput across aggregates is unaffected.
- The outbox does not shrink to nothing. Compaction keeps each aggregate's latest event, because new events are numbered from it, and DEAD events. That is about one row per upload, asset and legal hold. With `EVENT_QUEUE` unset, events are still written and stay PENDING; turning the queue on later publishes that whole backlog, and the handlers absorb it because they are idempotent.
- An upload completed by reconciliation after its API request failed part-way gets an `upload.completed` event without the request's correlation ID, which the session does not store.
- The `inconsistent` finding (handled, but no asset) is reported again on every pass until an operator acts.
- A consumer shares its worker process. The processing worker can run `PROCESSING_CONCURRENCY` poll jobs plus `EVENT_CONCURRENCY` event handlers at once, and only job leases keep them off each other's work.
- ElasticMQ keeps everything in memory. After a restart:
  - with `SQS_CREATE_QUEUES=true`, the adapter recreates the queues on the next failed call;
  - lost messages are republished by the `unconsumed` check, since the outbox still has every event.
