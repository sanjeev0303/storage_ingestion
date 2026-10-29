# Retention, deletion, orphan cleanup and compaction (local runbook)

| Piece | Where | What it does |
|---|---|---|
| Member routes | API | `DELETE /api/v1/assets/{id}` (soft delete) and `POST /api/v1/assets/{id}/restore` with `assets.delete`, `GET /api/v1/assets/{id}/lifecycle` with `assets.read`; `PUT /api/v1/retention/policies`, `POST /api/v1/legal-holds`, `DELETE /api/v1/legal-holds/{id}` with `retention.manage` (`docs/security.md`) |
| `cleanup-worker` | `cmd/cleanup-worker` | retention evaluation, the deletion workflow, inventory scans and orphan sweeps, missing-object checks |
| `compaction-worker` | `cmd/compaction-worker` | metadata compaction in bounded batches |
| `lifecycle-admin` | `cmd/lifecycle-admin` | operator CLI: policies, legal holds, deletions, restore, inspection, requeue, scan and candidate reports |
| `reconciliation-worker` | Phase 5.3.5 | abandoned multipart sessions (expiry, leftover provider sessions, stale part rows) |
| `processing-worker` | Phase 5.4 | rejected-upload purge after quarantine retention, now honouring legal holds and tenant policy |

Every destructive step that is not an explicit request defaults to report-only. These need an explicit setting:

- expiring assets: `LIFECYCLE_EXPIRATION=delete`;
- deleting proven orphans: `LIFECYCLE_ORPHAN_ACTION=delete`;
- deleting objects with no record at all: `LIFECYCLE_ORPHAN_UNRECORDED=delete`;
- compaction: `COMPACTION_ENABLED=true`.

A deletion requested by an owner or an operator is itself the authorized action.

## Retention policies

A policy version is stored for a scope: `TENANT`, `DATASET` (assets carry an optional dataset chosen at upload, `"dataset"` in the multipart start request) or `ASSET`. Versions are append-only. The version in effect at a moment is the latest whose `effective_from` has passed, so a change can be scheduled for later. Each change is also a `policy_changed` lifecycle event.

| Setting | Meaning |
|---|---|
| `min_retention` | the asset may not be permanently deleted earlier than this long after creation |
| `max_age` | the asset expires (deletion is scheduled) this long after creation; `0` = never |
| `deletion_grace` | how long a soft-deleted asset stays restorable before removal starts |
| `quarantine_retention` | how long a rejected upload is kept for investigation |

**Precedence:**

1. For each setting, the most specific policy that sets it wins: asset, then dataset, then tenant, then the platform default from configuration.
2. Mandatory platform floors then raise anything below them:
   - `RETENTION_MIN_RETENTION_FLOOR`;
   - `RETENTION_DELETION_GRACE_FLOOR`;
   - `QUARANTINE_RETENTION` (also the default).

   A policy below a floor is refused when stored, and clamped at evaluation if a floor is raised later.
3. Expiry never comes before the retention period ends: `expires_at = created_at + max(max_age, min_retention)`. Expiry is inclusive of its exact moment.
4. A legal hold overrides every policy: nothing in its scope is permanently deleted while it is active.

**Reference times:**

- Everything is computed in UTC from `assets.created_at`, a server timestamp set when the asset is created, never a client value.
- Durations are exact, not calendar days, so time zones and daylight saving cannot move an expiry.
- One evaluation pass uses one reference time for all its assets.

## Legal holds

A hold covers a tenant, a dataset or one asset until it is released. It:

- stops retention expiry from scheduling;
- stops a scheduled deletion from starting. The final start is a single SQL statement that checks for active holds, so a hold committed before that moment is always honoured;
- stops orphan sweeps in its scope;
- stops the processing worker's quarantine purge.

A hold placed after removal has started (`IN_PROGRESS`) cannot stop that deletion: objects are already going. Owners never learn about holds. A protected deletion is shown to them as `SCHEDULED`.

## The deletion workflow

| Brief's state | Here |
|---|---|
| Active | asset `AVAILABLE` (or `REJECTED`, …) with no live operation |
| Deletion requested / soft-deleted | one transaction: asset → `SOFT_DELETED` (never delivered from this moment), operation `SCHEDULED` with `purge_after = now + grace`, `deletion_requested` event |
| Pending permanent deletion | `SCHEDULED` until `purge_after` |
| Protected by retention or legal hold | `PROTECTED`, with the reason and the next check (`next_attempt_at`, at most `LIFECYCLE_RECHECK_INTERVAL` away) |
| Deletion in progress | `IN_PROGRESS` (claimed under a lease, attempt counted) |
| Deletion failed / awaiting retry | `RETRY_WAIT` (backoff with jitter), then `FAILED` after `LIFECYCLE_MAX_ATTEMPTS` or a permanent error; `lifecycle-admin requeue` restarts it |
| Storage deletion confirmed / metadata cleanup pending | `STORAGE_DELETED` (`storage_deleted_at` set) |
| Fully deleted | `COMPLETED`: one transaction tombstones the asset (`DELETED`), revokes its download links and records `deletion_completed` |
| Restored | `CANCELLED`: only from `SCHEDULED`/`PROTECTED`, never once removal started |

**Before anything is removed**, a fresh check runs on the current state, never on what the request saw:

- the asset is still `SOFT_DELETED`;
- no active legal hold (tenant, dataset or asset);
- the retention period (current policy) has ended;
- rejected-upload evidence retention (`quarantine_records.retain_until`) has ended;
- no active processing job and no upload in flight;
- no other live record points at the same objects (another asset, an active job's promotion key, an upload session, an open quarantine hold).

Then:

- **Objects removed:** exactly the asset's own objects, derived from its tenant and IDs, never from a client or a listing. They are its delivery copy, its quarantine source, and any other promotion copy its job recorded. A recorded version ID is deleted by that ID first. Then each key is deleted in full: on a bucket that keeps versions, every version and delete marker of exactly that key goes too (S3 `ListObjectVersions`, GCS generations, Azure blob versions). This includes versions an earlier attempt, or code before Phase 12, only hid behind a delete marker, so the key is deleted even when a HEAD already answers not-found. Keys are generated per upload and per asset, so every version of one belongs to that record. A key that only shares a prefix is never touched.
- **Confirmation:** each removal is confirmed with a HEAD (or version HEAD) that must answer not-found. The deletion is recorded as `STORAGE_DELETED` only after every key is confirmed; a failed version delete leaves it `IN_PROGRESS` for a retry.
- **No prefix deletes:** there is never a prefix or bucket delete.

**Crash recovery:**

- Every step is idempotent and the lease protects concurrent workers.
- A crash during removal leaves `IN_PROGRESS` under a lease that expires. The next worker re-runs removal, finds what is already gone, and continues.
- A failure after storage confirmation stays `STORAGE_DELETED`, and the retry only finishes the metadata.
- Completion is never recorded before both steps are done.

## Orphans and missing objects

The inventory scan lists `quarantine/<tenant?>/` and `<prefix>/<tenant?>/`:

- page by page with the provider's continuation token;
- `LIFECYCLE_SCAN_PAGE_SIZE` objects, `LIFECYCLE_SCAN_PAGES` pages per cycle;
- the cursor is saved after every page in `inventory_scans`, with a lease, so a restarted worker resumes rather than restarting.

Each object is classified by its records, never by name alone:

| Category | Authority | Default action |
|---|---|---|
| `DEAD_PROMOTION_COPY` | asset `REJECTED`, or processing job `FAILED` | report (`LIFECYCLE_ORPHAN_ACTION=delete` removes) |
| `LEFTOVER_QUARANTINE_SOURCE` | asset `AVAILABLE` and job `SUCCEEDED` | report / delete |
| `DELETED_ASSET_OBJECT` | asset tombstoned | report / delete |
| `EXPIRED_FAILED_UPLOAD` | session `FAILED`, no asset, `LIFECYCLE_FAILED_UPLOAD_RETENTION` passed | report / delete |
| `NO_UPLOAD_SESSION`, `NO_ASSET` | no record at all | report; deleted only with `LIFECYCLE_ORPHAN_UNRECORDED=delete` as well, since missing metadata alone does not prove ownership (another environment may share the bucket) |

Never candidates: objects younger than `LIFECYCLE_ORPHAN_MIN_AGE`; active uploads; processing or promotion in progress; held assets (their copy is reused on requeue); soft-deleted assets (the deletion workflow owns them); rejected sources (the quarantine purge owns them); aborted and expired uploads (multipart reconciliation owns them); anything uncertain.

A candidate is swept only when all of these hold:

- it was seen in at least `LIFECYCLE_ORPHAN_OBSERVATIONS` scans (a replayed page of the same scan counts once);
- it was first seen at least `LIFECYCLE_ORPHAN_SAFETY_INTERVAL` ago;
- its latest sighting was in a completed scan;
- a fresh check finds it still present, still the same object (same ETag), still unowned, and not under a legal hold.

A changed ETag clears it, so a new object starts over.

Scan failures are handled conservatively:

- A failed page keeps the scan resumable; after `LIFECYCLE_MAX_SCAN_ERRORS` in a row the scan ends `FAILED`.
- A failed or unfinished scan never makes anything due.
- Objects never listed are never candidates.

**Missing objects.** The check heads the expected object of each AVAILABLE, QUARANTINED and PROCESSING asset, in batches with a saved cursor. A missing object is reported:

- as an `object_missing` event, recorded at most once a day per asset;
- as a metric and a log line.

It is classified as one of: an AVAILABLE asset without its delivery object (inconsistent), awaiting processing (verification will reject it), or a held asset without its source. A storage error is no evidence and is checked again next pass. Metadata is never deleted.

## Metadata compaction

| Rule | Class (default retention) | Removes |
|---|---|---|
| `reconciliation_tasks` | operational (30d) | succeeded tasks |
| `upload_parts` | operational | part rows of VERIFIED uploads |
| `upload_idempotency_keys` | idempotency (7d) | the key only, of finished sessions (never of an upload in flight) |
| `download_tokens` | operational | expired links (hashes) |
| `orphan_candidates` | operational | resolved candidates (outcome stays in events) |
| `inventory_scans` | operational | finished scans, except each prefix's latest completed one and any an open candidate refers to |
| `compaction_runs` | operational | run records |
| `outbox_events` | operational | published events, except each aggregate's latest (the next event's sequence continues from it) and any an open dead letter refers to (Phase 6, `docs/events.md`) |
| `event_inbox` | operational | consumer records of completed or skipped events; keep this above SQS's 14-day message retention |
| `reconciliation_findings`, `delivery_audit_events`, `processing_events`, `lifecycle_events` | audit (365d) | records past audit retention |
| `dead_letters`, `event_discrepancies` | audit | closed dead letters with the operator's resolution, and event reconciliation findings |

**Floors:** audit 90 days, operational and idempotency 1 day. Lower settings are refused at startup.

**Never compacted:**

- assets, including tombstones (proof of deletion and of ownership);
- deletion operations;
- processing jobs (publication evidence);
- quarantine records;
- legal holds;
- retention policies;
- upload sessions;
- any live task.

**How each batch runs:**

- Each batch is one short statement over an indexed, ordered selection (`ORDER BY age, id LIMIT n FOR UPDATE SKIP LOCKED`). It never waits on rows other work holds and never takes table locks.
- `COMPACTION_MAX_BATCHES` bounds a run, and the next run continues where it stopped.
- `COMPACTION_BATCH_PAUSE` lets other traffic in between batches.
- Each run is recorded in `compaction_runs`.
- No `VACUUM FULL` or other blocking maintenance is run; plain autovacuum reclaims the space.

**Partitioning:** not introduced. The audit tables are small locally, and batched deletes over `(created_at, id)` indexes keep compaction cheap. Monthly range partitions on `created_at` would turn audit expiry into dropping partitions. That is worth it once these tables reach tens of millions of rows, at the cost of partition maintenance and primary keys that must include the partition key.

## Operator controls

| Control | |
|---|---|
| `cleanup-worker -once` / `-dry-run` / `-tenant T` / `-phases retention,deletions,scan,sweep,missing` / `-max-deletions N` | one cycle, JSON report; a dry run writes nothing (no claims, candidates, cursors or events) |
| `compaction-worker -once` / `-dry-run` / `-rules a,b` / `-list` | |
| `LIFECYCLE_BATCH_SIZE`, `LIFECYCLE_CONCURRENCY`, `LIFECYCLE_MAX_DELETIONS`, `LIFECYCLE_MAX_ATTEMPTS`, `LIFECYCLE_BACKOFF_*`, `LIFECYCLE_LEASE_DURATION` | bounds and retries |
| `lifecycle-admin` | `policy-set`, `policy-history`, `hold-place`, `hold-release`, `holds`, `delete`, `restore`, `inspect`, `requeue`, `scans`, `candidates`, `events`, `compaction-runs`; every change audited with `-actor` |

**Shutdown:** a worker stops claiming on SIGINT/SIGTERM. Unfinished claims are not written; their leases expire and another worker repeats the idempotent step.

## Observability

Metrics (expvar `/debug/vars` with `METRICS_ADDR`; `lifecycle` for the cleanup worker, `compaction` for the compaction worker):

| Group | Metrics |
|---|---|
| Retention | `retention_evaluated`, `retention_expired_scheduled`, `retention_expiry_reported`, `retention_protected_by_hold` |
| Deletions | `deletions_requested_*`, `deletions_completed`, `deletions_protected`, `deletions_retried`, `deletions_failed`, `deletions_cancelled`, `deletions_restored`, `deletions_lease_lost` |
| Storage | `storage_objects_deleted`, `storage_delete_failures` |
| Inventory | `inventory_pages_scanned`, `inventory_scans_completed`, `inventory_scans_failed`, `inventory_scan_errors` |
| Orphans | `orphan_candidates_marked`, `orphan_objects_deleted`, `orphan_candidates_cleared`, `orphan_candidates_reported`, `orphan_candidates_held`, `orphan_delete_failures`, `orphan_skipped_uncertain` |
| Missing objects | `missing_objects`, `missing_check_errors` |
| Compaction | `compaction_rows_<rule>`, `compaction_last_ms_<rule>`, `compaction_failures` |

Events (`lifecycle_events`: tenant, asset, operation, actor, reason, outcome, time; never URLs, credentials or content):

- deletions: `deletion_requested`, `deletion_protected` (when it starts or changes), `deletion_started`, `storage_deleted`, `deletion_completed`, `deletion_failed`, `deletion_cancelled`, `deletion_requeued`, `restored`;
- policies and holds: `policy_changed`, `hold_placed`, `hold_released`;
- orphans and missing objects: `orphan_marked` (first sighting), `orphan_deleted`, `orphan_cleared`, `object_missing`.

## Running it locally

```powershell
docker compose up -d                                   # postgres, minio, clamav
.\scripts\migrate.ps1                                  # through 000009
.\scripts\migrate.ps1 -Database object_storage_test    # for the integration tests
```

```bash
go build -o bin/ ./cmd/...
APP_ENV=development ./bin/api.exe &                     # member routes; sign in as in docs/security.md
SCANNER=clamav ./bin/processing-worker.exe &
./bin/cleanup-worker.exe &                              # report-only by default
./bin/compaction-worker.exe &                           # dry run unless COMPACTION_ENABLED=true
./bin/reconciliation-worker.exe &                       # abandoned multipart sessions

# A retention policy: keep 30 days, expire after 90, 7 days to restore
./bin/lifecycle-admin.exe policy-set -tenant local-tenant -scope TENANT -min-retention 30d -max-age 90d -grace 7d -reason "standard"
./bin/lifecycle-admin.exe policy-set -tenant local-tenant -scope DATASET -key finance -min-retention 2555d -reason "7-year records"

# Dry-run evaluation, then enforcement
./bin/cleanup-worker.exe -once -dry-run -phases retention
LIFECYCLE_EXPIRATION=delete ./bin/cleanup-worker.exe -once -phases retention

# Controlled deletion (a member with assets.delete), restore, and the worker run
curl -H @auth.hdr -X DELETE localhost:8080/api/v1/assets/$A
curl -H @auth.hdr -X POST localhost:8080/api/v1/assets/$A/restore
./bin/cleanup-worker.exe -once -phases deletions -max-deletions 10

# Legal holds
./bin/lifecycle-admin.exe hold-place -tenant local-tenant -scope ASSET -key $A -reason "case 42"
./bin/lifecycle-admin.exe hold-release -tenant local-tenant -id $HOLD

# Orphans: run scans; deletion needs repeated sightings and the safety interval
LIFECYCLE_ORPHAN_ACTION=delete ./bin/cleanup-worker.exe -once -phases scan,sweep
./bin/lifecycle-admin.exe candidates -status MARKED

# Compaction
./bin/compaction-worker.exe -once -dry-run
COMPACTION_ENABLED=true ./bin/compaction-worker.exe -once

# Inspect
./bin/lifecycle-admin.exe inspect -tenant local-tenant -asset $A
./bin/lifecycle-admin.exe events -tenant local-tenant
./bin/lifecycle-admin.exe scans
./bin/lifecycle-admin.exe compaction-runs
```

To simulate the failure cases:

| Case | How |
|---|---|
| Orphan object | put an object under `assets/<tenant>/<id>` of a deleted asset (`mc pipe`) |
| Missing object | remove an AVAILABLE asset's delivery object (`mc rm`) |
| Abandoned multipart upload | start an upload and upload a part without completing it; after `UPLOAD_SESSION_TTL_SECONDS` the reconciliation worker expires it and aborts the provider session |

Use a disposable database and bucket for destructive tries. The Phase 5.6 smoke run used `object_storage_smoke` and `object-storage-smoke`.

Tests:

```bash
go test ./internal/service/asset/ ./internal/service/cleanup/ ./internal/service/compaction/ ./tests/unit/
TEST_DATABASE_URL=... TEST_S3_BUCKET=object-storage-it TEST_S3_CREATE_BUCKET=true \
TEST_CLAMAV_ADDRESS=127.0.0.1:3310 go test ./tests/integration/
```

## Limitations

- Operator deletions stay CLI-only (`lifecycle-admin`). Policies and holds can also be set over the API by members with `retention.manage`.
- Versioned buckets were verified only on MinIO (`TestProviderVersionedDelete`). Removing GCS generations and Azure blob versions is implemented but unverified: fake-gcs-server cannot delete a generation once its object is deleted, and Azurite keeps no blob versions. Both emulators do accept the listing that includes versions.
- Datasets are fixed at upload; there is no API to move an asset between datasets.
- Retention is by age since creation; there is no "last access" tracking.
- Reconciliation's own `RECONCILIATION_UNRECORDED_ACTION=delete` still deletes unrecorded quarantine objects on one sighting. Keep it at `report` and let the cleanup worker's mark-and-sweep handle them.

## Object Lock

On a bucket created with S3 Object Lock, retention and legal holds are also set on each asset's stored version. The provider itself then refuses to delete it early, whoever holds the bucket's keys: a leaked key, a careless script or a lifecycle rule in the customer's own account.

- **When:** retention evaluation (cleanup worker) applies them to every `AVAILABLE` asset whose delivery copy has a recorded version. It sets the retention date `created_at + min_retention` in GOVERNANCE mode, and a legal hold while any hold covers the asset. The bucket's setting is read once per ten minutes per connection.
- **What is recorded:** applied settings go to `object_locks` (migration `000023_object_lock.sql`). An unchanged lock is not sent again.
- **Retention only grows:** a lock's date is extended, never shortened. A deletion waits (`PROTECTED`, "object lock until …") for the lock's date even when the policy was lowered since.
- **Holds:** a hold that is no longer active is taken off the version before its deletion, so a soft-deleted asset is not stuck behind its own lock.
- **Other providers:** GCS, Azure and S3 buckets without Object Lock are left alone.
- **Changing it:** GOVERNANCE can be shortened only by a principal allowed `s3:BypassGovernanceRetention`. COMPLIANCE mode is not used, because nobody could undo a mistaken date.
