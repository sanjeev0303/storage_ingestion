# Load-test helpers

Small helpers for the runs in `docs/load-testing.md`. The driver for ingestion is `cmd/ingestion-loadgen`; these only cover what it does not.

| File | What it does | Needs |
|---|---|---|
| `tenants.sh` | Runs one `ingestion-loadgen` per tenant at the same time, then sums their JSON reports into `summary.json` (total upload and publish rates, slowest tenant, worst error rate) | bash, `jq`, `bin/ingestion-loadgen` |
| `read-latency.js` | k6 probe of the read endpoints (catalog pages, datasets, ingestion jobs, one job's progress) with p95/p99 thresholds; exits non-zero when one fails | [k6](https://k6.io) |

Passwords come from the environment (`LOADGEN_PASSWORD` for `tenants.sh`, `PASSWORD` for k6), never from flags. Use dedicated load-test accounts with the developer role.

```bash
# Four tenants at once, 2,000 small files each
printf '%s\n' "lt1@example.test <ORG1>" "lt2@example.test <ORG2>" "lt3@example.test <ORG3>" "lt4@example.test <ORG4>" > tenants.txt
LOADGEN_PASSWORD=... scripts/loadtest/tenants.sh tenants.txt runs/4-tenants \
  -api https://api.example.test -files 2000 -concurrency 32 -classes 'small=100:1KiB-64KiB' -wait 30m

# Read latency while the ingestion run is going
PASSWORD=... k6 run -e API=https://api.example.test -e EMAIL=lt1@example.test -e ORG=<ORG1> \
  -e VUS=32 -e DURATION=10m --summary-export=runs/read-latency.json scripts/loadtest/read-latency.js
```

The probe counts against the per-organization `listing` limit (`RATE_LIMIT_LISTING`). Raise it for the run, or the probe measures 429s.
