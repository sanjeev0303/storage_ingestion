# Object catalog: logical objects, versions, metadata and listing (local runbook)

The catalog is the authoritative, tenant-scoped list of a tenant's files.

- **Logical object.** A file is a logical object: a stable ID, and a normalized path in a dataset namespace.
- **Versions.** Its versions are assets. Each upload is one asset, stored under its own server-generated key and numbered when processing publishes it.
- **Current version.** The object points at its current version and holds the mutable user metadata.
- **What PostgreSQL holds.** Everything a version says about its bytes (size, SHA-256, storage key, provider ETag and version ID) is immutable and lives in PostgreSQL. Object storage remains the authority for the bytes themselves.

| Piece | Where | Role |
|---|---|---|
| Schema | `migrations/000013_catalog.sql` | `catalog_objects`; `assets.object_id/version/metadata`; `upload_sessions.path/object_id/create_only/metadata`; cross-tenant foreign keys; listing indexes |
| Model | `internal/model/catalog.go`, `path.go` | objects, versioning modes, filters, positions, findings; path and metadata rules |
| Repository | `internal/infrastructure/database/postgres/catalog_repository.go`, `catalog_reconcile.go` | the claim at upload start, publication, deletion and restore hooks, listings, changes, reconciliation queries |
| Service | `internal/service/catalog` | reads, listings with cursors, metadata/rename/versioning/promote/delete, the replaced-version handler, reconciliation |
| HTTP | `internal/handler/http/catalog_handler.go` | `/api/v1/catalog/...` |
| Worker | `cmd/cleanup-worker` | `catalog.version_replaced` on the lifecycle queue |
| Tools | `cmd/catalog-admin`, `cmd/catalog-bench`, `internal/catalogbench` | reconciliation, sizes, capabilities; the synthetic catalog and its benchmark |

## Identity and namespace rules

- **Stable identity.** An object's ID is server-generated and never depends on its path or storage key. Renaming changes only metadata; no bytes move.
- **Namespace.** It is the tenant plus a dataset name; `""` is the tenant's root namespace. A dataset name follows the existing label rules (`[a-z0-9][a-z0-9._-]{0,63}`), the same label retention policies key on. One object exists per normalized path per namespace (`ux_catalog_objects_path`).
- **Path normalization** (`model.NormalizePath`, shared with bulk ingestion):
  - **Canonical form.** Unicode NFC, `/`-separated, with no empty, `.` or trailing segments.
  - **Refused, not resolved:**
    - absolute and drive-letter paths, and `..`;
    - backslashes, NUL, control and bidirectional-override characters;
    - characters Windows forbids;
    - segments with leading or trailing spaces;
    - reserved first segments (`_system`, `_ingestion`, `.well-known`).
  - **Limits.** Paths are at most 1024 bytes, 255 per segment and 64 segments deep.
- **Case and Unicode.** Paths are **case-insensitive and normalization-insensitive**: the key is `NFC(lowercase(NFC(path)))`. `Café/Menu.txt`, `café/menu.TXT` and `CAFÉ//./menu.txt` are one object. The path keeps the case the first upload gave it, for display. This one rule applies to uploads, lookups, renames, listing prefixes and ingestion manifests.
- **Storage keys.** These are never derived from paths: `quarantine/<tenant>/<upload>` while processing, `<prefix>/<tenant>/<asset>` once published. Composite foreign keys make an asset of another tenant's object, or an object whose current version is another tenant's asset, impossible.

## Metadata

| Kind | Where | Mutable | Who sets it |
|---|---|---|---|
| System: size, content type, SHA-256, storage key, provider ETag and version ID, status, timestamps | the version (asset); the current version's copied onto the object for listing | no | the upload pipeline |
| Version user metadata | `assets.metadata` | no | the upload (`metadata` in the upload request, or the manifest entry) |
| Object user metadata | `catalog_objects.metadata` | yes, with the object's revision | publication copies the newest upload's metadata; then `PATCH` |

**User metadata rules** (`model.ValidateUserMetadata`):
- **Keys.** 1–64 characters of letters, digits, `.`, `_` and `-`. Keys are case-sensitive.
- **Values.** UTF-8 strings without control or bidi characters, at most 1024 bytes.
- **Limits.** At most 64 keys and 8 KiB encoded.
- **Reserved.** Keys under `system.` and the names of system fields (`size_bytes`, `checksum_sha256`, `tenant_id`, `object_key` and so on) are refused.

User metadata is stored apart from system fields, so it cannot overwrite them; the reserved names keep responses and filters unambiguous.

**Changes and revisions:**
- **Revisions.** Every client-visible change bumps the object's `revision`, its optimistic-concurrency token: metadata, path, versioning, or the current version.
- **Conditional writes.** `PATCH`, rename, versioning and promote require the revision the client read, as `If-Match` (the object's `ETag` is the revision) or as `revision` in the body.
- **Responses.** A missing revision is `428`; a stale one is `412 revision_conflict`.
- **Merge patch.** `PATCH` applies a JSON merge patch to the metadata: `{"metadata":{"stage":"final","draft":null}}` sets `stage` and removes `draft`.
- **Audit.** Each change is audited with the keys changed, never their values.

## Versions

**Upload targeting.** An upload names its target with `dataset`, `path` (default: its file name), `metadata` and `create_only`. It is the same on single and multipart uploads, and bulk ingestion passes each entry's path and metadata.

**Claim at upload start.** When the upload starts, it claims the path's object, creating it if new, under the object's row lock:

| Situation | Result |
|---|---|
| `create_only` and the path has a current version | `409 path_exists` |
| `create_only` and another upload of the path is in flight | `409 path_busy` |
| any upload while a `create_only` upload of the path is in flight | `409 path_busy` |
| otherwise | the upload will be the path's next version |

**Publication.** When processing publishes the asset (AVAILABLE), the same transaction does all of the following:
1. Numbers it `next_version` (unique per object).
2. Makes it current and copies its system metadata onto the object.
3. Writes `catalog.version_published` (and `catalog.object_created` for version 1) to the tenant's audit chain.

**Never current:** a rejected or held upload never gets a number and is never current. Concurrent publications serialize on the object's row: N concurrent uploads become versions 1..N, the last published is current, and there is always exactly one current version.

**Versioning per object:**
- **`ENABLED`** (default). Every published version stays; any AVAILABLE version can be made current again (`promote`).
- **`DISABLED`**. A new version replaces the current one, and `catalog.version_replaced` (transactional outbox, lifecycle queue) hands the replaced version to the lifecycle. The cleanup worker requests its deletion as the `catalog` service principal; retention and legal holds still apply. A duplicate event, or one about a version current again, does nothing.

**Logical and provider versioning are separate:**
- **Logical versioning works everywhere.** Each version has its own immutable key, so the catalog never relies on the provider.
- **Provider versioning adds a version ID.** Where the bucket has versioning enabled, promotion records the provider's version ID. Reads, deletions and reconciliation then name that exact version.
- **Reported by the adapter.** It reports the bucket's capability explicitly (`Capabilities`, `GET /api/v1/catalog/capabilities`, `catalog-admin capabilities`).

**Deletion** goes through the Phase 5.6 lifecycle, never around it:

| Request | Effect |
|---|---|
| `DELETE /catalog/objects/{id}/versions/{n}` | that version's asset is soft-deleted (restorable during grace). If it was current, the newest remaining AVAILABLE version becomes current, in the same transaction |
| `DELETE /catalog/objects/{id}` (logical deletion) | every published or rejected version is soft-deleted, then the object is marked deleted and leaves listings. Versions still in processing are not touched (deleting them would race processing): they are reported and publish into the object later |
| restore of a version (`POST /assets/{id}/restore`) | it becomes current only if the object has none |
| purge (cleanup worker) | removes that version's own key, or its provider version, only. It is refused while any object names the version current, as well as while holds, retention or other references apply |

**After a logical deletion:**
- **Path and history stay.** The path keeps its object and version history: a later upload publishes the next version number and undeletes the object.
- **Tombstones.** Tombstoned versions stay as asset rows for audit and retention.

## Listing and cursors

`GET /api/v1/catalog/objects`. Filters, all ANDed after the caller's tenant:

| Parameter | Meaning | Index use |
|---|---|---|
| `dataset` | one namespace (`dataset=` is the root); absent: all | `ix_catalog_objects_listed` |
| `prefix` | path prefix, matched against keys (any case, NFC) as a range on the `"C"`-collated key | `ix_catalog_objects_listed` |
| `metadata.<key>=<value>` | exact user-metadata match, up to `CATALOG_MAX_METADATA_FILTERS` | `ix_catalog_objects_metadata` (GIN, `jsonb_path_ops`) |
| `order=updated`, `updated_after`, `updated_before` | modification time | `ix_catalog_objects_updated` |
| `content_type`, `min_size`, `max_size`, `created_after`, `created_before`, `object_id`, `asset_id` | refine the scan | none (filters on the chosen index scan) |
| `include=unpublished` | also objects with no current version | `ux_catalog_objects_path` |
| `limit` | 1–`CATALOG_MAX_PAGE_SIZE` (default 100) | |

**Ordering.**
- **Order types.** Path order is `(dataset, path_key)`, unique within a tenant. Time order is `(updated_at, id)`, with ties broken by ID. Both are total.
- **Keyset pages.** Pages are keyset pages: the position of the last row is re-applied as a row comparison, and `LIMIT n+1` tells whether there is more. No `OFFSET`, no unbounded materialization.
- **Statement timeout.** Each listing statement runs under `SET LOCAL statement_timeout` (`CATALOG_QUERY_TIMEOUT`, 5s): a filter so unselective that the scan runs past it is cancelled (`422 query_too_broad`).

**Cursors.**
- **Format.** `next_cursor` is base64url JSON: a format version, the order, a fingerprint of the tenant and every filter, and the position.
- **Refusals.** A cursor used with other filters, another order, or by another tenant is `400 invalid_request`; garbage is refused the same way.
- **Not a secret.** A cursor names only a position in the holder's own listing, and the tenant predicate always comes first. A hand-edited position can therefore only move within the caller's own objects.
- **No expiry.** Cursors stay valid until the format version changes.

**Consistency: a live view, not a snapshot.**
- Objects added or changed between pages appear if they sort after the position.
- Nothing is repeated, and the order holds.
- A page never includes another tenant's rows.

Version history: `GET /catalog/objects/{id}/versions?before=&limit=` returns the published versions, newest first; continue with `next_before`.

## API

| Route | Permission | Notes |
|---|---|---|
| `GET /api/v1/catalog/objects` | `assets.read` | the listing above |
| `GET /api/v1/catalog/objects/{id}` | `assets.read` | `ETag` is the revision |
| `GET /api/v1/catalog/lookup?dataset=&path=` | `assets.read` | by normalized path |
| `GET /api/v1/catalog/objects/{id}/versions` | `assets.read` | published versions, newest first |
| `GET /api/v1/catalog/objects/{id}/versions/{n\|current}` | `assets.read` | one version, with its upload metadata |
| `PATCH /api/v1/catalog/objects/{id}` | `catalog.write` | metadata merge patch; revision required |
| `POST /api/v1/catalog/objects/{id}/rename` | `catalog.write` | `{"path":...}` in the same namespace; `409 path_exists` |
| `PUT /api/v1/catalog/objects/{id}/versioning` | `catalog.write` | `{"versioning":"ENABLED"\|"DISABLED"}` |
| `POST /api/v1/catalog/objects/{id}/versions/{n}/promote` | `catalog.write` | make an AVAILABLE version current |
| `DELETE /api/v1/catalog/objects/{id}/versions/{n}` | `assets.delete` | `202`; through the lifecycle |
| `DELETE /api/v1/catalog/objects/{id}` | `assets.delete` | `202`; logical deletion |
| `GET /api/v1/catalog/capabilities` | `assets.read` | logical and provider versioning |

**Isolation and responses.**
- **Same answer as missing.** Another tenant's object, version, path or asset ID is answered exactly like a missing one (`404`), on every route.
- **Nothing internal leaks.** Responses never carry storage keys, bucket names or signed URLs. A version's bytes are reached through `/api/v1/assets/{asset_id}/...` with its asset ID.
- **Rate limits.** Reads count against the `listing` rate limit, changes against `upload_finish`.

`catalog.write` is in the developer, administrator and owner roles. Auditors read only.

Sample requests (with `auth.hdr` holding `Authorization: Bearer <token>`, see `docs/security.md`):

```bash
API=http://127.0.0.1:8080/api/v1
# An upload to a path, with metadata
curl -s -H @auth.hdr $API/uploads -d '{"file_name":"q1.txt","content_type":"text/plain","size_bytes":6,
  "checksum_sha256":"<sha256>","idempotency_key":"q1","dataset":"reports","path":"2026/Q1.txt","metadata":{"team":"finance"}}'
# → PUT the bytes to upload_url, POST /uploads/<id>/complete; processing publishes version 1

curl -s -H @auth.hdr "$API/catalog/lookup?dataset=reports&path=2026/q1.TXT"
# → {"id":"<OBJ>","dataset":"reports","path":"2026/Q1.txt","versioning":"ENABLED","revision":2,
#    "metadata":{"team":"finance"},"current_version":{"version":1,"asset_id":"...","size_bytes":6,...},"versions":1,...}
curl -s -H @auth.hdr "$API/catalog/objects?dataset=reports&prefix=2026/&metadata.team=finance&limit=50"
curl -s -H @auth.hdr "$API/catalog/objects?dataset=reports&prefix=2026/&metadata.team=finance&limit=50&cursor=<next_cursor>"
curl -s -H @auth.hdr "$API/catalog/objects/<OBJ>/versions"
curl -s -H @auth.hdr -H 'If-Match: "2"' -X PATCH "$API/catalog/objects/<OBJ>" -d '{"metadata":{"stage":"final"}}'
curl -s -H @auth.hdr -X POST "$API/catalog/objects/<OBJ>/versions/1/promote" -d '{"revision":3}'
```

## Reconciliation

`catalog-admin reconcile` and `catalog.Reconciler` do the following:
- **Pages.** They scan each tenant in keyset pages (`-batch`).
- **Dry run by default.** They report by default and repair with `-repair`. Every repair is audited (`catalog.reconciled`, `catalog.backfilled`), and so is the run (`catalog.reconcile`).

| Finding | Repair |
|---|---|
| `current_version_invalid`: the current version is not AVAILABLE | the newest AVAILABLE version becomes current (or none) |
| `current_metadata_stale`: copied size, type, checksum or number differ from the version | refreshed from the version |
| `current_version_missing`: AVAILABLE versions, no current one | the newest becomes current |
| `version_unnumbered`: an AVAILABLE version without a number (an interrupted publication) | numbered; becomes current only if the object has none valid |
| `replaced_version_not_deleted`: an object without versioning still has older AVAILABLE versions | handed to the lifecycle |
| `asset_not_in_catalog`: an AVAILABLE asset from before the catalog | attached to the object at its file name in its dataset (or `legacy/<asset-id>` when the name is no valid path) as the next version; current if published later than the current one |
| `stale_uploads`: completed uploads older than a day without an asset | none here: the processing reconciliation enqueues them |
| `path_index_invalid`: the unique path index is invalid (an interrupted rebuild) | none: rebuild the index |
| `physical_object_missing`, `physical_size_mismatch` (with `-storage`) | **none**: reported only |
| `physical_object_unreferenced` (with `-storage`): a stored object under the published prefix that no asset names | **none**: never deleted; the cleanup worker's orphan sweep decides after its grace period |

**Storage checks.**
- **Bounded.** They are capped per tenant (`-max-storage-checks`).
- **Version-aware.** A version with a recorded provider version ID is checked by that ID; otherwise by key.
- **Errors are not findings.** A storage error is counted and the run goes on. One failed check never produces a finding, let alone a deletion.
- **Dataset aggregates are not checked here.** `datasets.*_bytes` belongs to bulk ingestion, and `upload-worker -reconcile` checks it.

## Observability

Catalog counters are published with the API's expvar (`/debug/vars`, map `catalog`):
- **Listings:** `catalog_list_requests`, `catalog_list_ms` (total), `catalog_list_rows`, `catalog_list_continuations`, `catalog_list_slow` (over `CATALOG_SLOW_QUERY`), `catalog_list_timeouts`.
- **Writes:** `catalog_revision_conflicts`.
- **Reconciliation:** `catalog_findings_<kind>`, `catalog_missing_objects`.

Slow and timed-out listings are logged with their shape (which filters, order, tenant), never with paths or metadata values. `catalog-admin stats` reports object, published-object and version counts, and the bytes of every catalog table and index.

Version-creation failures surface as processing failures: the publication runs in the asset's transaction, so a failed version is a failed (retried) promotion. That keeps them in the processing metrics.

## Configuration

| Variable | Default | Meaning |
|---|---|---|
| `CATALOG_DEFAULT_PAGE_SIZE` | 100 | page size without `limit` |
| `CATALOG_MAX_PAGE_SIZE` | 1000 | largest `limit` (at most 10000) |
| `CATALOG_QUERY_TIMEOUT` | 5s | a listing statement is cancelled after this (at most 1m) |
| `CATALOG_SLOW_QUERY` | 500ms | listings slower than this are logged and counted |
| `CATALOG_MAX_METADATA_FILTERS` | 4 | metadata filters per listing (at most 16) |

Invalid values stop the API and the cleanup worker at startup.

## Running it locally

```bash
# 1-2. Dependencies and migrations (PowerShell for the script)
docker compose up -d
#   .\scripts\migrate.ps1                      # through 000013
#   .\scripts\migrate.ps1 -Database object_storage_test -Create

# 3. API and workers as in docs/security.md (the catalog needs no extra worker;
#    the cleanup worker handles catalog.version_replaced)

# 4-9. Upload to a path, list, read history, change metadata: the sample requests above.
#      Duplicate path: the same upload with "create_only":true → 409 path_exists.
#      Concurrent versions: start several uploads of one path, complete them, let processing run:
#      they become versions 1..N, the last published current.

# 10. Reconciliation, dry run (add -repair to repair, -storage to check storage)
go run ./cmd/catalog-admin reconcile -tenant <ORG> -storage
go run ./cmd/catalog-admin stats
go run ./cmd/catalog-admin capabilities

# 11. Tests
go test ./internal/... ./tests/unit/ ./cmd/...
TEST_DATABASE_URL=postgres://storage_app:...@127.0.0.1:5435/object_storage_test?sslmode=disable \
TEST_S3_BUCKET=object-storage-it TEST_S3_CREATE_BUCKET=true TEST_CLAMAV_ADDRESS=127.0.0.1:3310 \
TEST_SQS_ENDPOINT=http://127.0.0.1:9324 go test ./tests/integration/ -timeout 40m

# Catalog benchmark: a dedicated local database (it writes millions of rows)
#   .\scripts\migrate.ps1 -Database object_storage_bench -Create
go run ./cmd/catalog-bench -db "postgres://storage_app:...@127.0.0.1:5435/object_storage_bench?sslmode=disable" \
  -steps 10000,100000,1000000 -versions 1 -datasets 4 -out bench.json
```

## Measured catalog performance

Measured with `cmd/catalog-bench` on one Windows 11 laptop, PostgreSQL 16 in Docker (local, untuned), one synthetic tenant grown to 10 000, 100 000 and 1 000 000 objects, one published version each, spread over 4 namespaces. Each query ran 30 times through the real repository code (`CatalogRepository.List` and friends, statement timeout 30 s). These are single-machine numbers, not a capacity claim: a busy server, larger rows or many tenants will change them.

Listing latency at 1 000 000 objects (100 rows per page):

| Query | p50 ms | p95 ms | Plan |
|---|---|---|---|
| First page, path order | 3.96 | 5.55 | `ix_catalog_objects_listed` |
| Page from the middle (keyset cursor) | 3.78 | 5.95 | `ix_catalog_objects_listed` |
| One namespace | 3.77 | 4.71 | `ix_catalog_objects_listed` |
| Prefix `photos/2024/` | 14.26 | 22.44 | `ix_catalog_objects_listed` (range) |
| Metadata `camera=c3` (1 in 10) | 6.49 | 23.38 | `ix_catalog_objects_listed`, filter |
| Metadata `camera=c3 batch=13` (1 in 1000) | 11.87 | 16.14 | `ix_catalog_objects_metadata` (GIN) |
| Content type and size (unindexed) | 72.34 | 96.75 | `ix_catalog_objects_listed`, filter |
| Time order | 4.19 | 7.26 | `ix_catalog_objects_updated` |
| Version history (newest 100) | 0.75 | 1.13 | not captured |
| Lookup by path | 0.94 | 1.09 | not captured |

The same queries at 10 000 and 100 000 objects were within a few milliseconds of these (first page 5.5 and 6.4 ms p50), so keyset listings stay flat as the catalog grows; only filters the index cannot answer (content type and size: 10.9, 48.9, 72.3 ms p50 at the three sizes) grow with how far the scan reads to fill a page.

Populate throughput (synthetic rows generated by PostgreSQL in batches of 10 000 objects, including their sessions, assets and index maintenance): 2 241 objects/s from 10 000 to 100 000, 2 192 objects/s from 100 000 to 1 000 000. The first 10 000 ran at 181 objects/s; that step is one batch and includes the cold start, so it is not a throughput figure.

Sizes at 1 000 000 objects: `catalog_objects` 388 MiB of heap; its indexes `ux_catalog_objects_path` 202 MiB, `ux_catalog_objects_id_tenant` 189 MiB, `ix_catalog_objects_updated` 168 MiB, `catalog_objects_pkey` 130 MiB, `ix_catalog_objects_listed` 116 MiB, `ux_catalog_objects_current` 76 MiB, `ix_catalog_objects_metadata` 10 MiB; on assets `ix_assets_object` 114 MiB and `ux_assets_object_version` 113 MiB.

What the numbers do not show:

- **Filters correlated with path order are slower.** An earlier run used synthetic metadata that followed the path order. There the 1-in-10 metadata filter took about 520 ms p50 at 1 000 000 objects, because the matching rows sat far along the listing index. The planner cannot know such a correlation. A filter that is rare where the listing starts can read many rows before a page fills, up to the statement timeout (`CATALOG_QUERY_TIMEOUT`, answered as 422 `query_too_broad`). The generator now draws attributes from hashes, as most real filters behave.
- **The 1 000 000 "time range" row is not meaningful.** That run measured its window from the clock rather than from the generator's timestamps, so the range was empty at the last step (0 rows). The 10 000 and 100 000 steps (5.4 and 5.7 ms p50, 100 rows) are valid. The benchmark now uses the generator's base time.
- **One version per object.** Histories were one version long; `-versions N` measures longer ones.

## Limitations

**Unindexed filters.** `content_type`, size and creation-time filters ride on the chosen index scan. A highly selective one combined with path order may read many rows to fill a page; the statement timeout bounds it. Indexing them would add write cost to every publication for filters that have not been needed yet.

**Large tenants and partitioning.** Listings are always per tenant, so a tenant's objects are a contiguous range of every catalog index. The catalog table is not partitioned: at the measured sizes the per-tenant index ranges keep listings flat. Hash partitioning by `tenant_id` is the upgrade path once one table's maintenance (vacuum, index rebuilds) becomes the bottleneck. Uniqueness keys already lead with `tenant_id`, which keeps that migration possible.

**Renames.** A rename stays within the object's dataset: versions carry the dataset label their retention policies key on.

**Paths stay claimed.** A path's object is permanent: a deleted object's path is claimed by its tombstone, and an upload to it continues its history. A rename onto such a path is refused.

**Metadata is not versioned.** The object's metadata is mutable and is not itself versioned. Each version's upload metadata is kept unchanged, and the audit log records which keys changed.

**Provider versioning in tests.** The in-memory store models only a bucket's newest version. The real versioned-bucket behaviour is tested against MinIO.
