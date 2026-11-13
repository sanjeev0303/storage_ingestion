#!/usr/bin/env bash
# Prepares the Floci emulators for the integration tests and prints the
# variables to export. Run after `docker compose --profile floci up -d`:
#
#   eval "$(scripts/floci-setup.sh)"
#
# It creates the platform bucket ($S3_BUCKET, default object-storage-it-platform)
# and the customer bucket (object-storage-it) in floci-aws, and an IAM user with
# its own access key. The tests' leak checks search the logs and audit for the
# customer secret, so it has to be distinctive: Floci's default "test" also
# matches words like "storage_connection.test". The key only works inside the
# emulator. Needs curl 7.75 or later (--aws-sigv4).
set -euo pipefail

AWS=http://127.0.0.1:4566
PLATFORM_BUCKET=${S3_BUCKET:-object-storage-it-platform}
CUSTOMER_BUCKET=object-storage-it

s3() { curl -sS --fail --aws-sigv4 "aws:amz:us-east-1:s3" --user "test:test" "$@"; }
iam() {
  curl -sS --fail --aws-sigv4 "aws:amz:us-east-1:iam" --user "test:test" -X POST "$AWS/" \
    -H "Content-Type: application/x-www-form-urlencoded" --data "$1&Version=2010-05-08"
}

for bucket in "$PLATFORM_BUCKET" "$CUSTOMER_BUCKET"; do
  # An existing bucket answers 409, which is fine on a rerun.
  s3 -o /dev/null -X PUT "$AWS/$bucket" 2>/dev/null || true
done

iam "Action=CreateUser&UserName=orbidune-it" >/dev/null 2>&1 || true
# IAM allows two keys per user, so a rerun drops the old ones first.
for old in $(iam "Action=ListAccessKeys&UserName=orbidune-it" | grep -o '<AccessKeyId>[^<]*' | sed 's/<AccessKeyId>//'); do
  iam "Action=DeleteAccessKey&UserName=orbidune-it&AccessKeyId=$old" >/dev/null
done
key=$(iam "Action=CreateAccessKey&UserName=orbidune-it")
access=$(printf '%s' "$key" | sed -n 's:.*<AccessKeyId>\([^<]*\)</AccessKeyId>.*:\1:p')
secret=$(printf '%s' "$key" | sed -n 's:.*<SecretAccessKey>\([^<]*\)</SecretAccessKey>.*:\1:p')
if [ -z "$access" ] || [ -z "$secret" ]; then
  echo "floci-setup: could not create an access key; is floci-aws up on $AWS?" >&2
  exit 1
fi
# Already attached on a rerun answers 409.
iam "Action=AttachUserPolicy&UserName=orbidune-it&PolicyArn=arn:aws:iam::aws:policy/AmazonS3FullAccess" >/dev/null 2>&1 || true

cat <<EOF
export S3_ENDPOINT=$AWS S3_REGION=us-east-1 S3_FORCE_PATH_STYLE=true S3_BUCKET=$PLATFORM_BUCKET
export S3_ACCESS_KEY_ID=test S3_SECRET_ACCESS_KEY=test
export TEST_S3_BUCKET=$PLATFORM_BUCKET TEST_S3_CREATE_BUCKET=true
export TEST_S3_COMPATIBLE_ENDPOINT=$AWS TEST_S3_COMPATIBLE_BUCKET=$CUSTOMER_BUCKET
export TEST_S3_COMPATIBLE_ACCESS_KEY=$access TEST_S3_COMPATIBLE_SECRET_KEY=$secret
export TEST_SQS_ENDPOINT=$AWS
export TEST_GCS_ENDPOINT=http://127.0.0.1:4588
export TEST_AZURITE_ENDPOINT=http://127.0.0.1:4577
export TEST_AZURITE_KEY='Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw=='
EOF
