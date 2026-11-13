# Architecture, failure modes and recovery (research notes for an AWS + Kafka design)

Read-only analysis of the code on disk, 2026-10-30. Paths are relative to `server/` unless they start with `client/` or `sdk/`. `file:line` refers to the tree as it is now. "not found" means I searched and found nothing. Where docs and code disagree, the code wins and the doc is marked stale.

## 0. Key facts

- **No queue is required for correctness.** Events are always written to the outbox. With `EVENT_QUEUE` unset, "the existing polls still do all the work" (docs/events.md:22). One exception: `catalog.version_replaced` is consumed only with `EVENT_QUEUE=sqs` (docs/release-candidate.md:110). Without that queue, old catalog versions are never retired by event, and I found no poll fallback for them.
- **PostgreSQL is the only coordinator.** It holds the outbox, inbox, leases, dead letters, webhook queue and job tables, and it wakes workers through LISTEN/NOTIFY. Every binary needs `DATABASE_URL` (internal/platform/config/config.go:31,43-45).
- **The queue carries IDs only.** Messages never carry credentials or file content (internal/dto/event/envelope.go:46-47, docs/storage-connections.md:44). The outbox payload is capped at 64 KiB (docs/events.md:220).
- **Delivery is at least once by design** (internal/interfaces/job_queue.go:61-64, docs/events.md:5-8). The inbox plus compare-and-set domain updates make repeats harmless.
- **Terraform is empty.** Every module folder (cloudfront, iam, kms, monitoring, rds, s3-inventory, s3-lifecycle, s3-quarantine, s3-replication, s3-serving, sqs) and every environment folder (dev, staging, prod) holds only `.gitkeep` (infrastructure/terraform/**).
- **Build and runtime.** The Dockerfile builds one static binary per `CMD` build arg into distroless (Dockerfile:1-8). Locally the stack runs as plain processes or containers. No Kubernetes or ECS manifests in the repo: not found.

## 1. Components (server/cmd)

Every process does the following:
- loads `config.Load()`, which needs `DATABASE_URL`;
- opens a pgx pool hard-coded to MaxConns 20 / MinConns 2 (internal/infrastructure/database/postgres/db.go:29-32);
- exits if the database ping fails at startup (db.go:40-43), so it relies on the orchestrator to restart it;
- traps SIGINT/SIGTERM with `signal.NotifyContext`;
- names its lease owner `host-pid-rand` (cmd/outbox-worker/main.go:161-171; the same `workerID()` is in every worker).

Health endpoints:
- **API:** `GET /health` (liveness) and `GET /ready` (database ping) (internal/server/router.go:99-101, cmd/api/main.go:306).
- **Workers:** `/health`, `/ready` and `/debug/vars` only when `METRICS_ADDR` is set (internal/platform/metrics/metrics.go:71-85). The dataset worker uses `DATASET_METRICS_ADDR` (internal/platform/config/datasets.go:134).
- **webhook-worker:** no metrics or health listener at all (cmd/webhook-worker/main.go has no `metrics.Serve`).

All binaries are stateless. All state is in Postgres and object storage, so any binary can run as N replicas, coordinated by leases and SKIP LOCKED.

| Binary | Purpose | Consumes | Produces | Scaling knobs (defaults) |
|---|---|---|---|---|
| `api` | HTTP API on `HTTP_PORT` 8080 (config.go:30). Uploads (presign/multipart), delivery, catalog, datasets, connections, webhooks admin, identity, dead-letter admin (cmd/api/main.go:79-381) | HTTP only. Uses no queue client (no `sqsqueue` in cmd/api/main.go) | outbox rows in the same transaction as the business change: `upload.completed` (internal/service/upload/completion.go:87), `asset.deletion_scheduled` (internal/service/asset/service.go:330), `legal_hold.released` (:482), `ingestion.manifest_staged` (internal/service/ingestion/service.go:550), `dataset.import_queued` (internal/service/dataset/imports.go:162), `dataset.validation_queued` (internal/service/dataset/validation.go:122). Also presigned S3 URLs and webhook rows via DB triggers (migrations/000021_webhooks.sql:60-65) | `RATE_LIMIT_*` (internal/platform/config/security.go:81-99,166-168); shutdown timeout 10 s (config.go:32); `ReadHeaderTimeout` 5 s, `IdleTimeout` 60 s (cmd/api/main.go:323-328) |
| `outbox-worker` | Publishes PENDING outbox rows to the queue and runs event reconciliation (cmd/outbox-worker/main.go:1-2) | DB poll every `OUTBOX_POLL_INTERVAL` 1 s, woken by NOTIFY `outbox_events` (main.go:140-148; migrations/000018_outbox_notify.sql:13). Drains the native DLQs (internal/service/events/reconciler.go:133,186) | SQS messages to five queues (main.go:69). Refuses to start without `EVENT_QUEUE=sqs` (main.go:50-52) | `OUTBOX_BATCH_SIZE` 50, `OUTBOX_CONCURRENCY` 4, `OUTBOX_LEASE` 1m, `OUTBOX_PUBLISH_TIMEOUT` 10s, `OUTBOX_MAX_ATTEMPTS` 10, backoff 2s..5m, `EVENT_RECONCILE_INTERVAL` 1m (internal/platform/config/events.go:89-123) |
| `processing-worker` | VERIFY → SCAN (ClamAV) → PROMOTE → CLEANUP per upload. Also runs adoption jobs (cmd/processing-worker/main.go:344-373) | poll `PROCESSING_POLL_INTERVAL` 10s (config.go:241); `processing` queue when `EVENT_QUEUE=sqs` (main.go:288-295); clamd TCP | `processing_jobs` and asset state, promoted S3 objects, `upload.processed` (internal/infrastructure/database/postgres/processing_job_repository.go:390), `catalog.version_replaced` (internal/infrastructure/database/postgres/catalog_repository.go:172) | `PROCESSING_CONCURRENCY` 2, `PROCESSING_BATCH_SIZE` 10, lease 1m renewed every lease/3, `PROCESSING_MAX_JOB_DURATION` 30m, `PROCESSING_MAX_ATTEMPTS` 8, `PROCESSING_SCAN_PARALLELISM` 4 (config.go:241-306), plus `EVENT_CONCURRENCY` handlers in the same process (docs/events.md:300) |
| `upload-worker` | Bulk ingestion: staged manifests, slot-lease expiry, entry retries, outcome sync, cancellations, counter reconcile (cmd/upload-worker/main.go:1-5) | polls `INGESTION_POLL_INTERVAL` 5s and `INGESTION_RECONCILE_INTERVAL` 10m (internal/platform/config/ingestion.go:132-136); `ingestion` queue (main.go:184-199,231-247) | ingestion entry rows, upload sessions | `INGESTION_*` slots: global 256, tenant 64, job 32 (ingestion.go:105-107) |
| `dataset-worker` | Manifest imports, validations, readiness, recovery of stopped jobs (cmd/dataset-worker/main.go:1-5) | poll `DATASET_POLL_INTERVAL` 5s (config/datasets.go:127); `datasets` queue (main.go:140-169) | dataset rows | `DATASET_WORKER_SLOTS` 2, `DATASET_LEASE` 3m, `DATASET_MAX_JOB_DURATION` 6h (datasets.go:92,122-123); consumer concurrency = min(`EVENT_CONCURRENCY`, slots) (main.go:183) |
| `pack-worker` | Packs small assets into pack objects: plan, write, verify, publish, retire (cmd/pack-worker/main.go:1-5; docs/packing.md:93-141) | poll `PACKING_POLL_INTERVAL` 30s (config/packing.go); `packing` queue (main.go:133-169) | `pack.planned` (internal/service/packing/service.go:290), pack objects under `packs/`, pack rows | `PACKING_ACTIVE_WRITERS` 2, `PACKING_SOURCE_CONCURRENCY` 16, `PACKING_LEASE` 2m, `PACKING_MAX_ATTEMPTS` 5 (config/packing.go). Memory bound W×((C+1)×M+min(pack,P)) ≈ 66 MiB (docs/packing.md:110-114) |
| `cleanup-worker` | Retention, deletions, orphan scan/sweep, missing-object check (cmd/cleanup-worker/main.go:49); read-anomaly detection every 5m (:288-305); storage posture and drift checks every 15m (:306-327) | `LIFECYCLE_POLL_INTERVAL` 1m (config/lifecycle.go:86); `lifecycle` queue (consumer only with no `-dry-run`/`-tenant` and the deletions phase on; :252-261) | S3 deletes (version-aware), tombstones, webhook `object.erased` via trigger | `LIFECYCLE_CONCURRENCY` 2, `LIFECYCLE_MAX_DELETIONS` 100, `LIFECYCLE_LEASE_DURATION` 5m (lifecycle.go:86-141) |
| `reconciliation-worker` | Multipart session recovery and orphan provider sessions/objects (docs/multipart-reliability.md:21-60) | poll `RECONCILIATION_POLL_INTERVAL` 15s (config.go:115); provider listing scans per connection (cmd/reconciliation-worker/main.go:149-151) | completes, aborts and expires sessions, emitting `upload.completed` on recovery (internal/service/upload/multipart_recovery.go:303); `reconciliation_findings` | `RECONCILIATION_BATCH_SIZE` 10, lease 10m, `RECONCILIATION_MAX_ATTEMPTS` 8, `RECONCILIATION_SESSION_SCAN` provider/database/off (config.go:106-143) |
| `compaction-worker` | Metadata pruning only, in bounded SKIP LOCKED batches (docs/hardening-report.md:199; internal/infrastructure/database/postgres/compaction_repository.go:39-67) | `COMPACTION_POLL_INTERVAL` 1h | deletes old rows. **Dry run unless `COMPACTION_ENABLED=true`** (cmd/compaction-worker/main.go:69-75; config/lifecycle.go:182) | `COMPACTION_BATCH_SIZE` 1000, `COMPACTION_MAX_BATCHES` 100 (lifecycle.go:204-207) |
| `webhook-worker` | Signs and POSTs webhook deliveries (docs/webhooks.md:3) | `webhook_deliveries` table, claimed with SKIP LOCKED (internal/infrastructure/database/postgres/webhook_repository.go:172); NOTIFY `webhook_deliveries` (cmd/webhook-worker/main.go:88); poll `WEBHOOK_POLL_INTERVAL` 5s (:54) | outbound HTTPS through netguard (:74) | `WEBHOOK_CONCURRENCY` 16, max 256 (:50-52). Without the credential keyring it will not start (:47-48) |
| CLIs | `migrate` (advisory lock 7240512; internal/platform/migrate/migrate.go:21,62), `event-admin` (dead letters: retry, replay, resolve; docs/events.md:174-180), `lifecycle-admin`, `catalog-admin`, `identity-admin`; load tools `ingestion-loadgen`, `catalog-bench` | | | run as one-off tasks |

Console (`client/`, Next.js, port 3100):
- It is a BFF proxy: `${BACKEND_URL}/api/v1/...` (client/src/app/api/proxy/[...path]/route.ts:130).
- Env vars: `BACKEND_URL`, `CONSOLE_ORIGIN`, `COOKIE_SECURE`, `TRUSTED_PROXY_HOPS`.
- Request bodies are capped at 17 MiB (docs/phase15-report.md:232).

SDK: Python only (`sdk/python/osp`). It retries 429 and gateway errors with `Retry-After` (sdk/python/osp/client.py:107,151,237).

### Env vars by binary (names only)

- **Common to all:** `DATABASE_URL` (required), `APP_ENV`, `UPLOAD_MAX_SIZE_BYTES`, `UPLOAD_SESSION_TTL_SECONDS`, `UPLOAD_URL_TTL_SECONDS` (config.go:29-61).
- **Platform S3** (api, processing, upload, dataset, pack, cleanup, reconciliation):
  - `S3_REGION` (required), `S3_ENDPOINT` (https unless localhost), `S3_ACCESS_KEY_ID`, `S3_SECRET_ACCESS_KEY`, `S3_SESSION_TOKEN`, `S3_FORCE_PATH_STYLE` (internal/infrastructure/storage/s3/client.go:31-62).
  - `S3_BUCKET` (required; e.g. cmd/api/main.go:118-121, cmd/processing-worker/main.go:93-96).
  - `PROMOTION_BUCKET`, `PROMOTION_PREFIX` (default `assets`) (config.go:225,429).
  - With no keys set, the AWS default chain is used, so IAM task roles work (s3/client.go:67-68).
- **Queue** (outbox, processing, upload, dataset, pack, cleanup):
  - `EVENT_QUEUE` (=`sqs`).
  - `SQS_REGION` (falls back to `S3_REGION`), `SQS_ENDPOINT`, `SQS_ACCESS_KEY_ID`, `SQS_SECRET_ACCESS_KEY`, `SQS_QUEUE_PREFIX` (default `storage-`), `SQS_CREATE_QUEUES`, `SQS_MAX_RECEIVE_COUNT` 20 (config/events.go:61-115).
  - `EVENT_VISIBILITY` 1m, `EVENT_WAIT` 10s (≤20s), `EVENT_HANDLER_TIMEOUT` 15m, `EVENT_BATCH_SIZE` 10, `EVENT_CONCURRENCY` 4, `EVENT_MAX_ATTEMPTS` 8, `EVENT_BACKOFF_*` 5s..15m, `EVENT_STUCK_AFTER` 15m, `EVENT_RECONCILE_WINDOW` 72h, `EVENT_MAX_REPUBLISH` 3 (events.go:94-123).
- **Scanner** (processing): `SCANNER` (none|clamav, default none), `CLAMAV_ADDRESS` (127.0.0.1:3310), `CLAMAV_TIMEOUT` 2m, `SCAN_UNAVAILABLE_POLICY` (hold|reject|allow_unscanned) (config.go:222-224,246,327-341). Also `CONTENT_HOOK_URL`/`TOKEN` and `ARCHIVE_*`.
- **BYOS / keys** (every process that touches tenant storage, and webhook):
  - `BYOS_ENABLED`, `STORAGE_CREDENTIAL_KEYS` (`v:base64(32B)`), `STORAGE_CREDENTIAL_KEY_VERSION`, `STORAGE_ALLOW_PRIVATE_ENDPOINTS` (dev only).
  - `STORAGE_CLIENT_CACHE_*`, `STORAGE_CONNECTION_CACHE_TTL` 10s, `STORAGE_OPERATION_TIMEOUT` 2m, `STORAGE_MAX_CONCURRENT_PER_CONNECTION` 32, `STORAGE_TRANSFER_PART_BYTES` 16 MiB, `STORAGE_POSTURE_INTERVAL`, `STORAGE_DRIFT_*` (config/storage.go:60-130).
- **CDN / delivery** (api): `DELIVERY_CDN_ENABLED`, `DELIVERY_CDN_BASE_URL`, `DELIVERY_CDN_KEY_PAIR_ID`, `DELIVERY_CDN_PRIVATE_KEY_FILE` (the key is read from a file), `DELIVERY_URL_MODE` (storage|token), `DELIVERY_URL_TTL` 5m / `MAX_TTL` 15m, `DELIVERY_PROXY_ENABLED`, `DELIVERY_MAX_STREAMS` 64 (config.go:374-402).
- **Security** (api): `AUTH_*` (`AUTH_TRUST_PROXY_HEADERS`, `AUTH_ARGON2_*`, token TTLs, SSO/SAML), `RATE_LIMIT_<OP>`, `QUOTA_*`, `CERTIFICATE_SIGNING_KEY` (lifecycle.go:120).
- **KMS:** no platform KMS env var: not found. See §2.

## 2. Data stores

**Postgres** (34 SQL migrations, migrations/000001…000034; docs say PG 17).

Claims and leases:
- `FOR UPDATE SKIP LOCKED` claims with `lease_owner`/`lease_expires_at` for:
  - outbox (internal/infrastructure/database/postgres/outbox_repository.go:132-168);
  - processing jobs (processing_job_repository.go:185);
  - reconciliation tasks (reconciliation_repository.go:190);
  - deletions (deletion_repository.go:244);
  - packs (pack_repository.go:288);
  - webhooks (webhook_repository.go:172);
  - compaction (compaction_repository.go:39-67).
- An expired lease is reclaimable, and the abandoned attempt counts (outbox_repository.go:160-162).

Advisory locks:
- `pg_advisory_xact_lock` per aggregate when numbering outbox events (outbox_repository.go:78);
- lineage (lineage_repository.go:88);
- validation profiles (validation_repository.go:52);
- `pg_advisory_lock` session lock for migrations (migrate.go:62).

Event tables: `outbox_events`, `event_inbox` (PK consumer+event_id), `dead_letters`, `event_discrepancies` (docs/events.md:216-223).

LISTEN/NOTIFY:
- Triggers send `pg_notify('outbox_events')` (migrations/000018_outbox_notify.sql:13) and `pg_notify('webhook_deliveries')` (000021_webhooks.sql:65).
- Each listener uses one dedicated connection outside the pool and reconnects with backoff from 1 s to 30 s. Polling continues meanwhile (internal/infrastructure/database/postgres/notify.go:11-37).

Hot rows: per-tenant counters `tenant_usage` and `ingestion_tenant_usage` serialize ingestion (docs/hardening-report.md:171-177).

**S3 (platform), one bucket plus prefixes, not separate raw and quarantine buckets:**
- `S3_BUCKET` holds:
  - the staging/quarantine area `quarantine/<tenant>/<upload>` (internal/service/upload/multipart_service.go:679; internal/service/processing/service.go:1247);
  - ingestion manifests `manifests/<tenant>/<job>.jsonl` (internal/service/ingestion/service.go:477).
- `PROMOTION_BUCKET` (the serving bucket, defaulting to `S3_BUCKET`) holds `<PROMOTION_PREFIX=assets>/<tenant>/<asset>` (config.go:225,429; docs/hardening-report.md:67).
- Packs live under `packs/` (internal/service/catalog/reconcile.go:391; docs/packing.md:126).
- Terraform folders `s3-quarantine`, `s3-serving`, `s3-replication`, `s3-inventory` and `s3-lifecycle` suggest a two-bucket plan with replication, but they are empty.
- `S3_INVENTORY` is an adoption source (internal/model/adoption.go:14).
- Recommended backstop: a bucket lifecycle rule `AbortIncompleteMultipartUpload` (docs/multipart-reliability.md:186-190).

**Encryption and KMS:**
- Platform bucket: the code sets no SSE header. The only `ServerSideEncryption`/`SSEKMSKeyId` is per tenant connection (internal/infrastructure/storage/s3/connection.go:146-147), so the platform relies on bucket-default encryption.
- Per-connection KMS key: S3 SSE-KMS, GCS CMEK (internal/infrastructure/storage/gcs/gcs.go:57-58,158-160) and Azure (internal/dto/request/storage_connection.go:47; registry/build.go:148-154; migration 000034_residency_keys).
- Credential sealing is AES-256-GCM with app keys. A KMS envelope is a seam only, not deployed (docs/storage-connections.md:102-113).
- SQS: `KmsThrottled` and KMS access errors are classified as retryable (internal/infrastructure/queue/sqs/retry.go:85-91, retry_test.go).

**CloudFront and signed URLs:**
- A CloudFront signer exists (internal/infrastructure/cdn/cloudfront/signer.go). It is enabled by `DELIVERY_CDN_*`.
- "the CDN only fronts the platform's bucket" (docs/storage-connections.md:38).
- Other delivery modes:
  - S3 presigned URLs, not revocable;
  - token links: 32 random bytes, SHA-256 stored, revocable, served through the API proxy (docs/delivery.md:109).

**ClamAV:**
- One `CLAMAV_ADDRESS`; clamd INSTREAM over TCP (internal/infrastructure/scanner/clamav/scanner.go:1-2,131).
- Objects larger than 64 MiB are scanned as overlapping segments, 4 in parallel, each on its own clamd connection (docs/content-security.md:45-50).
- Objects above clamd `StreamMaxLength` are held unscanned and never promoted (docs/storage-connections.md:69).

## 3. Hybrid / BYOS

- **Providers:** AWS S3 (`ACCESS_KEY`, `ASSUME_ROLE` with an external ID), R2, S3-compatible, GCS (service-account key), Azure (account key or SAS) (docs/storage-connections.md:54-65; internal/model/storage_connection.go:50-53,94).
- **Binding:**
  - Every upload, asset and dataset has an immutable `storage_connection_id` (trigger `storage_binding_immutable`; docs/hardening-report.md:69,121; migrations/000015_storage_connections.sql:11,66-75).
  - Workers resolve the connection from the DB per record through `storageregistry.ForProcess` (cmd/processing-worker/main.go:167-173).
  - A record whose connection is unreachable fails and retries. It never falls back to another bucket (docs/storage-connections.md:48; empty-binding fallback removed: docs/hardening-report.md:29-30).
- **Credentials:**
  - Sealed with AES-GCM. The associated data is `storage-credential/v1|tenant|connection|provider|version`.
  - Plaintext exists only while a client is being built.
  - Key rotation uses multi-version keys (docs/storage-connections.md:102-112).
  - The startup check refuses a key that reuses `DATABASE_URL`, `S3_SECRET_ACCESS_KEY`, `SQS_SECRET_ACCESS_KEY` and similar secrets (config/storage.go:131-144).
- **Caches:** client cache with 15m TTL (at most 24h); connection-row cache 10s (config/storage.go:80-81,98-101). Rotation is safe across processes (fix H3, docs/hardening-report.md:31).
- **ASSUME_ROLE:** 1h STS sessions, renewed while 45 min remain. Keep URL TTLs ≤ 45m (docs/storage-connections.md:79; docs/release-candidate.md:97). The platform's own AWS identity must be allowed to assume the tenant role.
- **Network guard:**
  - https only; no private, loopback, metadata, NAT64 or 6to4 addresses; re-checked on every dial; no redirects, no proxy; TLS ≥ 1.2 (docs/storage-connections.md:115-129).
  - Webhooks use the same guard (docs/webhooks.md:38-44).
  - Implication for AWS: workers need NAT/egress to the internet for BYOS and webhooks.
- **Cross-connection copy:** a bounded stream, holding at most 525 MiB at 5 TiB; a multipart copy above 5 GiB within S3 (docs/storage-connections.md:69; docs/release-candidate.md:102).
- **Live clouds:** never tested. Only emulators and MinIO; the D8 check was run on AWS S3 (docs/release-candidate.md:21,100; docs/hardening-report.md:60).

## 4. Queue abstraction and the Kafka mapping

**Interface** `interfaces.MessageQueue` (internal/interfaces/job_queue.go:65-85):
- `Publish(queue, {Body, GroupKey, DedupKey}) → id`
- `Receive(queue, max, visibility, wait) → []{ID, Body, Receipt, ReceiveCount, SentAt, GroupKey}`
- `Ack(receipt)`
- `Retry(receipt, delay)`
- `Extend(receipt, visibility)`
- `ReceiveDeadLetters(queue, max, visibility)`
- `AckDeadLetter(receipt)`
- `Stats(queue) → {Waiting, InFlight, Delayed, DeadLetters}`

Typed errors: `ErrQueueThrottled`, `ErrQueueNotFound`, `ErrMessageTooLarge`, `ErrReceiptExpired` (job_queue.go:11-22).

Implementations:
- SQS: internal/infrastructure/queue/sqs.
- In-memory with failure injection, for tests (internal/infrastructure/queue/memory; docs/events.md:20).

**Queues and event types** (internal/dto/event/envelope.go:74-84):

| Queue | Event types | Consumer |
|---|---|---|
| `processing` | `upload.completed` | processing-worker |
| `lifecycle` | `asset.deletion_scheduled`, `legal_hold.released`, `catalog.version_replaced` | cleanup-worker (cmd/cleanup-worker/main.go:253-256) |
| `ingestion` | `ingestion.manifest_staged`, `upload.processed` | upload-worker |
| `datasets` | `dataset.import_queued`, `dataset.validation_queued` | dataset-worker |
| `packing` | `pack.planned` | pack-worker |

One consumer per queue, named after the queue (envelope.go:32-33).

**SQS specifics:**
- FIFO queue `<prefix><q>.fifo` with DLQ `<prefix><q>-dlq.fifo` (sqs/client.go:4-8,110-116).
- `GroupKey` = `aggregate_type:aggregate_id`; `DedupKey` = `event_id-republished` (internal/service/events/publisher.go:181-184). SQS drops repeats within 5 min (sqs/producer.go:18-36).
- Body ≤ 256 KiB (sqs/client.go:29).
- `Retry` and `Extend` are `ChangeMessageVisibility`, rounded up to whole seconds (sqs/retry.go:21; sqs/consumer.go:82,133).
- Redrive after `SQS_MAX_RECEIVE_COUNT` 20; the DLQ keeps messages 14 days (sqs/dead_letter.go:29,48).
- Request timeout 30 s (sqs/client.go:37).
- Queues are created only with `SQS_CREATE_QUEUES`; in AWS they are pre-provisioned (client.go:52-54,121-125).

**Retry budgets** (docs/events.md:135-156):
- publisher: 10 attempts, 2s..5m, jitter;
- consumer: 8 attempts recorded in the inbox, 5s..15m;
- SQS redrive (20) is only a backstop and must be greater than `EVENT_MAX_ATTEMPTS`;
- throttling spends no publisher attempt (publisher.go:199-209).

**Ordering:**
- Per aggregate, in four layers (docs/events.md:190-197): the outbox won't publish while an earlier event of the aggregate is PENDING (outbox_repository.go:147-153); FIFO groups; the inbox stale check (internal/service/events/consumer.go:349-364); domain state.
- The consumer handles one group's messages in order and hands back the rest when one doesn't finish (consumer.go:203-255).

**Poison messages:**
- Undecodable envelopes, unknown types and unsupported versions are written to `dead_letters` and acked. When even the ID is unreadable, only a SHA-256 digest is kept (consumer.go:485-506).
- Permanent handler errors and exhausted attempts go through `giveUp` (consumer.go:467-483).

**Kafka mapping (SUGGESTIONS, not in code):**
- Topic and partitioning:
  - one topic per logical queue (5 topics), message key = `GroupKey`, so one aggregate stays in one partition and keeps its order;
  - one consumer group per queue name (`ConsumerConfig.Name`).
- `Publish`: use an idempotent producer with `acks=all`, wait for the ack, put `DedupKey` and `event_id` in headers. Kafka will not drop a republish the way the SQS 5-minute dedup does. That is acceptable: docs/events.md:105 already relies on the inbox after 5 minutes.
- `Receive`/`Ack`: fetch, then commit the offset only once every earlier offset in that partition is settled. That needs per-partition offset tracking, because the consumer runs groups concurrently (consumer.go:148-191).
- `Extend`: make it a no-op. Fencing is the inbox lease (consumer.go:400-438). Keep `max.poll.interval.ms` above the handler time by polling on a separate goroutine (handler timeout 15m; pack builds up to 30m, cmd/pack-worker/main.go:155-156), or use pause/resume.
- `Retry(delay)` is the hard part. SQS FIFO holds the whole group while a message is hidden; Kafka cannot hide one message. Options:
  - (a) Pause the partition and re-handle in process after the delay. This keeps order but blocks every aggregate on that partition.
  - (b) Produce to `<q>.retry` with a not-before header, then commit. Later events of the same aggregate can then overtake it, and the inbox stale check SKIPs the older one (consumer.go:359-364). That is a semantic change: SQS waits; Kafka would skip.
  - Recommendation: (a) for `processing`/`lifecycle`, or (b) only where skipping is safe.
- `ReceiveCount`: Kafka has none. Carry an attempt header, or rely on `rec.Attempts` from the inbox, which the consumer already uses for its budget (consumer.go:343-347,394).
- `ReceiveDeadLetters`/`AckDeadLetter`: a `<q>.dlq` topic read by its own group, or return empty. `dead_letters` is the authoritative record anyway (docs/events.md:160).
- `Stats`: consumer-group lag → Waiting; uncommitted → InFlight; retry-topic lag → Delayed; DLQ size → DeadLetters.
- Error classification: map broker "not leader", timeouts and throttling to retryable or `ErrQueueThrottled`; map `MESSAGE_TOO_LARGE` to `ErrMessageTooLarge`; map a missing topic to `ErrQueueNotFound`.
- Config: `EVENT_QUEUE=sqs` is checked as a string literal in each main (cmd/outbox-worker/main.go:50; cmd/processing-worker/main.go:289), and every main calls `sqsqueue.New` directly, so this needs a small factory change in 6 mains.
- Cost note: the polls already do everything except `catalog.version_replaced`. A lowest-cost deployment could run with no broker (`EVENT_QUEUE` unset) once that one event gets a poll.

## 5. Failure modes and existing recovery

| Failure | Behaviour today | Cite |
|---|---|---|
| Crash after the business commit, before publish | the event is in the outbox (same transaction) and is published on the next claim | docs/events.md:3,80-86 |
| Publisher crash after the queue accepted the message, before recording it | the lease expires and it is sent again; SQS dedup within 5 min, then the inbox drops it | docs/events.md:105; publisher.go:121-124 |
| Slow queue overruns the lease | sends start only while they can finish before the lease ends (`sendBy`); otherwise released unsent | publisher.go:121-124,148-162 |
| Queue throttling | batch given back after a backoff, no attempt spent | publisher.go:199-209; docs/events.md:102 |
| Queue down or missing, IAM or KMS error | retryable within budget, never permanent | sqs/retry.go:45-110; docs/events.md:146 |
| Duplicate or concurrent delivery | inbox `ClaimDone`/`ClaimDead` → ack; `ClaimBusy` → hand back until the lease ends | consumer.go:315-341; tests listed at docs/release-candidate.md:251 |
| Consumer crash mid-handler | nothing written; visibility and inbox lease expire, then redelivered; handlers are idempotent | consumer.go:366-374; docs/events.md:124-131 |
| Lost ack | outcome already recorded, so the redelivery is dropped as a duplicate | consumer.go:521-527 |
| Database down during consume | claim fails, so the message is retried with a growing delay | consumer.go:321-328 |
| Database down at runtime (worker loops) | batch logged, retried next tick; LISTEN reconnects with backoff | internal/worker/outbox/worker.go; notify.go:20-37 |
| Database down at startup | process exits with an error | db.go:40-43 |
| Message never consumed, ElasticMQ restart, lost message | reconciler `unconsumed`: republish with a new generation up to 3×, then dead letter `UNCONSUMED` | docs/events.md:206,301-303 |
| Stalled inbox (worker died mid-handle) | `inbox_stalled` → FAILED and due, republished | docs/events.md:207 |
| Completed upload without an event | `missing_event` → emit, de-duplicated under the aggregate lock | docs/events.md:208; reconciler.go:282 |
| Native DLQ messages | drained into `dead_letters` (`NATIVE_DLQ/REDRIVE`) | docs/events.md:205; reconciler.go:133-186 |
| Operator recovery | `event-admin retry/replay/resolve/discard`; stale replays refused | docs/events.md:174-188 |
| Processing worker crash | 1m lease renewed every lease/3; a crashed job is reclaimed in about 1 min. Per-claim fencing token `worker#attempt`; promotion re-renews before publishing; max 30m per claim | docs/content-security.md:161-183; config.go:242-243,306-315. Stale: release-candidate R1 "15 min" (fixed per docs/release-candidate.md:104) |
| Job enqueue lost | jobs are derived from state (`EnqueueDue` from COMPLETED sessions); the next cycle creates them | docs/content-security.md:151-159 |
| ClamAV down or slow | startup ping only warns (`scanner_up` 0) (cmd/processing-worker/main.go:265-276). TIMEOUT/ERROR retried with backoff, then EXHAUSTED → quarantine HELD / SCAN_INCOMPLETE. Never promoted (fail closed). `SCANNER=none` defaults to `hold`; `allow_unscanned` only in development. Operator: `processing-worker -requeue` | docs/content-security.md:22-35; config.go:327-341; main.go:253-262 |
| Content swapped after verification | the scanned stream is re-hashed; a mismatch is rejected; reads pinned to the ETag | docs/release-candidate.md:57; docs/content-security.md:39-43 |
| Checksums | client SHA-256 required (`checksum_missing`); size and streamed SHA-256 verified; ETag never trusted; promotion copy re-verified | docs/content-security.md:11-20; docs/storage-connections.md:67 |
| Multipart failures (lost initiate ID, lost part confirmation, lost completion, abort/update split, expiry, concurrent complete/abort) | compare-and-set states plus reconciliation tasks; backoff to `RECONCILIATION_MAX_ATTEMPTS`, then EXHAUSTED | docs/multipart-reliability.md:3-36 |
| Orphan provider sessions or staging objects | per-connection listing scans with cursors in `inventory_scans`; a page that fails five times ends the scan FAILED; nothing deleted on a DB error; unrecorded items in tenant buckets only reported | docs/multipart-reliability.md:37-60 |
| Deletion crash | IN_PROGRESS lease expires and is re-run idempotently; HEAD must answer not-found; never a prefix delete | docs/lifecycle.md:61-95 |
| Pack worker crash or stale writer | lease plus per-claim owner; rewrite from scratch; verify then publish atomically | docs/packing.md:128-141; docs/phase15-report.md:125-138 |
| Webhook receiver down | retries after 10s, 1m, 5m, 30m, 2h, 6h, 12h, 24h, then FAILED; manual retry via API; 1-minute lease; no redirects. **No auto-disable of failing endpoints: not found** | internal/service/webhook/sender.go:28-38; docs/webhooks.md:33-36,62 |
| Ingestion client abandons uploads | slot lease sweep, `FAILED_RETRYABLE`, backoff; a lost `upload.processed` is caught by the outcome sweep | docs/ingestion.md:257-287 |
| API idempotency | `idempotency_key` in upload, multipart, batch and dataset bodies; unique index `ux_upload_sessions_idempotency`; `Idempotency-Key` header required for dataset imports | internal/dto/request/create_upload.go:10; migrations/000001_create_upload_sessions.sql:36; internal/handler/http/dataset_handler.go:355-362 |
| Rate limits | in-memory sliding window per process; defaults e.g. `upload_finish` 600/1m, `ingestion_entry` 3000/1m | internal/platform/ratelimit/limiter.go:1-9; config/security.go:81-99 |
| Graceful shutdown | API `Shutdown` with 10 s, then `Close` (cmd/api/main.go:364-377); consumers write nothing and let leases lapse (consumer.go:297-301); workers wait for consumers (W1 fix, docs/release-candidate.md:64) | |
| S3 errors | a short write aborts (D10); retries via job budgets; promotion copies use `if-match` ETag; a copy whose answer was lost is re-checked | docs/release-candidate.md:60; docs/storage-connections.md:69 |
| BYOS credential rotation or disable | re-read row on a newer version; a disabled connection drains, existing objects still served | docs/hardening-report.md:31,125 |

## 6. Gaps and single points of failure

1. **Postgres is the SPOF and the bottleneck.** It holds the work queues, leases, outbox and webhooks, and it was the busiest component in every load run (docs/release-candidate.md:235). A Multi-AZ or failover target is needed. Each process opens up to 20 pooled connections plus 1 LISTEN connection (db.go:29; notify.go:41), so 11 services × N replicas × 21 must fit `max_connections` (local default 100; docs/release-candidate.md:169). RDS Proxy is complicated by LISTEN, which needs a pinned direct connection.
2. **Clock dependence.** Leases are computed from worker time (`now` passed in, e.g. outbox_repository.go:144-160); risk noted in docs/phase15-report.md:142,285.
3. **Rate limits are per process** (limiter.go:7-9): N API replicas allow N× the limit. Login throttling also depends on `AUTH_TRUST_PROXY_HEADERS` behind an ALB (docs/phase15-report.md:227-228).
4. **Single ClamAV address:** no pool or failover. An outage HOLDs uploads; they need an operator requeue after exhaustion (docs/content-security.md:28-30).
5. **The `catalog.version_replaced` path needs the queue** (docs/release-candidate.md:110). A queue outage delays retirement only, since the reconciler republishes.
6. **webhook-worker has no health or metrics endpoint;** endpoints are never auto-disabled.
7. **Compaction defaults to dry run.** Outbox, inbox, parts and idempotency rows grow unless `COMPACTION_ENABLED=true`. Inbox retention must stay above the queue's message retention, 14 days for SQS (docs/events.md:228). For Kafka, set retention below the inbox retention.
8. **Synchronous snapshots:** 1M members take 160 s in one HTTP request and transaction; at 5M, about 13 minutes (docs/hardening-report.md:44,105). This matters behind an ALB with a 60 s idle timeout.
9. **Per-tenant counter rows** cap single-tenant ingestion (P1, docs/hardening-report.md:43,171-186). More processing workers make it worse.
10. Smaller gaps:
    - graceful shutdown uses up a job attempt;
    - no body read deadline on the API;
    - no tool re-seals credentials after a key rotation;
    - the inbox, validation and import workers share one lease owner per process (docs/phase15-report.md:239-243).
11. **KMS envelope for credentials not deployed;** platform-bucket SSE is not set in code. Both are infrastructure responsibilities.
12. **Untested at scale or on real cloud:** never more than one replica per role, never live AWS/GCS/Azure, never runs over an hour (docs/release-candidate.md:256-264; docs/hardening-report.md:188-195).
13. **Packed reads on MinIO** were 15-51% of standalone reads at 16 workers. Measure on real S3 first (docs/phase15-report.md:175,284).
14. **Terraform modules are empty,** so all infrastructure is still to be written.
15. **The startup database ping fails hard.** Restart policies and backoff are needed so a database blip doesn't crash-loop every service at once.

## 7. Load numbers (local laptop only, not capacity claims)

All runs: one i5-1135G7 laptop (4C/8T), 15.7 GB RAM, Docker; PostgreSQL with defaults (128 MB shared buffers, 100 connections) (docs/release-candidate.md:167-172).

| Measurement | Value | Cite |
|---|---|---|
| Single-tenant small-file upload | 35-40 files/s (39.8 at 1,000 files; 34.6 at 2,000); PUT p50/p95 39/90 ms | docs/release-candidate.md:180-181 |
| End-to-end published (verify + ClamAV + promote) | 11.0-14.9 files/s; 10.4 files/s in the hardening run | docs/release-candidate.md:178-181; docs/hardening-report.md:154 |
| Default rate limit effect | 11.0 files/s, with 1,170 of 2,170 completions answered 429 | docs/release-candidate.md:178 |
| Two tenants | 28 files/s together (41.2 in the hardening run) | docs/release-candidate.md:182; docs/hardening-report.md:155 |
| Mixed sizes up to 48 MiB | 10.4 MiB/s upload; ClamAV CPU peak 424% | docs/release-candidate.md:183 |
| Processing concurrency 2 / 4 / 8 | 13.2 / 12.7 / 9.6 files/s published (more is worse) | docs/hardening-report.md:156 |
| Postgres | CPU mean 64-85%, peak 116-386%; at most 45 connections; commit p50 3.3 ms; round trip p50 0.54 ms | docs/hardening-report.md:157-165; docs/release-candidate.md:185 |
| Manifest | 1M lines read at 4,723 lines/s (12,794 in Phase 7); cancelling 1M entries takes 24 min; database +927 MiB | docs/release-candidate.md:185,231-232 |
| Catalog | 1M objects ≈ 3,008 MiB of database; keyset listing p50 ≈ 5-9 ms flat from 10k to 1M; unindexed filter 158 ms at 1M | docs/release-candidate.md:202-219; docs/hardening-report.md:230 |
| Snapshot | 10k in 3.8 s, 100k in 11.7 s, 1M in 160.5 s (48.6 GiB referenced) | docs/hardening-report.md:99-103 |
| Peak heap | API 12 MiB (130 MiB during Argon2 sign-in, 64 MiB per hash), upload worker 8-10 MiB, processing 10-11 MiB, outbox 4.8 MiB, pack writer ≤ 48 MiB measured (66 MiB bound) | docs/release-candidate.md:189-198; docs/phase15-report.md:167-176 |
| Pack write | 1,679-1,803 objects/s at 4 KiB; 8.2 MiB/s at 64 KiB | docs/phase15-report.md:167-171 |
| Large copy | 5.06 GiB copy on MinIO 65.6 s; hashing it 141 s | docs/storage-connections.md:69 |
| SQS FIFO limit noted | 300 msg/s per queue without batching | docs/events.md:296 |

Sizing take-away: small compute is enough. Go heaps are about 10 MiB, so 0.25-0.5 vCPU tasks suffice except for ClamAV and the Argon2 sign-in. Spend on Postgres CPU and IOPS and on ClamAV CPU.
