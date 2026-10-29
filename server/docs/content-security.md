# Content security pipeline (local runbook)

Every completed upload goes through VERIFY → SCAN → PROMOTE → CLEANUP in the
processing worker before it can be delivered. Nothing is AVAILABLE until all
four have passed.

## Stages and what is recorded

| Stage | Checks | Recorded on `processing_jobs` | On failure |
|---|---|---|---|
| VERIFY: verification | object exists at its quarantine key; stored size and streamed SHA-256 equal the client-declared values. The multipart ETag is never used as a checksum. | `verification_status` PASSED/FAILED, `checksum_source` DECLARED, `checksum_algorithm`, `checksum_verified_at` | asset REJECTED, quarantine REJECTED / VERIFICATION_FAILED |
| VERIFY: validation | basic (empty file, policy size limit, unsafe file name), type (bytes vs declared type vs extension, deny list, allow list, unknown types), archive (optional) | `validation_status` PASSED/REJECTED, `validation_reason`, `detected_content_type` | asset REJECTED, quarantine REJECTED / VALIDATION_REJECTED |
| SCAN | ClamAV health check (PING), then INSTREAM scan with its own timeout | `scanner`, `scan_result`, `scan_signature`, `scan_error`, `scanned_at` | see below |
| PROMOTE | reuse a verified leftover copy, otherwise copy; verify the copy's size and SHA-256; asset PROCESSING → AVAILABLE | `promotion_key` (saved before copying), `promotion_status` PENDING → PUBLISHED, `promoted_at` | retried; a bad copy is deleted |
| CLEANUP | delete the quarantine source; release any HELD record | job SUCCEEDED | retried |

`checksum_source` is always DECLARED: initiation requires a client SHA-256
and verification fails without one (`checksum_missing`). COMPUTED exists in
the schema for a future upload path without a declared checksum; such a
checksum proves only that the bytes did not change after upload.

## Scan outcomes

| Outcome | Meaning | What happens |
|---|---|---|
| CLEAN | clamd answered `OK` | promote |
| INFECTED | clamd answered `<signature> FOUND` | asset REJECTED (`malware_detected`), quarantine REJECTED / MALWARE_DETECTED |
| UNSUPPORTED | clamd's stream size limit exceeded | not retried; job EXHAUSTED, quarantine HELD / SCAN_INCOMPLETE |
| TIMEOUT | scan exceeded `CLAMAV_TIMEOUT` | retried with backoff; on exhaustion HELD / SCAN_INCOMPLETE |
| ERROR | scanner unreachable, PING failed, or any other answer | retried with backoff; on exhaustion HELD / SCAN_INCOMPLETE |

Only CLEAN (or NOT_SCANNED under the development-only `allow_unscanned`
policy) can ever reach promotion. With no scanner configured,
`SCAN_UNAVAILABLE_POLICY` decides: `hold` (default, HELD), `reject`, or
`allow_unscanned` (refused unless `APP_ENV=development`).

## Large objects

Every full read of a stored object (verification's SHA-256, the scan's
hash, promotion's check of its copy) goes through
`internal/infrastructure/storage/rangeread`: 8 MiB ranges, eight in
flight, delivered in order and all pinned to the ETag read first, so an
overwrite during the read fails it instead of mixing two objects.

clamd takes at most `StreamMaxLength` (100 MiB by default) in one stream
and answers a longer one with a connection reset, which loses its verdict.
So an object larger than `PROCESSING_SCAN_SEGMENT_BYTES` (64 MiB) is
scanned as segments of that size, each reaching 1 MiB back into the one
before so a signature on a boundary is still read whole,
`PROCESSING_SCAN_PARALLELISM` (4) at a time on their own clamd
connections, while one more pinned read hashes the whole object. The first
INFECTED segment stops the others. Two limits follow:

- a segment is scanned as raw bytes: ClamAV cannot unpack part of an
  archive, so an archive (zip, gzip, rar, 7z) larger than one segment is
  UNSUPPORTED and held, never passed;
- `PROCESSING_SCAN_SEGMENT_BYTES` plus 1 MiB must not exceed clamd's
  `StreamMaxLength`, or every segment fails and the job is retried until
  held.

A job's time limit is `PROCESSING_MAX_JOB_DURATION` plus its object's size
at `PROCESSING_MIN_THROUGHPUT_BYTES` a second (16 MiB/s: about 18 hours
more for 1 TiB), so a large object is not cut off and restarted forever.
Promotion copies objects over 256 MiB as four part copies at a time.

Uploads above 5 GiB also need `UPLOAD_MAX_SIZE_BYTES` (API, at most 5 TiB)
and `PROCESSING_MAX_FILE_BYTES` raised; both default to 5 GiB.

Measured locally (MinIO and ClamAV in Docker on one laptop), 1 GiB of
random bytes: verification 3.5 s, scan 14.2 s, promotion 18.7 s, 36.4 s in
all; before, the scan never finished and the object stayed held.

## Reusing a clean verdict for the same bytes

Scanning is the slowest stage, and datasets are often uploaded again
unchanged. When a tenant uploads bytes it already had scanned CLEAN, the scan
stage reuses that verdict instead of scanning again, if all of these hold:

- the earlier asset belongs to the same tenant and has the same SHA-256 and
  size (migration `000019`, index `ix_assets_tenant_checksum`);
- its verdict came from the same scanner at the same engine and signature
  version. ClamAV reports this with `VERSION` (for example
  `ClamAV 1.4.6/28166/Tue Oct 27 06:24:12 2026`); new signatures change it,
  and the next upload of those bytes is scanned again;
- that scan read exactly the declared bytes. `scanner_version` is written only
  for a CLEAN scan whose scanned bytes matched the declared SHA-256 and only
  when the version did not change during the scan, so a scan that saw swapped
  bytes, or spanned a signature reload, is never reused.

Verification and content validation still run for every upload, and
promotion still re-reads the published copy and checks its size and SHA-256,
so the reused verdict is about exactly the bytes that get published. The job
records `scan_reused_from` (the earlier asset) and `scanner_version`, and a
`scan_reused` processing event names both. A scanner that cannot report a
version (`interfaces.VersionedScanner`) never reuses verdicts.

Measured locally: the same 174 files (1.2 GiB) ingested again with ClamAV's
own cache cleared went from 108.4 s to 79.2 s from first upload to
completed, and from 65.1 s to 23.7 s after the last upload; ClamAV CPU fell
from 465% to 0.3% and all 174 scans were reused.

## Quarantine records

`quarantine_records` holds one row per decision, with at most one HELD row
per asset (partial unique index).

| Status | Meaning | Leaves by |
|---|---|---|
| HELD | asset kept out of delivery; job EXHAUSTED | job succeeds after `-requeue` (RELEASED), or is rejected (REJECTED) |
| RELEASED | held asset later passed every check | terminal |
| REJECTED | permanent; object kept until `retain_until` for investigation | purge after `QUARANTINE_RETENTION` (PURGED) |
| PURGED | quarantined object deleted | terminal |

Categories: VERIFICATION_FAILED, VALIDATION_REJECTED, MALWARE_DETECTED,
SCAN_INCOMPLETE, PROCESSING_EXHAUSTED. Scan signatures are stored for
operators and never returned by the API (clients see only the rejection
reason code).

Every decision is appended to `processing_events` (verified, scanned,
promoted, rejected, scan_incomplete, quarantine_held, requeued,
quarantine_released, purged), with the worker or operator as actor. Event
writes are best effort: a failed write is logged and counted
(`event_write_failures`). The job columns and quarantine records remain the
source of truth.

## Content policy

The platform policy comes from the environment. A tenant row in
`tenant_content_policies` can only narrow it:

| Rule | Platform | Tenant |
|---|---|---|
| Always denied | html, xml, svg, xhtml, javascript, PE/ELF/Mach-O executables, Java classes, shell scripts (by bytes, declared type or extension) | cannot change |
| Extra denied types | `PROCESSING_DENIED_CONTENT_TYPES` | `denied_types` (added) |
| Allowed types | `PROCESSING_ALLOWED_CONTENT_TYPES` (empty = any) | `allowed_types`; only entries the platform allows are kept, and none left is an error (the job is held) |
| Size limit | `PROCESSING_MAX_FILE_BYTES` | `max_size_bytes` (the smaller one wins) |
| Unknown types | `PROCESSING_UNKNOWN_TYPE_ACTION` | `unknown_type_action=reject` tightens |

```sql
INSERT INTO tenant_content_policies (tenant_id, allowed_types, denied_types, max_size_bytes, unknown_type_action)
VALUES ('local-tenant', '{image/*,application/pdf}', '{image/gif}', 104857600, 'reject');
```

Archive inspection (`ARCHIVE_INSPECTION_ENABLED=true`) spools a zip to one
temp file (removed afterwards) and decompresses each entry into a discard
writer. It measures real expanded size rather than trusting headers, and
rejects (`unsafe_archive`) on: too large, too many entries, too much
expanded data, a compression ratio above the limit, absolute, `..` or
backslash paths, symlinks, or an unreadable archive.

## Durable work (no outbox needed)

Jobs are derived from state, not sent as messages. `EnqueueDue` creates the
asset and its single job in one transaction from COMPLETED sessions that
have none. A crash between "upload completed" and "job created" therefore
loses nothing: the next cycle creates it. Claims are leases
(`lease_owner`, `lease_expires_at`), so a crashed worker's job is picked up
again when its lease expires. Every write checks the lease, so a stale
worker cannot overwrite newer state.

The lease is short (`PROCESSING_LEASE_DURATION`, 1 min) and renewed from the
claim until the job ends, every `PROCESSING_LEASE_RENEW_INTERVAL` (a third
of the lease by default), also while a claimed job waits for one of the
worker's `PROCESSING_CONCURRENCY` slots. A crashed worker's jobs are picked
up within about a minute, and a long scan or copy still keeps its job.
Renewal works as fencing:

- Each claim is its own owner token, `worker#attempt`. A renewal, a saved
  stage or an outcome must name the token of the current claim, so a worker
  that lost its job (paused past its lease, then reclaimed by another)
  cannot write, even when the same worker claimed the job again since.
- A renewal that finds the lease taken cancels the job's work at once:
  content reads, the malware scan and the copy stop, and nothing is recorded
  (`lease_renewals_lost`).
- Promotion renews once more just before it publishes, so a worker that
  lost the job never makes the asset `AVAILABLE`.
- A renewal that fails for another reason (the database is briefly down)
  is retried; the job is stopped only once a whole lease has passed without
  one, since by then another worker may hold it (`lease_renewal_errors`).
- Renewal ends with the job, and no job holds its claim longer than
  `PROCESSING_MAX_JOB_DURATION` (30 min, counted from the claim): it is
  stopped, gives its lease back and waits for its retry, so an unhealthy
  worker cannot keep a job forever.

Retries and EXHAUSTED/HELD are unchanged.

## Operating it locally

```powershell
docker compose up -d                      # postgres, minio, clamav (clamav takes ~1 min to load signatures)
.\scripts\migrate.ps1                     # applies pending migrations (tracked in schema_migrations)
# A database migrated before schema_migrations existed: mark what it has once
.\scripts\migrate.ps1 -Baseline 000006
```

```bash
# One cycle, JSON stats on stdout, logs on stderr
SCANNER=clamav ./bin/processing-worker.exe -once
# Everything recorded about one asset: job, quarantine records, events
./bin/processing-worker.exe -inspect local-tenant/<asset-id>
# Retry a held (EXHAUSTED) asset with a fresh attempt budget
./bin/processing-worker.exe -requeue local-tenant/<asset-id>
# Long-running, with metrics at http://127.0.0.1:9102/debug/vars
METRICS_ADDR=127.0.0.1:9102 ./bin/processing-worker.exe
```

Metrics (expvar, under `processing`): `jobs_succeeded`, `jobs_rejected`,
`jobs_retried`, `jobs_exhausted`, `jobs_lease_lost`, `verification_failed`,
`validation_rejected`, `scan_CLEAN|INFECTED|UNSUPPORTED|TIMEOUT|ERROR`,
`scan_duration_ms_total`, `scanner_up`, `quarantine_held`,
`quarantine_released`, `quarantine_purged`, `event_write_failures`, and the
backlog gauges `backlog_due`, `backlog_leased`, `backlog_lease_expired`
(stuck: claimed work whose worker stopped) and `backlog_exhausted`.

## Testing with EICAR

Only the EICAR test string is ever used as "malware". Windows Defender
deletes an EICAR file as soon as it is written to disk, so the tests and
the smoke script build it in memory and stream it.

## Storing identical files once

An upload whose verified bytes equal a file the organization already stores, on the same storage connection, is published pointing at that stored object instead of a copy (migration `000024_dedup.sql`).

- **How it's found:** promotion looks for the oldest `AVAILABLE` asset with the same SHA-256 and size. The upload was already verified and scanned like any other.
- **Within one batch:** identical files are promoted at the same moment. An asset waits while an older asset with the same bytes is still being promoted (up to a minute, polling every 250 ms), then shares it. They wait in order of age, so never in a circle. After the minute it copies as usual.
- **Checked before sharing:** the stored object must still be exactly what was recorded when it was published (same size, ETag and version). Publication then runs in one transaction that also checks the original is still available under that object. If either check fails, it falls back to a normal copy.
- **What changes:** the new asset keeps its own ID, catalog version, quota charge and lifecycle. `shared_from` names the asset that first stored the bytes (the ID in the object's key), even when the twin found was itself a sharer. `object_key` points at that object. Delivery serves a shared asset from that key and from no other.
- **Deletion:** bytes are removed only with their last user. Deleting either asset first lets go of its reference without deleting bytes another live asset uses. Sharing never blocks a deletion. Packing does the same when it retires the loose copies of packed files. The orphan sweep keeps a deleted asset's object while another asset still points at it.
- **Scope:** one organization and one connection. Bytes are never shared across organizations. A deleted user's file therefore leaves nothing behind except what another asset in the same organization legitimately holds.
- **Visible:** `GET /api/v1/operations/status` counts shared assets and the bytes not stored twice. The console shows them on the Status page.
- **Not deduplicated:** chunks within files, and files on different connections.
