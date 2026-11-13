#!/usr/bin/env bash
# Applies pending database migrations with the migration task's current
# image (cmd/migrate up), and fails if any migration fails. Migrations are
# forward-only in production: write them so the previous release still runs
# against the new schema, which keeps rollback.sh safe.
#
#   scripts/migrate.sh

source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

require terraform aws jq
tf_init
check_account

run_one_off "$(tf_output migrate_task_family)" migrate
