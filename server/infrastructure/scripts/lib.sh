#!/usr/bin/env bash
# Shared helpers for the production scripts. Source it, do not run it.

set -euo pipefail

SCRIPTS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
INFRA_DIR="$(dirname "$SCRIPTS_DIR")"
SERVER_DIR="$(dirname "$INFRA_DIR")"
REPO_DIR="$(dirname "$SERVER_DIR")"
CLIENT_DIR="$REPO_DIR/client"
TF_DIR="$INFRA_DIR/terraform/environments/prod"

# ASSUME_YES=1 (or --yes on the scripts that take it) skips confirmations,
# for CI. Destroying plans still need ALLOW_DESTROY=1.
ASSUME_YES="${ASSUME_YES:-0}"
ALLOW_DESTROY="${ALLOW_DESTROY:-0}"

log()  { printf '\033[1;34m==>\033[0m %s\n' "$*" >&2; }
warn() { printf '\033[1;33mwarning:\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31merror:\033[0m %s\n' "$*" >&2; exit 1; }

require() {
  local tool
  for tool in "$@"; do
    command -v "$tool" >/dev/null 2>&1 || die "$tool is required but not installed"
  done
}

confirm() {
  if [[ "$ASSUME_YES" == "1" ]]; then
    return 0
  fi
  local answer
  read -r -p "$1 Type 'yes' to continue: " answer
  [[ "$answer" == "yes" ]] || die "stopped: nothing was changed by this step"
}

tf() {
  terraform -chdir="$TF_DIR" "$@"
}

tf_output() {
  tf output -raw "$1"
}

tf_output_json() {
  tf output -json "$1"
}

tf_init() {
  [[ -f "$TF_DIR/backend.hcl" ]] || die "missing $TF_DIR/backend.hcl: run scripts/bootstrap-state.sh first"
  [[ -f "$TF_DIR/terraform.tfvars" ]] || die "missing $TF_DIR/terraform.tfvars: copy terraform.tfvars.example and fill it in"
  log "terraform init"
  tf init -input=false -backend-config=backend.hcl >/dev/null
}

# The stack's account must be the caller's, so a wrong profile never
# touches production (the provider's allowed_account_ids checks it again).
check_account() {
  local expected actual
  expected="$(tf_output account_id 2>/dev/null || true)"
  actual="$(aws sts get-caller-identity --query Account --output text)"
  if [[ -n "$expected" && "$expected" != "$actual" ]]; then
    die "AWS credentials are for account $actual, but the stack is in $expected"
  fi
  log "AWS account $actual"
}

aws_region() {
  tf_output region
}

# Plans, shows what changes, refuses deletions unless ALLOW_DESTROY=1, asks
# for confirmation, then applies exactly that plan. Arguments go to plan.
tf_plan_apply() {
  local plan="$TF_DIR/tfplan"
  log "terraform plan $*"
  tf plan -input=false -out=tfplan "$@"

  local summary deletes
  summary="$(tf show -json tfplan | jq -r '
    [.resource_changes[]? | select(.change.actions != ["no-op"])]
    | group_by(.change.actions | join("+"))
    | map("\(.[0].change.actions | join("+")): \(length)")
    | join(", ")')"
  deletes="$(tf show -json tfplan | jq -r '
    [.resource_changes[]? | select(.change.actions | index("delete")) | .address] | join("\n")')"

  if [[ -z "$summary" ]]; then
    log "no changes"
    rm -f "$plan"
    return 0
  fi
  log "changes: $summary"

  if [[ -n "$deletes" ]]; then
    warn "this plan deletes or replaces:"
    printf '%s\n' "$deletes" | sed 's/^/  /' >&2
    [[ "$ALLOW_DESTROY" == "1" ]] || die "refusing a plan that deletes resources; review it and rerun with ALLOW_DESTROY=1"
  fi

  confirm "Apply this plan to PRODUCTION?"
  tf apply -input=false tfplan
  rm -f "$plan"
}

# Runs a one-off task (migrations, admin) to the end and returns the exit
# code of its main container, printing its log.
run_one_off() {
  local family="$1" container="$2"
  local cluster region net subnets groups task status code reason

  cluster="$(tf_output cluster_name)"
  region="$(aws_region)"
  net="$(tf_output_json one_off_network)"
  subnets="$(jq -r '.subnets | join(",")' <<<"$net")"
  groups="$(jq -r '.security_groups | join(",")' <<<"$net")"

  log "running one-off task $family"
  task="$(aws ecs run-task \
    --region "$region" \
    --cluster "$cluster" \
    --launch-type FARGATE \
    --task-definition "$family" \
    --started-by "deploy-scripts" \
    --network-configuration "awsvpcConfiguration={subnets=[$subnets],securityGroups=[$groups],assignPublicIp=DISABLED}" \
    --query 'tasks[0].taskArn' --output text)"
  [[ -n "$task" && "$task" != "None" ]] || die "ECS did not start $family"

  # The CLI waiter gives up after ten minutes; migrations may take longer.
  while true; do
    status="$(aws ecs describe-tasks --region "$region" --cluster "$cluster" --tasks "$task" \
      --query 'tasks[0].lastStatus' --output text)"
    [[ "$status" == "STOPPED" ]] && break
    sleep 10
  done

  code="$(aws ecs describe-tasks --region "$region" --cluster "$cluster" --tasks "$task" \
    --query "tasks[0].containers[?name=='$container'].exitCode | [0]" --output text)"
  reason="$(aws ecs describe-tasks --region "$region" --cluster "$cluster" --tasks "$task" \
    --query 'tasks[0].stoppedReason' --output text)"

  log "log of $family:"
  aws logs tail "/ecs/$family" --region "$region" --since 2h --format short \
    --log-stream-name-prefix "$container/" >&2 || warn "could not read the task's log"

  if [[ "$code" != "0" ]]; then
    die "$family failed (exit code $code): $reason"
  fi
  log "$family finished"
}

wait_for_services() {
  local cluster region services
  cluster="$(tf_output cluster_name)"
  region="$(aws_region)"
  services="$(tf_output_json service_names | jq -r '.[]')"

  log "waiting for every service to settle on the new tasks"
  # describe-services takes at most ten services per call.
  printf '%s\n' "$services" | xargs -n 10 aws ecs wait services-stable --region "$region" --cluster "$cluster" --services
  log "all services are stable"
}
