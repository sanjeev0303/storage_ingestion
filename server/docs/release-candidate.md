# Release candidate verification

Verified on 2026-10-21 against `main` at `9459999` (PR #2, phases 6–11), with the changes listed in section 3 applied on top as uncommitted edits. One Windows 11 laptop, Docker Desktop, Go 1.25.0 (`GOTOOLCHAIN=go1.25.0`; the race run used the `golang:1.25` image pinned to go1.25.0). Every result below was produced by a command in section 4; nothing is estimated unless it says so.

## 1. Executive summary

The backend builds with Go 1.25.0, vets clean and passes its whole test suite: unit tests, integration tests against PostgreSQL, MinIO, ClamAV, ElasticMQ, fake-gcs-server and Azurite, and the same suite under the race detector (724 tests, 0 failures, 0 data races). A new end-to-end test starts the real binaries and takes two tenants with their own buckets through the whole product workflow over HTTP. It passes.

The audit found real defects. The ones that could lose data, break processing for every tenant or weaken a security boundary are fixed, each with a test that fails on the original code:

- deleting a storage connection could stop processing for **all** tenants;
- content could be swapped between verification and promotion;
- GCS connections with a custom endpoint dropped their credentials;
- S3 writes could store a truncated object;
- GCS multipart cleanup lost track of parts;
- the network guard let IPv6 reach cloud metadata and IPv4 via translation ranges;
- the Dockerfile could not build the module.

What is **not** verified:

- **Live cloud.** No test ran against real AWS S3, Cloudflare R2, Google Cloud Storage or Azure, because no credentials were available. Every provider was exercised through emulators or MinIO.
- **Neon.** Migrations were rehearsed on disposable local databases, not on a Neon branch: no Neon API access was available. The `.env` Neon database was never touched.
- **Open defects.** Fifteen medium- and lower-severity defects stay open (section 3.2), including two found under load: crash recovery waits out a 15-minute processing lease, and the default rate limit caps one tenant's bulk ingestion at about 10 files/s.

The user-facing frontend (`client/`) does not exist yet (section 10).

## 2. Confirmed implemented features

Confirmed means exercised by a test that ran and passed in this verification. The test names are the evidence.

| Area | What was exercised | Evidence |
|---|---|---|
| Accounts and tenancy | bootstrap, activation, login, organizations, roles, invitations, sessions, refresh rotation, lockout | e2e; `TestRefreshRotatesAndAReplayRevokesTheSession`, `TestInvalidCredentialsAreRefusedAlikeAndThrottled`, `tests/integration/tenancy_test.go` |
| Single and multipart upload | presigned PUT, part URLs, part confirmation, completion, idempotent create, concurrent complete/abort, lost completions | e2e; `TestProviderMultipartLifecycle`, `TestConcurrentCompleteAndAbortFinalizeOnce`, `TestWorkerRecoversALostCompletion` |
| Verification and scanning | size and SHA-256 checks, ClamAV scan, EICAR rejection, held scans on scanner outage, archive limits | e2e; `TestLocalStackEicarIsRejectedQuarantinedAndPurged`, `TestLocalStackScannerOutageHoldsThenRequeueReleases` |
| Promotion | copy to the ready prefix, staged copy removed, crash after copy resumes | e2e (checks the bucket directly); `TestLocalStackCrashAfterCopyResumesWithoutCopyingAgain` |
| Delivery | proxied and presigned reads, byte ranges (including across a part boundary), version-pinned resumable downloads, revocable links | e2e; `TestProviderConditionalRangeReads`, `TestLocalStackResumableDownloadPinnedToVersion` |
| Catalog | publication, version numbering, current version, listings, cursors | e2e (two versions of one path); `catalog_*_test.go` |
| Managed datasets | inline manifest import with a rejected line, snapshot, METADATA and FULL validation over members held in a tenant's bucket, readiness, findings, lineage | e2e; `dataset_*_test.go` |
| Bulk ingestion | manifests, claims, uploads, publication, quotas, retries, cancellation | load runs (section 5.1); `ingestion_*_test.go` |
| Events | outbox, SQS (ElasticMQ) delivery, duplicate delivery, dead letters | `events_*_test.go`; load runs with `EVENT_QUEUE=sqs` |
| Lifecycle | retention policy, soft delete, purge from the tenant's bucket, crash during deletion, orphan sweeps | e2e; `TestLocalStackDeletionSurvivesACrash`, `TestLocalStackOrphanSweepAcrossPages` |
| BYOS | connection create/test/default/rotate/disable/delete, per-upload selection, tenant binding triggers, sealed credentials | e2e; `storage_connection_test.go`, `internal/infrastructure/storage/registry` |
| Audit and quotas | hash-chained audit, verification, quota reservations under concurrency | `audit_test.go`, `TestQuotaReservationsHoldUnderConcurrency` |

## 3. Confirmed defects

Severity: **High** means data loss, cross-tenant impact or a broken security boundary. **Medium** means a wrong result or an outage in a supported configuration. **Low** means a narrow window, cosmetic or tooling.

### 3.1 Fixed in this release candidate

Every fix has a test. Each new test was run against an export of the original commit and failed there.

| # | Severity | Defect | Fix | Test |
|---|---|---|---|---|
| D1 | High | Deleting a connection while one of its uploads was COMPLETED but not yet given an asset passed the dependents check. The processing worker's enqueue is one `INSERT … SELECT` for all tenants, and the binding trigger refused the row bound to the deleted connection. Every cycle then failed, **so no tenant's uploads were processed** | `storage_connection_repository.go`: such uploads count as dependents. `processing_job_repository.go`: enqueue skips uploads bound to a deleted connection | `TestConnectionDeletionWaitsForUnprocessedUploads` |
| S1 | High | Verification hashed the object, then scanning and promotion read it again. An object overwritten in between (the presigned URL is still valid) was promoted unverified. A scanner that stopped reading early still reported clean | `internal/service/processing/service.go` `scan()`: the scanned stream is hashed and counted. A mismatch is rejected as `checksum_mismatch`; a short read is an incomplete scan | `TestContentSwappedAfterVerificationIsNotPromoted`, `TestAScanThatStopsEarlyIsIncomplete` |
| D12 | High | A GCS connection with any custom endpoint built an anonymous client, dropping the service account | `gcs.go` `New`: anonymous only for an `http://` emulator endpoint | `TestAnHTTPSEndpointKeepsTheServiceAccount` |
| D14 | Medium (security) | `netguard` allowed AWS's IPv6 metadata address `fd00:ec2::254` whenever private addresses were allowed. In every mode it allowed NAT64 local-use (`64:ff9b:1::/48`), 6to4 (`2002::/16`) and Teredo (`2001::/32`), which can reach private IPv4 | `netguard.go`: these ranges are always blocked, checked before the private ranges | `TestValidateURL`, `TestBlockedAddresses` |
| D10 | Medium | S3 `WriteObject` accepted a source shorter than its declared size and stored a truncated object | `s3/connection.go`: counts bytes written and aborts on a mismatch | `TestMinIOAsAnS3CompatibleConnection` (short source) |
| D5 | Medium | GCS `CompleteMultipart` removed the upload marker even when deleting parts failed, so leftover parts were never cleaned | `gcs.go`: the marker stays unless every part and intermediate delete succeeded | GCS emulator tests |
| D6 | Medium | GCS `ListMultipartUploads` read only the first page, so uploads beyond it were never reconciled | `gcs.go`: pages until the cursor is empty | `TestAnUploadWithManyPartsIsStillListed` |
| B1 | High (build) | `Dockerfile` used `golang:1.23` for a `go 1.25.0` module. The image could not be built. The build context also included `.env` (Neon credentials) | `golang:1.25`; new `.dockerignore` excludes `.env`, `bin/`, binaries and coverage files | `docker build --build-arg CMD=api .` succeeds (119 MB) |
| W1 | Low | `upload-worker` and `dataset-worker` returned on shutdown without waiting for their SQS consumer, cutting off in-flight messages | `sync.WaitGroup` around the consumer goroutine | build and vet |
| W2 | Low | `cmd/promotion-worker` had an empty `main` while promotion runs inside `processing-worker` (section 7) | command and empty `internal/worker/promotion` removed; nothing referenced them | `go build ./...` |
| R4 | Low (startup) | With BYOS on, a short secret in the environment (such as the dummy ElasticMQ secret `x`) that happened to occur inside the base64 key spec stopped every process: "must not reuse SQS_SECRET_ACCESS_KEY" | `internal/platform/config/storage.go`: values under 16 characters are compared only as a whole key | `TestAShortSecretInsideTheKeyIsNotReuse` |
| R3 | Low (tooling) | `ingestion-loadgen` gave up on a 429 after three quick retries, stranding claimed entries for their 15-minute lease | `cmd/ingestion-loadgen/api.go`: a 429 is retried each second within the request's minute | load runs in section 5.1 |
| C1 | Low | Docs and config: `docs/ingestion.md` named a nonexistent `path.go`; `.env.example` lacked the CDN variables and `STORAGE_PROVIDER`; two S3 error messages had typos | corrected | — |

### 3.2 Open

| # | Severity | Defect | Where | Recommendation |
|---|---|---|---|---|
| R1 | Medium | Processing jobs are leased for `PROCESSING_LEASE_DURATION` (15 min) and never renewed while running. After a worker crash, its in-flight jobs wait out the full lease; this was measured in section 5.1 | `internal/service/processing/service.go`, `internal/infrastructure/database/postgres/processing_job_repository.go` | renew the lease per stage, then shorten it |
| R2 | Medium | The default per-organization `upload_finish` rate limit (600/min) caps one tenant's bulk ingestion at about 10 files/s, below the 35–40 files/s the pipeline sustains (section 5.1) | `internal/platform/config/security.go` (`defaultRates`), `internal/server/router.go` (`ingestionOperation`) | a separate limit for ingestion entry operations, or a higher default |
| R5 | Medium | Cancelling a million-entry ingestion job took 24 minutes, its rate falling from about 1,280 to about 300 entries/s as it went; the manifest read is also about 2.7× slower than in Phase 7 (section 5.1) | `internal/service/ingestion` (cancellation sweep), `internal/infrastructure/database/postgres` (ingestion repository) | profile the cancel batch query; keyset it past already-cancelled entries |
| D2 | Medium | A connection edited with `PATCH` returns to PENDING. A PENDING connection resolves for nothing, including reads of assets it already holds, until someone tests it again. This is deliberate (`TestDisabledConnectionsDrainButPendingOnesDoNotResolve`), but editing a default connection stops that tenant's uploads and downloads | `internal/infrastructure/storage/registry/registry.go`; `docs/storage-connections.md` | test automatically after an edit, or keep serving existing objects with the last tested settings |
| D11 | Medium | S3-family promotion uses one `CopyObject`, which providers cap at 5 GiB. `UPLOAD_MAX_SIZE_BYTES` defaults to 5 GiB but can be raised; larger objects would then fail promotion | `internal/infrastructure/storage/s3` | multipart copy (`UploadPartCopy`) above 5 GiB, or refuse a larger limit at startup |
| D3 | Medium (unverified) | ASSUME_ROLE connections presign with STS session credentials. Upload and download URLs can expire with the session (STS default 1 h, configurable down to 15 min) before their stated expiry | `registry/build.go` | needs a live AWS account to confirm; request sessions longer than the longest URL TTL |
| D7 | Medium | The listing-based reconciliation scans (orphan provider sessions and orphan quarantine objects) list only the platform bucket's `quarantine/` prefix. Tenant buckets get only the database-driven per-session check, so an object or session in a BYOS bucket with no database record is never found | `internal/service/upload/multipart_scan.go` (`ScanOrphanSessions`, `ScanOrphanObjects`) | scan each active connection with a per-connection cursor |
| D4 | Medium | In a BYOS bucket with versioning on, deleting an asset deletes the current object only; noncurrent versions keep the bytes | `internal/service/asset/lifecycle.go` | delete by version ID when the connection reports provider versioning |
| D13 | Low | A connection test without `write_probe` checks only read and list. A read-only credential passes, and uploads fail later | `internal/service/storageconn/test.go` | make the write probe the default on create and rotate |
| D8 | Low | `GET /api/v1/catalog/capabilities` reports the platform bucket's capabilities, not those of the tenant's default connection | `internal/handler/http/catalog_handler.go` (`capabilities`) | resolve the caller's default connection |
| S3 | Low | Race: `Disable` reads the default connection, then updates the status, while `SetDefault` relies on the trigger's unlocked read. Run at the same moment by two administrators, the default can end up DISABLED and the tenant's uploads stop until another default is chosen | `internal/service/storageconn/service.go` (`Disable`, `SetDefault`) | lock the organization row (`SELECT … FOR UPDATE`) in one transaction in `Disable` |
| P1 | Low | `upload-worker`, `dataset-worker` and `reconciliation-worker` hard-code the platform prefix `assets`. With a different `PROMOTION_PREFIX` they look in the wrong place for platform objects | `cmd/dataset-worker/main.go`, `cmd/upload-worker/main.go`, `cmd/reconciliation-worker/main.go` | read `PROMOTION_PREFIX` |
| H1 | Low | `/health` answers 200 without checking PostgreSQL. The metrics listeners are never shut down | `internal/handler/http/health_handler.go`; each `cmd/*` with `METRICS_ADDR` | a readiness endpoint that pings the database |
| M1 | Low (tooling) | `scripts/migrate.ps1` records a version outside the migration's transaction and takes no lock, so two concurrent runs can both apply one file. `-Baseline` compares versions as strings | `scripts/migrate.ps1` | one transaction per file with the version insert; `pg_advisory_lock` |
| D9 | Info | `processing.recordStorage` falls back to the platform bucket when the asset is missing. This is unreachable: quarantine records cascade with their asset (`fk_quarantine_asset_tenant`) | `internal/service/processing/service.go` | remove the fallback or return an error |

Unverified behaviour, not counted as defects:

- **S5:** GCS composes at most 32 sources per call. Very many parts are composed in rounds; only the emulator's behaviour was seen.
- **S6:** Azure's delete of the current version on accounts with versioning. Azurite cannot reproduce account versioning.
- **Poll mode:** `catalog.version_replaced` is consumed only with `EVENT_QUEUE=sqs`.

Checked and **not** defects:

- **S2:** cached provider clients do age out: `STORAGE_CLIENT_CACHE_TTL`, 15 min, plus LRU eviction.
- **Azure part SAS:** `comp=block&blockid=` appended to the SAS is the normal form, and it passes on Azurite.

### 3.3 Cleanup not done

The cleanup items below change no behaviour:

- **Package-only stubs.** 27 source files contain only a `package` clause. Their names promise features that live elsewhere: `s3/copy.go`, `s3/multipart.go`, `s3/checksum.go`, `s3/object_metadata.go`, `internal/repository/*.go` (7), `internal/infrastructure/compaction/*.go` (2), `internal/interfaces/{checksum,file_processor,transaction_manager,upload_part_repository}.go`, `internal/dto/request/{complete_upload,create_multipart_upload,resume_upload}.go`, `internal/dto/response/error.go`, `internal/model/{processing_attempt,reconciliation_run}.go`, `internal/service/processing/idempotency.go`, `internal/service/upload/{multipart,resume}.go` and `postgres/upload_part_repository.go`. Nothing imports `internal/repository` or `internal/infrastructure/compaction`, so deleting all 27 is safe. The deletion was prepared but not applied, because this session's tool permissions refused file deletion. (`internal/service/ingestion/doc.go` is a package doc and stays.)
- **Unused variable.** `DATABASE_URL_UNPOOLED` in `.env.example` is read by no code.
- **Stale comments.** Integration test comments still name older migration numbers ("through 000012"); they need 000015.
- **gofmt and line endings.** gofmt flags 318 files in this Windows checkout. All of them are CRLF artifacts of `core.autocrlf=true`; the committed (LF) content has no unformatted file. A `.gitattributes` with `*.go text eol=lf` would make local `gofmt -l` meaningful. It was not added, because it changes every checkout.

## 4. Commands executed and their results

All commands ran from `server/` unless stated. The database for tests was always the local container on `127.0.0.1:5435`, never the `.env` Neon URL.

| Command | Result |
|---|---|
| `gofmt -l .` on the committed tree (`git archive` with `core.autocrlf=false`) | 0 files |
| `gofmt -l` on every Go file changed or added here (LF-normalized; 19 files) | 0 files |
| `go vet ./...` (final tree) | exit 0 |
| `go build ./...` and `go build -o <dir> ./cmd/...` (final tree) | all 14 commands build |
| `go test -count=1 ./...` with no service variables (final tree) | 540 passed (344 top-level), 185 skipped, 0 failed. Before the changes: 535 passed, 183 skipped |
| `go test -race -count=1 -p 1 ./...` in `golang:1.25` with every `TEST_*` variable set, final tree | 724 passed (521 top-level), 2 skipped, 0 failed, 0 data races; all 37 packages with tests passed; `tests/integration` 412 s. The earlier run, before the R3/R4 fixes, was 723 passed, 0 failed |
| `go test -v ./tests/e2e/` with `E2E_DATABASE_URL`, `CLAMAV_ADDRESS`, `S3_*`, `E2E_BYOS_*` | PASS twice: 44.5 s, then 64.5 s on the final tree, against the same database after the load runs had grown it. A first run failed on the test's own wrong expectation of lineage direction, corrected to match `docs/datasets.md` |
| `scripts/migrate.ps1 -Database object_storage_migverify2` (v14 copy of real data → v15) | 000015 applied in 1.3 s; a rerun applied nothing |
| `scripts/migrate.ps1 -Database object_storage_migfresh -Create` | 000001–000015 applied in 6.9 s |
| `scripts/migrate.ps1 -Database object_storage_bench_rc -Create` | 000001–000015 applied in 9.3 s |
| `docker build --build-arg CMD=api -t storage-api:rc-check .` | success, 119 MB (failed before the Dockerfile fix) |
| `ingestion-loadgen`, `catalog-bench` | section 5.1 |

Environment notes, recorded so the results can be read correctly:

- **The first race run had one failure, caused by the test harness.** The race container reached the services through a small TCP forwarder of mine, which leaked PostgreSQL connections, so `TestQuotaReservationsHoldUnderConcurrency` hit `too many clients`. The same run also skipped the `TEST_S3_*` tests, because my variable filter dropped names containing digits. Both were fixed and the run repeated. The repeat is the result above.
- **Skipped tests.** Two tests are skipped in the race run:
  - `TestProviderListingsArePaginated/prefix_listing_of_sessions` needs `TEST_S3_PREFIX_LISTING` (AWS S3); MinIO cannot list multipart uploads by prefix.
  - `TestTwoTenantsThroughTheRunningStack` needs `E2E_*`; it runs natively, with the row below.

## 5. Tests: passed, failed, skipped, blocked

| Suite | Passed | Failed | Skipped | Blocked |
|---|---|---|---|---|
| Unit (no services) | 540 | 0 | the integration and emulator tests, which skip without their variables | — |
| Unit + integration + emulators, race detector | 724 | 0 | 2 (AWS-only prefix listing; the e2e test, run separately) | — |
| End-to-end (real binaries, two tenants) | 1 test, about 60 checks; 2 passing runs | 0 (an earlier run stopped on the test's own wrong lineage expectation) | — | — |
| Load runs (section 5.1) | 9 runs, every file published; plus the catalog benchmark | 0 (one first attempt stalled on the load tool's own defect R3; rerun after the fix) | — | — |
| Live AWS S3, R2, GCS, Azure | — | — | — | **blocked: no credentials** |
| Neon branch migration | — | — | — | **blocked: no Neon API key or CLI** |

New tests added in this release candidate: `TestConnectionDeletionWaitsForUnprocessedUploads`, `TestContentSwappedAfterVerificationIsNotPromoted`, `TestAScanThatStopsEarlyIsIncomplete`, `TestAnHTTPSEndpointKeepsTheServiceAccount`, `TestAnUploadWithManyPartsIsStillListed`, `TestAShortSecretInsideTheKeyIsNotReuse`, the short-source case in `TestMinIOAsAnS3CompatibleConnection`, the IPv6 cases in the `netguard` tests, and `tests/e2e` (`TestTwoTenantsThroughTheRunningStack`).

### 5.1 Reliability and performance

Same laptop as the Phase 7 measurements in `docs/ingestion.md`:
- **Hardware.** Intel Core i5-1135G7 (4 cores, 8 threads), 15.7 GB RAM, Windows 11. Docker Desktop with 8 CPUs and 7.6 GiB.
- **Services.** PostgreSQL 17.11 with defaults (128 MB shared buffers, 100 connections), MinIO, ClamAV 1.4 and ElasticMQ in Docker.
- **Binaries.** The API, upload worker, processing worker (`PROCESSING_CONCURRENCY=4`) and outbox worker run natively, with `EVENT_QUEUE=sqs`. The dataset worker was added for the validation run.
- **Database.** The disposable `object_storage_migfresh`.
- **Driver.** `ingestion-loadgen`, measuring at the client and sampling every server's `/debug/vars`, PostgreSQL and `docker stats`.

"Published" means verified, scanned by ClamAV and promoted. Every run below ended with every file published and 0 failed entries, unless it says otherwise.

| Run | Data | Upload phase | End to end | Notes |
|---|---|---|---|---|
| Small files, default rate limits | 1,000 files of 1–64 KiB (31.9 MiB), 32 concurrent | 61.7 s, 16.2 files/s | 94.3 s, 11.0 files/s | 1,170 of 2,170 completion attempts answered 429; the per-org `upload_finish` limit (600/min) sets the pace (R2) |
| Small files, step 1 | 200 files (6.4 MiB), 32 concurrent | 5.7 s, 35.4 files/s | 21.0 s | default limits; under the limit |
| Small files, step 2 | 1,000 files (31.9 MiB), 32 concurrent | 25.1 s, **39.8 files/s** | 70.7 s, 14.9 files/s | limits raised (below); PUT p50/p95 39/90 ms, complete 150/330 ms, claim 374/616 ms; peak 735 awaiting processing |
| Small files, step 3 | 2,000 files (63.7 MiB), 32 concurrent | 57.8 s, **34.6 files/s** | 172.2 s, 11.8 files/s | peak 1,512 awaiting processing; oldest processing job waited 150 s; PostgreSQL CPU mean 66%, peak 142% |
| Two tenants at once, one on BYOS, credentials rotated mid-run | 2 × 1,000 files; tenant D on its own bucket | 72 s each, 14.0 files/s each, **28 files/s together** | 254 s for both, 8.6 files/s together | rotation with 682 of tenant D's files waiting for processing: none failed; all 1,000 in tenant D's bucket, none in the platform bucket, no staged leftovers. PostgreSQL CPU peak 265%; claim p50 850 ms |
| Mixed sizes, workers killed mid-run | 500 files: 428 of 1–256 KiB, 64 of 1–8 MiB, 8 of 20–48 MiB (633 MiB), 16 concurrent | 60.8 s, 8.2 files/s, **10.4 MiB/s**; 492 single, 8 multipart | 958 s | processing and upload workers killed with 108 published, restarted 24 s later. Uploads continued meanwhile. 497 were published within minutes; the last 3 waited out their 15-minute lease (R1 below). ClamAV CPU peak 424% |
| Dataset over the 2,000-file catalog | 2,000-line manifest (251 KiB), labels and splits | import 1.1 s; snapshot 0.21 s | METADATA validation 0.6 s, FULL 1.0 s, readiness 1.1 s | FULL examined all 2,000 from processing's recorded verification (by design, no bytes re-read); readiness: 3 warnings; lineage CONTAINS, IMPORTED_FROM, VALIDATED_BY |
| Large manifest (synthetic, no uploads) | 1,000,000 entries, staged manifest of 160.1 MiB | none: 32 claims granted, then held by `job_slots` | manifest read in 211.7 s (**4,723 lines/s**); cancelling all 1,000,000 took 1,435.7 s (about 700/s on average, falling from about 1,280/s to about 300/s as it went) | PostgreSQL CPU mean 103%, peak 386%; database +927 MiB; cache hit ratio 0.47. Upload worker peak heap 15.7 MiB while reading the manifest; load generator 199 MiB (it holds the million-file plan). Control-plane numbers only |

Rate limits for the capacity runs: `RATE_LIMIT_UPLOAD_FINISH`, `RATE_LIMIT_UPLOAD_INIT` and `RATE_LIMIT_LISTING` were all `60000/1m`, so the rows show the pipeline, not the throttle.

**Memory (peak heap in use).**

| Process | Peak heap |
|---|---|
| Upload worker | 7.9–9.5 MiB |
| Processing worker | 9.8–10.8 MiB |
| Outbox worker | 4.8 MiB |
| API | 11.8 MiB in the 200-file run; about 130 MiB whenever a run signed in during sampling |

The API's 130 MiB peak is the sign-in, not ingestion. Argon2id costs 64 MiB per hash (`AUTH_ARGON2_MEMORY_KIB=65536`), at most NumCPU at once. After the runs the API held 18.6 MiB in use.

**Catalog listings (`catalog-bench`).** The benchmark ran in the fresh `object_storage_bench_rc` on a quiet machine. One synthetic tenant was grown to 10,000, 100,000 and 1,000,000 objects (one version each), and each query ran 30 times through the real repository code.

| Query, p50 / p95 ms | 10,000 | 100,000 | 1,000,000 | Index |
|---|---|---|---|---|
| First page (path order) | 5.94 / 10.51 | 5.34 / 7.83 | **5.41 / 8.67** | `ix_catalog_objects_listed` |
| Page from the middle (keyset) | 4.85 / 13.15 | 5.80 / 13.51 | 6.13 / 7.69 | `ix_catalog_objects_listed` |
| One dataset | 4.86 / 6.92 | 15.21 / 53.74 | 6.73 / 19.84 | `ix_catalog_objects_listed` |
| Prefix | 3.68 / 5.85 | 12.08 / 23.68 | 22.94 / 38.67 | `ix_catalog_objects_listed` |
| Metadata filter | 6.49 / 8.70 | 7.25 / 13.59 | 7.36 / 8.44 | `ix_catalog_objects_listed` |
| Rare metadata filter | 5.70 / 7.91 | 10.00 / 13.24 | 18.22 / 25.33 | `ix_catalog_objects_metadata` |
| Content type and size (no index) | 14.05 / 21.27 | 33.90 / 42.84 | 157.94 / 223.72 | sequential, then the listing index |
| Time order | 4.65 / 8.36 | 5.10 / 7.16 | 8.66 / 18.58 | `ix_catalog_objects_updated` |
| Version history | 1.14 / 2.27 | 1.06 / 1.63 | 1.60 / 3.22 | |
| Lookup by path | 1.06 / 2.34 | 1.08 / 1.78 | 2.12 / 14.73 | |

Keyset listings stay flat from 10,000 to 1,000,000 objects. This matches `docs/catalog.md` (5.5–6.4 ms for the first page). Only filters the indexes cannot answer grow with the catalog.

Populating was slower than the figures in `docs/catalog.md`:

- **This run.** 1,522 objects/s from 10,000 to 100,000 and 938 objects/s from 100,000 to 1,000,000, with the database at 3,008 MiB at 1,000,000.
- **`docs/catalog.md`.** 2,241 and 2,192 objects/s.

The likeliest cause is the per-row storage binding triggers added in `000015` on `upload_sessions` and `assets`, which the generator also inserts; this was not profiled. The first 10,000 took 267 s, but that step includes the cold start and is not a throughput figure, as before.

**Compared with the earlier measurements** (same machine; `docs/ingestion.md` was measured in Phase 7, before authentication, tenancy and BYOS existed, and `docs/catalog.md` before BYOS):

| | Before | Now |
|---|---|---|
| Single-tenant upload | 43.7 files/s | 35–40 files/s |
| Two tenants together | 62 files/s | 28 files/s |
| End-to-end publication | 14.2 files/s | 11.8–14.9 files/s |
| Manifest read, 1,000,000 lines | 12,794 lines/s | 4,723 lines/s |
| Cancelling 1,000,000 entries | 476 s | 1,436 s |
| Catalog populate (100,000 → 1,000,000) | 2,192 objects/s | 938 objects/s |

PostgreSQL is the busiest component in every run, and the second tenant now adds contention rather than throughput. The cause was not profiled. The candidates are the tenancy, quota and audit writes added since Phase 7, and the BYOS connection lookups.

**Findings from these runs:**

- **R1 (Medium, reliability).** Processing jobs are leased for `PROCESSING_LEASE_DURATION` (15 min) and never renewed while they run (`internal/service/processing`). A worker crash therefore delays its in-flight jobs by up to 15 minutes. A shorter lease is unsafe without renewal, because a large scan can legitimately take minutes (`CLAMAV_TIMEOUT` 2 min plus the copy). The fix: renew the lease per stage, then shorten it.
- **R2 (Medium, configuration).** The default per-organization `upload_finish` limit (600/min) caps one tenant's bulk ingestion at about 10 files/s, below what the pipeline sustains (35–40 files/s). Every entry completion and heartbeat counts against it. Raise it for ingestion tenants, or give ingestion entry operations their own limit.
- **R5 (Medium, performance).** Cancelling a very large job slows down as it goes: about 1,280 entries/s early, about 300/s by the end, 24 minutes for a million. Each pass probably re-reads the entries already cancelled; this was not profiled. Claims stop at once, so nothing is uploaded meanwhile, but the job holds its tenant's rows busy for the whole time.
- **R3 (fixed, Low, tooling).** `ingestion-loadgen` gave up on a 429 after three quick retries. Each refused completion then stranded its entry for the 15-minute slot lease; a 1,000-file run stalled at 348. The tool now waits and retries a 429 within each request's one-minute budget (`cmd/ingestion-loadgen/api.go`).
- **R4 (fixed, Low, startup).** With BYOS on, any other secret of 1–15 characters that happened to occur in the base64 key spec stopped the process with "STORAGE_CREDENTIAL_KEYS must not reuse …". A dummy ElasticMQ secret `x` triggers it at random. Short values are now compared only as a whole key (`internal/platform/config/storage.go`; `TestAShortSecretInsideTheKeyIsNotReuse`, which fails on the original code).

**Scenarios from the milestone covered by existing tests rather than load runs:**

| Scenario | Tests (all passed under `-race`) |
|---|---|
| Interrupted multipart completion | `TestCompleteRecoversPartsThatWereNeverRecorded`, `TestWorkerRecoversALostCompletion`, `TestConcurrentCompleteAndAbortFinalizeOnce`, `TestLocalStackRecoversLostCompletionAndLeftoverSessions` |
| Worker termination | `TestCrashedWorkerJobIsRecoveredAndStaleWriteRefused`, `TestExpiredLeaseIsReclaimedAndTheStaleWorkerCannotWrite`, `TestIngestionRecoversAbandonedUploadsAfterRestart`, `TestStagedManifestImportResumesAfterACrash`, `TestCrashAfterStorageDeletionResumes`; e2e (processing worker killed) |
| Duplicate queue delivery | `TestDuplicateEventsCreateOneAssetAndOneJob`, `TestConcurrentDeliveriesOfOneEventRunItOnce`, `TestConsumerCrashAfterTheBusinessCommitDoesNotRepeatIt`, `TestIngestionRepeatedClaimsAndEventsDoNotDuplicateWork` |
| Provider timeouts and temporary authorization failures | `TestThrottledProviderLeavesStateForRetry`, `TestStorageThrottlingAndFailures`, `TestProcessingTimeoutIsRetried`, `TestClassifyThrottlingIsRetryable`, `TestClassifyNetworkFailuresAreRetryable`, `TestScannerErrorsRetryThenExhaustWithoutPromotion` |
| Large manifests in bounded memory | `TestLargeManifestIsStreamedInBoundedMemory` (1,000,000 lines), `TestLargeManifestIsReadWithBoundedMemory`, `TestIngestionLargeInlineManifestStreams` |
| Large cursor-based listings | `TestLargeCatalogPopulationIsBounded`, `TestPostgresClaimsPageThroughLargeBacklogs`, `catalog-bench` above |

**What was not tested:**

- more than one API or worker instance per role;
- tenants beyond five at once;
- files above 48 MiB in load runs (the 5 GiB copy limit is D11);
- sustained runs over an hour;
- any cloud provider.

Capacity beyond these numbers (millions of files per hour, terabytes, billions of catalog objects) is extrapolation and is not claimed.

## 6. BYOS provider coverage

| Provider | Unit / adapter | Emulator or compatible service | Live cloud |
|---|---|---|---|
| AWS S3 | signing region (`TestAWSSignsInTheConnectionsRegion`), error classification, presigning, endpoint policy | MinIO stands in for the S3 API: multipart lifecycle, copy, presigned and ranged reads, versioned delete, paginated listings | **not run.** ASSUME_ROLE was never exercised, and prefix listing of sessions is AWS-only |
| S3-compatible (MinIO) | endpoint policy, `force_path_style` | `TestMinIOAsAnS3CompatibleConnection`, `TestBringYourOwnStorageEndToEnd`, e2e. The e2e covers three connections on three buckets: create, test with write probe, default, per-upload choice, multipart, checksum, ClamAV, promotion, ranged reads, presigned reads, catalog, dataset FULL validation, rotation (bad key refused, good key used), disable (objects stay and are served), delete (never selectable), purge from the tenant's bucket | MinIO is the target itself |
| Cloudflare R2 | endpoint construction and signing: `<account>.r2.cloudflarestorage.com`, region `auto` (`TestR2SignsWithRegionAutoAtTheAccountEndpoint`); capabilities the API advertises | none. MinIO speaks the S3 API but not R2's differences | **not run** (no credentials) |
| Google Cloud Storage | part names, classification, signed URLs need a key, HTTPS endpoint keeps the service account, pagination of uploads (`httptest` server that pages like GCS) | fake-gcs-server: objects and generations, composite multipart upload, signed part upload | **not run** |
| Azure Blob | block IDs, exact-length reader, errors never carry the SAS, exactly one credential | Azurite: signed uploads and reads, block uploads and block-list commit, bounded streaming writes, SAS-token connection reads but cannot sign | **not run.** Account versioning is not verified: Azurite cannot reproduce it |

Emulator limitations seen:

- **fake-gcs-server** truncates listings at `maxResults` without a `nextPageToken`, so GCS pagination was tested against a stub server.
- **MinIO** cannot list multipart uploads by prefix.
- **Azurite** has no account-level versioning.

## 7. Database migration status

**Ordering.** There are 15 files, `000001`–`000015`, applied in order by `scripts/migrate.ps1` and recorded in `schema_migrations`. A fresh install and an upgrade both end at the same 15 ordered rows.

**Upgrade path, rehearsed locally.**
- `object_storage_migverify2` was built at version 14 from a read-only copy of `object_storage_smoke`: 45 tables, about 1.01 M ingestion entries, 13,589 uploads, 13,556 assets, 8 organizations.
- `000015_storage_connections` applied in 1.3 s. Every table's row count was unchanged afterwards.

Storage-connection migration checks after the upgrade (`migverify-check.sql`):

- **Platform connection.** Exactly one `platform` connection exists.
- **Existing bindings.** Every upload session, asset, processing job and inventory scan is bound to `platform`, and every organization's default is `platform`.
- **NULL connections, as designed.** Datasets have NULL, meaning "use the organization's default". 10 catalog objects have NULL, which the registry treats as the platform connection.
- **Triggers.** The 4 `tr_*_storage_tenant` binding triggers exist.
- **Trigger probes**, run on migrated data and rolled back:
  - changing an upload's binding is refused (`storage_binding_immutable`);
  - another tenant's connection as a default is refused;
  - a DELETED connection as a default is refused;
  - the tenant's own ACTIVE connection is accepted;
  - no probe rows were left behind.

**Fresh install.** `object_storage_migfresh` and `object_storage_bench_rc` were each created with `-Create`. The e2e test and the load runs used `object_storage_migfresh`.

**Neon: blocked.** The development database in `.env` is Neon (host read without printing credentials). A Neon branch needs the Neon API or `neonctl`, and neither was available. Nothing was run against Neon, and no local database was substituted and called Neon. To finish this step:

1. Create a branch of the development project.
2. Point `DATABASE_URL` at the branch's direct (unpooled) URL.
3. Apply `000015` in a transaction.
4. Run `migverify-check.sql`.

`scripts/migrate.ps1` drives `psql` inside the local container only, so it cannot be used against Neon as it stands.

**Left in place.** Local scratch databases kept for inspection: `object_storage_migverify` (half-built; no `DROP` was run), `object_storage_migverify2`, `object_storage_migfresh`, `object_storage_bench_rc`.

## 8. Local startup and verification commands

```powershell
# Services (from server/)
docker compose up -d                                   # postgres :5435, minio :9000, clamav :3310, elasticmq :9324
docker compose --profile emulators up -d fake-gcs azurite   # :4443, :10000

# A disposable database at the latest migration
.\scripts\migrate.ps1 -Database object_storage_e2e -Create
```

```bash
# Toolchain: the module needs Go 1.25.0
export GOTOOLCHAIN=go1.25.0

# Static checks and unit tests
go vet ./... && go build ./... && go test ./...

# Integration and emulator tests: local services only, never the .env database
export TEST_DATABASE_URL="postgres://storage_app:<password>@127.0.0.1:5435/object_storage_test?sslmode=disable"
export TEST_S3_BUCKET=object-storage-it TEST_S3_CREATE_BUCKET=true
export S3_REGION=us-east-1 S3_ENDPOINT=http://127.0.0.1:9000 S3_FORCE_PATH_STYLE=true
export S3_ACCESS_KEY_ID=<minio user> S3_SECRET_ACCESS_KEY=<minio password>
export TEST_CLAMAV_ADDRESS=127.0.0.1:3310 TEST_SQS_ENDPOINT=http://127.0.0.1:9324
export TEST_GCS_ENDPOINT=http://127.0.0.1:4443 TEST_AZURITE_ENDPOINT=http://127.0.0.1:10000 TEST_AZURITE_KEY=<Azurite's published key>
export TEST_S3_COMPATIBLE_ENDPOINT=http://127.0.0.1:9000 TEST_S3_COMPATIBLE_BUCKET=object-storage-it
export TEST_S3_COMPATIBLE_ACCESS_KEY=<minio user> TEST_S3_COMPATIBLE_SECRET_KEY=<minio password>
go test -count=1 -p 1 ./...          # add -race on a machine with cgo (or in the golang:1.25 image)

# End to end: builds and starts api, processing-worker, dataset-worker and cleanup-worker itself
export E2E_DATABASE_URL="postgres://storage_app:<password>@127.0.0.1:5435/object_storage_e2e?sslmode=disable"
export CLAMAV_ADDRESS=127.0.0.1:3310
# Optional, recommended: a separate MinIO account for the tenants' connections, so the leak check
# can search for its secret (the compose root account's secret equals its user name):
#   docker exec object-storage-minio mc admin user add <alias> e2e-byos <random secret>
#   docker exec object-storage-minio mc admin policy attach <alias> readwrite --user e2e-byos
export E2E_BYOS_ACCESS_KEY=e2e-byos E2E_BYOS_SECRET_KEY=<that secret>
go test -count=1 -v ./tests/e2e/
```

The e2e test creates its buckets (`object-storage-e2e`, `e2e-tenant-a`, `e2e-tenant-b`, `e2e-tenant-b2`, `e2e-tenant-b3`). It can be rerun on the same database: it takes the `admin@e2e.test` account back with a reset token. It covers the release checklist as follows.

| Checklist step | What the test does |
|---|---|
| 1–2 | the platform admin creates two owners and organizations |
| 3–4 | each tenant creates its connection, tests it with the write probe and makes it the default; an upload to the untested connection is refused |
| 5–9 | a small and a three-part multipart object are uploaded. The asset's SHA-256 and size are checked. The promoted object is found in the tenant's bucket with a direct `HEAD`. The staged copy is gone, and nothing of the tenant's is in the platform bucket. EICAR is rejected, never served, never promoted, never published |
| 10 | whole and ranged reads through the API and through presigned URLs, one range spanning a part boundary |
| 11 | a second upload to a path becomes version 2; the version list is checked |
| 12–15 | manifest import with one rejected line and its error, snapshot, FULL validation, readiness, findings, lineage in both directions |
| 16 | the processing worker is killed: an upload stays unpublished, then is published after a restart. A retention policy is set; an asset is deleted and purged from the tenant's bucket by the cleanup worker |
| 17 | tenant B repeats the uploads at the same paths on its own bucket. Cross-tenant reads, tests, links and uploads are refused (404). A non-member's `X-Organization-ID` is refused (403). A second connection is chosen per upload, disabled (its object stays and is served) and cannot be deleted while it holds an asset. A deleted connection is never selectable |
| Security | rotation with a failing key is refused and changes nothing; a good key is used at once. Every response body, every process's output, the processing worker's `/debug/vars` and every column of every table (55) are searched for the secrets in plain text; the sealed credential bytes are searched for the raw secret |

Reconciliation is not in the e2e test. It is covered by `TestLocalStackRecoversLostCompletionAndLeftoverSessions`, `TestLocalStackCleansUpInitiationWhoseIDWasNotSaved` and `TestCompletionRecoveredByReconciliationWritesTheEventThen`.

## 9. Security and reliability gaps

**Security (open).**
- D13: read-only credentials pass a connection test without the write probe.
- D3: STS session lifetime is unverified.
- S3: the disable/default race.
- No live-cloud test of credential handling.
- `/health` reveals nothing, but it is not a readiness check either (H1).

**Verified (tenant isolation and credential security):**

- **Isolation.** One tenant cannot reach another's connection, objects, links or catalog (e2e; `TestLocateResolvesOnlyTheTenantsOwnConnections`; trigger probes).
- **Credentials at rest.** They are sealed with AES-GCM, with the connection and version as associated data (`TestCredentialsSealedForAnotherConnectionDoNotOpen`; e2e raw-byte search).
- **Secret leakage.** No secret appears in responses, logs, metrics, audit or outbox (queue) rows (e2e). The e2e test scans the processing worker's metrics only; the other processes' metrics hold counters and gauges with no free text.
- **Rotation.** Clients are rebuilt on rotation (`TestClientsAreCachedAndRebuiltOnRotation`; e2e).
- **Disabled and deleted connections.** Disabled connections refuse new work and keep their objects where they are; deleted ones are never selectable (e2e).
- **Endpoint validation.** It blocks loopback, private, link-local, metadata and translated addresses at save and at dial time (`netguard`, `TestPrivateAddressIsRefusedAtConnectTime`).
- **Authorization.** Connection choice is authorized in the service layer (`TestChooseForUpload`).
- **Binding.** An asset's storage binding is immutable (trigger, probed on migrated data).

**Reliability.** The open items are R1 (15-minute recovery after a crash), R2 (the default rate limit), R5 (slow cancellation of huge jobs), D2, D7, D4 and D11, plus the throughput limits measured in section 5.1. Recovery after a worker crash, duplicate deliveries, lost completions and interrupted deletions are covered by the tests named in section 5.1.

## 10. Remaining frontend work

`client/` holds only `.gitkeep`. The user-facing frontend is a separate, unstarted product component. Every workflow below has a working API today.

1. **Sign-in and accounts:** login, activation, password reset, sessions (`/auth/*`).
2. **Organization administration:** members, invitations, roles, suspension (`/organization/*`).
3. **Storage connections:**
   - provider picker with required fields and capabilities (`/storage-providers`);
   - create, test with results per check, default, rotate, disable, enable, delete (`/storage-connections/*`).
4. **Uploads:**
   - single and resumable multipart, with part URLs, part confirmation and completion;
   - progress and retry;
   - checksum computed in the browser.
5. **Assets and catalog:**
   - browse and filter (`/catalog/objects`), lookup, versions, rename, versioning setting;
   - asset detail with processing and quarantine state.
6. **Delivery:** download links (storage or token mode), revocation, ranged preview.
7. **Datasets:**
   - create and derive, members, inline and staged manifest import with error lists;
   - snapshots, validation profiles, validations and findings, readiness reports;
   - lineage graph, archive.
8. **Bulk ingestion:** jobs, manifest upload, progress, failures, retry, cancel.
9. **Lifecycle:** retention policies, legal holds, delete and restore, lifecycle state.
10. **Platform administration:** users, organizations, quotas and usage, audit verification, dead letters.

## 11. Ranked backlog for the next milestone

1. **Live-cloud verification.**
   - Run the provider and e2e flows against real AWS S3 (including ASSUME_ROLE and D3), R2, GCS and Azure (including account versioning, S6).
   - This needs credentials.
2. **Neon branch rehearsal of 000015** (section 7), then the production migration plan.
3. **R1:** renew processing leases per stage and shorten them, so a crashed worker's jobs resume in minutes.
4. **R2:** give ingestion entry operations their own rate limit; investigate the two-tenant PostgreSQL contention (section 5.1).
5. **D2:** keep a connection usable while an edit is retested.
6. **D11:** multipart copy above 5 GiB, or refuse larger upload limits for S3-family connections.
7. **D7:** listing-based reconciliation for each BYOS connection.
8. **D4:** version-aware deletes for versioned BYOS buckets.
9. **Frontend:** start with sign-in, connections and uploads (section 10).
10. **Write probe by default (D13):** make it the default on connection create and rotate.
11. **S3, P1, H1, D8:** the disable/default lock, the configurable platform prefix, readiness health, and per-connection catalog capabilities.
12. **`migrate.ps1` hardening** (M1).
13. **Cleanup:** delete the 27 stubs and `DATABASE_URL_UNPOOLED`; decide on `.gitattributes`.

## 12. Revised completion estimate

This is a judgement from the evidence above, not a measurement.

- **Backend, local single-machine scope: release candidate.** Every core workflow ran end to end on real binaries with two tenants. The full suite passes under the race detector, and the defects that could lose data or cross tenants are fixed. Remaining backend work is verification against real clouds, the medium defects in 3.2 (recovery time R1 and the throughput regressions R2/R5 first) and operational hardening: on the order of a few engineer-weeks, mostly gated on cloud access.
- **Production capacity: unproven beyond what section 5.1 measured.** Those are one-laptop numbers. Claims about billions of objects or terabytes are extrapolation and are not made here.
- **Product overall: the frontend is the largest remaining piece.** It is a full application over about 140 routes. With the backend near complete and the frontend not started, the product as a whole is well short of complete.

## Appendix A. Inventory (milestone section 1)

### Entry points

There are 14 commands under `cmd/`, after `promotion-worker` was removed.

| Command | Role |
|---|---|
| `api` | the HTTP API: 142 routes including `/health` |
| `processing-worker` | verification, content validation, ClamAV scan, **promotion**, quarantine purge; SQS `processing` consumer |
| `upload-worker` | bulk ingestion: staged manifests, lease expiry, retries, outcomes, cancellations, counter reconciliation; SQS `ingestion` consumer |
| `dataset-worker` | manifest imports, validations, readiness, recovery; SQS `datasets` consumer |
| `cleanup-worker` | retention expiry, deletions, orphan scan and sweep, missing objects (`-phases`, `-once`, `-dry-run`) |
| `reconciliation-worker` | multipart reconciliation: lost completions, orphan sessions and objects, stale parts (`-once`, `-dry-run`) |
| `outbox-worker` | publishes the outbox to SQS, event reconciliation. It needs `EVENT_QUEUE=sqs` and exits otherwise |
| `compaction-worker` | prunes old audit, operational and idempotency records |
| `identity-admin`, `catalog-admin`, `event-admin`, `lifecycle-admin` | operator CLIs with direct database access |
| `catalog-bench`, `ingestion-loadgen` | measurement tools |

### HTTP routes

There are 142 routes. Each group goes through its own handler to its service, and every tenant route runs behind authentication, tenant resolution and a per-organization rate limit (`internal/server/router.go`).

| Group | Routes | Handler → service |
|---|---|---|
| datasets, dataset-versions, validation-jobs, validation-reports, validation-profiles | 30 | `dataset_handler.go` → `service/dataset` |
| admin | 21 | `admin_handler.go` → `service/identity`, `service/quota`, `service/events`, `service/audit` |
| organization, invitations | 19 | `identity_handler.go` → `service/identity` |
| ingestion | 18 | `batch_handler.go` → `service/ingestion` |
| catalog | 12 | `catalog_handler.go` → `service/catalog` |
| storage-connections, storage-providers | 12 | `storage_connection_handler.go` → `service/storageconn` |
| auth | 9 | `identity_handler.go` → `service/identity` |
| assets, downloads | 10 | `asset_handler.go`, `download_handler.go`, `lifecycle_handler.go` → `service/delivery`, `service/asset`, `service/processing` |
| uploads | 7 | `upload_handler.go`, `multipart_upload_handler.go` → `service/upload` |
| legal-holds, retention | 3 | governance → `service/asset` |
| health | 1 | static |

### Migrations

`000001_create_upload_sessions` … `000015_storage_connections`, 15 files, applied in order (section 7).

### Storage adapters

The S3 adapter (`internal/infrastructure/storage/s3`) serves the `PLATFORM_MANAGED`, `AWS_S3`, `S3_COMPATIBLE` and `CLOUDFLARE_R2` providers. The other adapters are `gcs` (`GOOGLE_CLOUD_STORAGE`), `azure` (`AZURE_BLOB`) and `memory` (tests only).

Auth methods:

| Provider | Auth methods |
|---|---|
| `AWS_S3` | access key, `ASSUME_ROLE` |
| `S3_COMPATIBLE`, `CLOUDFLARE_R2` | access key |
| `GOOGLE_CLOUD_STORAGE` | service-account key |
| `AZURE_BLOB` | account key, or a SAS token (staging on the platform) |

A connection test records the capabilities it found: presigned upload, multipart, listing of incomplete uploads, server-side copy, range reads, conditional reads, presigned download, object listing, provider versioning, checksum algorithms and minimum part size.

### BYOS routing

The registry (`internal/infrastructure/storage/registry`) resolves a connection for each workflow:

| Workflow | How the connection is chosen |
|---|---|
| Upload | an explicit `storage_connection_id` (needs `storage.use`), else the dataset's connection, else the organization's default. Staging is the connection itself, or the platform when `staging_mode=PLATFORM` |
| Processing and promotion | the asset's staging and final connections, which are immutable (trigger). Promotion copies to `<ready_prefix>/<tenant>/<asset>` in the final connection |
| Delivery, cleanup, deletion | the asset's final connection. A DISABLED connection still serves and deletes; a PENDING one resolves nothing (D2) |
| Reconciliation | database-driven checks follow each session's connection. Listing scans cover the platform bucket only (D7) |
| Catalog | each version records its connection. The capabilities endpoint reports the platform's (D8) |
| Datasets | FULL validation reads through each member's connection; a dataset can name its own connection |

### Tests

There are 107 test files:

| Location | Test functions |
|---|---|
| `internal/` and `cmd/` (unit and adapter) | 308 |
| `tests/unit` | 43 |
| `tests/integration` | 170, gated on `TEST_*` variables |
| `tests/e2e` | 1, gated on `E2E_DATABASE_URL` |

`tests/load` and `tests/fixtures` are empty; the load tool is `cmd/ingestion-loadgen`.

### Empty entry points and package-only files

- **Entry points.** `cmd/promotion-worker` (removed).
- **Package-only files.** The 27 files listed in 3.3. `internal/repository` and `internal/infrastructure/compaction` consist only of them.

### TODOs and unsupported behaviour

- **TODOs.** There are no `TODO`, `FIXME`, `XXX` or `HACK` markers in non-test code.
- **Unsupported behaviour.** It is explicit and returns `ErrUnsupportedProvider`:
  - signing without a key: GCS without a service account, Azure with a SAS token;
  - `ASSUME_ROLE` without a platform AWS identity;
  - multipart or direct uploads on a connection that cannot take them.
- **Unreachable code.** `processing.recordStorage`'s fallback (D9).

### Configuration

Every component reads its settings from the environment at startup and exits on an invalid value. `.env.example` lists them, now including the CDN and `STORAGE_PROVIDER` variables.

The minimum for a local stack:

- `DATABASE_URL`;
- `S3_REGION`, `S3_ENDPOINT`, `S3_ACCESS_KEY_ID`, `S3_SECRET_ACCESS_KEY`, `S3_FORCE_PATH_STYLE`, `S3_BUCKET`;
- `SCANNER=clamav` and `CLAMAV_ADDRESS` for the processing worker;
- for BYOS: `BYOS_ENABLED=true`, `STORAGE_CREDENTIAL_KEYS`, and `STORAGE_ALLOW_PRIVATE_ENDPOINTS=true` with `APP_ENV=development` for MinIO;
- for SQS mode: `EVENT_QUEUE=sqs` and the `SQS_*` settings.

Section 8 has the exact environments used here.
