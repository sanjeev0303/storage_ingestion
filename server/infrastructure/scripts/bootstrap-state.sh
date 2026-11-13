#!/usr/bin/env bash
# Creates the Terraform state bucket (once per account) and writes the
# production stack's backend.hcl to use it.
#
#   scripts/bootstrap-state.sh <state-bucket-name> <region>

source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

[[ $# -eq 2 ]] || die "usage: $0 <state-bucket-name> <region>"
bucket="$1"
region="$2"

require terraform aws

bootstrap_dir="$INFRA_DIR/terraform/bootstrap"
log "AWS account $(aws sts get-caller-identity --query Account --output text)"

terraform -chdir="$bootstrap_dir" init -input=false
terraform -chdir="$bootstrap_dir" plan -input=false -out=tfplan \
  -var "state_bucket_name=$bucket" -var "region=$region"
confirm "Create the state bucket $bucket in $region?"
terraform -chdir="$bootstrap_dir" apply -input=false tfplan
rm -f "$bootstrap_dir/tfplan"

key_arn="$(terraform -chdir="$bootstrap_dir" output -raw state_kms_key_arn)"
cat >"$TF_DIR/backend.hcl" <<EOF
bucket     = "$bucket"
region     = "$region"
kms_key_id = "$key_arn"
EOF

log "wrote $TF_DIR/backend.hcl"
log "keep $bootstrap_dir/terraform.tfstate safe: it is the only record of the state bucket's own stack"
