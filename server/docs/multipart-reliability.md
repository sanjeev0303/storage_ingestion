# Multipart reliability and reconciliation (local runbook)

## Upload states

| State | Meaning | Next |
|---|---|---|
| PENDING | session saved; provider session may not exist yet | UPLOADING, ABORTING, FAILED, EXPIRED |
| UPLOADING | provider session exists, parts confirmed | COMPLETING, ABORTING, FAILED, EXPIRED |
| COMPLETING | completion claimed; provider outcome may be unknown | COMPLETED, FAILED |
| COMPLETED | object assembled and its size verified at the provider | VERIFIED, FAILED (processing) |
| ABORTING | abort or expiry claimed; provider not yet confirmed | ABORTED, EXPIRED, FAILED |
| ABORTED | client abort confirmed: no provider session, no object | terminal |
| EXPIRED | lifetime passed; provider session and parts removed | terminal |
| FAILED | unrecoverable (reason in `failure_reason`) | terminal |

Every change is a compare-and-set (`UPDATE ... WHERE status = ANY(...)`), so
completion and abort can never both finalize a session, and a stale worker
can never overwrite newer state. No database transaction is held open across
a provider call.

## Recovery rules

| Situation | What happens |
|---|---|
| Initiation timed out | Session stays PENDING without a provider ID. The INITIATE task lists the provider's sessions for that exact key: one is adopted, none means FAILED, several are all aborted and the session fails. Never initiates twice. |
| Initiation succeeded, saving its ID failed | The provider session is aborted on the spot. If that fails too, the INITIATE task finds and adopts it. |
| Part uploaded, confirmation lost | Completion lists the provider's parts and records the missing ones. Confirmations are checked against the provider's list. |
| Completion response lost | Session stays COMPLETING (HTTP 202). The COMPLETE task checks the object first; it completes again only if there is no object and the provider session is still active. |
| Completion succeeded, database update failed | Same COMPLETE task: finds the object, verifies its size, records COMPLETED. |
| Abort succeeded, database update failed | Session stays ABORTING; the ABORT task treats "session not found" as done once no object remains. |
| Active record, provider session gone | Completion answers 409; expiry and abort finish without the provider session. |
| Concurrent completions | One claims COMPLETING; the others answer as retries (200 or 202). The provider completes once. |
| Expired upload | Refuses part URLs and completion; the EXPIRE task aborts at the provider and marks it EXPIRED. |
| Single-request start repeated with the same idempotency key | A `PENDING` session within its lifetime answers 200 with a fresh `upload_url`. A `COMPLETED` or `VERIFIED` one answers 200 with its status and no `upload_url`: there is nothing left to send. Any other (expired, aborted, failed, or past its lifetime) answers **409** `{"code": "session_ended", "error": "upload session is not active: it has ended; start again with a new idempotency key"}`. The SDKs read "not active" as: start again under a new key. |
| Worker crash | Its leases expire; another worker claims the task. A stale worker's writes are refused. |
| Retries | Exponential backoff with jitter up to `RECONCILIATION_MAX_ATTEMPTS`, then EXHAUSTED (kept for an operator, never re-run on its own). Permanent provider errors exhaust at once. See "Exhausted reconciliation tasks" below. |

Discrepancy scans (each bounded per cycle, resuming from a cursor). The two
listing scans run on every place uploads land: the platform's `quarantine/`,
and the staging prefix (`<staging>/<tenant>/`) of every tenant connection
that holds work (active or disabled) and stages uploads in its own bucket.
A connection that cannot be reached is recorded and skipped for that cycle;
it never falls back to the platform, and the others go on.

| Scan | Action |
|---|---|
| Provider sessions with no live owner | aborted (`RECONCILIATION_SESSION_SCAN=provider` lists them by prefix, AWS S3; `database` checks recently finished sessions key by key, MinIO) |
| Staging objects of ABORTED or EXPIRED sessions | deleted, every version of the key on a versioned bucket (FAILED objects are kept as evidence) |
| Part rows of ABORTED, FAILED or EXPIRED sessions | deleted |
| Items with no upload session at all | **reported only**, unless `RECONCILIATION_UNRECORDED_ACTION=delete`; on a tenant's own bucket always reported only, since they may be the customer's |
| Items under a tenant's staging prefix whose key names no upload | reported only (`UNVERIFIABLE_ITEM`) |
| Sessions stuck longer than `RECONCILIATION_STUCK_AFTER` | logged and counted |

Nothing younger than `RECONCILIATION_ORPHAN_MIN_AGE` is touched. Actions taken
are stored in `reconciliation_findings`, with the connection they were taken
on. Each listing scan of each place keeps its progress in `inventory_scans`
(kind `UPLOAD_SESSIONS` or `UPLOAD_OBJECTS`, migration 000016): cursor, pages,
findings, and the sanitized last error, under a lease. A restarted worker
resumes from the saved cursor; a scan that fails five pages in a row ends
`FAILED` and the next one starts over. A session lookup that fails stops the
page: nothing is deleted on a database error.

## Declaring the checksum

The SHA-256 may be declared when the upload starts (`POST /uploads/multipart`,
`checksum_sha256`) or, for a client that hashes a large file while its parts
upload, when it completes (`POST /uploads/{id}/complete`, `checksum_sha256`
next to `parts`). Completion is refused (400) without one at either point, or
with one that differs from the one given at start. A completion's checksum
is recorded once, only while the upload is `UPLOADING`, and before the object
is assembled, so the completion event, the asset and verification always
have it. Verification is the same either way: the stored bytes are hashed
and must match. Bulk ingestion manifests still require a checksum per entry;
single-request uploads still declare it at creation.

## Exhausted reconciliation tasks

An EXHAUSTED task leaves its upload where it stopped, COMPLETING or ABORTING,
with its quota still reserved, and no new task is made for that upload. Find
them, fix the cause (credentials, a bucket policy, a provider outage), then
requeue each one:

```sql
SELECT id, tenant_id, upload_id, operation, attempt_count, last_error, updated_at
FROM reconciliation_tasks WHERE state = 'EXHAUSTED' ORDER BY updated_at;
```

```powershell
.\bin\reconciliation-worker.exe -requeue <task id>
```

`-requeue` moves only an EXHAUSTED task back to PENDING, with its attempts
reset to 0 and due at once; its `last_error` stays until the next attempt.
Anything else (an unknown ID, a task already PENDING or SUCCEEDED) is refused
and nothing changes. A running worker picks the task up on its next cycle.

## Client behaviour (console)

The console's upload engine (`client/src/features/uploads/engine`) keeps the
work it has done whatever the network does. The SDKs do the same where it
applies (docs/sdks.md, "Bad networks").

| Situation | What the console does |
|---|---|
| Offline | The browser's `offline` event, or any request that cannot get through, triggers a check against the API (`GET /auth/me`; any answer counts). If the API cannot be reached, every upload pauses at once: requests in flight are aborted, finished parts are kept, and the row says "Paused: no connection" with the bytes kept. No retries are spent while offline. |
| Back online | The `online` event is only a hint: uploads go on once the API answers, checked again every 2 s backing off to 30 s, or at once with "Check connection". |
| By hand | Pause stops the parts in flight and keeps the rest; Resume goes on; Cancel aborts the upload at the API. Once `/complete` has been sent, Cancel is no longer offered: the server may publish the file whatever the console does, so the row follows the real outcome instead of claiming Cancelled. Retry and Start again start one attempt however often they are pressed. |
| Organization | Each upload is pinned to the organization it started in (a resume uses the record's organization). Every request it makes, including the abort, names that organization, so switching organization mid-upload does not move it. |
| Checking stopped | Status checks stop after their limit, whether the upload is finishing, verifying or processing; "Check again" starts them once more. It does nothing while checks are still running, so two loops never run side by side. |
| Leaving the page | Leaving the uploads screen stops its uploads as closing the tab would: nothing is aborted at the API, a multipart upload keeps its resume record and can be continued from Unfinished uploads, and nothing keeps uploading unseen. Removing a queued file cancels it, so a running queue cannot start it. |
| Stalled part | No upload progress for 20 s stops the PUT and sends the part again, with one fewer part in flight. There is no whole-request timeout, so a slow link that keeps moving is never cut off. |
| Slow or fast link | Parts in flight start at 4 and follow the measured speed, between 1 and 6 (a browser's connections to one host): fewer when a part would take over a minute, more when under 15 s. New uploads are sized from the page's measured speed, the sum of the uploads sending at the time (they share one link): 5 MiB parts below 256 KiB/s, 16 MiB above 8 MiB/s, else 8 MiB, always within 5 MiB and 10,000 parts. The row shows speed, time left and "Slow connection" below 256 KiB/s. |
| Retries | Network errors, 408, 429 and 5xx: up to 6 tries with full-jitter backoff (0 to 1 s, 2 s, 4 s ... capped at 30 s); an API `Retry-After` wins. An expired part URL (403 saying the request or token expired, the signature is outside its time frame, or the clock is skewed) gets a fresh URL for that part from `parts/urls` and goes again at once. A plain `AccessDenied` is a refusal and fails the upload. Other 4xx answers are not retried. Storage's own `Retry-After` is not readable from the browser's upload request, so storage 429/503 use the backoff. |
| Closed tab or crash | The resume record in IndexedDB holds the confirmed parts and, from the moment each PUT finishes, the parts sent but not yet confirmed. On resume, after the person chooses the file again (a browser cannot reopen it on its own; the name, size and SHA-256 of the first MiB must match), the record's unconfirmed parts are confirmed first; the API checks them against the provider's list, and a refusal (400) sends them again. Only the missing parts are sent. The whole-file SHA-256 is read again only if it had not finished before the tab closed. |
| Completion | Unchanged: completion sends every part and the file's SHA-256, and the upload counts as stored only when the API answers. |

The API keeps no list of uploaded parts a client can read, so a resume
skips only the parts this browser recorded.

## Environment

```
DATABASE_URL=postgresql://storage_app:<POSTGRES_PASSWORD>@127.0.0.1:5435/object_storage?sslmode=disable
S3_REGION=ap-southeast-1
S3_ENDPOINT=http://127.0.0.1:9000
S3_ACCESS_KEY_ID=minioadmin
S3_SECRET_ACCESS_KEY=minioadmin
S3_FORCE_PATH_STYLE=true
S3_BUCKET=quarantine-bucket
APP_ENV=development
HTTP_PORT=8080
UPLOAD_MAX_SIZE_BYTES=5368709120      # up to the 5 TiB S3 object limit
UPLOAD_SESSION_TTL_SECONDS=1800
UPLOAD_URL_TTL_SECONDS=900

RECONCILIATION_SESSION_SCAN=database  # MinIO; use provider on AWS S3
RECONCILIATION_UNRECORDED_ACTION=report
RECONCILIATION_ORPHAN_MIN_AGE=24h
RECONCILIATION_STUCK_AFTER=1h
RECONCILIATION_SESSION_LOOKBACK=168h
RECONCILIATION_SCAN_PAGE_SIZE=100
RECONCILIATION_SCAN_PAGES=5
RECONCILIATION_POLL_INTERVAL=15s
RECONCILIATION_BATCH_SIZE=10
RECONCILIATION_LEASE_DURATION=10m
RECONCILIATION_GRACE_PERIOD=2m
RECONCILIATION_MAX_ATTEMPTS=8
RECONCILIATION_BACKOFF_BASE=30s
RECONCILIATION_BACKOFF_MAX=30m
METRICS_ADDR=127.0.0.1:9101           # optional: /debug/vars
```

If the bucket is shared with another environment (another database),
keep `RECONCILIATION_UNRECORDED_ACTION=report`: to this database, the other
environment's uploads look unowned.

## Commands (PowerShell, from `server/`)

```powershell
docker compose up -d                                   # Postgres 17 + MinIO
.\scripts\migrate.ps1                                  # migrate object_storage
.\scripts\migrate.ps1 -Database object_storage_test -Create

go test ./...                                          # unit tests

# integration tests against the local stack
$env:TEST_DATABASE_URL = "postgresql://storage_app:<POSTGRES_PASSWORD>@127.0.0.1:5435/object_storage_test?sslmode=disable"
$env:TEST_S3_BUCKET = "object-storage-it"; $env:TEST_S3_CREATE_BUCKET = "true"
go test -count=1 -v ./tests/integration/

go build -o .\bin\api.exe .\cmd\api
go build -o .\bin\reconciliation-worker.exe .\cmd\reconciliation-worker
.\bin\api.exe
.\bin\reconciliation-worker.exe                        # continuous
.\bin\reconciliation-worker.exe -once -dry-run         # report only, changes nothing
.\bin\reconciliation-worker.exe -once                  # one real cycle, JSON report on stdout
.\bin\reconciliation-worker.exe -requeue <task id>     # retry an EXHAUSTED task once its cause is fixed
```

## Example requests

```powershell
$api = "http://localhost:8080/api/v1/uploads"
$start = Invoke-RestMethod -Method Post "$api/multipart" -ContentType application/json -Body (@{
  file_name = "video.mp4"; content_type = "video/mp4"; size_bytes = 5242890
  checksum_sha256 = "<sha256 of the file>"; idempotency_key = "client-key-1" } | ConvertTo-Json)
$id = $start.upload_id

$urls = Invoke-RestMethod -Method Post "$api/$id/parts/urls" -ContentType application/json -Body '{"part_numbers":[1,2]}'
# PUT each part's bytes to $urls.parts[i].url and keep the ETag response header

Invoke-RestMethod -Method Post "$api/$id/parts/confirm" -ContentType application/json -Body '{"parts":[{"part_number":1,"etag":"\"<etag1>\"","size_bytes":5242880},{"part_number":2,"etag":"\"<etag2>\"","size_bytes":10}]}'
Invoke-RestMethod -Method Post "$api/$id/complete" -ContentType application/json -Body '{"parts":[{"part_number":1,"etag":"\"<etag1>\""},{"part_number":2,"etag":"\"<etag2>\""}]}'
Invoke-RestMethod -Method Post "$api/$id/abort"
```

## Reproducible failure recovery

`TestLocalStackRecoversLostCompletionAndLeftoverSessions` (tests/integration)
uploads real parts to MinIO through presigned URLs, completes them through a
provider wrapper that completes and then reports a timeout, leaves an aborted
upload with a surviving provider session, and runs one reconciliation cycle:
the upload ends COMPLETED with a single provider completion and the leftover
session is aborted and recorded. `TestLocalStackCleansUpInitiationWhoseIDWasNotSaved`
covers initiation whose ID could not be saved.

## Migration 000016

`000016_reconciliation_scans.sql` is additive: `inventory_scans.kind`
(existing rows become `OBJECTS`, the cleanup worker's scans) with the
running-scan index extended by it, and
`reconciliation_findings.storage_connection_id` (existing rows: `platform`).
Apply it with `scripts/migrate.ps1`. To roll back, stop the reconciliation
workers, then:

```sql
BEGIN;
DELETE FROM inventory_scans WHERE kind <> 'OBJECTS';
DROP INDEX IF EXISTS ux_inventory_scans_running;
ALTER TABLE inventory_scans DROP COLUMN kind;
CREATE UNIQUE INDEX ux_inventory_scans_running
    ON inventory_scans (storage_connection_id, bucket, prefix) WHERE status = 'RUNNING';
ALTER TABLE reconciliation_findings DROP COLUMN storage_connection_id;
DELETE FROM schema_migrations WHERE version = '000016_reconciliation_scans.sql';
COMMIT;
```

Older workers then scan the platform's `quarantine/` only, as before.

## Provider limitations

- **MinIO** lists multipart sessions only for an exact object key, not by
  prefix. Use `RECONCILIATION_SESSION_SCAN=database`; it checks finished
  sessions from the last `RECONCILIATION_SESSION_LOOKBACK`. A provider
  session with no database row at all cannot be found this way: add a bucket
  lifecycle rule (AbortIncompleteMultipartUpload) as the backstop. The same
  holds for a tenant connection on MinIO or another provider that lists
  sessions only by exact key: the provider scan finds nothing there, and the
  database scan checks each finished session through its own connection.
- GCS and Azure have no provider sessions to list: their adapters list the
  markers of uploads in progress, which is what the provider scan sees.
