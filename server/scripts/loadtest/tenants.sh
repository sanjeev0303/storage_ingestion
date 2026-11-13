#!/usr/bin/env bash
# Runs one ingestion-loadgen per tenant at the same time and sums the reports.
#
#   LOADGEN_PASSWORD=... scripts/loadtest/tenants.sh tenants.txt out/ -files 2000 -concurrency 32 -classes 'small=100:1KiB-64KiB'
#
# tenants.txt has one "email org-id" pair per line; every account shares
# LOADGEN_PASSWORD (use dedicated load-test accounts). Flags after the output
# directory go to every loadgen as they are; each run gets its own -seed so
# tenants upload different files. Needs bin/ingestion-loadgen and jq.
set -euo pipefail

if [ $# -lt 2 ] || [ -z "${LOADGEN_PASSWORD:-}" ]; then
  echo "usage: LOADGEN_PASSWORD=... $0 tenants.txt out-dir [loadgen flags...]" >&2
  exit 2
fi
tenants=$1
out=$2
shift 2
loadgen=${LOADGEN:-./bin/ingestion-loadgen}
mkdir -p "$out"

n=0
pids=()
while read -r email org; do
  [ -z "$email" ] && continue
  n=$((n + 1))
  "$loadgen" -email "$email" -org "$org" -seed "$n" -out "$out/tenant-$n.json" "$@" > "$out/tenant-$n.txt" 2>&1 &
  pids+=($!)
done < "$tenants"

failed=0
for pid in "${pids[@]}"; do
  wait "$pid" || failed=$((failed + 1))
done

# Per-tenant rates add up because the runs overlap; end_to_end is the slowest.
jq -s '{
  tenants: length,
  failed_runs: '"$failed"',
  upload_files_per_second: (map(.upload.files_per_second // 0) | add),
  published_files_per_second: (map(.published_files_per_second // 0) | add),
  slowest_end_to_end_seconds: (map(.end_to_end_seconds) | max),
  slowest_tenant_upload_files_per_second: (map(.upload.files_per_second // 0) | min),
  request_error_rate_max: (map(.request_error_rate) | max)
}' "$out"/tenant-*.json | tee "$out/summary.json"
[ "$failed" -eq 0 ]
