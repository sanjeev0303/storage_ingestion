#!/usr/bin/env bash
# Moves every service back to an earlier image tag. Database migrations are
# not undone: they are written to be backward compatible (see migrate.sh).
# Undoing one needs `migrate down N` and a decision, not this script.
#
#   scripts/rollback.sh <tag> [--yes]
#   scripts/rollback.sh --list      # recent tags in ECR

source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

require terraform aws jq
tf_init
check_account
region="$(aws_region)"
server_repo="$(tf_output_json ecr_repositories | jq -r '.server')"

if [[ "${1:-}" == "--list" ]]; then
  aws ecr describe-images --region "$region" --repository-name "${server_repo#*/}" \
    --query 'reverse(sort_by(imageDetails,&imagePushedAt))[:15].[imagePushedAt,imageTags[0]]' --output table
  exit 0
fi

[[ $# -ge 1 ]] || die "usage: $0 <tag> [--yes] | --list"
tag="$1"
[[ "${2:-}" == "--yes" ]] && ASSUME_YES=1

current="$(tf_output image_tag)"
[[ "$tag" != "$current" ]] || die "$tag is already deployed"

for repo in server console; do
  url="$(tf_output_json ecr_repositories | jq -r --arg r "$repo" '.[$r]')"
  aws ecr describe-images --region "$region" --repository-name "${url#*/}" --image-ids "imageTag=$tag" >/dev/null 2>&1 \
    || die "$repo image $tag is not in ECR"
done

log "rolling back from $current to $tag"
tf_plan_apply -var "image_tag=$tag"
wait_for_services
"$SCRIPTS_DIR/smoke-test.sh"
log "rolled back to $tag (previous: $current)"
