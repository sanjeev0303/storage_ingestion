# Bulk ingestion: datasets, manifests and bounded-concurrency uploads (local runbook)

Bulk ingestion loads thousands to millions of files into a **dataset** from one **manifest**. Each manifest line becomes an **entry** with durable state. The server never handles file bytes:

- **Uploads:** clients upload straight to object storage, through the presigned URLs the API grants.
- **Processing:** after each upload, the Phase 5 pipeline (verification, content validation, malware scan, promotion) runs exactly as for any upload.
- **Events:** the Phase 6 event pipeline carries the outcome back to the entry.

Scheduling is **pull-based**: a client *claims* entries and gets one upload slot per entry granted. Every limit is enforced in the claim's database transaction:

- global, per-tenant and per-job slots, with fair shares;
- multipart sessions;
- the storage quota;
- the processing backlog.

Nothing is queued in memory: the `PENDING` entries in PostgreSQL are the queue.

| Piece | Where | Role |
|---|---|---|
| Schema | `migrations/000011_bulk_ingestion.sql` | datasets, jobs, entries, tenant usage and quotas, the claim lock |
| Model | `internal/model/batch.go` | states, error codes, readiness policies |
| Repository | `internal/infrastructure/database/postgres/batch_repository.go`, `batch_accounting.go` | every state change with its counters, in one transaction |
| Service | `internal/service/ingestion` | manifests (`manifest.go`, `path.go`), claims and uploads (`scheduler.go`), outcomes, retries and cancellation (`outcome.go`), worker sweeps (`sweeps.go`) |
| API | `internal/handler/http/batch_handler.go` | `/api/v1/datasets`, `/api/v1/ingestion/jobs/...` |
| Worker | `cmd/upload-worker` | sweeps, staged-manifest reads, the `ingestion` queue consumer, reconciliation |
| Load generator | `cmd/ingestion-loadgen` | synthetic datasets, measured runs |

## Datasets and readiness

A dataset is a tenant's named collection: `name` matches `[a-z0-9][a-z0-9._-]{0,63}`, unique per tenant. Its name is the dataset label the upload sessions and assets carry, so the Phase 5 retention policies for that dataset apply to ingested assets. Creating a dataset with the same name and settings again returns the existing one; different settings give 409.

Its status is derived from its jobs' counters every time a job finishes or starts. The rules are in `model.DatasetStatusOf`:

| Status | When |
|---|---|
| `OPEN` | no job yet |
| `INGESTING` | at least one job is not finished. A dataset is **never** ready while any entry is pending, in flight or unprocessed |
| `READY` | `ALL`: every valid entry of every job published, and no invalid, failed or cancelled entry. `REQUIRED`: no required entry invalid, failed or cancelled, and at least one published. `PARTIAL`: everything published |
| `PARTIALLY_READY` | `PARTIAL` only: something published, something not |
| `INCOMPLETE` | every job finished and the policy is not met |

`max_bytes` caps a dataset's size on top of the tenant quota.

## Manifest

JSON Lines: one object per line, UTF-8. A line's 1-based number is its entry's **position**, the entry's stable identity. Blank lines are skipped but keep their number.

```json
{"path":"images/2026/cat.jpg","size":48213,"content_type":"image/jpeg","checksum":"sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"}
{"path":"video/a.mp4","size":734003200,"content_type":"video/mp4","checksum":"sha256:...","required":false,"metadata":{"camera":"a7","take":"3"}}
```

| Field | Rule |
|---|---|
| `path` | required. The logical path inside the dataset, never a storage key (see below) |
| `size` | required, `1 ≤ size ≤ INGESTION_MAX_OBJECT_BYTES`. Both upload paths check the stored object against it |
| `content_type` | required, a valid media type (`mime.ParseMediaType`), at most 255 bytes |
| `checksum` | required, `sha256:<64 hex>`. Processing publishes an object only if its SHA-256 matches, so an entry without one could never be published |
| `required` | optional, default `true`. Used by the `REQUIRED` policy |
| `metadata` | optional flat object of string values: at most `INGESTION_MAX_METADATA_KEYS` keys of `[A-Za-z0-9._-]{1,64}`, at most `INGESTION_MAX_METADATA_BYTES`. Stored re-encoded with sorted keys; never logged |

Unknown fields are ignored. A line longer than `INGESTION_MAX_LINE_BYTES` is read to its end, not kept, and becomes `line_too_long`.

**Invalid lines do not fail the manifest.** Each becomes an `INVALID` entry with a stable error code, and the next line is read:

`invalid_json`, `line_too_long`, `invalid_path`, `duplicate_path`, `invalid_size`, `too_large`, `invalid_checksum`, `checksum_required`, `invalid_content_type`, `invalid_metadata`.

The manifest as a whole fails only in these cases:

- it has more than `INGESTION_MAX_ENTRIES_PER_JOB` lines;
- it is larger than its byte limit;
- it does not match its declared SHA-256;
- the tenant's pending entries would pass `max_pending_entries`. That batch is rolled back, and the manifest can be sent again later.

### Paths (`internal/model/path.go`)

Normalization: `.` segments and repeated `/` collapse. The **path key** used to detect duplicates is the normalized path in lower case, so `Photos/Cat.jpg` and `photos//./cat.jpg` collide.

Rejected (`invalid_path`):

- empty paths;
- invalid UTF-8;
- absolute paths;
- a trailing `/`;
- drive letters (`C:`);
- `..` segments;
- backslashes;
- the characters `< > : " | ? *`;
- control characters;
- segments with leading or trailing spaces;
- paths longer than `INGESTION_MAX_PATH_BYTES`, segments longer than `INGESTION_MAX_SEGMENT_BYTES`, or deeper than `INGESTION_MAX_PATH_DEPTH`;
- the reserved top-level names `_system`, `_ingestion`, `.well-known`.

Storage keys are generated by the upload services from trusted IDs (`quarantine/<tenant>/<upload>`). A manifest path never reaches a storage key.

A path is unique among the **live** entries of a dataset, across all its jobs. A partial unique index (`ux_ingestion_entries_path`) enforces it. A later duplicate becomes `duplicate_path`. Once the entry holding a path is invalid, failed or cancelled, the path is free again.

### Submitting

| Mode | When | How |
|---|---|---|
| Inline | up to `INGESTION_MAX_INLINE_MANIFEST_BYTES` (16 MiB) | `PUT /jobs/{id}/manifest` with the JSONL body, optional `X-Manifest-SHA256` |
| Staged | up to `INGESTION_MAX_MANIFEST_BYTES` (2 GiB) | `POST /jobs/{id}/manifest/staged-url {"size_bytes":N}` gives a presigned PUT to `manifests/<tenant>/<job>.jsonl` (send `Content-Type: application/x-ndjson`); then `POST /jobs/{id}/manifest/staged {"sha256":"..."}` |

Both are read as a stream, in batches of `INGESTION_INSERT_BATCH` lines. Memory is bounded by one line plus one batch, whatever the manifest's size. Each batch is stored **with its checkpoint** (last line and byte offset) in one transaction. A crash or failed batch leaves either the whole batch and its checkpoint or neither.

- **Inline:** sending the same manifest again resumes after the checkpoint. Once the manifest is complete, the same bytes return the same report and different bytes give 409.
- **Staged:** submitting records the object's size and ETag and writes `ingestion.manifest_staged` in the same transaction. The upload worker then reads the object from its checkpoint with ranged GETs:
  - `If-Match` is set on the ETag, so a manifest replaced after submission fails the job instead of mixing two manifests;
  - the reader holds a lease (`INGESTION_MANIFEST_LEASE`), so only one reader works on a job at a time;
  - each pass stops after `INGESTION_PARSE_BUDGET`;
  - a stopped or crashed reader is resumed by the worker's poll from the last checkpoint;
  - once every line is stored, a declared SHA-256 is checked with one streaming pass over the object.

`POST /jobs/{id}/finalize` moves a fully read job to `READY`. A job with no valid entry is `FAILED`. A job whose expected bytes cannot fit the remaining tenant or dataset quota is refused with 403. Entries are claimable only after finalizing.

## State machines

Job:

```
PENDING_VALIDATION ──manifest──▶ VALIDATING ──finalize──▶ READY ──first claim──▶ RUNNING ──last entry ends──▶ COMPLETED | PARTIALLY_COMPLETED | FAILED
        │                            │  └─ rejected manifest ─▶ CANCELLING (with error) ─▶ FAILED                 ▲
        └──────────── cancel ────────┴────────────── cancel ────────▶ CANCELLING ──last open entry ends──▶ CANCELLED
                                                                                                 retry ──┘ (terminal → RUNNING)
```

When a job's last open entry ends:

- every valid entry published and no invalid line → `COMPLETED`;
- some published → `PARTIALLY_COMPLETED`;
- none published → `FAILED`.

Retrying failed entries of a terminal job other than `CANCELLED` makes it `RUNNING` again. That counts against the active-job limit again.

Entry:

```
INVALID (validation; final)

PENDING ──claim──▶ SCHEDULED ──heartbeat──▶ UPLOADING
   ▲                  │  └──────────┬───────────┘
   │                  │        complete (size checked)
   │                  ▼             ▼
   │   FAILED_RETRYABLE ◀── UPLOADED ──asset processing──▶ PROCESSING ──▶ PUBLISHED (asset AVAILABLE; final)
   │    │  (lease expired,           │                         │
   │    │   storage error,           └────────────┬────────────┘
   │    │   processing held)              content rejected / deleted
   └────┘ backoff over                            ▼
                                              FAILED (final unless manually retried)
any open state ──job cancelled──▶ CANCELLED (stored uploads still finish and publish)
```

| From | To | Trigger |
|---|---|---|
| PENDING | SCHEDULED | a claim grants a slot and reserves `size` bytes |
| SCHEDULED/UPLOADING | UPLOADED | `complete`: a SINGLE object exists with exactly the declared size, or a MULTIPART upload completed from its parts. The slot is released and processing takes over |
| SCHEDULED/UPLOADING | FAILED_RETRYABLE | lease expired (`upload_abandoned`), the upload could not be started (`storage_unavailable`). Next attempt gets a new upload session (new generation) |
| SCHEDULED/UPLOADING | FAILED | the object's size differs (`size_mismatch`), the upload service refused the request, or attempts are spent (`attempts_exhausted`) |
| UPLOADED/PROCESSING | PUBLISHED | the asset is AVAILABLE; its verified size becomes stored bytes |
| UPLOADED/PROCESSING | FAILED | the asset was rejected (`content_rejected`) or deleted |
| UPLOADED/PROCESSING | FAILED_RETRYABLE | processing exhausted its budget (`processing_exhausted`, e.g. scanner outage). The upload is kept; the retry requeues processing |
| FAILED_RETRYABLE | PENDING | the backoff is over (worker), or a manual retry |
| FAILED | PENDING | manual retry of a retryable code with manual retries left |

Every transition is a compare-and-set on the entry's current state, and on its current upload ID where one is involved. An outcome of an older attempt therefore changes nothing.

## Upload strategy

| Size | Strategy | Client flow |
|---|---|---|
| `≤ INGESTION_SMALL_FILE_THRESHOLD` (16 MiB) | `SINGLE` | the grant carries a presigned PUT: `PUT` the bytes with `Content-Type` = the entry's content type (it is signed), then `POST .../entries/{pos}/complete` |
| larger | `MULTIPART` | `POST .../parts/urls {"part_numbers":[1,2,...]}` (≤ `INGESTION_MAX_PART_URLS` per call), `PUT` each part (≥ 5 MiB except the last), `POST .../parts/confirm` with ETags and sizes, `POST .../complete` with the part list |

For small files this avoids the multipart begin, list-parts and complete round trips, and the session bookkeeping, so one PUT and one completion are all a file costs.

- **Large files** reuse the Phase 4 multipart service: part verification against the provider, completion recovery and abort cleanup.
- **Upload sessions** are created when the slot is granted, with the idempotency key `ingestion:<job>:<position>:<generation>`. A repeated claim or `upload-url` of the same attempt finds the same session.
- **Expired URLs:** `POST .../upload-url` gives a fresh PUT URL.
- **Long uploads:** `POST .../heartbeat` renews the slot lease (`INGESTION_SLOT_LEASE`).

## Concurrency, fairness and backpressure

One claim is one transaction (`IngestionRepository.Claim`):

1. **Global claim lock.** Take the lock on the `ingestion_capacity` row. Claims of all tenants run one at a time here, so each sees every earlier claim's grants.
2. **Global free slots.** Count the entries in `SCHEDULED`/`UPLOADING` across tenants, using the partial index `ix_ingestion_entries_lease`. The free global slots are `INGESTION_GLOBAL_UPLOAD_SLOTS` minus that count. Slots are not kept as a counter, so releasing a slot (complete, expiry, cancel) never touches a row other tenants need.
3. **Lock the tenant's rows.** Lock the tenant usage row, the dataset and the job, in that order.
4. **Backpressure.** If the tenant's uploads awaiting processing (`UPLOADED` + `PROCESSING`) reach `INGESTION_TENANT_PROCESSING_BACKLOG`, grant nothing (`processing_backlog`). Uploads stop piling up in quarantine while scanning catches up.
5. **Fair shares.**
   - Tenant cap = `min(tenant slots, ceil(global slots / tenants with pending entries))`.
   - Job cap = `min(INGESTION_JOB_UPLOAD_SLOTS, ceil(tenant cap / that tenant's jobs with work), job max_concurrency)`.

   One large tenant or job cannot take every slot while another waits. An idle one does not hold slots back.
6. **Size the grant.** Grant `min(asked, free global, tenant cap − tenant uploads, job cap − job uploads)`.
7. **Pick entries.** Take PENDING entries in position order, `FOR UPDATE SKIP LOCKED`. Leave out an entry that would open one multipart session too many (`INGESTION_TENANT_MULTIPART_SESSIONS`) or pass the tenant's or dataset's storage quota.
8. **Grant.** Mark the entries `SCHEDULED` with a lease, reserve their bytes and update the counters.

The answer names the limit that held the claim back (`limited_by`):

`global_slots`, `tenant_slots`, `job_slots`, `multipart_sessions`, `storage_quota`, `processing_backlog`, `no_pending_entries`, `job_not_running`.

When nothing was granted for a limit, it also gives `retry_after_seconds`.

After the transaction, the granted entries' upload sessions are prepared eight at a time (each is a database write and a presign). Storage calls go through a process-wide limiter (`INGESTION_STORAGE_CONCURRENCY`), each with `INGESTION_STORAGE_TIMEOUT`. Staged manifest reads are limited to `INGESTION_PARSE_CONCURRENCY` at once. No goroutine is started per entry: worker sweeps take one batch of `INGESTION_WORKER_BATCH` per pass.

**Lock order.** Taken in this order everywhere, so there are no deadlocks:

1. entry;
2. claim lock (claims only);
3. tenant usage;
4. dataset;
5. job.

Two rules keep it:

- A change that leaves the tenant usage or dataset unchanged does not lock them. A heartbeat or an `UPLOADED → PROCESSING` change touches only the entry and its job.
- A change that closes an entry always locks both, because finishing the job updates them. A transition settles its job only when it closes an entry.

## Quotas and reservations

| Limit | Default | Where |
|---|---|---|
| Storage bytes | `QUOTA_MAX_STORAGE_BYTES` (1 TiB) | reserved per entry at claim; checked again entry by entry |
| Dataset bytes | dataset `max_bytes` | same |
| Active jobs | `QUOTA_MAX_ACTIVE_JOBS` | conditional increment at job creation and at revival |
| Upload slots | `INGESTION_TENANT_UPLOAD_SLOTS` | claim |
| Pending entries | `QUOTA_MAX_PENDING_ENTRIES` | conditional increment per manifest batch |
| Manifest entries and bytes | `QUOTA_MAX_MANIFEST_ENTRIES`, `QUOTA_MAX_MANIFEST_BYTES` | at submission, within the `INGESTION_MAX_*` ceilings |

These are the organization's quotas (`docs/security.md`). `tenant_quotas`, which Phase 8 made of Phase 7's `ingestion_tenant_quotas`, overrides any of them per organization through `PUT /api/v1/admin/organizations/{id}/quota`.

**Lifecycle of a reservation:**

1. **Reserve:** a claim reserves the entry's declared size against both the tenant and the dataset, in the claim transaction.
2. **Hold:** the bytes stay reserved through `UPLOADED` and `PROCESSING`.
3. **Release:** they are released when the entry fails or is cancelled.
4. **Convert:** they are converted into stored bytes when it publishes, using the asset's verified size.

**Why it is race-safe:**

- Only claims add reservations, and claims are serialized.
- A publication moves bytes from reserved to stored in one transaction.

So two concurrent jobs can never reserve past the quota. `TestIngestionQuotaHoldsUnderConcurrentJobs` runs eight concurrent claimers and checks it. `Finalize` also refuses a job whose total cannot fit at all, before anything is uploaded.

**Counters.** Every entry change updates these in the same transaction:

- the job's per-state counters;
- the tenant usage: reserved and stored bytes, uploads, multipart sessions, pending entries, backlog;
- the dataset's bytes.

Decrements floor at zero, so a drifted counter never blocks a transition.

**Reconciliation.** The worker runs it every `INGESTION_RECONCILE_INTERVAL`; it can also be run on demand with `upload-worker -reconcile`. It recounts each unfinished job from its entries and each tenant's usage from its jobs and entries, repairs any difference, settles a job whose stale counters kept it open, and counts the repairs (`ingestion_reconcile_*_repairs`).

## Retries, failures and cancellation

| Situation | What happens |
|---|---|
| Upload could not start (provider error) | entry `FAILED_RETRYABLE`, slot and reservation released, retried after backoff (`INGESTION_RETRY_BACKOFF_BASE`…`_MAX`, jittered) with a new session |
| Client stops mid-upload | after `INGESTION_SLOT_LEASE` the lease sweep looks at the session. Completed (the client never said so): recorded as `UPLOADED`. A SINGLE object that is there: completed. A started multipart upload: aborted. Otherwise `FAILED_RETRYABLE` |
| Client disconnects during `complete` | once the provider and the session say the upload is complete, the entry is recorded with a context detached from the request, so it does not wait for its lease |
| Client disconnects during a claim | entries already granted keep their lease: the client can ask for them again with `upload-url`. Any whose session could not be prepared are given back (`FAILED_RETRYABLE`) with a detached context |
| Wrong bytes uploaded | `size_mismatch`: the session fails, its object is deleted, the entry is `FAILED`. A manual retry re-uploads |
| Database or provider error in a request | the request fails with no state change; repeating it is safe |
| Scanner outage | processing holds the asset (Phase 5 `hold` policy); the entry becomes `FAILED_RETRYABLE` with `processing_exhausted`, keeps its upload, and its retry requeues processing instead of uploading again |
| `upload.processed` lost | the outcome sweep reads the asset of any entry waiting longer than `INGESTION_OUTCOME_SYNC_AFTER` |
| `INGESTION_MAX_ATTEMPTS` reached | `FAILED` with `attempts_exhausted` |

**Manual retry.** `POST /jobs/{id}/retry {"positions":[...]}` or `{"all":true}`, up to `INGESTION_MAX_MANUAL_RETRIES` times per entry. It retries `FAILED_RETRYABLE` entries and `FAILED` entries with a retryable code: `upload_abandoned`, `object_missing`, `size_mismatch`, `upload_failed`, `storage_unavailable`, `processing_exhausted`, `attempts_exhausted`.

It never touches these:

- published entries, so a retry cannot duplicate an asset;
- validation failures;
- rejected content;
- entries whose path another entry has taken since.

**Cancellation.** `POST /jobs/{id}/cancel` makes the job `CANCELLING`. From then on:

- claims and completions are refused;
- the worker cancels waiting entries in batches, and aborts and cancels uploads in progress;
- uploads already stored still finish processing, so assets already made are kept;
- the job ends `CANCELLED` once nothing is open.

A multipart abort the provider refuses leaves the session `ABORTING`. The Phase 5 multipart reconciliation finishes it (`ResolveAbort`), so no provider upload is left behind.

## Processing integration (Phases 5 and 6)

```
claim → session (upload service) → client PUT → complete ─┬─ upload.completed (outbox, same tx as the session)
                                                          │
                     processing-worker: Intake → verify SHA-256 → validate → scan → promote
                                                          │
                ProcessingJobRepository.Finish ── upload.processed (outbox, same tx, only for ingestion uploads)
                                                          │
                      upload-worker ingestion consumer → OnUploadProcessed → entry PUBLISHED / FAILED
```

The handler reads the asset's state rather than trusting the event, and records it with a conditional transition. Duplicate or late deliveries are therefore harmless: `TestIngestionRepeatedClaimsAndEventsDoNotDuplicateWork` delivers every outcome three times. The same applies to the staged-manifest event: a delivery that finds the job read, being read or cancelled does nothing.

Without `EVENT_QUEUE=sqs`, the worker's sweeps still do all the work: the outcome sweep and the manifest resume pass.

## API

All routes need an authenticated caller (`middleware.Authenticate`). Everything is scoped to the caller's tenant: another tenant's IDs answer 404.

| Route | Answer |
|---|---|
| `POST /api/v1/datasets` `{"name","completion_policy","max_bytes"}` | 201 created, 200 existing |
| `GET /api/v1/datasets/{id}` | dataset with status and bytes |
| `POST /api/v1/ingestion/jobs` `{"dataset_id","idempotency_key","max_concurrency"}` | 201 created, 200 same key |
| `GET /api/v1/ingestion/jobs?dataset_id=&after=&limit=` | page of jobs, newest first, `next_after` |
| `GET /api/v1/ingestion/jobs/{id}` | job with progress counters and `percent_published` |
| `PUT /api/v1/ingestion/jobs/{id}/manifest` | manifest report (accepted, invalid, duplicates, replayed, invalid samples); 413 too large; 409 different manifest |
| `POST .../manifest/staged-url` `{"size_bytes"}` | presigned PUT for the manifest |
| `POST .../manifest/staged` `{"sha256"}` | 202, read by the worker |
| `POST .../finalize` | job `READY` (403 over quota) |
| `POST .../claims` `{"max"}` | grants (entry, upload URL), `limited_by`, `retry_after_seconds` |
| `GET .../entries?state=PENDING,FAILED&after=&limit=` | page of entries by position, `next_after` |
| `GET .../failures?after=&limit=` | invalid, retryable, failed and cancelled entries, with error codes |
| `GET .../entries/{position}` | one entry |
| `POST .../entries/{position}/upload-url` | fresh grant for an entry the caller holds |
| `POST .../entries/{position}/parts/urls` `{"part_numbers"}` | part URLs (MULTIPART) |
| `POST .../entries/{position}/parts/confirm` `{"parts":[{"part_number","etag","size_bytes"}]}` | 204 |
| `POST .../entries/{position}/heartbeat` | lease renewed |
| `POST .../entries/{position}/complete` (`{"parts":[...]}` for MULTIPART) | 200 entry, 202 if the completion is still being confirmed |
| `POST .../retry` `{"positions"}` or `{"all":true}` | retried and requeued counts |
| `POST .../cancel` | 202 job `CANCELLING` |

Pages hold `limit` items (default 100, at most 1000) and give `next_after` while more may follow. Keyset pagination keeps every page cheap at any depth.

Errors:

| Code | Meaning |
|---|---|
| 400 | bad input |
| 401 | no caller |
| 403 | quota |
| 404 | not found, or another tenant's |
| 409 | state conflict, idempotency conflict, different manifest |
| 413 | manifest too large |
| 422 | manifest checksum mismatch |

A 500 is logged with a sanitized message.

## Database (`000011_bulk_ingestion.sql`)

| Table | Key and indexes |
|---|---|
| `datasets` | unique `(tenant_id, name)` |
| `ingestion_jobs` | unique `(tenant_id, idempotency_key)`; `(tenant_id, created_at, id)` for listing; partial `(state, updated_at)` for jobs with work |
| `ingestion_entries` | primary key `(job_id, position)`; partial unique `(dataset_id, path_key)` for live entries; unique `upload_id`; `(job_id, state, position)` for claims and pages; partial indexes for leases (`SCHEDULED`/`UPLOADING`, also used to count slots), due retries and entries awaiting processing |
| `ingestion_tenant_usage` | one row per tenant: reserved and stored bytes, active jobs, uploads, multipart sessions, pending entries, backlog |
| `ingestion_tenant_quotas` | per-tenant overrides |
| `ingestion_capacity` | one row, `upload_slots`: the lock all claims take |

Manifests insert with one `unnest` statement per batch; replayed and duplicate lines are found through `ON CONFLICT DO NOTHING`.

## Metrics

Exposed through expvar at `/debug/vars` with `METRICS_ADDR`, under `ingestion`, in the API and in the upload worker. `memstats` is always there.

| Area | Names |
|---|---|
| Manifests | `ingestion_manifest_lines`, `ingestion_manifest_batches`, `ingestion_batch_ms_total` (÷ batches = database batch latency), `ingestion_entries_accepted`, `ingestion_entries_rejected` |
| Scheduling | `ingestion_claims`, `ingestion_slots_granted`, `ingestion_claims_limited_<limit>`, `ingestion_slots_in_use` (gauge) |
| Uploads | `ingestion_uploads_single`, `ingestion_uploads_multipart`, `ingestion_entries_uploaded`, `ingestion_bytes_uploaded`, `ingestion_prepare_failures`, `ingestion_multipart_aborted` |
| Outcomes | `ingestion_entries_published`, `ingestion_entry_latency_ms_total` (÷ published = per-file latency, manifest to publication), `ingestion_entries_failed`, `ingestion_outcomes_duplicate`, `ingestion_leases_expired`, `ingestion_entries_retried`, `ingestion_jobs_cancelled` |
| Reconciliation | `ingestion_reconcile_job_repairs`, `ingestion_reconcile_usage_repairs` |
| Process | `ingestion_storage_calls_in_flight`, `ingestion_manifest_reads_in_flight`, `go_heap_alloc_bytes`, `go_heap_inuse_bytes`, `go_sys_bytes`, `go_goroutines` (gauges, upload worker) |

Where the bottleneck is:

| Bottleneck | Signs |
|---|---|
| Slots | many `claims_limited_global_slots` or `tenant_slots` |
| Storage | `storage_calls_in_flight` at its limit, and slow `storage_put` in the load generator |
| Processing and scanning | `claims_limited_processing_backlog`, with the outbox and processing backlogs in the load generator's database sample |
| PostgreSQL | `batch_ms_total` and claim latency rising while the containers' CPU stays low (lock waits) |
| Memory | the heap gauges |

Logs carry `job_id`, `position` and `tenant_id`, never URLs, credentials, file content or manifest metadata.

## Running it locally

From `server/`, with Docker Desktop. Both shells work. The examples use bash (Git Bash) and `curl`. **Always point `DATABASE_URL` at the local database.**

```bash
# 1. Local dependencies
docker compose up -d postgres minio clamav elasticmq

# 2. Migrations (PowerShell): local DB and the test DB
#    .\scripts\migrate.ps1
#    .\scripts\migrate.ps1 -Database object_storage_test -Create

# 3. API and workers
export DATABASE_URL="postgres://storage_app:<password>@127.0.0.1:5435/object_storage?sslmode=disable"
export APP_ENV=development HTTP_PORT=8080 S3_REGION=us-east-1 S3_ENDPOINT=http://127.0.0.1:9000 \
       S3_ACCESS_KEY_ID=minioadmin S3_SECRET_ACCESS_KEY=minioadmin S3_FORCE_PATH_STYLE=true S3_BUCKET=object-storage-it \
       SCANNER=clamav CLAMAV_ADDRESS=127.0.0.1:3310 \
       EVENT_QUEUE=sqs SQS_ENDPOINT=http://127.0.0.1:9324 SQS_ACCESS_KEY_ID=local SQS_SECRET_ACCESS_KEY=local SQS_CREATE_QUEUES=true
go build -o bin/ ./cmd/...
METRICS_ADDR=127.0.0.1:9201 ./bin/api &
METRICS_ADDR=127.0.0.1:9202 ./bin/upload-worker &      # ingestion sweeps + ingestion queue consumer
METRICS_ADDR=127.0.0.1:9203 ./bin/processing-worker &  # verification, scan, promotion
./bin/outbox-worker &                                  # publishes upload.completed / upload.processed / manifest_staged
API=http://127.0.0.1:8080/api/v1
# Sign in as a member with the developer role (docs/security.md shows how to
# make one). The headers go in a file, so the token stays off command lines.
TOKEN=$(curl -s $API/auth/login -d '{"email":"dev@example.test","password":"..."}' \
  | python -c 'import json,sys; print(json.load(sys.stdin)["access_token"])')
printf 'Authorization: Bearer %s\n' "$TOKEN" > auth.hdr   # add "X-Organization-ID: <ORG>" for an account in several
H='-H @auth.hdr'                                         # every request below sends them

# 4. A dataset
curl -s $H -X POST $API/datasets -d '{"name":"photos","completion_policy":"REQUIRED"}'
# → {"id":"<DATASET>",...}

# 5. A synthetic manifest (real checksums of files the load generator can produce again)
./bin/ingestion-loadgen -files 200 -seed 42 -manifest-out manifest.jsonl

# 6. A job and its manifest
curl -s $H -X POST $API/ingestion/jobs -d '{"dataset_id":"<DATASET>","idempotency_key":"photos-2026-10-19"}'
curl -s $H -X PUT $API/ingestion/jobs/<JOB>/manifest -H 'Content-Type: application/x-ndjson' --data-binary @manifest.jsonl
curl -s $H -X POST $API/ingestion/jobs/<JOB>/finalize

# 7. Progress
curl -s $H $API/ingestion/jobs/<JOB>                        # state, progress, percent_published
curl -s $H "$API/ingestion/jobs/<JOB>/entries?state=PENDING&limit=50"

# 8. A failed upload, then a retry of that entry
curl -s $H -X POST $API/ingestion/jobs/<JOB>/claims -d '{"max":1}'      # → grants[0].upload_url, entry.position
curl -s -X PUT -H 'Content-Type: text/plain' --data-binary 'wrong bytes' '<UPLOAD_URL>'
curl -s $H -X POST $API/ingestion/jobs/<JOB>/entries/1/complete        # → FAILED, size_mismatch
curl -s $H -X POST $API/ingestion/jobs/<JOB>/retry -d '{"positions":[1]}'

# 9. Cancel
curl -s $H -X POST $API/ingestion/jobs/<JOB>/cancel

# 10. Failures and reconciliation
curl -s $H "$API/ingestion/jobs/<JOB>/failures?limit=100"
./bin/upload-worker -reconcile          # recount counters and usage, print what was repaired
./bin/upload-worker -once               # one pass of every sweep

# 11. Tests
go test ./...                            # unit tests; integration tests skip without the variables below
TEST_DATABASE_URL="postgres://storage_app:<password>@127.0.0.1:5435/object_storage_test?sslmode=disable" \
TEST_S3_BUCKET=object-storage-it TEST_S3_CREATE_BUCKET=true TEST_CLAMAV_ADDRESS=127.0.0.1:3310 \
TEST_SQS_ENDPOINT=http://127.0.0.1:9324 go test ./tests/integration/ -run 'Ingestion|BulkIngestion' -v
```

The whole flow with uploads, from a client's side, is what the load generator does:

```bash
# Signs in as a developer of the organization; the password comes from the environment
LOADGEN_PASSWORD=... ./bin/ingestion-loadgen -api http://127.0.0.1:8080 -email dev@example.test -org <ORG> -files 500 -concurrency 16 \
  -classes 'small=85:1KiB-256KiB,medium=13:1MiB-8MiB,large=2:20MiB-48MiB' -seed 1 \
  -vars http://127.0.0.1:9201/debug/vars,http://127.0.0.1:9202/debug/vars,http://127.0.0.1:9203/debug/vars \
  -db "$DATABASE_URL" -containers object-storage- -out report.json
```

A run must finish within one access token's lifetime (`AUTH_ACCESS_TOKEN_TTL`); raise it on the local API for long runs. The measurements below were taken in Phase 7, before authentication existed.

`-mode synthetic` uploads nothing. It reads the manifest, claims until a limit holds the job back, then cancels the job and times the cancellation. Use it for manifests of millions of entries. Its numbers are control-plane numbers, never storage throughput.

## Measured on the local stack

One laptop: Intel Core i5-1135G7 (4 cores, 8 threads), 15.7 GB RAM, Windows 11, Docker Desktop (WSL2) with 8 CPUs and 7.6 GiB for containers.

- **Services:** PostgreSQL 17 with default settings (128 MB shared buffers), MinIO, ClamAV 1.4 and ElasticMQ, all in Docker.
- **Binaries:** the API, upload worker, processing worker (`PROCESSING_CONCURRENCY=4`) and outbox worker, natively, with `EVENT_QUEUE=sqs`.
- **Driver:** `ingestion-loadgen`, measuring at the client. It sampled `/debug/vars` of the three servers, PostgreSQL (`pg_stat_database`, `pg_stat_activity`, the outbox and processing backlogs) and `docker stats`.

Upload throughput is bytes actually PUT to MinIO. Published means verified, scanned by ClamAV and promoted.

| Run | Parameters | Upload phase | End to end | Notes |
|---|---|---|---|---|
| Small files | 2,000 files, 1–64 KiB (63.7 MiB), 32 concurrent uploads, seed 7 | 45.8 s: **43.7 files/s**, 1.4 MiB/s | all 2,000 published in 143.4 s (14.2 files/s published) | claim p50/p95 219/545 ms, complete 229/686 ms, PUT 49/121 ms; 0 errors; peak backlog 1,102 awaiting processing |
| Two tenants at once | 2 × 1,000 small files, 32 concurrent uploads each, seed 11 | 32.3 s each: 31.0 files/s each, **62 files/s together** | both COMPLETED in 127.9 s | 0 errors |
| Mixed sizes | 500 files: 428 of 1–256 KiB, 64 of 1–8 MiB, 8 of 20–48 MiB (633 MiB), 16 concurrent, seed 1 | 38.5 s: 13.0 files/s, **16.5 MiB/s**; 492 SINGLE, 8 MULTIPART | all 500 published in 116.4 s | ClamAV CPU mean 188%, peak 372% |
| Synthetic, no uploads | 1,000,000 entries, staged manifest of 160.1 MiB, seed 3 | none: claims granted 32, then held by `job_slots` | manifest read in 78.2 s (**12,794 lines/s**); cancelling all 1,000,000 entries took 475.6 s (≈2,100/s) | PostgreSQL CPU mean 89%, database +865 MiB, cache hit ratio 0.46 |
| Inline manifest (integration test) | 100,000 entries, 14 MiB, batches of 1,000 | n/a | 4.4 s, 22,755 lines/s | heap in use +5 MiB |
| Parser (unit test) | 1,000,000 lines, about 100 MB, no database | n/a | n/a | heap in use grew by at most 240 KiB |

Memory (peak heap in use):

| Process | Peak heap in use |
|---|---|
| API | 5–11.2 MiB |
| Upload worker | 5.9–8.0 MiB, including while reading the 160 MiB manifest |
| Processing worker | 4.7–8.1 MiB |
| Load generator | 17–23 MiB in upload runs; 201 MiB in the million-entry run, where it holds the million-file plan |

Where the time goes:

- **Small files, one tenant: PostgreSQL row locks.** PostgreSQL CPU averages 73% while up to 30 queries wait on the tenant's usage row and the job row: every entry change of a tenant and of a job updates them in its transaction. A second tenant adds throughput (62 against 43.7 files/s) because it has rows of its own.
- **First measured, the global slot row was the hot spot.** It was 28 files/s, with up to 18 sessions waiting on the one `ingestion_capacity` row that every slot change updated. That counter is gone (slots are counted from the entries), and changes that leave the tenant usage or dataset unchanged no longer lock them.
- **End to end: processing.** Publication trails uploads: up to 1,102 entries waited for processing, and the oldest processing job waited 55–75 s. For large files ClamAV is busy (mean 188% CPU). The processing backlog limit (`INGESTION_TENANT_PROCESSING_BACKLOG`, 2,000 by default) is what holds uploads back when it grows.
- **Not the Go processes.** Their heaps stay under 12 MiB; storage PUTs take 49–90 ms at the median.

To reproduce, run the API and workers as in "Running it locally", then:

```bash
./bin/ingestion-loadgen -files 2000 -concurrency 32 -seed 7 -classes 'small=100:1KiB-64KiB' -wait 20m \
  -vars http://127.0.0.1:9201/debug/vars,http://127.0.0.1:9202/debug/vars,http://127.0.0.1:9203/debug/vars \
  -db "$DATABASE_URL" -containers object-storage- -out small.json
./bin/ingestion-loadgen -files 500 -concurrency 16 -seed 1 -out mixed.json
./bin/ingestion-loadgen -mode synthetic -files 1000000 -seed 3 -classes 'small=100:1KiB-64KiB' -claim 100 -out synthetic.json
```

## Limitations

- **A tenant's entry changes serialize on its usage row,** and a job's on its job row, because counters are kept in the same transaction as every change. That costs a ceiling per tenant: about 44 small files per second here. More tenants add throughput.
  - Upgrade path if one tenant needs more: shard the usage row, or append counter deltas and fold them asynchronously. The claim already serializes the only increments the limits depend on.
- **Claims of all tenants take one lock.** A claim is short (20 ms uncontended), and a client should claim in batches (`max` up to `INGESTION_MAX_CLAIM`).
- **Cancelling a very large job is a background task.** Claims stop at once, but the worker cancels at most `INGESTION_WORKER_BATCH` × 20 entries per pass: about 2,100 entries per second here, 476 s for a million. Raise `INGESTION_WORKER_BATCH` for faster bulk cancellation.
- **A staged manifest with a declared SHA-256 is read twice:** once in checkpointed passes, once to hash it. The hashing pass is not under the manifest lease, so a reader that outlives its lease while hashing may hash it again in another worker (harmless).
- **An entry's identity is its manifest position.** A resubmitted manifest must be the same bytes; changing the file list needs a new job.
- **Cancelling keeps what was already published.** Deleting those assets is a separate request (`DELETE /api/v1/assets/{id}`, `docs/lifecycle.md`).
- **The API retry of held processing only makes it due.** The upload worker, which runs processing, requeues it.
- **The fair share counts tenants with pending entries.** A tenant whose entries are all in flight is not counted, so others may briefly hold more than an even split, never more than their tenant limit.
- **Synthetic runs measure only the control plane.** Their claim and manifest numbers are not storage throughput.
- **The numbers above are one laptop with default PostgreSQL settings.** They show where the limits are, not what production hardware would reach.
