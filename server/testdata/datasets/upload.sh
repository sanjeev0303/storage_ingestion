#!/usr/bin/env bash
# Uploads the fixture's files to the catalog at pets/<name>, as $DEV, through
# the API at $API. Processing (verification, scanning, promotion) publishes
# them; the manifest then names them by path.
set -euo pipefail
cd "$(dirname "$0")/pets"
for f in *.txt; do
  size=$(wc -c < "$f" | tr -d ' ')
  sum=$(sha256sum "$f" | cut -d' ' -f1)
  up=$(curl -sf "$API/api/v1/uploads" -H "Authorization: Bearer $DEV" \
    -d "{\"file_name\":\"$f\",\"content_type\":\"text/plain\",\"size_bytes\":$size,\"checksum_sha256\":\"$sum\",\"idempotency_key\":\"fixture-$f\",\"path\":\"pets/$f\"}")
  url=$(printf '%s' "$up" | python -c 'import json,sys; print(json.load(sys.stdin)["upload_url"])')
  id=$(printf '%s' "$up" | python -c 'import json,sys; print(json.load(sys.stdin)["upload_id"])')
  curl -sf -X PUT -H 'Content-Type: text/plain' --data-binary @"$f" "$url" > /dev/null
  curl -sf -X POST "$API/api/v1/uploads/$id/complete" -H "Authorization: Bearer $DEV" -d '{}' > /dev/null
  echo "uploaded pets/$f"
done
