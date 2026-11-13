#!/usr/bin/env bash
# Full production deployment of the current commit.
#
#   scripts/deploy.sh [--tag TAG] [--yes] [--skip-tests] [--allow-dirty] [--plan-only]
#
# Every run:
#   1. picks the release: the current commit (the tree must be clean)
#   2. runs the server and console tests
#   3. builds, scans and pushes the images (build-and-push.sh)
#   4. moves only the migration task to the new image, applies migrations
#      (migrate.sh), then moves every service to it; ECS rolls each service
#      task by task and rolls back on its own if new tasks keep failing
#   5. waits until every service is stable and runs the smoke test
#
# The first run (no deployed image yet) creates the foundation the migration
# needs first, fills the generated secrets (init-secrets.sh), migrates, and
# then creates the rest. After it, run bootstrap-admin.sh once.
#
# Every terraform apply shows the plan and asks first (--yes skips that for
# CI). A plan that deletes anything stops unless ALLOW_DESTROY=1.

source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

usage() {
  sed -n '2,20p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
}

tag=""
skip_tests=0
allow_dirty=0
plan_only=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --tag)         tag="${2:?--tag needs a value}"; shift 2 ;;
    --yes)         ASSUME_YES=1; shift ;;
    --skip-tests)  skip_tests=1; shift ;;
    --allow-dirty) allow_dirty=1; shift ;;
    --plan-only)   plan_only=1; shift ;;
    -h|--help)     usage; exit 0 ;;
    *)             die "unknown option $1 (see --help)" ;;
  esac
done

require terraform aws docker git jq curl openssl

# ─── 1. The release ───────────────────────────────────────────────────

cd "$REPO_DIR"
if [[ -n "$(git status --porcelain)" && "$allow_dirty" != "1" ]]; then
  die "the working tree has uncommitted changes: a release is a commit (or pass --allow-dirty to ship them anyway)"
fi
if [[ -z "$tag" ]]; then
  tag="$(git rev-parse --short=12 HEAD)"
  [[ "$allow_dirty" == "1" && -n "$(git status --porcelain)" ]] && tag="$tag-dirty-$(date +%Y%m%d%H%M%S)"
fi
log "release $tag"

tf_init
check_account

if [[ "$plan_only" == "1" ]]; then
  tf plan -input=false -var "image_tag=$tag"
  exit 0
fi

# ─── 2. Tests ─────────────────────────────────────────────────────────

if [[ "$skip_tests" != "1" ]]; then
  require go npm
  log "server: vet and unit tests"
  (cd "$SERVER_DIR" && go vet ./... && go test -count=1 ./internal/... ./cmd/...)
  log "console: type-check, lint and unit tests"
  (cd "$CLIENT_DIR" && npm ci --no-audit --no-fund && npm run typecheck && npm run lint && npx vitest run)
else
  warn "skipping tests"
fi

# ─── 3 and 4. Images, migrations, services ────────────────────────────

current="$(tf_output image_tag 2>/dev/null || true)"
first=0

if [[ -z "$current" ]]; then
  first=1
  log "first deployment: creating what the migration needs"
  # Targeted only this once: the network, the database and its secret, the
  # image repositories, the cluster and the one-off tasks. Services come
  # after the schema exists, so they do not start against an empty database.
  tf_plan_apply -var "image_tag=$tag" \
    -target=module.network \
    -target=aws_secretsmanager_secret_version.database_url \
    -target=aws_secretsmanager_secret.app \
    -target=aws_ecr_repository.main \
    -target=aws_ecr_lifecycle_policy.main \
    -target=aws_ecs_cluster.main \
    -target=aws_vpc_security_group_egress_rule.tasks_out \
    -target=module.migrate \
    -target=module.admin

  "$SCRIPTS_DIR/init-secrets.sh"
  "$SCRIPTS_DIR/build-and-push.sh" "$tag"
  "$SCRIPTS_DIR/migrate.sh"

  log "creating everything else"
  tf_plan_apply -var "image_tag=$tag"
else
  if [[ "$current" == "$tag" ]]; then
    warn "$tag is already deployed; applying it again"
  fi
  "$SCRIPTS_DIR/build-and-push.sh" "$tag"

  log "moving the migration task to $tag (services stay on $current)"
  tf_plan_apply -var "image_tag=$current" -var "migration_image_tag=$tag"
  "$SCRIPTS_DIR/migrate.sh"

  log "moving the services to $tag"
  tf_plan_apply -var "image_tag=$tag"
fi

# ─── 5. Verify ────────────────────────────────────────────────────────

wait_for_services
"$SCRIPTS_DIR/smoke-test.sh"

log "deployed $tag"
if [[ "$first" == "1" ]]; then
  log "next, once: scripts/bootstrap-admin.sh"
elif [[ "$current" != "$tag" ]]; then
  log "to roll back: scripts/rollback.sh $current"
fi
