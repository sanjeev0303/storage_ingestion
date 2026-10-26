# Small-file packing

Packing stores many small published objects as one physical object (a *pack*) on the same storage connection, while every object keeps its identity: clients still ask for an asset (a logical object version) by its ID, and get exactly its bytes.

## 1. What exists today (inspected before Phase 13)

- **No physical packing.** `internal/infrastructure/compaction/archive_packer.go` and `manifest_index.go` contained only a `package` line. `internal/service/compaction` and `cmd/compaction-worker` compact *database rows* (finished operational and audit records); they never touch stored bytes. Phase 13 fills the two stub files and leaves metadata compaction as it is.
- **Logical model.** A catalog object (`catalog_objects`, by tenant, namespace and path) has versions; each version is an asset (`assets`), with its own ID, version number, size, SHA-256 and status. Dataset snapshots and validation reports name assets. An asset is the unit of identity, authorization and lifecycle; it is what a pack entry refers to.
- **Provider binding.** Each asset records `storage_connection_id` (immutable, enforced by a trigger) and, once published, `object_key = <ready prefix>/<tenant>/<asset>` plus the stored object's ETag and version ID. Every reader resolves the connection through the registry (`interfaces.StorageRouter`) and reads that key, pinned to the ETag or version.
- **Read paths of published bytes:** delivery (`internal/service/delivery`: a presigned or CDN URL, a token link, or streaming through the API with `OpenRange`); dataset content validation (`internal/service/dataset/validation.go`, a full `OpenRange`); catalog reconciliation (a `HEAD` per version); the cleanup worker's missing-object check (a `HEAD`); lifecycle deletion (`HEAD` and delete of each target).
- **Lifecycle.** `AVAILABLE → SOFT_DELETED → DELETED`. A deletion operation goes `SCHEDULED → IN_PROGRESS → STORAGE_DELETED → COMPLETED`, rechecks legal holds and retention before it starts, and records `STORAGE_DELETED` only after every target is confirmed gone.

## 2. Design

**Logical stays logical.** Packing never changes an asset's ID, key, version, checksum, status, catalog row or dataset membership. It adds a mapping, *asset → (pack, offset, length)*, that readers consult after they have authorized the asset.

**Pack, entries, manifest.**

- A pack is one row in `packs` and one object at `<ready prefix>/<tenant>/packs/<pack>.pack` on the assets' own storage connection: the entries' bytes, concatenated in order, nothing else. Reading an entry is one range read; nothing else in the pack is read.
- Its entries are rows in `pack_entries`, one per asset version: ordinal, offset, stored length, original size, SHA-256, and the source object it was copied from. A published entry's geometry cannot be changed (a trigger refuses it).
- The manifest is a canonical JSON document at `<pack key minus .pack>.manifest.json`: format and version, pack ID, tenant, connection, pack size and SHA-256, and the entries in ordinal order, with a checksum over the document. The same inputs always produce the same bytes and the same hash. It makes a pack self-describing and verifiable without the database; readers use the entry rows, which are validated against it at publication.

**One packing job is one pack.** The pack row carries the job: state, attempts, lease, next attempt. Its ID is the idempotency key for everything it writes.

**States.**

```
pack:   PLANNED → WRITING → VERIFYING → PUBLISHED → RETIRING → DELETED
        PLANNED | WRITING | VERIFYING → FAILED → DELETED
entry:  RESERVED → PUBLISHED → DELETED (its asset was deleted) | MOVED (rewritten into another pack)
        RESERVED → RELEASED (the pack failed) | EXCLUDED (its source failed verification)
```

A cloud upload cannot join a database transaction, so every step that touches storage is followed by a recorded state, and every state can be resumed. `WRITING` starts over: it aborts any multipart upload and deletes any partial object at the pack's own key first. `VERIFYING` re-verifies what was written. Publication is one transaction: the entries become `PUBLISHED` (or `DELETED`, for an asset deleted meanwhile) and the pack `PUBLISHED` together, so a reader sees all of a pack or none of it.

**Sources stay until they are not needed.** A published pack's sources (the standalone objects) are deleted only after `PACKING_SOURCE_RETIRE_AFTER`, the window in which a pack could still be withdrawn, and only while the asset is still the same version and not permanently deleted.

**Deleting a packed asset.** Lifecycle deletion marks the entry `DELETED` at once (it is never served again). The deletion is recorded `STORAGE_DELETED` only once no pack holds its bytes. A pack whose entries are all deleted is retired, and its objects deleted. A pack with some deleted entries is rewritten: its live entries are copied into a new pack, published, and the old pack retired. This happens when the deleted share reaches `PACKING_REWRITE_DEAD_RATIO`, or when a deletion has waited `PACKING_MAX_DELETION_DELAY`. The guarantee stays: storage deleted means the bytes are gone.

**Delivery.** A packed asset is never given a provider or CDN URL: a signed URL to a pack could read its neighbours. Download URLs for packed assets are token links, streamed through the API. Every read is the entry's exact byte range of the pack (a full read too, never "to the end"), pinned to the pack's ETag or version; a full read is checked against the asset's SHA-256 as it streams.

**Connections.** A pack holds entries of one tenant and one storage connection, and lives on that connection; packing never moves bytes between connections. A connection that cannot do ranged reads is not packed.

## 3. Configuration

The pack worker (`cmd/pack-worker`) reads `PACKING_*` (all in `.env.example`); `config.LoadPacking` refuses limits that contradict each other.

| Setting | Default | Meaning |
|---|---|---|
| `PACKING_MIN_OBJECT_BYTES` / `PACKING_MAX_OBJECT_BYTES` | 1 / 1 MiB | Sizes that are packed. At most 64 MiB: each source is held in memory while it is packed. |
| `PACKING_MIN_AGE` | 1h | How long after publication an asset becomes a candidate. New assets are often read or replaced at once. |
| `PACKING_TARGET_PACK_BYTES` / `PACKING_MAX_PACK_BYTES` | 64 MiB / 256 MiB | A pack is filled to the target; the maximum (at most 5 GiB) bounds it. |
| `PACKING_MAX_ENTRIES` | 10000 | Entries per pack, and the manifest parser's limit. |
| `PACKING_SOURCE_CONCURRENCY` | 16 | Sources read ahead per pack. A writer holds at most this × the maximum object size (at most 1 GiB). |
| `PACKING_ACTIVE_WRITERS` | 2 | Packs one worker builds at once. |
| `PACKING_TENANT_ACTIVE_PACKS` | 1 | Packs of one tenant being built at once, across workers, so no tenant takes every writer. |
| `PACKING_TENANTS_PER_CYCLE` / `PACKING_TENANTS` | 16 / all | Tenants planned per cycle, in turn; `PACKING_TENANTS` limits packing to a list of organizations. |
| `PACKING_LEASE`, `PACKING_MAX_JOB_DURATION` | 2m, 30m | The job lease (renewed every third of it) and the most one attempt may take. |
| `PACKING_MAX_ATTEMPTS`, `PACKING_RETRY_BASE`, `PACKING_RETRY_MAX` | 5, 30s, 30m | Attempts and backoff before a pack fails. |
| `PACKING_SOURCE_RETIRE_AFTER` | 24h | How long sources are kept after their pack is published. |
| `PACKING_REWRITE_DEAD_RATIO`, `PACKING_MAX_DELETION_DELAY` | 0.5, 24h | When a pack with deleted entries is rewritten. |
| `PACKING_REMOVE_AFTER` | 1h | How long a retired or failed pack's objects are kept, for reads that resolved it just before. |
| `PACKING_POLL_INTERVAL` | 30s | The worker's cycle. |

## 4. Eligibility

An asset is a candidate when:

- it is `AVAILABLE` and its size is within the limits;
- it was published at least `PACKING_MIN_AGE` ago;
- its stored ETag and SHA-256 are recorded;
- no active legal hold covers it (tenant, dataset or asset scope);
- it has no reserved, published or excluded entry already.

It must also be at its published key on its connection, and the connection must do ranged reads (the platform's always does). Assets are taken in ID order, so the same candidates always make the same pack.

A pack is planned when it reaches the target size or entry count, or when a tenant's candidates run out and at least two remain. A single leftover asset is never packed alone.

## 5. Format

The pack object is the entries' bytes back to back, uncompressed, with nothing between or around them (`format = ospack`, version 1). Entry *i* starts where entry *i − 1* ends.

The manifest (`.manifest.json` beside the pack) is canonical JSON: fixed field order, no extra whitespace, entries in ordinal order, and a `checksum` field holding the SHA-256 of the document with that field empty. The pack row records the SHA-256 of both the pack and the manifest.

`compaction.ParseManifest` rejects a manifest with any of these:

- unknown fields, non-canonical bytes, or a wrong checksum;
- a different format version;
- gaps or overlaps between entries, or offsets that overflow;
- duplicate assets, or an entry whose length differs from its size;
- more entries than the limit.

## 6. The worker

Each cycle plans, builds and retires; a failing step is logged and the others still run. `pack-worker -once` runs one cycle and prints what it did as JSON.

**Plan.** For each tenant in turn, the worker reserves one pack's worth of candidates on one connection, in one transaction with a `pack.planned` outbox event. A unique index allows one reserved or published entry per asset, so two workers planning at once cannot pack an asset twice: the second gets a conflict and plans around it next cycle.

**Build.** The worker claims due packs (taking a lease and counting an attempt), then:

1. Moves the pack from `PLANNED` to `WRITING`.
2. Deletes anything at the pack's keys and aborts unfinished uploads there.
3. Streams every source into one upload, each read pinned to its recorded ETag or version.
4. Checks that every entry landed at its planned offset and that the stored size is right.
5. Moves the pack to `VERIFYING`, recording the pack's SHA-256 and ETag.
6. Reads the pack back and checks every entry's SHA-256 and the whole pack's.
7. Writes the manifest and reads it back.
8. Publishes.

With `EVENT_QUEUE=sqs`, `pack.planned` events on the `packing` queue start builds at once; polling remains the safety net.

**Publish.** One transaction locks the entries' assets and publishes each entry whose asset is still the same bytes (`AVAILABLE` or `SOFT_DELETED`, same ETag); the others become `DELETED`. Readers see a pack whole or not at all.

**Retire.** The worker:

- deletes sources past their grace period;
- retires packs with no published entries;
- plans rewrites;
- deletes the objects of retired and failed packs after `PACKING_REMOVE_AFTER`;
- lists one page of each tenant's `packs/` prefix and reports objects no pack row names, leaving them in place.

**Failures.**

| What happens | Result |
|---|---|
| Worker stops while `WRITING` | The lease expires; another worker writes again from scratch. |
| Worker stops while `VERIFYING` | Another worker verifies the stored pack and publishes it as written. |
| Lease lost (another worker took over) | The lease check refuses the step's write; the other worker carries on. |
| A source's bytes do not match its recorded size or SHA-256 | The pack fails. That asset's entry is `EXCLUDED` and it is never packed again; the others are released and planned again. |
| A source is gone or replaced since planning | The pack fails at once, without retries. Its entries are released and planned again as they are now. |
| The stored pack or manifest does not read back as written | The pack fails; its assets are planned again. |
| Storage or database errors | Retried with backoff; after `PACKING_MAX_ATTEMPTS` the pack fails. |
| A pack has failed | Its objects are deleted after `PACKING_REMOVE_AFTER`, then it is `DELETED`. Assets were never served from it. |

## 7. Reading packed assets

Readers resolve an asset as before (authorization, status, key check), then ask `PackLocator.PackedLocation` for a published entry of a published pack. If there is none, the asset is read from its own object as before.

**Delivery** reads exactly the entry's byte range:

- A full download reads `[offset, offset + length − 1]`. A client range is mapped inside the entry and refused past its end, so no request can reach a neighbour's bytes.
- Reads are pinned to the pack's ETag or version; a `HEAD` checks the pack's size and ETag.
- A full read is hashed as it streams. If the bytes are not the asset's, it ends in an error, not a clean end.
- The download's ETag and size are still the asset's own.
- Packed assets get token links only, never a presigned or CDN URL: a `storage` request gets a token link instead. With `DELIVERY_PROXY_ENABLED=false` a download URL for a packed asset is refused; streaming it through the API still works.

**Dataset validation** reads a packed member through its entry, pinned to the pack.

**Catalog reconciliation** checks a packed version with a `HEAD` of its pack (its size must equal the recorded pack size), and does not report pack objects as unreferenced.

**The cleanup worker's** missing-object check looks at the pack for a packed asset.

## 8. Deletion, holds and retention

Lifecycle deletion of a packed asset deletes its standalone object (if not yet retired) and marks its entry `DELETED` at once, so it is never served again.

A pack may still hold its bytes: a published pack with its deleted entry, a retiring pack, or a pack still being built. Until none does:

- The deletion goes back to `RETRY_WAIT` until `LIFECYCLE_RECHECK_INTERVAL`, without using an attempt (`deletions_waiting_for_pack`).
- It completes, and `STORAGE_DELETED` is recorded, only once no pack holds its bytes.
- A deletion in `RETRY_WAIT` is not restorable, so an asset can never come back without its bytes.

A pack is rewritten when deleted entries reach `PACKING_REWRITE_DEAD_RATIO` of its bytes, or when a deletion has waited `PACKING_MAX_DELETION_DELAY`. The rewrite copies the live entries from the old pack, pinned to it, into a new pack. Publishing the new pack moves those entries and retires the old one, whose objects are deleted `PACKING_REMOVE_AFTER` later. So a deleted asset's bytes are gone within about `PACKING_MAX_DELETION_DELAY + PACKING_REMOVE_AFTER`, plus a few cycles.

Held assets are not packed. An asset placed under hold after it was packed keeps its standalone object: its source is not retired while the hold is active. Retention is unchanged; it applies to assets, not packs.

## 9. Providers and BYOS

A pack holds one tenant's entries from one connection and is stored on that connection, under its ready prefix (`<ready prefix>/<tenant>/packs/`), so a connection scoped to its prefixes can hold it. Packing never moves bytes across connections or tenants.

A tenant connection is packed only if its recorded capabilities include ranged reads. Otherwise its assets stay as they are (`packing_unsupported_connections`), and the tenant's other connections are still packed.

Reads and writes go through the connection registry and its per-connection limits, so packing shares a connection's concurrency with everything else.

## 10. Observability

The worker publishes counters on `/debug/vars` (`METRICS_ADDR`), under `packing`:

| Area | Counters |
|---|---|
| Pack lifecycle | `packs_planned`, `packs_published`, `packs_failed`, `packs_retried`, `packs_lease_lost`, `packs_retired`, `packs_removed` |
| Entries | `pack_entries_published`, `pack_entries_dead_at_publication` |
| Planning | `packing_candidates_selected`, `packing_plan_conflicts`, `packing_unsupported_connections` |
| Data moved | `packing_bytes_read`, `packing_bytes_written`, `packing_build_ms_total` |
| Integrity | `pack_verification_failures`, `packing_source_integrity_failures`, `packing_sources_changed`, `pack_orphan_objects` |
| After publication | `packing_sources_retired`, `pack_rewrites_planned` |

Delivery adds `downloads_packed` and `packed_read_bytes`; the lifecycle adds `deletions_waiting_for_pack`. Every state change is logged with the pack, tenant and connection.

There is no gauge for the pack backlog by state yet; query `packs` for it (`ix_packs_tenant_state`).

## 11. Migration and rollback

`000017_packs.sql` is additive. It adds:

- the `packs` and `pack_entries` tables and their indexes;
- the trigger that keeps entry geometry immutable;
- `ix_assets_pack_scan`.

Without the pack worker running nothing changes: readers find no packed location and read standalone objects as before.

Rolling back is safe only while no source has been retired, that is, within `PACKING_SOURCE_RETIRE_AFTER` of the first publication (set it high while trying packing out). To roll back:

1. Stop the pack worker.
2. Check that `SELECT count(*) FROM pack_entries WHERE source_retired_at IS NOT NULL AND state = 'PUBLISHED'` returns 0.
3. Delete the `packs/` objects.
4. Run:

```sql
BEGIN;
DROP TABLE IF EXISTS pack_entries, packs;
DROP FUNCTION IF EXISTS pack_entry_immutable();
DROP INDEX IF EXISTS ix_assets_pack_scan;
DELETE FROM schema_migrations WHERE version = '000017_packs.sql';
COMMIT;
```

Once sources have been retired, packed assets' bytes exist only in their packs. There is no tool yet to copy them back to standalone objects, so do not roll back then.
