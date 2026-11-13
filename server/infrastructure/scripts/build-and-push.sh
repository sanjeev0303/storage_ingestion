#!/usr/bin/env bash
# Builds the server and console images for linux/amd64, tags them with the
# release tag, pushes them to ECR, and mirrors the ClamAV and busybox images
# so tasks never pull from Docker Hub. Tags are immutable: an existing tag
# is not rebuilt.
#
#   scripts/build-and-push.sh <tag>

source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

[[ $# -eq 1 ]] || die "usage: $0 <tag>"
tag="$1"
[[ "$tag" != "latest" ]] || die "use an immutable tag such as the git commit, not latest"

require terraform aws docker jq
tf_init
check_account
region="$(aws_region)"
repos="$(tf_output_json ecr_repositories)"
mirrors="$(tf_output_json mirror_image_tags)"
registry="$(jq -r '.server' <<<"$repos" | cut -d/ -f1)"

log "logging in to $registry"
aws ecr get-login-password --region "$region" | docker login --username AWS --password-stdin "$registry" >/dev/null

image_exists() {
  local repo_url="$1" image_tag="$2"
  aws ecr describe-images --region "$region" --repository-name "${repo_url#*/}" \
    --image-ids "imageTag=$image_tag" >/dev/null 2>&1
}

# Fails on critical findings when trivy is installed; ECR also scans on push.
scan() {
  if command -v trivy >/dev/null 2>&1; then
    log "scanning $1"
    trivy image --quiet --exit-code 1 --severity CRITICAL --ignore-unfixed "$1"
  else
    warn "trivy is not installed: relying on ECR's scan on push only"
  fi
}

build() {
  local name="$1" context="$2" dockerfile="$3"
  local repo_url image
  repo_url="$(jq -r --arg n "$name" '.[$n]' <<<"$repos")"
  image="$repo_url:$tag"
  if image_exists "$repo_url" "$tag"; then
    log "$image already exists; not rebuilding"
    return 0
  fi
  log "building $image"
  docker build --platform linux/amd64 --pull -f "$dockerfile" -t "$image" \
    --label "org.opencontainers.image.revision=$tag" "$context"
  scan "$image"
  docker push "$image"
}

mirror() {
  local name="$1" source="$2"
  local repo_url mirror_tag image
  repo_url="$(jq -r --arg n "$name" '.[$n]' <<<"$repos")"
  mirror_tag="$(jq -r --arg n "$name" '.[$n]' <<<"$mirrors")"
  image="$repo_url:$mirror_tag"
  if image_exists "$repo_url" "$mirror_tag"; then
    return 0
  fi
  log "mirroring $source:$mirror_tag to $image"
  docker pull --platform linux/amd64 "$source:$mirror_tag"
  docker tag "$source:$mirror_tag" "$image"
  docker push "$image"
}

build server  "$SERVER_DIR" "$INFRA_DIR/docker/Dockerfile.server"
build console "$CLIENT_DIR" "$INFRA_DIR/docker/Dockerfile.console"
mirror clamav  clamav/clamav
mirror busybox busybox

log "images for $tag are in ECR"
