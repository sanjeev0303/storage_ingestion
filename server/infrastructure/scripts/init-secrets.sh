#!/usr/bin/env bash
# Fills the platform's generated secrets in Secrets Manager, once. A secret
# that already has a value is left alone: changing the token pepper
# invalidates every API token, and new credential keys need
# `credential-admin reseal` (docs/storage-connections.md). Values are
# generated locally with openssl and never printed or put on a command line.
#
#   scripts/init-secrets.sh

source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

require terraform aws jq openssl
tf_init
check_account
region="$(aws_region)"

generate() {
  case "$1" in
    api-token-pepper)         openssl rand -base64 32 ;;
    storage-credential-keys)  printf '1:%s' "$(openssl rand -base64 32)" ;;
    certificate-signing-key)  openssl rand -base64 32 ;;
    # Letters and digits, 32 long: above AUTH_PASSWORD_MIN_LENGTH.
    admin-bootstrap-password) openssl rand -base64 48 | tr -dc 'A-Za-z0-9' | cut -c1-32 ;;
    *) die "no generator for secret $1" ;;
  esac
}

has_value() {
  aws secretsmanager describe-secret --region "$region" --secret-id "$1" \
    --query 'length(keys(VersionIdsToStages || `{}`))' --output text 2>/dev/null | grep -qv '^0$'
}

secrets="$(tf_output_json app_secret_ids)"
umask 077
scratch="$(mktemp)"
trap 'rm -f "$scratch"' EXIT

for name in $(jq -r 'keys[]' <<<"$secrets"); do
  id="$(jq -r --arg n "$name" '.[$n]' <<<"$secrets")"
  if has_value "$id"; then
    log "$name already has a value; left unchanged"
    continue
  fi
  generate "$name" | tr -d '\n' >"$scratch"
  [[ -s "$scratch" ]] || die "generating $name produced nothing"
  aws secretsmanager put-secret-value --region "$region" --secret-id "$id" \
    --secret-string "file://$scratch" >/dev/null
  : >"$scratch"
  log "$name generated and stored"
done

log "secrets are in place"
