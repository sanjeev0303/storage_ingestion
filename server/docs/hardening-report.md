# Hardening report: dataset reproducibility, BYOS correctness, ingestion performance

**Date and base.** Verified on 2026-10-21 on top of `release-candidate-verification` (`83a0301`). All changes are uncommitted edits.

**Machine and toolchain.** One Windows 11 laptop: Intel Core i5-1135G7, 4 cores and 8 threads, 15.7 GB RAM. Docker Desktop runs PostgreSQL 17.11, MinIO, ClamAV 1.4, ElasticMQ, fake-gcs-server and Azurite. Go is 1.25.0 (`GOTOOLCHAIN=go1.25.0`; the race runs use the `golang:1.25` image pinned to go1.25.0).

**Status labels.** Every result below comes from a command that ran here. Estimates and extrapolations say so.

The previous verification's report, `docs/release-candidate.md`, still applies: its defects, coverage matrix and frontend list. This report covers what changed and what was found since.

## 1. Confirmed implemented functionality

Everything listed in `docs/release-candidate.md` §2 was verified again by the full suite on this tree (§3). New in this milestone:

- **Content checks read the version a snapshot names.** In a versioned bucket, `content_checksum` reads the provider version recorded at publication (S3/Azure version ID, GCS generation). Otherwise it reads the version's own key, so an overwrite out of band shows as a mismatch.
- **A record without a storage binding fails closed.** Before, it was served, scanned or deleted on the platform's bucket. Leftover multipart sessions are now reconciled on the connection they staged on.
- **Credential rotation no longer briefly breaks other processes.** Clients built during the row-cache window after a rotation used to fail.
- **`ingestion-loadgen` profiles the database.** It samples PostgreSQL's active statements and their waits ten times a second, and retries 429s (`cmd/ingestion-loadgen`).

## 2. Confirmed defects and file paths

### Fixed

Each fix has a test that fails on the code before it.

| # | Severity | Defect | Files | Test |
|---|---|---|---|---|
| H1 | Medium | Dataset content validation read the object's **current key**, never the provider version recorded for the snapshot's version. In a versioned bucket, bytes written over a member's key after the snapshot failed a version whose own bytes were intact. Delivery and reconciliation already pinned reads to the recorded version; validation did not (`docs/datasets.md` listed it as a limitation). | `internal/service/dataset/validation.go` (`checkContent`), `internal/service/dataset/service.go` (`ContentReader`), `internal/infrastructure/database/postgres/validation_repository.go` (`MemberEvidence`), `internal/model/dataset.go` | `TestContentValidationReadsTheSnapshotsVersions` (fails with the key-only read: "pinned reads: FAILED") |
| H2 | Medium (BYOS) | An empty storage binding was treated as the platform's (`IsPlatformConnection("")`, `Registry.Locate("")`, cleanup's `locate` and `boundTo`). Every stored binding is `NOT NULL` and was bound to `platform` by migration, and the tenant trigger refuses `''`. So the fallback served no stored record and could only hide a lost binding, sending a tenant's object to the platform bucket. | `internal/model/storage_connection.go`, `internal/infrastructure/storage/registry/registry.go`, `internal/service/cleanup/service.go`, `internal/service/cleanup/classify.go`, `internal/service/upload/routing.go`, `internal/handler/http/{upload,multipart_upload}_handler.go` (the now-pointless `boundConnection` is removed) | `TestAnAssetWithoutABindingIsNotServed` (fails before: a URL was signed on the platform bucket); `TestLocateResolvesOnlyTheTenantsOwnConnections` asserts the refusal |
| H4 | Medium (BYOS) | Leftover-session reconciliation read finished sessions without their storage binding (`FinishedSessions` never selected the connection columns). With the old empty-binding fallback, every session was treated as the platform's. A leftover multipart session in a tenant's bucket was looked for in the platform bucket and never aborted. H2 surfaced this: the race run failed `TestLocalStackRecoversLostCompletionAndLeftoverSessions` until the query loaded the bindings. | `internal/infrastructure/database/postgres/reconciliation_repository.go` (`FinishedSessions`) | `TestPostgresFinishedSessionsCarryTheirBinding` (fails before: the session comes back without its binding) |
| H3 | Medium (BYOS) | Credential rotation could fail requests in other processes. Only the newest credential is stored and it is read by version, but the registry caches the connection row for `STORAGE_CONNECTION_CACHE_TTL` (10 s). A process that built a client in that window asked for a version that no longer existed and failed with "the storage connection has no credentials". | `internal/infrastructure/storage/registry/registry.go` (`Locate` re-reads the row while it finds a newer version) | `TestRotationDuringConcurrentUseIsSafe`: 15 of 20 runs failed before; 100 of 100 pass after, and 10 of 10 under `-race` |

The in-memory test store (`internal/infrastructure/storage/memory`) now keeps every version's bytes in a versioned bucket, as S3 does. One assertion that relied on the old "newest only" model was updated to the real behaviour, in `TestProviderVersioningIsRecordedWhereSupported`: an overwrite of the key leaves the recorded version readable, and only deleting that version makes it missing. The in-memory repositories give a record stored without a binding the platform's, as the column default does.

### Investigated, not a defect

- **GCS client closed while in use.** The registry closes a cached client on rotation, eviction or a concurrent rebuild while other requests may hold it. The GCS SDK documents that uses after `Close` may panic. With the HTTP transport in v1.68, eight operations after `Close` returned normal errors, with no panic. `TestAGCSClientIsUsableAfterTheCacheLetsItGo` guards this against an SDK upgrade. S3 and Azure closes are no-ops.

### Open, found in this milestone

| # | Severity | Finding | Where |
|---|---|---|---|
| P1 | Medium | Ingestion throughput per tenant is capped by two per-tenant counter rows held through each commit (§6). | `internal/infrastructure/database/postgres/batch_accounting.go` (`updateUsage`), `quota_repository.go` (`CreateReservedUpload`, `addUsage`) |
| P2 | Medium | Snapshots run inside the HTTP request, in one transaction. 1,000,000 members took 160.5 s. The default cap of 5,000,000 extrapolates to about 13 minutes. A client or proxy that gives up first cancels the request's context, so the snapshot rolls back. This last point is from reading the code, not observed. | `internal/service/dataset/service.go` (`Snapshot`), `internal/infrastructure/database/postgres/dataset_repository.go` (`CreateSnapshot`) |
| P3 | Feature gap | No physical small-file packing (§7). | `internal/infrastructure/compaction` (two empty files) |

The open defects of `docs/release-candidate.md` §3.2 stand unchanged: R1, R2, R5, D2, D11, D3, D7, D4, D13, D8, S3, P1 (prefix), H1 (health), M1 (`migrate.ps1`), D9.

## 3. Go toolchain and test results

| Command | Result |
|---|---|
| `go version` | go1.25.0 windows/amd64 (`go.mod`: `go 1.25.0`, unchanged) |
| `gofmt -l` on the 25 Go files changed or added since `83a0301` (LF-normalized) | 0 files |
| `go vet ./...` | exit 0 |
| `go build ./...`; `go build -o <dir> ./cmd/...` | all 14 commands build |
| `go test -count=1 ./...`, no service variables | 543 passed (347 top-level), 187 skipped (service-gated), 0 failed |
| `go test -race -count=1 -p 1 ./...` in `golang:1.25`, every `TEST_*` variable set (PostgreSQL, MinIO, ClamAV, ElasticMQ, fake-gcs-server, Azurite) | **729 passed** (526 top-level), 2 skipped, **0 failed, 0 data races**; all 37 packages with tests passed; `tests/integration` 398 s. The skipped tests are the AWS-only prefix listing (MinIO cannot list by prefix) and the e2e test (run natively, below). An earlier run on this tree failed one test, which led to H4; it passed once that was fixed |
| `go test -v ./tests/e2e/` (real binaries, two tenants, local MinIO and ClamAV) | **PASS**, 95.5 s, on the final tree: two tenants, own buckets, the whole workflow, plus a search of every response, every process's output, the processing worker's metrics and 55 tables for 3 secrets |
| Live AWS S3, R2, GCS, Azure | **skipped**: no credentials configured |
| Neon | **blocked**: no Neon API access. Never touched; see §8 |

## 4. Dataset reproducibility

**How versions are stored.**

- **Catalog versions.** Each Phase 9 catalog version is an asset with its own key `<ready_prefix>/<tenant>/<asset>`.
- **What is recorded at publication.** `storage_etag`, and `storage_version_id` (the S3/Azure version ID, or the GCS generation).
- **Binding.** The storage connection is the immutable binding (`storage_connection_id`, enforced by the `storage_binding_immutable` trigger).
- **Immutability.** The version columns are written only while the asset is `PROCESSING` (`AssetRepository.RecordStorageVersion`), so they never change after publication.
- **Snapshot members.** A Phase 10 snapshot member (`dataset_version_members`) names the asset, with its size, content type and SHA-256 copied at snapshot time.

**What changed.**

- **The read.** Validation now reads the member's version with `OpenRange(…, ReadCondition{VersionID: storage_version_id})`. Every adapter already honoured that condition for delivery: S3 `VersionId`, GCS generation, Azure `WithVersionID`.
- **The checksum evidence.** It was already version-specific: it joins the member's own asset and processing job, and requires the snapshot's checksum to equal that asset's. Confirmed, unchanged.

**Defined behaviour.**

| Storage | Content check reads | Bytes replaced out of band | Version deleted | Read fails (timeout, error) |
|---|---|---|---|---|
| Versioned bucket (version ID recorded) | that exact version | not seen: the recorded version is still read and passes | `CONTENT_MISSING` (examined, failed) | not examined; the check is `PARTIAL` or `NOT_ASSESSED`, never `PASSED` |
| Unversioned bucket | the version's own key | `CHECKSUM_MISMATCH` / `SIZE_MISMATCH` | `CONTENT_MISSING` | the same |
| Published before migration 000008 (no records) | the key | the same as unversioned | `CONTENT_MISSING` | the same |

**What a snapshot guarantees.**

- **Always.** It fixes which version of each object is a member, together with that version's size and SHA-256.
- **Immutable bytes.** It does **not** guarantee them on unversioned storage. Anyone who can write to the bucket can replace them; validation detects that and cannot prevent it.
- **Retention and deletion.** Semantics are unchanged: a member's version can still be deleted under its retention, and is then reported missing.
- **Docs.** `docs/datasets.md` says this now.

**Memory and transactions.**

- **Validation memory.** Validation reads members in pages (`DATASET_VALIDATION_BATCH`) and holds no member list.
- **Snapshot memory.** The snapshot copies members with one `INSERT … SELECT` and streams them back through SHA-256 without holding them.
- **Measured.** These were measured on a kept `catalog-bench` tenant of 1,000,000 objects in `object_storage_bench_rc`. The harness is a scratch test, not in the repository.

| Members | Snapshot (`CreateSnapshot`) | Bytes referenced | While it ran |
|---|---|---|---|
| 10,000 | 3.8 s | 483.8 MiB | the dataset's row was locked (`FOR UPDATE NOWAIT` refused); another dataset's row was free |
| 100,000 | 11.7 s | 4.9 GiB | same |
| 1,000,000 | 160.5 s | 48.6 GiB | same |

The lock is per dataset. Only membership changes and other snapshots of that dataset wait. At the default cap (`DATASET_MAX_SNAPSHOT_MEMBERS=5000000`), linear extrapolation gives about 13 minutes in one transaction, inside one HTTP request (P2). Recommendation: cap at about 350,000 behind a 60-second proxy, or make snapshots an asynchronous job like validations. Not changed here, because it changes the API's contract.

**Tests.** `TestContentValidationReadsTheSnapshotsVersions` covers:

- object replacement out of band;
- a new catalog version at the same path (the next snapshot names it, the old one keeps the old);
- historical validation of the old snapshot;
- a failed read (not examined, not passed);
- a deleted version (`CONTENT_MISSING`, naming the right asset).

`TestContentValidationModes` still covers the unversioned mismatch.

## 5. BYOS correctness and security

| Requirement | Result | Evidence |
|---|---|---|
| Every upload and object version keeps an immutable binding | ✓ | `storage_binding_immutable` trigger (probed on migrated data in the RC run); `TestStorageConnectionsInPostgres` |
| Background jobs resolve the bound connection | ✓ | processing, promotion, delivery, cleanup and deletion follow the asset's binding: `internal/service/processing/byos_test.go`, `internal/service/cleanup/connections_test.go`, e2e. Reconciliation listing scans cover the platform bucket only (RC D7, open) |
| A default change cannot redirect an existing asset | ✓ | new assertion in `TestBringYourOwnStorageEndToEnd`: after the default returns to `platform`, the earlier asset is still served from the tenant's bucket |
| Rotation and cache invalidation are safe under concurrent requests | ✓ after H3 | `TestRotationDuringConcurrentUseIsSafe` (8 goroutines × 200 requests across 5 rotations, `-race`); RC load run rotated with 682 files pending and none failed |
| Disabling blocks new work without rerouting | ✓ | e2e (a disabled connection's object stays in its bucket and is served; new uploads refused); `TestDisabledConnectionsDrainButPendingOnesDoNotResolve` |
| Deletion blocked by dependent assets or retained versions | ✓ | the dependents query counts every asset that is not `DELETED` (soft-deleted and non-current versions included) and completed uploads without an asset (RC D1); e2e |
| Tenant isolation in the service layer | ✓ | e2e cross-tenant 404s; `Locate` scopes rows by tenant; trigger refuses another tenant's connection |
| No credentials in responses, logs, queue or audit | ✓ | e2e searches every response, every process's output, the processing worker's `/debug/vars` and every column of every table (55) |
| Provider version, checksum, upload-session and deletion behaviour | ✓ on emulators | deletion by version (`TestVersionedObjectsAreDeletedByVersion`), GCS generation and composition (fake-gcs), Azure blocks and SAS (Azurite), S3 multipart and versioned delete (MinIO). Live clouds not run |
| Empty binding → platform fallback | removed (H2, H4) | stored bindings are never empty, so the fallback was not needed for backward compatibility. Request parsing still treats an empty **requested** connection as "none chosen" |
| Unsupported capabilities stay explicit | ✓ | `ErrUnsupportedProvider` for direct or multipart uploads a connection cannot take, signing without a key, and `ASSUME_ROLE` without a platform identity |

## 6. Ingestion benchmark: method and measurements

**Method.** `ingestion-loadgen` drives the real API, upload, processing and outbox workers, run natively with `EVENT_QUEUE=sqs`. It ran against a freshly migrated `object_storage_ingest_bench` with local MinIO, ClamAV and ElasticMQ, and measures at the client:

- entries per second;
- latency per call (claim, complete, PUT);
- failures and retries, including 429s.

Its samplers read:

- every server's `/debug/vars` (memory);
- PostgreSQL (connections, active queries, commits, rows, cache, size, outbox and processing backlog and their oldest age);
- `docker stats`;
- **new**: the active statements and their waits ten times a second.

The per-organization request limits were raised to `60000/1m` so that the pipeline is measured, not the throttle. The default limit's effect was measured separately in the RC run: 11.0 files/s published. The statement round trip and commit latency were measured from the same host with a scratch test.

**Measurements.**

| Run | Upload | Published | Avg. sessions waiting on row locks | Avg. sessions committing |
|---|---|---|---|---|
| 2,000 × 1–64 KiB, one tenant | 25.9 files/s; claim p50/p95 547/1,012 ms, complete 226/546 ms, PUT 55/162 ms | 10.4 files/s, end to end 197 s | 2.98 | 0.67 |
| 2 × 1,000, two tenants at once | 20.6 files/s each (41.2 together) | 5.8 files/s each | 3.03 | 0.81 |
| 1,000, processing concurrency 2 / 4 / 8 | 38.7 / 28.9 / 23.1 files/s | 13.2 / 12.7 / 9.6 files/s | — / — / 3.19 | — / — / 0.77 |
| Statement round trip, host → PostgreSQL | p50 0.54 ms, p95 1.58 ms | | | |
| Small write transaction commit | p50 3.3 ms, p95 4.3 ms | | | |

**Resource figures from these runs:**

- **PostgreSQL CPU.** Mean 64–85%, peak 116–169%.
- **ClamAV CPU.** Mean 12–55%.
- **Connections.** At most 45.
- **Failures.** 0 failed entries in every run. One status poll (`get_job`) failed once, of about 2,000 requests, in one two-tenant run.

Run-to-run variation is large on this laptop: the same 1,000-file run measured 28.9–39.8 files/s. Earlier measurements on the same machine are in `docs/release-candidate.md` §5.1. They cover mixed sizes (10.4 MiB/s), worker crash recovery, two tenants with rotation, and a 1,000,000-entry manifest (4,723 lines/s; 24 minutes to cancel).

**What the measurements show:**

- **Per-tenant serialization on two rows.** Roughly three sessions on average wait on `Lock:transactionid`, almost all on:
  - `ingestion_tenant_usage`, updated by every entry transition (`UPDATE ingestion_tenant_usage …`, 1.40 sessions);
  - `tenant_usage`, locked by every upload reservation (`INSERT INTO tenant_usage … ON CONFLICT`, 1.06) and by every publication (0.28).
- **The cost is the commit, not the queries.** The counts under the reservation lock use partial indexes and take tens of microseconds. A reservation holds the row through four round trips and its commit, about 5.5 ms. So a tenant's transactions on one usage row are bounded at a couple of hundred per second, and each file needs several.
- **The job and dataset rows are masked.** They are locked after the usage row, so they do not show as waits. The job row is the next limit for a single job.
- **More processing workers make it worse.** Publication takes the same tenant rows.
- **A second tenant adds throughput.** It has its own rows.

**Optimizations.** None of the candidates is implemented, because each changes how hard quotas and counters are kept and needs its own review and tests. They are listed in `docs/ingestion.md`, "Where the database's time goes":

1. **Reserve quota once per claim** instead of once per grant. This takes the lock on `tenant_usage` up to 32× less often.
2. **Fold the two counter changes of a publication** into one transaction, or into one batch per worker poll.
3. **Stripe the per-tenant counters.** Admission locks all stripes, so hard limits stay exact.
4. **Fold decrements asynchronously.** That is safe for hard limits, but usage figures lag.

Changing processing concurrency is not an optimization (measured above). One thing was fixed in the tooling: the load generator now retries 429s instead of stranding entries.

**Not benchmarked:**

- retry transitions under load (no fault injection in the load generator; correctness is covered by integration tests);
- several API or worker instances;
- more than two tenants in one run;
- anything at production scale.

Nothing here supports a capacity claim beyond these numbers.

## 7. Physical small-file packing

- **Status: not implemented.** Compaction (`internal/service/compaction`, `cmd/compaction-worker`) is metadata compaction only. It prunes 15 kinds of old records (succeeded tasks, part rows, idempotency keys, expired link hashes, published outbox events, audit past retention and so on) in bounded `SKIP LOCKED` batches. It never reads, writes or moves object bytes.
- **What exists for packing.** `internal/infrastructure/compaction/archive_packer.go` and `manifest_index.go` contain only a `package` clause.
- **Today's model.** Every small file is its own object (`<ready_prefix>/<tenant>/<asset>`). No existing contract depends on packing, so nothing was built here.

**Recommended design, as a separate milestone:**

- **Immutable packs.** A pack is a write-once object holding many small versions. Its manifest (pack ID, version, connection, offset, length, SHA-256 of each member and of the pack) is stored in PostgreSQL and inside the pack, and is never edited; a new pack replaces an old one.
- **Logical to physical.** A version gains an optional `(pack_id, offset, length)` beside its own key. Its identity, catalog version, size and SHA-256 do not change. Packing never applies across storage connections; a pack lives on the version's own binding.
- **Reads.** Delivery and validation read `offset..offset+length` with a range read pinned to the pack's version or ETag, and verify the member's SHA-256 (full reads) or the pack's recorded checksums. Range requests within a member become offsets within the pack.
- **Deletion and retention.** Deleting a packed version removes its pack row, making it a tombstone in the pack, at once. A pack is rewritten or deleted only when its live fraction falls below a threshold and no member is held (legal hold, retention, snapshot reference, quarantine evidence). Garbage is reclaimed by repacking, never by editing a pack in place.
- **Leases, retries, recovery.** Packing runs as leased jobs like processing:
  1. write the pack;
  2. verify every member's checksum from the pack;
  3. publish the pack row and the members' pack pointers in one transaction;
  4. only after a grace period, delete the members' loose objects.
  
  A crash before step 3 leaves an unreferenced pack, which the orphan sweep removes. A crash after step 3 leaves loose copies, which are deleted later.
- **Consistency.** Snapshots name versions, not storage locations, so they stay valid. Validation and delivery follow the pointer. Rollback means clearing the pointers; the loose objects are still there until the grace period ends.
- **Publication safety.** Readers prefer the loose object while it exists, so publishing a pack never creates a window without readable bytes.

## 8. Database migrations and local environment

- **Migrations.** None added: 000001–000015 (`docs/release-candidate.md` §7). The changes above need no schema change: `assets.storage_version_id` exists since 000008, and binding columns are `NOT NULL DEFAULT 'platform'` since 000015.
- **Neon.** Not used. The `.env` database is Neon, and no Neon API key or `neonctl` is available to create a branch. Testing on the configured database itself was ruled out to avoid writing to a remote, possibly valuable database. Nothing was sent to Neon.
- **Local databases** (container `object-storage-postgres`, port 5435), all disposable:

  | Database | Used for |
  |---|---|
  | `object_storage_test` | integration suite |
  | `object_storage_migfresh` | e2e |
  | `object_storage_ingest_bench` | load runs (new) |
  | `object_storage_bench_rc` | snapshot sizing; holds the kept `snapshot-bench` tenant, 1,000,000 objects, about 3 GB |

  `object_storage_migverify` and `object_storage_migverify2` are from the RC run. Nothing valuable was truncated or reset.
- **Services.** Local MinIO, ClamAV, ElasticMQ, fake-gcs-server and Azurite via `server/docker-compose.yml`. A MinIO user `e2e-byos` serves as the tenants' credential in e2e and load runs.

## 9. Frontend gaps

`client/` still holds only `.gitkeep`. The frontend is a separate, unstarted product component; no framework was introduced. The screens and their APIs are listed in `docs/release-candidate.md` §10. This milestone adds two needs:

- **Dataset version view.** For each member, show whether its version is pinned by provider version or by key only (unversioned storage), and show the content check's coverage (examined, read, not examined). That way "passed" is never read as "bytes guaranteed".
- **Snapshot flow.** Long snapshots need a progress or retry UX. Better, the backend makes snapshots asynchronous first (P2).

## 10. Remaining work, by priority

1. **Live-cloud verification** of S3 (with `ASSUME_ROLE`), R2, GCS and Azure versioning. This needs credentials.
2. **Neon branch rehearsal of migration 000015.** This needs Neon API access.
3. **Per-tenant ingestion ceiling (P1).** Start with one quota reservation per claim, measured with `ingestion-loadgen -db` before and after.
4. **Processing lease renewal (RC R1).** Crash recovery currently waits out the 15-minute lease.
5. **Asynchronous or capped snapshots (P2).**
6. **RC D2, D11, D7, D4, R2, R5,** in the order of `docs/release-candidate.md` §11.
7. **Frontend:** sign-in, connections and uploads first.
8. **Physical small-file packing** (§7), as its own milestone.
9. **Cleanup** (RC §3.3): the 27 package-only stubs and `DATABASE_URL_UNPOOLED`.
