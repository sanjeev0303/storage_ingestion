# Phase 15: release candidate verification

Date: 2026-10-27. Tree: branch `perf/upload-pipeline-scale` at `4388762`, plus the uncommitted Phase 15 changes listed in section 12. Nothing in this report is carried over from an earlier report. Each result below was produced on this tree in this phase, with the command shown.

## 1. Environment

| Part | Version, setup |
|---|---|
| Go | `go1.25.0 linux/amd64`: container `golang:1.25` with `GOTOOLCHAIN=go1.25.0`, `go.mod` says `go 1.25.0`. The host has Go 1.26.0, used for nothing here. |
| Node, npm | Node v26.10.0, npm 11.19.1 (Windows host). The lockfile needs Node 22.22.2+ (22 line), 24.15+ or 26+; `client/README.md` now says so. |
| Services | `postgres:17`, `cgr.dev/chainguard/minio:latest`, `clamav/clamav:1.4`, `softwaremill/elasticmq-native:1.6.14`, `fsouza/fake-gcs-server:1.52.2`, `mcr.microsoft.com/azure-storage/azurite:3.34.0` (Docker Desktop, WSL 2) |
| App stack | API and nine workers in Docker (`storage-local/docker-compose.yml`, outside the repo), binaries built with Go 1.25.0, `CGO_ENABLED=0`. Console on `npm run dev`, port 3100. |
| Machine | 8 CPUs inside the Docker VM. C: has 7.3 GB free, which matters in section 8. |

Every Go command runs in the container `storage-verify`, which shares the network of `storage-app-net-1`. There, 127.0.0.1 forwards to the services: 9000 MinIO, 5432 PostgreSQL, 3310 ClamAV, 9324 ElasticMQ, 4443 fake-gcs, 10000 Azurite. The test settings are in `/tmp/p15/test.env`: `TEST_DATABASE_URL` (database `object_storage_test`), `TEST_S3_BUCKET=object-storage-it`, `TEST_CLAMAV_ADDRESS`, `TEST_SQS_ENDPOINT`, `TEST_GCS_ENDPOINT`, `TEST_AZURITE_ENDPOINT` with the public Azurite development key, `TEST_S3_COMPATIBLE_*` (bucket `object-storage-it-compat`) and the `S3_*` settings. The remote database in `server/.env` was never used, read or changed.

## 2. Verification matrix

States: **verified** (run on this tree, this phase), **earlier revision only** (evidence exists only for an older commit), **mocked only** (only msw or in-memory tests), **emulator** (fake-gcs or Azurite), **live provider** (real AWS, GCS, Azure or R2), **blocked** (could not be run here, with the reason).

| Area | State | Evidence |
|---|---|---|
| gofmt, vet, build, every `cmd/` | verified | §3 |
| Unit and integration tests | verified | §3: 823 passed, 0 failed, 6 skipped |
| Race detector, whole module | verified | §3 |
| Go end-to-end, real binaries over HTTP | verified | §3, `TestTwoTenantsThroughTheRunningStack` |
| Console: `npm ci`, typecheck, lint, unit tests, build | verified | §4 |
| Console journeys against the real API | verified, except the homepage | §5 |
| Homepage (marketing page) motion and contrast checks | failing, not fixed | §5 |
| Packing: crash, lease, rewrite, range and retirement failures | verified (MinIO, PostgreSQL) | §6 |
| Packing benchmark | verified, one machine | §7 |
| S3 adapter | verified against MinIO | §8 |
| S3 copy above 5 GiB | blocked: not enough disk | §8 |
| GCS adapter | emulator | §8 |
| GCS deletion of every generation | blocked: the emulator has no versioned buckets | §8 |
| Azure adapter | emulator | §8 |
| Azure deletion of every version | blocked: Azurite keeps no versions | §8 |
| AWS S3, R2, real GCS, real Azure | not run. No live-provider test exists, and none was added. | §8 |
| Migrations: fresh, no-op, concurrent, failure, recovery, baseline | verified | §9 |
| Earlier reports (`release-candidate.md`, `hardening-report.md`, `phase12-*`) | earlier revision only: `9459999`, `83a0301`, `bd9b7c1` | not used as evidence |
| Phase 13 packing | no earlier report exists | this report is the first |

## 3. Backend gates

All from `/src` in `storage-verify`, with `. /tmp/p15/test.env` and `GOFLAGS=-p=2`:

| Command | Result |
|---|---|
| `gofmt -l` on every `.go` file | 0 files. The working tree is CRLF (`core.autocrlf=true`) while the index is LF, so plain `gofmt -l` lists almost every file; the check runs on `tr -d '\r' < file \| gofmt -l`. One real misformat, `internal/service/catalog/reconcile.go` from `40b127f`, was fixed. |
| `go vet ./...` | clean |
| `go build ./...` and `go build ./cmd/<each>` (10 commands) | all build |
| `go test -count=1 -p 2 -json ./...` | 41 packages; 823 passed, 0 failed, 6 skipped |
| `CGO_ENABLED=1 go test -race -count=1 -p 1 -timeout 60m ./...` | No data race reported. 40 packages passed. `tests/integration` had one flaky failure (below). After the fix, that package alone passed under `-race`: 197 passed, 2 skipped, 502 s. |
| `E2E_DATABASE_URL=<fresh database> go test -count=1 -run TestTwoTenantsThroughTheRunningStack ./tests/e2e/` | PASS (31.2 s). The test also reports no secret in process output, metrics, responses or 57 tables. |

The six skips, each for a stated reason:

| Test | Why it skips |
|---|---|
| `azure TestAzuriteDeleteRemovesEveryVersion` | Azurite keeps no blob versions |
| `gcs TestEmulatorDeleteRemovesEveryGeneration` | fake-gcs-server cannot create a versioned bucket |
| `s3 TestMinIOLargeCopyAboveFiveGiB` | opt-in; blocked here (§8) |
| `e2e TestTwoTenantsThroughTheRunningStack` | needs `E2E_DATABASE_URL`; run separately above |
| `integration TestPackingBenchmark` | opt-in; run separately (§7) |
| `integration TestProviderListingsArePaginated/prefix_listing_of_sessions` | MinIO cannot list multipart sessions by prefix; `RECONCILIATION_SESSION_SCAN=database` covers it |

Regressions found and fixed on the way:

- **A failing job used up all its attempts in one cycle.** The claim-one-at-a-time loop from `4388762` claimed the same job again within the cycle as soon as it was due, so `TestIngestionSurvivesDatabaseAndStorageOutages` failed. `ClaimDue` now takes a `dueBy` cutoff (the cycle's start), and `RunOnce` claims only jobs due by then. New test: `TestOneCycleTriesAFailingJobOnce` (three jobs, a scanner that always fails, a clock that moves 70 s per scan). It makes 7 claims without the fix and 3 with it.
- **The integration package timed out under `-race`.** This was Go's default 10-minute limit, not a failure; it passes with `-timeout 60m` (197 passed, 2 skipped, 667 s).
- **`TestPostgresDeletionWorkflow` failed about one run in four, with or without `-race`.** Four concurrent requests use event IDs at `now`..`now+3ns`; when request 0 won, the later request reused its ID (`lifecycle_events_pkey`). Present since `40b127f` or earlier. The original reproduced within 40 runs; the fixed version passed 20 in a row. The fix is test-only.
- **The race run took Docker Desktop down.** The first full race run exhausted the 8 GB Docker VM, and the engine stopped. Docker was restarted, the stack and emulators were started again, and the verify container was capped at 3.5 GB. The run then completed, as above.
- **The Go end-to-end test was racy.** It looked for the staged object straight after the version showed AVAILABLE. Cleanup runs a moment later in the same pass, by design (`processing/service.go`, `cleanup`). The test now waits up to 10 s for the deletion; it still fails if the object stays.

## 4. Console gates

From `client/`, with the dev and production servers stopped first:

| Command | Result |
|---|---|
| `npm ci` | 0 vulnerabilities. `package-lock.json` and `package.json` have the same SHA-256 before and after. npm warns that `unrs-resolver`'s install script is not in `allowScripts`; nothing depends on it running. |
| `npm run typecheck` | exit 0 |
| `npm run lint` | exit 0 |
| `npm run test` | 58 files passed, 1 skipped; 401 tests passed, 2 skipped |
| `npm run build` | compiled, exit 0 |
| `REAL_API=1 REAL_EMAIL=… REAL_PASSWORD=… npx vitest run src/features/uploads/engine/real-api` | 2 passed: 1 MiB single and 25 MiB multipart reach AVAILABLE through the real API and MinIO |

The two skipped unit tests are that `real-api` file, which runs only with `REAL_API=1`; it was run above. It was stale: since the engine frees its queue slot once the bytes are stored, `run()` returns before processing ends, and the test now waits for `settled()` as the unit tests do.

## 5. Console journeys against the real backend

`npx playwright test` drives Chrome against the console on 3100, the API and workers in Docker, MinIO, ClamAV and PostgreSQL. No spec mocks the network. Logins are the development accounts in `client/.env.e2e`.

Final run on the settled tree: `npx playwright test`, 39 tests, **34 passed, 5 failed** (all five in `homepage.spec.ts`), 8.0 min.

| Journey | Spec | Result |
|---|---|---|
| Sign in, wrong password, redirect back, HttpOnly cookies only, sign out | `journeys.spec.ts` | passed |
| Tenant isolation: an object in Acme is not listed in Globex and is 404 by id there | `journeys.spec.ts` | passed |
| Roles: an auditor reads but is offered no changes; the API refuses what the UI hides | `journeys.spec.ts` | passed |
| Registration, duplicate address | `register.spec.ts` | passed |
| Storage connection: create on MinIO, test, rotate with a wrong secret, disable, delete | `storage.spec.ts` | passed (BYOS turned on for the local stack, §12) |
| Uploads: single and 20 MiB multipart, size and SHA-256 in the catalog | `uploads.spec.ts` | passed |
| Catalog download byte for byte; a deleted version offers no download | `catalog-download.spec.ts` | passed |
| Dataset, snapshot, validation report, lineage | `datasets.spec.ts` | passed |
| Accessibility (axe) of the console screens | `journeys.spec.ts` | passed |
| Homepage | `homepage.spec.ts` | 5 failing, see below |

The first run had 20 failures, all from tests that no longer matched the UI:

- **15 failures:** the password field gained a "Show password" button, so `getByLabel("Password")` matched two elements. These now use `exact: true`, or `/^Password/` on the registration form, where the label is "Password (required)".
- **The dataset test** looked for a field labelled "Path"; it is now "Path (required)".
- **The tenant-isolation test** depended on a `smoke.txt` object that the earlier data wipe removed. It now uploads its own object, checks the other organization by listing and by id, and deletes it.
- **The storage test** needs customer connections, which were off on the local stack.

The five homepage failures were not fixed in this phase. They test the marketing page's scroll motion, and Phase 15 changed none of that code.

1. The snapshot slider starts at its last stage when the test focuses it. Focusing scrolls the page, and the scroll-linked stage moves before the reduced-motion preference applies.
2. With motion on, the closing form's Bucket field is still hidden when the test fills it.
3. & 4. axe reports colour contrast on the readiness rows while they fade in. This happens in both themes, only with motion on.
5. The lifecycle strip (`[data-flow-line]`) is not found.

These need a look at the landing page's motion, which is backlog item 1.

## 6. Packing reliability

An audit of `internal/service/packing`, `postgres/pack_repository.go`, `infrastructure/compaction` and the range readers found these defects. All are fixed, each with a test that fails without the fix:

| Defect | Fix | Test |
|---|---|---|
| A provider that ignores `Range` (or sends more than asked) could pass a neighbour's bytes on as a packed asset's | `rangeread.CheckContentRange` and `rangeread.Exact`; the S3, GCS and Azure `OpenRange` check what came back | `rangeread/exact_test.go`, `s3/range_test.go` (an httptest server that ignores `Range`) |
| A rewrite whose old pack object was lost failed every cycle, for ever, and the deletion waiting on it never completed | `FailPack` excludes the entry from the old pack | `TestPackingRewriteOfADamagedPackDoesNotLoop` |
| A writer paused past its lease could delete a pack another worker had since published (step 2 deletes whatever is at the pack's keys) | `RenewPack` before deleting | covered by the fencing below |
| Every claim by one worker had the same lease owner, so a worker that claimed its own expired pack again was not fenced from its earlier attempt | per-claim owner `<worker>#<attempt>`; every build step passes `p.LeaseOwner` | `TestPackingRecoversFromCrashesAndDuplicateWorkers` (updated) |
| A rewrite that moved an entry before its standalone copy was retired left that copy stored for ever | `SourcesToRetire` includes MOVED entries of first-generation packs | `TestPackingRetiresTheStandaloneCopyOfAMovedEntry` |
| A rewrite checked its entries against the asset's own object, not the old pack | `stillRecorded` compares against the old pack when it is a rewrite | the rewrite tests above |

All packing integration tests pass (MinIO, PostgreSQL). The integrity checks are unchanged: per-entry SHA-256, whole-pack SHA-256, layout and size check, read-back verification, manifest read-back, and reads pinned to ETag or version. A pack is still visible to readers only once `PublishPack` commits, and only entries whose asset is still the same bytes are published.

Audit findings not changed in this phase (backlog 3–6):

- Leases depend on the database clock and the worker clocks agreeing.
- `BuildingPacks` takes tenants in order, not fairly.
- Verification uses the current `MaxEntries`, not the value at planning time.
- A `ReleaseAsset` racing a rewrite was not confirmed either way.
- Retirement versus holds and already-issued presigned URLs: a URL issued before retirement fails once the source is deleted.

## 7. Packing benchmark

```
PACKING_BENCH=1 PACKING_BENCH_OBJECTS=<n> PACKING_BENCH_BYTES=<size> \
  go test -count=1 -timeout 30m -v -run TestPackingBenchmark ./tests/integration/
```

The test writes n objects to MinIO, then streams them into one pack with 16 sources read ahead, as the worker does. It reads the pack back and verifies it, then times n reads of the standalone objects and n ranged reads of the pack, with 1 and 16 workers. Phase 15 added a heap sampler (`runtime.ReadMemStats` every 5 ms) around writing and verifying the pack.

**What was measured first.** Every request from the Go SDK took 44 ms, standalone or packed, write or read. A probe placed the stall in the local Docker forwarder, not the code:

| Path | p50 |
|---|---|
| SDK GetObject, through socat | 44.0 ms |
| curl GET, through socat | 2 ms |
| SDK GetObject, direct to MinIO | 0.75 ms |

The socat forwarders lacked `nodelay`, so small requests waited on a delayed ACK. This also slowed the local upload tests of earlier phases. With `nodelay` on both sides (§12) the numbers became:

| Objects × size | Pack write | Verify | Peak heap | Allocated | Standalone read, 1 / 16 workers (p50, p95) | Packed read, 1 / 16 workers (p50, p95) |
|---|---|---|---|---|---|---|
| 500 × 4 KiB | 277 ms, 1,803 objects/s | 8 ms | 8.7 MiB | 31.7 MiB | 1.08 / 3.45 ms; 1.58 / 9.78 ms | 1.82 / 6.55 ms; 2.78 / 21.61 ms |
| 5,000 × 4 KiB | 2.98 s, 1,679 objects/s | 138 ms | 40.3 MiB | 311.6 MiB | 1.78 / 3.99 ms; 3.10 / 11.11 ms | 3.05 / 19.75 ms; 7.73 / 75.14 ms |
| 2,000 × 64 KiB | 15.17 s, 132 objects/s (8.2 MiB/s) | 639 ms | 48.1 MiB | 700.4 MiB | 2.68 / 9.62 ms; 6.10 / 33.09 ms | 3.06 / 17.48 ms; 8.54 / 185.61 ms |

Reading of these numbers:

- On this single-node MinIO, many concurrent ranged reads of one pack object are slower than reads of separate objects, and the gap grows with concurrency. At 16 workers the packed read rate was 51%, 15% and 24% of the standalone one. The cause was not measured; it is the main question before relying on packing for read-heavy tenants (backlog 2).
- Peak heap stays bounded as packs grow. From a 2 MiB pack to a 20 MiB one it rises 8.7 to 40.3 MiB, because the upload buffer grows until it reaches its 16 MiB part size. From 20 MiB to 125 MiB it rises only to 48.1 MiB. The bound is in `packing.md`, "Memory".
- One machine, one MinIO drive, one writer. These are not capacity figures, and nothing here supports a claim about billions or trillions of objects.
- Database lock contention and CPU per object were not measured separately.

## 8. Storage providers (BYOS)

| Provider | Unit | Emulator | MinIO | Live |
|---|---|---|---|---|
| AWS S3 | `s3` tests: copy part limits, errors, presigning, ranged reads | n/a | copy above the single-copy limit in parts, replaced source, cancelled copy, ranged reads; all passed | not run |
| S3 copy above 5 GiB | n/a | n/a | blocked | not run |
| S3-compatible | `registry` endpoint and region rules | n/a | `TestMinIOAsAnS3CompatibleConnection`, `TestBringYourOwnStorageEndToEnd`, Go e2e, console journey; all passed | not run |
| Cloudflare R2 | `TestR2SignsWithRegionAutoAtTheAccountEndpoint` | n/a | n/a | not run |
| Google Cloud Storage | names, errors, signing rules | fake-gcs: objects and generations, composite multipart, signed part upload; deleting every generation blocked | n/a | not run |
| Azure Blob | block ids, SAS redaction, credentials | Azurite: signed uploads and reads, bounded streaming blocks, block uploads, SAS read-only; deleting every version blocked | n/a | not run |

The S3 copy above 5 GiB (`TEST_S3_LARGE_COPY_BYTES`) writes the object and then copies it, so it needs about twice its size free. MinIO's disk is the WSL virtual disk on C:, and C: had 7.3 GB free, so the run could have filled the system drive. It was not run.

Checks the brief asked for:

- **A connection change never moves existing assets:** `TestConnectionChangesKeepExistingWorkReachable`, `TestClientsAreCachedAndRebuiltOnRotation`, `TestRotationDuringConcurrentUseIsSafe`. All passed.
- **A failed connection never blocks other tenants:** `TestUnreachableConnectionRetriesWithoutSwitchingStorage` (no fallback to the platform bucket). New in this phase: `TestOneTenantsOutageDoesNotHoldBackAnother`, in which one tenant's provider is down and another tenant's upload finishes in the same cycle. Passed.
- **No credential leaks:** `TestErrorsNeverCarryTheSASToken`, `TestSensitiveDataStaysOutOfLogsAndAudit`, `TestCredentialsSealedForAnotherConnectionDoNotOpen`, and the Go e2e search for the tenants' secret. All passed.
- **Live-provider tests:** none exist, and none were added. Running them needs accounts and must stay opt-in.

## 9. Migrations

`storage-local/phase15/migration-tests.ps1` drives `server/scripts/migrate.ps1` against throwaway databases on the local PostgreSQL. All seven checks passed:

| Check | Result |
|---|---|
| Fresh database | 17 of 17 recorded |
| Run again | 0 applied |
| Three runs at once | 17 applied across the three, no errors, 17 recorded |
| A failing migration | the run fails, nothing recorded, no table left |
| The failing file fixed | applies, recorded |
| `-Baseline 6` | marks 000001–000006 |
| `-Baseline abc` | refused |

The pack tables' constraints, indexes and the `tr_pack_entries_immutable` trigger were listed from the migrated database (`migration-tests.log`). Rollback is documented for 000014 and 000017. Rolling back 000017 after sources were retired loses data: those assets exist only in packs, and there is no unpack tool (backlog 7). The Neon database was not touched.

## 10. API capability gaps

- `client/docs/api-capability-matrix.md` was brought up to date. `GET /uploads/{id}` returns the upload's `asset`; multipart `complete` takes the SHA-256; the uploader no longer looks objects up by path; the "asset id after an upload" gap is closed; preview needs `DELIVERY_PROXY_ENABLED`.
- The queue said "Catalog says SCANNING" for a status the upload reports, not the catalog. It now says "Held in quarantine until the checks finish" and "Available in the catalog".
- **No simulated features.** Resume after a reload uses IndexedDB, and "Imports started in this browser" uses `localStorage`. Both work over real API features, and both say on screen that they are local to the browser.
- No endpoints were added. Still missing and still stated in the matrix: listing uploads or imports, worker and queue health, reconciliation and packing status, object-to-dataset lookup, reading retention policies and holds.

## 11. Security and operations

An audit of the console proxy, the session routes and the API middleware found two HIGH defects. Both are fixed:

- **Spoofed client addresses.** With `AUTH_TRUST_PROXY_HEADERS=true` the API took the first `X-Forwarded-For` entry, which the caller controls. That made per-address sign-in throttling and audit addresses spoofable. It now takes the last entry, the one the trusted proxy added (`middleware/security.go`). The console sends exactly one address: the right-most entry after `TRUSTED_PROXY_HOPS` (default 0, the socket address), and nothing that is not an address. Refresh calls forward it too.
- **Every console user throttled as one address.** With the defaults, the API saw every user as the console's own address, so one person's failed sign-ins throttled everyone. The local stack now sets `AUTH_TRUST_PROXY_HEADERS=true`, and `server/.env.example` and `docs/security.md` say to set it whenever the API is reached only through the console.

Lower-severity fixes:

- **MEDIUM:** the proxy read request bodies of any size into memory. Bodies are now capped at 17 MiB (413), and anonymous requests are refused before the body is read.
- **LOW:** download tokens and `s*t_` tokens are now redacted from console logs.
- **LOW:** decoded path segments are now encoded again, so an encoded `?` cannot change the API URL.
- **LOW:** signing out with an expired access cookie now ends the server session (new test).

Not changed (backlog 8):

- A spent refresh token is honoured for 30 s.
- Graceful shutdown uses up a job attempt.
- The API has `ReadHeaderTimeout` (5 s) but no deadline for slow request bodies.
- No tool re-seals credentials after a key rotation.
- The inbox, validation and import workers have the same shared lease-owner pattern that this phase fixed for packing.

## 12. Changes in this phase

All uncommitted.

**Server:**

| Files | Change |
|---|---|
| `internal/interfaces/processing_job_repository.go`, `postgres/processing_job_repository.go`, `repository/memory/processing_job_repository.go`, `service/processing/service.go` | `ClaimDue` and the cycle cutoff |
| `infrastructure/storage/rangeread/exact.go` (new); `s3/presigner.go`, `gcs/gcs.go`, `azure/azure.go` | exact ranged reads |
| `postgres/pack_repository.go`, `service/packing/build.go` | packing fixes from §6 |
| `server/middleware/security.go` | right-most `X-Forwarded-For` |
| `service/catalog/reconcile.go` | gofmt |

New or updated tests: `processing/scan_test.go`, `processing/byos_test.go`, `tests/integration/lifecycle_test.go` (flaky event ID), `rangeread/exact_test.go`, `s3/range_test.go`, `tests/integration/packing_test.go`, `tests/integration/packing_bench_test.go` (heap sampler), `tests/e2e/workflow_test.go`, `tests/unit/security_middleware_test.go`.

Docs: `docs/packing.md`, `docs/security.md`, `.env.example`, this report.

**Console:**

| Files | Change |
|---|---|
| `src/server/auth.ts`, `src/app/api/proxy/[...path]/route.ts`, `src/server/log.ts`, `src/app/api/session/logout/route.ts` | the security fixes in §11 |
| `src/features/uploads/components/queue-table.tsx` | status wording |

New or updated tests: `route.test.ts` (proxy), `logout/route.test.ts`, `real-api.test.ts`, and the e2e specs `helpers.ts`, `journeys.spec.ts`, `register.spec.ts`, `datasets.spec.ts`.

Docs: `README.md` (Node versions), `docs/security.md`, `docs/api-capability-matrix.md`, `.env.example`.

**Local stack only** (`storage-local/docker-compose.yml`, not in the repo):

- `AUTH_TRUST_PROXY_HEADERS=true`.
- BYOS on with a credential key generated for this stack, and `STORAGE_ALLOW_PRIVATE_ENDPOINTS=true`, which the API accepts only with `APP_ENV=development`.
- `nodelay` on every socat forwarder.
- The fake-gcs and Azurite forwarders written into the file; before this they were added by hand.

## 13. Backlog and risks

1. The homepage motion tests (§5): decide what the slider should show on focus, and wait for the fade before measuring contrast or start without it.
2. Concurrent ranged reads of one pack are slower than separate objects on MinIO (§7). Measure on a real provider before relying on packing for read-heavy tenants.
3. Packing leases depend on the clocks agreeing. Consider database time for lease checks.
4. `BuildingPacks` fairness across tenants.
5. Verification should use the `MaxEntries` recorded at planning time.
6. Confirm or rule out `ReleaseAsset` racing a rewrite.
7. No unpack tool, so rolling back 000017 after retirement loses data.
8. The security items listed at the end of §11.
9. Upload engine unit tests still mock `/catalog/lookup`, versions and `/assets/{id}`, which the engine no longer calls. msw's default `warn` policy would let a stray call reach the network, so set `onUnhandledRequest: "error"` before removing them.
10. The S3 copy above 5 GiB, live-provider runs, and versioned deletion on real GCS and Azure: all still unverified.
11. The host has 7.3 GB free on C:. Big local tests need room first.
