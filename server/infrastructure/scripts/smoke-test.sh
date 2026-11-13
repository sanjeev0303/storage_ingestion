#!/usr/bin/env bash
# Checks the public endpoints after a deployment: the API is alive and can
# reach its database, the console serves its sign-in page, and plain HTTP
# redirects to HTTPS. Retries for a while, since a deployment may still be
# draining old tasks.
#
#   scripts/smoke-test.sh

source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

require terraform curl
tf_init

api="$(tf_output api_url)"
console="$(tf_output console_url)"
attempts="${SMOKE_ATTEMPTS:-12}"

expect_status() {
  local url="$1" want="$2" got i
  for ((i = 1; i <= attempts; i++)); do
    got="$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 "$url" || true)"
    if [[ "$got" == "$want" ]]; then
      log "ok   $url -> $got"
      return 0
    fi
    sleep 10
  done
  die "$url answered $got, expected $want"
}

expect_status "$api/health" 200
expect_status "$api/ready" 200
expect_status "$console/login" 200
expect_status "${api/https:/http:}/health" 301

log "smoke test passed"
