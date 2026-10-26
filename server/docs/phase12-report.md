# Phase 12 report: reliability closure, BYOS reconciliation, large-object promotion, ingestion recovery

Baseline: `bd9b7c1` on `hardening-reproducibility-byos-ingestion` (Go 1.25.0). Every change below is uncommitted in the working tree. Everything ran locally against the Docker Compose services: PostgreSQL 16 on `127.0.0.1:5435`, MinIO, ClamAV, ElasticMQ, fake-gcs-server and Azurite. No test, migration or cleanup touched the Neon database in `.env`. No live AWS, R2, GCS or Azure account was used.

## 1. Findings confirmed in the source

Each finding of `docs/release-candidate.md` §3.2 in scope was re-read in the code before it was changed.

| ID | What the source showed | Status |
|---|---|---|
| R1 | Processing jobs leased for 15 min and never renewed; a crashed worker's jobs waited out the lease. The old worker also leased a whole claimed batch, not only the jobs it was running | fixed |
| R2 | Ingestion's per-entry calls (claims, part URLs, confirmation, heartbeat, complete) counted against `upload_finish` (600/min, shared with interactive uploads); `GET` of one job or entry did too | fixed |
| R5 | `CancelWaiting` took `ORDER BY position LIMIT n` with a state filter, from position 0 every batch, so each batch read past every entry already cancelled | fixed |
| D2 | `UpdateConnection` sent an `ACTIVE` connection to `PENDING` on any endpoint, region or path-style change; `Enable` moved `DISABLED` to `PENDING` before testing. `PENDING` holds no work, so every existing upload and asset on the connection stopped resolving | fixed |
| S3 | `Disable` read the default, then changed the status in a second statement; a concurrent `SetDefault` could leave a disabled default | fixed |
| D4 | Staging sources, promotion copies, purged quarantine objects and orphan objects were deleted by key only; on a versioned bucket that only adds a delete marker. Lifecycle also skipped a key whose `HEAD` answered not-found, leaving noncurrent versions behind | fixed |
| D11 | S3-family promotion was one `CopyObject` (5 GiB limit, permanent `InvalidRequest` above it). Streaming transfers between connections used fixed parts: S3 at most 10,000 × 16 MiB (156 GiB), Azure at most 50,000 blocks × 16 MiB (781 GiB). `UPLOAD_MAX_SIZE_BYTES` had no upper bound at configuration | fixed |
| D7 | Reconciliation's listing scans (orphan provider sessions, objects of aborted or expired uploads) listed only the platform's `quarantine/`, from cursors kept in memory. The cleanup worker deliberately leaves aborted and expired staging objects to reconciliation, so on tenant buckets nobody removed them | fixed |
| H1 | `/health` answered 200 without PostgreSQL; eight metrics listeners were `http.ListenAndServe` goroutines never shut down | fixed |
| P1 | `upload-worker`, `dataset-worker` and `reconciliation-worker` hard-coded `assets`; the API fell back to `assets` silently if the delivery configuration failed to load | fixed |
| M1 | `migrate.ps1` recorded each version in a second psql call, took no lock, and compared `-Baseline` as strings (`"000010" -le "6"` is `True`). Testing found a fourth defect: two runs creating `schema_migrations` at once collide in `pg_type` | fixed |
| D9 | `processing.recordStorage` fell back to the platform bucket when the asset was missing. Unreachable: `quarantine_records.asset_id` cascades with its asset (000007, made tenant-composite in 000012), assets are tombstoned rather than deleted, and every asset carries a `NOT NULL` storage binding (000015) | fallback removed; a missing asset is an error |
| Stale docs | Integration test comments named old migrations; `DATABASE_URL_UNPOOLED` in `.env.example` was read by nothing | fixed |

Not in this milestone and still open: D3 (STS session expiry; needs AWS), D13 (write probe not the default), D8 (catalog capabilities not per tenant), the remaining 26 package-only stub files (`s3/copy.go` now holds the multipart copy), and `.gitattributes`.

## 2. Files changed and migrations added

Migration **`000016_reconciliation_scans.sql`** (additive): `inventory_scans.kind` (`OBJECTS` for existing rows) with the running-scan unique index extended by it, and `reconciliation_findings.storage_connection_id` (`platform` for existing rows). The rollback is in `docs/multipart-reliability.md`. It was applied with `scripts/migrate.ps1` to the local `object_storage_test`, `object_storage_migfresh` and `object_storage_ingest_bench` only.

| Area | Files |
|---|---|
| R1 leases | `internal/service/processing/service.go`, `internal/service/promotion/service.go`, `internal/infrastructure/database/postgres/processing_job_repository.go`, `internal/repository/memory/processing_job_repository.go`, `internal/interfaces/processing_job_repository.go`, `internal/platform/config/config.go`, `cmd/processing-worker/main.go`; tests `internal/service/processing/lease_test.go` (new: slow scans, lost and reclaimed leases, earlier claims, renewal ending, the duration cap, queued jobs), `internal/service/promotion/service_test.go` (new), `intake_test.go`, `service_test.go`, `tests/integration/postgres_processing_test.go`, `tests/integration/events_test.go` |
| R2 limits | `internal/platform/config/security.go`, `internal/server/router.go`; tests `internal/server/router_test.go` (new), `internal/platform/config/security_test.go`, `tests/integration/security_http_test.go` |
| R5 cancellation | `internal/service/ingestion/sweeps.go`, `internal/service/ingestion/service.go`, `internal/infrastructure/database/postgres/batch_accounting.go`, `internal/interfaces/batch_repository.go`; test `tests/integration/ingestion_test.go` |
| D2, S3 connections | `internal/service/storageconn/service.go`, `internal/service/storageconn/test.go`, `internal/handler/http/storage_connection_handler.go`, `internal/infrastructure/database/postgres/storage_connection_repository.go`, `internal/repository/memory/storage_connection_repository.go`, `internal/interfaces/storage_provider.go`; tests `internal/service/storageconn/service_test.go`, `tests/integration/storage_connection_test.go` |
| D4 versions | `internal/infrastructure/storage/{s3/object_storage.go,s3/versioning.go,gcs/gcs.go,azure/azure.go,memory/object_storage.go}`, `internal/interfaces/{object_storage.go,inventory_reader.go}`, `internal/service/asset/lifecycle.go`, `internal/service/storageconn/test.go`; tests `internal/service/asset/lifecycle_test.go`, `tests/integration/lifecycle_test.go`, `gcs/gcs_test.go`, `azure/azure_test.go` |
| D11 promotion | `internal/infrastructure/storage/s3/copy.go` (was an empty stub), `s3/object_storage.go`, `s3/connection.go`, `azure/azure.go`, `internal/platform/config/config.go`; tests `internal/infrastructure/storage/s3/copy_test.go` (new), `tests/integration/content_security_test.go`, `internal/platform/config/config_test.go` |
| D7 reconciliation | `internal/service/upload/multipart_scan.go`, `internal/service/reconciliation/service.go`, `internal/service/cleanup/service.go`, `internal/infrastructure/database/postgres/{inventory_repository.go,reconciliation_repository.go}`, `internal/repository/memory/lifecycle_repository.go`, `internal/model/{lifecycle.go,reconciliation_task.go}`, `cmd/reconciliation-worker/main.go`, `migrations/000016_reconciliation_scans.sql` (new); tests `internal/service/reconciliation/connections_test.go` (new), `service_test.go`, `cleanup/service_test.go`, `tests/integration/storage_connection_test.go` |
| Operations | `internal/handler/http/health_handler.go`, `internal/server/router.go`, `internal/platform/metrics/metrics.go`, `internal/platform/config/{config.go,lifecycle.go}`, `cmd/{api,cleanup-worker,compaction-worker,dataset-worker,outbox-worker,processing-worker,reconciliation-worker,upload-worker}/main.go`, `scripts/migrate.ps1`, `internal/service/processing/service.go` (D9); tests `internal/handler/http/health_handler_test.go` (new), `internal/platform/metrics/serve_test.go` (new), `internal/platform/config/config_test.go`, `internal/service/processing/service_test.go` |
| Docs and examples | `.env.example`, `docs/{content-security,datasets,lifecycle,multipart-reliability,security,storage-connections}.md`, this report; stale migration comments in seven `tests/integration` files |

76 tracked files changed (2,252 insertions, 414 deletions) and 9 files added, this report included.

## 3. Tests run and results

| Check | Result |
|---|---|
| `gofmt -l` on every Go file, line endings normalized to LF (the checkout is CRLF) | clean on the final tree (`internal/platform/config/security.go` needed realigning after the new limit) |
| `go vet ./...` | clean |
| `go build` of each of the 14 commands | all build |
| `go test -count=1 ./...` with every local service configured (before the queued-lease change of §6) | 757 passed, 5 skipped, 1 failed. The failure was `TestStaleEventsAreNotReplayed`, which still finished a job with the bare owner `it`; after R1 the claim's owner is `it#1`, so the write was rightly refused. The test now uses the claim's token and passes |
| `go test -race -count=1 -p 1 ./...` in the `storage-race` container (Go 1.25.0, Linux), on the final tree | **759 passed, 5 skipped, 0 failed, 0 data races**, 13.5 min (baseline `bd9b7c1`: 729 passed, 2 skipped) |
| `tests/e2e` (`TestTwoTenantsThroughTheRunningStack`) | passed on the final tree, 80.9 s (73.9 s before the queued-lease change); no secret in output, metrics, responses or 55 tables |
| `scripts/migrate.ps1` on new local databases | fresh migrate of all 16 files; no-op re-run; three concurrent runs (one applied, two waited and skipped every file); a failing file rolled back and unrecorded; `-Baseline 6` marks exactly 000001–000006; `-Baseline abc` refused |

The five skips: `TestAzuriteDeleteRemovesEveryVersion` and `TestEmulatorDeleteRemovesEveryGeneration` (the emulators cannot show version deletion, see §10), `TestMinIOLargeCopyAboveFiveGiB` (opt-in, run separately, §5), `TestTwoTenantsThroughTheRunningStack` (needs the e2e environment, run separately) and `TestProviderListingsArePaginated/prefix_listing_of_sessions` (MinIO cannot list sessions by prefix).

Under heavy parallel load, `TestPrivateAddressIsRefusedAtConnectTime` timed out once resolving `localhost`; it passes alone in 1.35 s. `TestLocalStackDeletionAndHoldReleaseThroughTheEventPipeline` failed once in a busy batch earlier in the phase and passed 3 of 3 alone. Neither involves code changed here.

## 4. BYOS reconciliation coverage by provider

Reconciliation's two listing scans now run on every place uploads land. That is the platform's `quarantine/`, plus the `<staging>/<tenant>/` prefix of every tenant connection that is `ACTIVE` or `DISABLED` and stages in its own bucket; `PENDING` connections, and those staging on the platform, are not listed. Each scan of each place keeps its own cursor, page count, findings and sanitized last error in `inventory_scans`, under a lease. A restart resumes from the saved cursor; five failed pages in a row end the scan `FAILED` and the next one starts over.

A connection that cannot be resolved, or whose listing fails, has its failure recorded and is skipped for that cycle, while the others go on; it never falls back to the platform. On a tenant's bucket, anything no upload session names is only reported, whatever `RECONCILIATION_UNRECORDED_ACTION` says, as is any key that names no upload (`UNVERIFIABLE_ITEM`). A session lookup that fails stops the page, so nothing is deleted on a database error. Staging objects of `ABORTED` and `EXPIRED` uploads are deleted with every version of the key. The cleanup worker still covers ready objects and other staging objects on every connection, as before.

| Provider | Staging objects | Orphan multipart sessions | Version deletion |
|---|---|---|---|
| AWS S3 | listed (`ListObjectsV2`) | listed by prefix (`ListMultipartUploads`) | `ListObjectVersions`, exact key; **not run against AWS** |
| MinIO / S3-compatible | listed; tested | MinIO lists sessions only by exact key: the provider scan finds none, so use `RECONCILIATION_SESSION_SCAN=database`, which checks each finished session through its own connection | **verified on MinIO** (`TestProviderVersionedDelete`) |
| Cloudflare R2 | S3 API; **not run** | S3 API, by prefix; **not run** | R2 keeps no versions: no delete marker comes back, so a delete stays a plain delete |
| GCS | listed | the adapter lists its own part markers | generations listed and deleted; **unverified**: fake-gcs-server lists them but answers 404 to deleting one once the object is gone |
| Azure Blob | listed | the adapter lists its own block markers | versions listed and deleted; **unverified**: Azurite keeps no blob versions (the listing that includes versions does work there) |

Tests: `internal/service/reconciliation/connections_test.go`. They cover several connections at once (active, disabled, pending, staging on the platform), pagination and resuming after a restart, unrecorded and unverifiable items, an unreachable connection and a failing listing (with sanitized errors), the `FAILED` end after five failures, a database outage that deletes nothing, a failed delete retried next cycle, and versioned objects. `TestReconciliationScansKeepTheirOwnProgressInPostgres` covers the same against PostgreSQL.

## 5. Large-object promotion: limits and measurements

| Limit | Where it is enforced |
|---|---|
| `UPLOAD_MAX_SIZE_BYTES` ≤ 5 TiB, the largest S3-family or GCS object | configuration load: the process does not start |
| S3 `CopyObject` ≤ 5 GiB | above it, a multipart copy |
| S3 copy parts 5 MiB–5 GiB, ≤ 10,000 | 128 MiB parts, raised to fit 10,000 (525 MiB at 5 TiB); above 5 TiB refused before any request |
| S3 streaming between connections, ≤ 10,000 parts | part size grows to fit; memory is one part (525 MiB at 5 TiB) |
| Azure streaming, ≤ 50,000 blocks | block size grows to fit |
| GCS copy | provider rewrite, any size to 5 TiB |
| Malware scan: clamd `StreamMaxLength` (25 MiB default, 4 GiB at most) | larger objects are `HELD` unscanned, never promoted |

The multipart copy pins every part to the source ETag read first, so a replaced source fails the copy. It runs four parts at a time, and a failed or cancelled copy aborts its upload. A retry first aborts any upload left on the asset's derived destination key, so the provider's own listing is the persisted record of a copy in progress, with no new table. A lost completion answer is caught by the size and SHA-256 check that runs before publication.

Tests:
- `internal/infrastructure/storage/s3/copy_test.go`, against MinIO: copy in parts with a crashed attempt's upload left on the key; a source replaced mid-copy (precondition failure, nothing written, upload aborted); a copy cancelled during its first part (upload aborted); part-size limits.
- `TestLocalStackPromotesAboveTheSingleCopyLimitInParts`: upload, verification, ClamAV scan, a copy in three parts, checksum check and publication, with the limit lowered to 6 MiB.
- The existing promotion tests cover a failed copy, a lost copy answer, a corrupted copy, a leftover copy and a swapped source.

Measured on MinIO (`TEST_S3_LARGE_COPY_BYTES=5435817984`, 5.06 GiB, Docker on a laptop disk):

| Step | Time |
|---|---|
| Streaming write of the source (64 MiB parts) | 156.7 s (an earlier run: 331.6 s) |
| One `CopyObject` of 5.06 GiB | **accepted by MinIO**: the 5 GiB limit is AWS's and could not be reproduced locally |
| Multipart copy, 41 parts of 128 MiB | 65.6 s, 79 MiB/s |
| SHA-256 of the copy, streamed, as promotion checks it | 141.1 s, 37 MiB/s |

The first measured run used 512 MiB parts and failed with MinIO `IncompleteBody`, a part copy cut short on the slow virtual disk. That is why parts are 128 MiB. Multi-terabyte objects are **not** claimed: nothing above 5.06 GiB was promoted. With malware scanning on, nothing above clamd's limit (at most 4 GiB) is promoted at all.

## 6. Processing recovery before and after lease renewal

The same 300-file mixed load each time (255 files of 1 KiB–256 KiB, 39 of 1–8 MiB, 6 of 20–48 MiB, seed 7, 16 concurrent) ran on the local stack (API, upload, processing with concurrency 4, outbox; SQS on ElasticMQ; ClamAV). The processing worker was killed once 60 files were published and restarted 5 s later.

| | `bd9b7c1` processing worker | Phase 12 |
|---|---|---|
| Lease | 15 min, never renewed | 1 min, renewed every 20 s from the claim |
| Jobs leased by the killed worker | 11 (a claimed batch of up to 10, plus Intake); 6 unfinished | 7; 6 unfinished |
| Kill to completion of those jobs | **898 s** each | **65–68 s** |
| Kill to the last publication | 908 s | 68 s |
| End to end | 963.6 s | 97.0 s |
| Published | 300 of 300 | 300 of 300 |

A worker claims up to `PROCESSING_BATCH_SIZE` (10) jobs and runs `PROCESSING_CONCURRENCY` (4) at a time. Writing this report found that renewal started only when a job ran, so a job waiting for a slot could lose its 1-minute lease in the queue and spend an attempt it never used. Renewal now starts at the claim (`TestQueuedJobsKeepTheirLeasesWhileTheyWait`), and the Phase 12 column above was measured after that change. An earlier run, without it, recovered its 2 jobs in 67 and 70 s.

## 7. Ingestion throughput and cancellation

**Rate limits (R2).** The RC's run of 1,000 files of 1–64 KiB, 32 concurrent, was repeated with every limit at its default. Per-entry calls now count against `ingestion_entry` (3,000/min per organization: 50/s, above the 35–40 files/s one tenant's pipeline sustains and inside the 6,000/min organization quota); interactive `upload_finish` stays 600/min.

| | RC (`bd9b7c1`) | Phase 12 |
|---|---|---|
| Requests answered 429 | 1,170 of 2,170 completions, 41 job polls, 7 claims (33% of requests) | none |
| Upload phase | 61.7 s, 16.2 files/s | 32.8 s, 30.5 files/s |
| End to end, all published | 94.3 s, 11.0 files/s | 87.6 s, 11.9 files/s (processing-bound) |

`TestIngestionAndInteractiveUploadsHaveSeparateLimits` shows the two budgets apart: a spent interactive budget lets ingestion through, and each limit still holds.

**Cancellation (R5).** Synthetic manifests (`-mode synthetic`, 1–64 KiB entries, seed 3), each in a new organization: read, scheduled until `job_slots` held the job, then cancelled.

| | 100k, `bd9b7c1` | 100k, Phase 12 | 1M, Phase 12 | 1M, RC (`bd9b7c1`, not re-run) |
|---|---|---|---|---|
| Manifest read | 14.3 s | 20.9 s | 211.9 s, 4,719 lines/s | 211.7 s |
| Cancellation | 27.0 s, ≈3,700/s | 28.8 s, ≈3,480/s | **347.7 s, ≈2,880/s** | **1,435.7 s**, ≈700/s, falling from ≈1,280/s to ≈300/s |
| Correct end | `CANCELLED`, 100,000 cancelled | `CANCELLED`, 100,000 cancelled | `CANCELLED`, 1,000,000 cancelled | `CANCELLED`, 1,000,000 cancelled |
| PostgreSQL CPU (peak / mean) | 167% / 75% | 128% / 66% | 319% / 100% | 386% peak / 103% mean |
| PostgreSQL memory peak | 1,255 MiB | 456 MiB | 1,362 MiB | — |
| Upload worker peak heap | 8.8 MiB | 9.1 MiB | 9.6 MiB | — |
| Database growth | 31 MiB | 126 MiB | 914 MiB | 927 MiB |

At 100k the two are equal: the cost of re-reading cancelled entries shows only with size. At 1M, cancellation is 4.1× faster and its rate holds instead of falling. The old code was not re-run at 1M in this session, to spare a disk 96% full; its figure is the RC's, measured on this machine with the same cancellation code. The cursor is held in the process; after a restart, the first pass reads from the start once. Claims stop the moment cancellation is accepted, in-flight entries are cancelled and their multipart uploads aborted, and usage is settled in the same transaction as each batch, so cancellation never exceeds quota (unchanged, and covered by the existing ingestion tests).

## 8. Connection lifecycle and security regression results

- `TestDisableAndSetDefaultNeverLeaveADisabledDefault`: 40 rounds of `Disable` racing `SetDefault` on PostgreSQL. Exactly one wins each round and the default is never disabled. `Disable` locks the organization's row, the row `SetDefault` updates.
- `TestConnectionChangesKeepExistingWorkReachable`, against MinIO over HTTP: an edit to an equivalent endpoint, a refused edit to an unreachable one, a credential rotation, and a disable/enable cycle, while four readers resolved and read a published asset (2,264 reads, none failed). An upload granted before the changes was completed and published on its bound connection afterwards.
- Unit tests: an edit is tested with the stored credentials and a `HEAD` of an object the connection holds; it is refused with 422 and the test result, storing nothing. Renames need no test. `PENDING` connections change untested. `Enable` stays `DISABLED` on a failing test and goes straight to `ACTIVE` on a passing one.
- Existing suites pass unchanged: `TestBringYourOwnStorageEndToEnd`, credential and tenant-isolation tests, and the e2e search for secrets in every output, metric, response and table.

What an edit means for existing work is documented in `docs/storage-connections.md`. Records are bound to the connection, not to a copy of its settings, so after an edit they are read through the new settings. There is one configuration per connection and no versioned copy of the old one, so an edit is accepted only if the new settings provably reach the same bucket; otherwise it is refused and the old settings stay. Other processes pick an edit up when their connection cache expires (`STORAGE_CONNECTION_CACHE_TTL`, 10 s), because cached clients are keyed by revision.

## 9. Operational issues fixed

- **Readiness.** `/health` is liveness and checks no dependency. `/ready` pings PostgreSQL within 2 s and answers 503 without the cause. The API serves both on its HTTP port, and every worker serves both, with `/debug/vars`, on `METRICS_ADDR` (`docs/security.md`).
- **Shutdown.** `metrics.Serve` returns a stop function that each command defers: on SIGINT or SIGTERM the listener stops with the worker, letting a request in progress finish (`TestServeAnswersLivenessReadinessAndMetricsUntilStopped`). The worker loops and consumers already stopped on the signal context.
- **Prefixes.** `config.PromotionPrefix()` is the one place `PROMOTION_PREFIX` is read (trimmed of slashes, `assets` by default). Every loader and command uses it (`TestEveryLoaderAgreesOnThePromotionPrefix`).
- **`migrate.ps1`.** Each run is one psql session holding `pg_advisory_lock`; `schema_migrations` is created under the same lock; pending files are re-checked under the lock; each version is inserted inside its file's own transaction; `-Baseline` is validated and compared as a number; files are read and piped as UTF-8.
- **D9.** The platform fallback is gone; `TestPurgeNeverFallsBackToThePlatformBucket` fails on the old code.
- **Docs.** Lease renewal (`docs/content-security.md`), connection edits and enabling, version deletion, large objects, reconciliation coverage and the rollback of 000016 (`docs/multipart-reliability.md`), readiness, and the `ingestion_entry` limit (`docs/security.md`, `.env.example`).

## 10. Unverified provider behavior and remaining risks

- **No live cloud.** AWS S3 (multipart copy above 5 GiB, `ListObjectVersions` permissions, the 5 GiB `CopyObject` refusal), Cloudflare R2, GCS and Azure were not reached. Live tests stay opt-in.
- **Version deletion** is verified only on MinIO. GCS generation deletion and Azure version deletion are implemented and their listings run on the emulators, but neither emulator can show the deletion itself. A versioned S3 bucket needs `s3:ListBucketVersions`; without it, deletions fail and retry instead of leaving bytes silently.
- **Large objects.** The largest object promoted was 5.06 GiB, on MinIO. Promotion's SHA-256 check reads the whole copy through the worker (37 MiB/s here). Azure's copy signs its source for 15 minutes, which an asynchronous copy across accounts could outlast. Multi-terabyte support is not claimed.
- **MinIO session listing.** Tenant connections on MinIO need `RECONCILIATION_SESSION_SCAN=database`; a provider session with no database row is then found only by a bucket lifecycle rule.
- **R5.** The cancellation cursor lives in the process; a restart reads from the start once.
- **Disk.** The measured 5 GiB copies (each source and copy deleted afterwards) and the 1M synthetic run grew Docker Desktop's virtual disk (`docker_data.vhdx`) from 71.8 GB to 100.2 GB, leaving 11 GB free on C:. The space is free inside the virtual disk but the file does not shrink by itself. Reclaiming it needs Docker Desktop's disk clean-up, or `wsl --shutdown` followed by compacting the file, which was not done here.
- **Left in place locally:** the scratch databases `object_storage_migp12_a` to `_d` (migration tests); the benchmark organizations `r5-before-100k`, `r5-after-100k`, `r5-after-1m` and `r2-defaults` in `object_storage_ingest_bench`; and objects under `versioned/` in `object-storage-it-versioned` from two early runs of the versioned-delete test, which nested keys under an object.
- **Flaky under load**, neither in changed code: `TestPrivateAddressIsRefusedAtConnectTime` and `TestLocalStackDeletionAndHoldReleaseThroughTheEventPipeline` (§3).
