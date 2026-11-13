# Testing with Floci

[Floci](https://floci.io/) is a set of MIT-licensed local emulators, one per cloud: AWS on port 4566, Azure on 4577, Google Cloud on 4588 (and OCI on 4599, see below). It can stand in for MinIO, ElasticMQ, Azurite and fake-gcs-server in the integration tests, and it adds services they lack: STS `AssumeRole`, KMS, S3 Object Lock and versioning, and signed GCS XML part uploads.

The default stack (MinIO, ElasticMQ) stays the reference: the tests were written against it, and Floci differs from real AWS in the places listed under "Known gaps".

## Run the suite against Floci

```sh
cd server
docker compose --profile floci up -d          # floci-aws, floci-az, floci-gcp (pinned versions)
eval "$(scripts/floci-setup.sh)"              # buckets, an IAM user and key, the TEST_* variables
export TEST_DATABASE_URL=postgres://...@127.0.0.1:5435/object_storage_test?sslmode=disable
export TEST_CLAMAV_ADDRESS=127.0.0.1:3310     # ClamAV still comes from the default stack
go test ./tests/integration/ -count=1 -timeout 60m
go test ./internal/infrastructure/storage/... -count=1
```

- `floci-aws` runs with `FLOCI_SERVICES_S3_ENFORCE_AUTH=true`. Without it Floci accepts any signature, so the signed-URL test and the wrong-credential checks pass whatever the code does.
- `scripts/floci-setup.sh` creates an IAM user with its own access key for the customer-bucket (BYOS) tests. The tests search logs and the audit trail for that secret, so it has to be distinctive; Floci's default `test` also matches words such as `storage_connection.test`.
- Floci keeps state in memory: restarting a container empties it, so run the setup script again after a restart.

## What was verified (2026-11-12, floci 2.4.0, floci-az 0.16.0, floci-gcp 0.12.0)

| Area | Result |
|---|---|
| Integration suite, S3 and SQS on floci-aws (setup script, enforced S3 auth) | 241 pass, 3 fail (the gaps below), 1 skip (the packing benchmark, opt-in) |
| GCS adapter on floci-gcp | 9 of 9 pass, including the signed XML part upload that fake-gcs-server cannot take |
| Azure adapter on floci-az | 3 of 4 emulator tests pass; blob versions are not emulated (skipped, as on Azurite) |

## Known gaps (Floci, not the platform)

These tests fail on Floci and pass on MinIO or real providers. Each is Floci behaving differently from the real service, usually by being less strict:

- **Object Lock mode changes.** Floci lets a GOVERNANCE lock change mode without `x-amz-bypass-governance-retention`; AWS refuses it (`TestProviderObjectLock`).
- **SSE-C over plain HTTP.** Floci accepts a write encrypted with a customer-provided key over HTTP; AWS and MinIO require HTTPS, which is what the posture check `sse_c_denied` expects (`TestStoragePostureAndHealthEndToEnd`).
- **SSE-KMS with enforced auth.** With `FLOCI_SERVICES_S3_ENFORCE_AUTH=true`, a presigned upload that carries an SSE-KMS key is refused with 403 although the connection's KMS write test passes; with enforcement off the same test passes, so Floci's signature check does not accept the signed encryption headers (`TestResidencyAndCustomerKeysEndToEnd`).
- **Azure SAS response headers.** A SAS download link that sets `rscd` (Content-Disposition) returns the blob without that header on floci-az; Azure and Azurite honour it (`TestAzuriteSignedUploadsAndReads`).
- **Azure blob versions** are not emulated, as on Azurite (`TestAzuriteDeleteRemovesEveryVersion` skips).

## OCI

floci-oci emulates OCI Object Storage through its native REST API (multipart uploads and pre-authenticated requests). The platform has no OCI storage provider yet, so there is nothing to point at it. Adding one (native API, with pre-authenticated requests for direct uploads) is a feature; floci-oci on port 4599 would then be its test target.
