#!/usr/bin/env bash
# Makes the first platform administrator (admin_email in terraform.tfvars)
# with the password stored in Secrets Manager by init-secrets.sh. Run once,
# after the first deployment; later runs fail harmlessly ("bootstrap done").
# The password is never printed: fetch it from Secrets Manager, sign in,
# change it, then delete the secret's value if you like.
#
#   scripts/bootstrap-admin.sh

source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

require terraform aws jq
tf_init
check_account

run_one_off "$(tf_output admin_task_family)" identity-admin

secret_id="$(tf_output_json app_secret_ids | jq -r '."admin-bootstrap-password"')"
log "the administrator can sign in at $(tf_output console_url)"
log "password: aws secretsmanager get-secret-value --region $(aws_region) --secret-id '$secret_id' --query SecretString --output text"
