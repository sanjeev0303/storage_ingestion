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
