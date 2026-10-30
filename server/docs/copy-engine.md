# The copy engine

Copies between storage connections, every one verified: the bytes are hashed as they are read, the copy is read back and hashed again, and a copy that does not match the recorded SHA-256 is deleted and reported. Migration `000035_copy_engine.sql`. The dataset worker runs the engine every 30 seconds.

## Protecting a dataset

`PATCH /api/v1/datasets/{id}` (`datasets.manage`; a replica connection also needs `storage.use`):

| Field | Meaning |
|---|---|
| `replica_connection_id` | A second connection (another provider, account or region) that keeps a verified copy of every object the dataset refers to: every snapshot's members and the draft's current versions. `""` removes it. |
| `escrow` | Nothing in the dataset is deleted (by a member, an operator, a bulk delete or retention expiry) until its copy at the replica is verified. Deletion answers 409 until then. |
| `vault_retention_days` | A locked vault: copies are locked in COMPLIANCE mode for this many days. The replica's bucket must have Object Lock, as its last posture check found, and must be in a different account from the dataset's own. It can only be lengthened. |
| `scrub_days` | Every member is re-read and re-hashed once in this many days. |

Without a vault, a copy keeps the Object Lock its source holds (mode, date, legal hold) where the replica's bucket can lock. A source under COMPLIANCE whose copy cannot be locked is not copied.

`GET /api/v1/datasets/{id}/replication` shows how many objects are copied, pending and failing (with the last error), the newest copy, and the **recovery point**: the age of the oldest object not yet copied. Failed copies are retried with a backoff of up to a day.

**Read failover.** When a dataset's own connection does not answer, or an object there cannot be read as published, downloads (streams, share links and presigned URLs) are served from its verified replica. Whole reads from a replica are re-hashed as they stream.

**Scrubs and repair.** A scrub that finds an object damaged or missing rewrites it from a verified replica, and points the asset at the rewritten version. Packed objects are left to the pack rewrite. Either way it is recorded (`GET /api/v1/datasets/{id}/scrubs`), raised as a `SCRUB_MISMATCH` security alert and sent as the webhook `object.scrub_failed`. A provider that does not answer says nothing about the object; it is tried again next time.

## Jobs

Each is a copy job, `GET /api/v1/copy-jobs[?dataset_id=]`, `GET /api/v1/copy-jobs/{id}` (with its report), `GET /api/v1/copy-jobs/{id}/items?state=FAILED`, and `POST /api/v1/copy-jobs/{id}/cancel`. Each job that ends sends `copy.job_finished`. Jobs are resumable: they carry on from their last object after a restart.

- **Migration**: `POST /api/v1/datasets/{id}/migrations {"target_connection_id": "...", "dry_run": true}` (`storage.manage` and `datasets.manage`).
  - The dry run returns the objects and bytes to copy and an egress estimate at the providers' list prices.
  - Without `dry_run` it queues the migration. Every object is copied, verified and given its source's lock. Only when all are does the dataset switch, in one transaction: its assets (and any sharing their bytes) read from the target, keeping their IDs; packed assets read their own copy; adopted objects become the platform's own copies; and the dataset's new uploads go to the target.
  - If any object fails, nothing switches and the report lists the failures.
  - The source objects are left in place.
- **Restore**: `POST /api/v1/datasets/{id}/restores {"version_id", "target_connection_id", "prefix"}` copies a snapshot's members to `<ready prefix>/<organization>/.restores/<prefix>/<path>` on the target, with a `manifest-sha256.txt` beside them.
- **Restore drill**: `POST /api/v1/datasets/{id}/drills {"sample": 200, "from_replica": true}` re-reads a sample, from the replica as a restore would. The report gives:
  - how many read back intact, and what was missing or damaged,
  - the read speed, and how long restoring the whole dataset would take at that speed.

  Any problem raises a `SCRUB_MISMATCH` alert.
- **Export bundle**: `POST /api/v1/exports {"target_connection_id"}` (`audit.read` and `storage.use`) writes the organization's export-everything bundle to `<ready prefix>/<organization>/.exports/<time>-<job>/`. `PUT /api/v1/export-schedule {"connection_id", "interval_hours"}` (`org.manage`) writes it on a schedule; 0 stops it.

## Clean points

After an incident, `PUT /api/v1/datasets/{id}/clean-point {"version_id"}` marks the last snapshot known good. `GET /api/v1/datasets/{id}/clean-point/diff` lists every path added, removed or changed since then, with who uploaded each new version and when. Restore the clean snapshot to a fresh connection with a restore job.

## Alerts

- `MASS_DELETION`: one member asked for at least `COPY_MASS_DELETION_ALERT` (100) deletions within an hour (retention expiry does not count).
- `SCRUB_MISMATCH`: a scrub or drill found damage.

Both go to the `security.alert` webhook. For deletions, the two-person rule (docs/governance.md) adds approval.

## The platform's folders

Replicas, restores and exports live under dot folders of an organization's prefix (`.replicas`, `.restores`, `.exports`), with connection tests and tombstones. The drift scan (docs/storage-posture.md) does not report them.

## Rebuilding from an export

The bundle is self-describing (its `README.md`), and `SHA256SUMS` covers every file. To rebuild without this platform's database:

1. Check `SHA256SUMS`.
2. Check the audit chain: each line of `audit.jsonl` carries its `hash` and the `prev_hash` before it. A broken link marks tampering or loss.
3. Restore the platform from its database backup if there is one. If not, start an empty platform at the same version and recreate the connections in `connections.jsonl`, re-entering their credentials (the bundle never holds secrets).
4. For each line of `catalog.jsonl`, the object is at `object_key` (and `storage_version_id`) on `storage_connection_id`, or in `replicas.jsonl`. Adopt them (docs/adoption.md) with a JSONL manifest that carries each `checksum_sha256`, so every byte is verified as it is adopted.
5. Recreate datasets from `datasets.jsonl` and their snapshots from `versions.jsonl` and `version-members.jsonl`. Each version's `manifest_sha256` proves the rebuilt snapshot is the same one.
6. Re-apply `retention.jsonl`: policies and active legal holds.
