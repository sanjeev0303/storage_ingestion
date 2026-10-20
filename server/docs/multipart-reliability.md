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
| Worker crash | Its leases expire; another worker claims the task. A stale worker's writes are refused. |
| Retries | Exponential backoff with jitter up to `RECONCILIATION_MAX_ATTEMPTS`, then EXHAUSTED (kept for an operator, never re-run). Permanent provider errors exhaust at once. |

Discrepancy scans (each bounded per cycle, resuming from a cursor):

| Scan | Action |
|---|---|
| Provider sessions with no live owner | aborted (`RECONCILIATION_SESSION_SCAN=provider` lists them by prefix, AWS S3; `database` checks recently finished sessions key by key, MinIO) |
| Quarantine objects of ABORTED or EXPIRED sessions | deleted (FAILED objects are kept as evidence) |
| Part rows of ABORTED, FAILED or EXPIRED sessions | deleted |
| Items with no upload session at all | **reported only**, unless `RECONCILIATION_UNRECORDED_ACTION=delete` |
| Sessions stuck longer than `RECONCILIATION_STUCK_AFTER` | logged and counted |

Nothing younger than `RECONCILIATION_ORPHAN_MIN_AGE` is touched. Actions taken
are stored in `reconciliation_findings`.

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

## Provider limitations

- **MinIO** lists multipart sessions only for an exact object key, not by
  prefix. Use `RECONCILIATION_SESSION_SCAN=database`; it checks finished
  sessions from the last `RECONCILIATION_SESSION_LOOKBACK`. A provider
  session with no database row at all cannot be found this way: add a bucket
  lifecycle rule (AbortIncompleteMultipartUpload) as the backstop.
- Scan cursors live in the worker process; after a restart a scan starts over.
