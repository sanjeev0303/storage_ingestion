# Load testing on real infrastructure (runbook)

This is the plan and the harness for measuring the platform on cloud infrastructure. **No cloud results exist yet**: this machine cannot reach real cloud infrastructure, so everything below is what to run and how to judge it.

The thresholds come from one laptop (`docs/release-candidate.md` section 5.1, `docs/ingestion.md`). They are **a baseline to beat, not production numbers**:

- the laptop ran PostgreSQL with default settings (128 MB shared buffers) in Docker Desktop;
- every service shared four cores;
- the object store was MinIO on the same disk.

A cloud environment that only matches them has a problem.

| Tool | What it drives |
|---|---|
| `cmd/ingestion-loadgen` | bulk ingestion through the public API: datasets, manifests, claims, uploads to the object store, completion, processing, cancellation. It samples the servers' `/debug/vars`, PostgreSQL and `docker stats`, and writes a JSON report (`-out`) |
| `scripts/loadtest/tenants.sh` | N loadgen runs at once, one per tenant, and their summed JSON (`summary.json`) |
| `scripts/loadtest/read-latency.js` | k6 probe of the read endpoints with p95/p99 thresholds |

## What to measure

| Measurement | Where it comes from | Field or command |
|---|---|---|
| Upload finish rate (files/s, MiB/s to the object store) | loadgen, upload mode | `upload.files_per_second`, `upload.mib_per_second` |
| Ingestion entries/s, one tenant | loadgen | `upload.files_per_second` (entries uploaded and completed), `published_files_per_second` |
| Ingestion entries/s with N tenants | `tenants.sh` | `summary.json`: `upload_files_per_second` (sum), `slowest_tenant_upload_files_per_second` |
| Processing throughput (verified, scanned, promoted) | loadgen | `published_files_per_second`; backlog peaks in `job_backlog`; oldest pending processing job in `database.sampled` |
| API latency p50/p95/p99, write path | loadgen | `latency_ms` per request kind: `claim`, `complete`, `storage_put`, `get_job`, ... |
| API latency p50/p95/p99, read path | k6 | `--summary-export` JSON, `http_req_duration{read:...}` |
| Request error rate | loadgen, k6 | `request_error_rate`, `request_failures`; k6 `http_req_failed` |
| Manifest read rate (lines/s) | loadgen, synthetic mode | `manifest_lines` / `manifest_seconds` |
| Cancellation of a large job | loadgen, synthetic mode | `scheduling.cancel_seconds` |
| PostgreSQL wait profile | loadgen `-db` | `database_activity`: average sessions per wait event and statement, ten readings a second |
| PostgreSQL totals | loadgen `-db` | `database`: commits, rows written, cache hit ratio, size growth |
| Worker and API memory | loadgen `-vars` | `server_memory`: peak heap in use and peak sys per process |
| Worker CPU | the platform's metrics (CloudWatch, Cloud Monitoring, Azure Monitor, `kubectl top`) | per process or container. `-containers` covers Docker hosts only |

Record the PostgreSQL server's own CPU, IOPS, commit latency and connection count from the provider's metrics as well. On the laptop PostgreSQL was the busiest component in every run.

## Environment

Run the load generator on its own machine in the same region as the API and the bucket, never on an API or worker host:

- **Load generator.** 4 vCPU and 2 GB RAM. A million-entry plan holds about 200 MiB, and the upload mode hashes every file before it starts.
- **API.** At least two instances behind the load balancer.
- **Workers.** Run the upload, processing and outbox workers as separate services.
- **Processing workers.** Size them by ClamAV. On the laptop ClamAV peaked at 424% CPU for 633 MiB of mixed files, so give each processing worker its own scanner with 2–4 vCPU, and keep `PROCESSING_CONCURRENCY` at 2–4 per worker. Raising it to 8 lowered throughput on the laptop.
- **PostgreSQL.** Managed, with SSD storage:
  - `shared_buffers` about 25% of RAM;
  - enough RAM to keep the active jobs' entries cached. They grow by about 0.9 GiB per million entries, indexes included (`release-candidate.md` 5.1: +927 MiB per million).
  - Commit latency sets the per-tenant ceiling: each tenant's entry changes serialize on its usage rows, held through each commit (`docs/ingestion.md`, "Where the database's time goes"). The laptop committed a small write in 3.3 ms at p50, so record the cloud figure.
  - Keep `synchronous_commit` on.
- **Connections.** `max_connections` must cover every process's pool, plus the loadgen sampler (one connection).
- **Object store.** In the same region. Use a bucket for the test only, so the run can be cleaned up by deleting it.

Size two environments if you can: a small one (2 vCPU / 8 GB PostgreSQL, one instance per role) to compare with the laptop, and the intended production size.

## Before a run

1. **Point everything at the test environment.** loadgen takes `-db` only as a flag and never reads `DATABASE_URL`.
2. **Raise the per-organization limits** for the load-test organizations, or the runs measure the throttle (R2):
   - `RATE_LIMIT_UPLOAD_FINISH=60000/1m`
   - `RATE_LIMIT_UPLOAD_INIT=60000/1m`
   - `RATE_LIMIT_LISTING=60000/1m`
3. **Raise `AUTH_ACCESS_TOKEN_TTL`** above the longest run. loadgen signs in once.
4. **Create one account with the developer role per tenant**, in its own organization (`docs/security.md`). The password goes in `LOADGEN_PASSWORD`, or `PASSWORD` for k6, never in a flag.
5. **Expose `/debug/vars`** of the API and the workers (`METRICS_ADDR`) to the load-generator host only.
6. **Build the binaries:**

   ```bash
   go build -o bin/ ./cmd/ingestion-loadgen
   ```

   The same seed gives the same files, so a run can be repeated exactly.

Every command below uses these:

```bash
API=https://api.loadtest.example
VARS=http://api-1:9201/debug/vars,http://upload-worker-1:9202/debug/vars,http://processing-worker-1:9203/debug/vars
DB='postgres://loadtest_reader:...@db.loadtest.example:5432/object_storage?sslmode=require'   # read access is enough for sampling
export LOADGEN_PASSWORD=...
```

## Runs

Run each one three times, and keep the median and the spread. On the laptop the same 1,000-file run gave 28.9–39.8 files/s.

**1. Small files, one tenant (upload finish rate, per-tenant ceiling).**

```bash
./bin/ingestion-loadgen -api $API -email lt1@example.test -org $ORG1 -files 2000 -concurrency 32 -seed 7 \
  -classes 'small=100:1KiB-64KiB' -wait 30m -vars $VARS -db "$DB" -out run1.json
```

Repeat with `-concurrency 64` and `128`. The per-tenant ceiling is where more concurrency stops adding files/s while `database_activity` shows `Lock:transactionid` / `Lock:tuple` waits growing.

**2. Mixed sizes (bytes to the object store, multipart).**

```bash
./bin/ingestion-loadgen -api $API -email lt1@example.test -org $ORG1 -files 500 -concurrency 16 -seed 1 \
  -classes 'small=85:1KiB-256KiB,medium=13:1MiB-8MiB,large=2:20MiB-48MiB' -wait 60m -vars $VARS -db "$DB" -out run2.json
```

**3. N tenants at once (scaling across tenants).** N = 1, 2, 4, 8:

```bash
printf '%s\n' "lt1@example.test $ORG1" "lt2@example.test $ORG2" "lt3@example.test $ORG3" "lt4@example.test $ORG4" > tenants4.txt
LOADGEN=./bin/ingestion-loadgen scripts/loadtest/tenants.sh tenants4.txt runs/tenants-4 \
  -api $API -files 2000 -concurrency 32 -classes 'small=100:1KiB-64KiB' -wait 60m
```

Give `-db` and `-vars` to one tenant only: run that one by hand alongside the script, so the database is not sampled N times.

**4. End-to-end processing throughput.** Use the reports of runs 1–3: `published_files_per_second`, and `job_backlog.peak_awaiting_processing`. Then:

- raise the processing workers (instances × `PROCESSING_CONCURRENCY`) until uploads stop being held by `processing_backlog`;
- stop when PostgreSQL's row-lock waits stop the gain.

**5. Large manifest and cancellation (control plane, R5).**

```bash
./bin/ingestion-loadgen -api $API -email lt1@example.test -org $ORG1 -mode synthetic -files 1000000 -seed 3 \
  -classes 'small=100:1KiB-64KiB' -claim 100 -vars $VARS -db "$DB" -out run5.json
```

Nothing is uploaded. It reads a staged manifest of 160 MiB, claims until a limit holds the job, cancels it and times the cancellation (`scheduling.cancel_seconds`). Watch the upload worker's log: each `ingestion sweeps` line gives `cancelled` per pass, so the rate over time can be read off it.

**6. Read latency under write load.** Run during run 3:

```bash
PASSWORD=... k6 run -e API=$API -e EMAIL=lt1@example.test -e ORG=$ORG1 -e JOB=<a job of run 3> \
  -e VUS=32 -e DURATION=10m --summary-export=runs/read-latency.json scripts/loadtest/read-latency.js
```

Do it once more on a catalog grown to 1,000,000 objects (`cmd/catalog-bench` populates one tenant). This checks that keyset pages stay flat over HTTP as they did in the repository benchmark.

**7. Sustained run.** One hour or more. Repeat run 3 with N = 4 back to back (`-files` sized to about 15 minutes per round) and watch for:

- drift in files/s;
- heap growth in `server_memory`;
- the outbox and processing backlogs (`database.sampled`) never draining.

No run on the laptop lasted over an hour.

### PostgreSQL wait profile

loadgen's `-db` samples `pg_stat_activity` ten times a second, and reports the busiest wait and statement pairs as average sessions. On the laptop:

- about 3 sessions waited on row locks;
- the rows were each tenant's usage rows: `ingestion_tenant_usage` and `tenant_usage`.

On a managed database, also take `pg_stat_statements` before and after each run:

```sql
SELECT calls, round(mean_exec_time::numeric, 2) AS mean_ms, round(total_exec_time::numeric) AS total_ms, left(query, 100)
FROM pg_stat_statements ORDER BY total_exec_time DESC LIMIT 20;
```

## Pass/fail thresholds

Derived from the laptop (`release-candidate.md` 5.1 unless noted). "Laptop" is the number to beat; "pass" is the minimum for the environment intended for production. All rates are medians of three runs.

| Measurement | Laptop | Pass |
|---|---|---|
| Small files, one tenant, upload finish rate | 35–40 files/s (Phase 7: 43.7) | ≥ 44 files/s at 32 concurrent |
| Two tenants together | 28 files/s (Phase 7: 62) | ≥ 1.4 × the one-tenant rate (Phase 7 reached 1.42 ×). Below 1 × means tenants contend, as in the RC run |
| N tenants (4, 8) | not measured above 5 tenants | aggregate keeps rising with N; slowest tenant ≥ 50% of the one-tenant rate |
| Mixed sizes, bytes to the object store | 10.4 MiB/s (Phase 7: 16.5) | ≥ 16.5 MiB/s with 16 concurrent |
| End-to-end publication | 11.8–14.9 files/s | ≥ 15 files/s with one processing worker at concurrency 4; scales with workers until PostgreSQL row locks stop it |
| Manifest read, 1,000,000 lines | 4,723 lines/s (Phase 7: 12,794) | ≥ 12,800 lines/s (≤ 80 s per million) |
| Cancelling 1,000,000 entries | 1,436 s, falling from 1,280 to 300/s (R5); after the R5 fix 65 s at a steady 13,000–21,000/s (below) | ≤ 300 s, and the rate in the last tenth ≥ ⅓ of the first |
| Claim latency | p50 374 ms, p95 616 ms | p95 ≤ 616 ms, p99 ≤ 1 s |
| Complete latency | p50 150 ms, p95 330 ms | p95 ≤ 330 ms, p99 ≤ 1 s |
| Storage PUT (small file) | p50 39 ms, p95 90 ms | p95 ≤ 90 ms from the same region |
| Read endpoints (k6) | repository only: catalog first page p95 8.7 ms at 1,000,000 objects (`catalog-bench`); `get_job` p95 50 ms over HTTP (R5 run) | p95 ≤ 150 ms, p99 ≤ 400 ms; job progress p95 ≤ 100 ms; flat from 10,000 to 1,000,000 objects |
| Request error rate | 0 with limits raised | < 0.1%, no 5xx |
| Row-lock waits (average sessions) | 2.98–3.19 per busy tenant | ≤ 3 per busy tenant; must not grow with N tenants |
| Upload worker heap | 7.9–15.7 MiB (15.7 reading a 160 MiB manifest) | ≤ 64 MiB peak heap in use |
| Processing worker heap | 9.8–10.8 MiB | ≤ 64 MiB |
| API heap (outside sign-in bursts) | 11.8–18.6 MiB | ≤ 64 MiB per instance; sign-in adds 64 MiB per concurrent Argon2id hash |
| Sustained hour | not run | rates within 10% of the first 15 minutes; no heap growth; backlogs drain |

A failed line is a finding, not a retry: record it with the run's JSON and the PostgreSQL wait profile.

### R5 on the laptop (2026-11-12)

Same laptop as section 5.1, measured with `-mode synthetic -files 1000000 -seed 3 -claim 100`:

- the API and upload worker ran natively (`INGESTION_POLL_INTERVAL` 5 s, `INGESTION_WORKER_BATCH` 500), with no event queue;
- PostgreSQL 17 had default settings, on a scratch database.

| Code | Manifest read | Cancelling 1,000,000 entries | Rate over the run |
|---|---|---|---|
| Release candidate (2026-10-21, no cursor) | 211.7 s | 1,435.7 s | 1,280/s falling to 300/s |
| Before this fix (cursor kept per worker, 20 batches a pass) | 77.1 s | 498.4 s | flat 2,006/s: exactly 10,000 per 5 s pass, PostgreSQL idle most of the time |
| After (`ix_ingestion_entries_waiting`, a pass works for one poll interval) | 89.0 s | **65.0 s** | 65,000–106,500 per 5 s pass (13,000–21,000/s), no trend |

The integration test `TestIngestionCancellingALargeJobKeepsItsRate` drives the same sweep without the worker's ticker (`INGEST_CANCEL_ENTRIES=1000000`). It cancelled the million in 66.8 s; the slowest tenth ran at 11,543/s and the fastest at 17,937/s.

`EXPLAIN (ANALYZE, BUFFERS)` of one batch, with 600,000 of 1,000,000 entries cancelled:

| Query | Without the index | With it |
|---|---|---|
| The release candidate's query (no cursor) | primary-key scan, 600,000 rows removed by the filter, 1,526 ms | 1.4 ms |
| Today's query, cursor lost (worker restart, second worker) | the same walk, 1,431 ms | 1.2 ms |
| Today's query, cursor held | 1.0 ms | 1.3 ms |

The first scan after 200,000 more were cancelled, before vacuum, took 46 ms: it marks their dead index entries once.

## Recording results

For each run keep these together:

- the loadgen JSON (`-out`), `summary.json` and the k6 summary;
- the environment: instance types, PostgreSQL version and settings, worker counts and `PROCESSING_CONCURRENCY`, region;
- the commit that was built.

Write the results into a new section of `docs/release-candidate.md` next to the laptop rows, so the two can be compared line by line.
