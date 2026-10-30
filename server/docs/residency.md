# Residency pinning and customer-managed keys

Where an organization's data may be stored, shown and enforced, and the keys it is encrypted with. Migration `000034_residency_keys.sql`.

## Where a bucket is

Every posture check (docs/storage-posture.md) asks the provider where the bucket is:

- AWS S3 and S3-compatible stores: `GetBucketLocation` (`us-east-1` when S3 answers empty, `eu-west-1` for the oldest `EU` buckets). The credentials need `s3:GetBucketLocation`.
- GCS: the bucket's location (`asia-south1`, `eu`, `us`, a dual region).
- Azure and R2 do not say, to container or bucket credentials. Their location stays **declared, not confirmed**.

The `region` check passes when the reported location matches the connection's region, fails when it does not, and is `NOT_CHECKED` when the provider did not say. The report carries `location` and `location_verified`.

The platform's own bucket is placed where the operator configured it (`S3_REGION`, recorded at API start).

## A customer-managed key per connection

`PATCH /api/v1/storage-connections/{id}` with `"encryption_key"`:

| Provider | Value | What the platform does |
|---|---|---|
| AWS S3, S3-compatible | KMS key ARN, key ID, or alias (`alias/aws/s3` is AWS's own) | SSE-KMS on every PUT, multipart upload and copy. Presigned uploads are signed with the key, so clients must send `upload_headers`, which every upload and ingestion grant returns. |
| GCS | `projects/…/locations/…/keyRings/…/cryptoKeys/…` | `kmsKeyName` on every write, compose and copy, and a signed `x-goog-encryption-kms-key-name` header on uploads. Composite upload parts are temporary and use the bucket's default until composed. |
| Azure | an encryption scope | The scope on every write and block commit, and in every SAS. Server-side copies cannot name a scope, so also set it as the container's default with "prevent override"; the posture check shows the container's default scope. |
| R2 | none | R2 does not take customer-managed keys. |

A new key is tested with a write, through the key, before it is saved: a key the credentials may not use (missing `kms:GenerateDataKey`, for example) is refused with the test result (422), and nothing changes. `""` returns to the bucket's default. The `customer_key` posture check passes while a key is set. The Python SDK and the console send the `upload_headers` they are given.

## Rules on a dataset

`PATCH /api/v1/datasets/{id}` (`datasets.manage`):

- `"residency": ["ap-south-1", "eu-*"]`: the locations its data may be stored in. These are provider location codes; a trailing `*` allows every location starting with that prefix. `[]` allows anywhere.
- `"require_customer_key": true`: only connections that encrypt every write with a customer-managed key.
- `"data_class": "PHI"` (docs/storage-posture.md).

They are enforced wherever data is placed:

- **Binding the dataset to a connection**, or changing its rules, is refused (409 `placement_refused`) when the connection does not satisfy them.
- **Every upload** into the dataset is checked when its connection is chosen, whether single, multipart or bulk ingestion.
- A location only **declared, not confirmed** by the provider never satisfies a residency rule: residency must be shown, not assumed.

Every check under rules is recorded in the audit log as `placement.check`: the rules, the connection, allowed or denied, and why.

## Showing residency

- `GET /api/v1/datasets/{id}/versions/{versionID}/residency` (`datasets.read`) lists where a snapshot's members are: each connection's location, whether the provider confirmed it, its key, the member count and bytes, and whether it is allowed. `inside` is true when every location is.
- Readiness: version 4 of the built-in profiles adds the `residency` rule (ERROR). It flags `RESIDENCY_OUTSIDE` for a member stored outside the allowed locations and `RESIDENCY_UNVERIFIED` for a member whose location the provider never confirmed. A profile can give the rule its own `{"locations": [...]}`; otherwise it uses the dataset's. It finds nothing for datasets that allow anywhere. Versions 1 to 3 stay resolvable.

## SDK and CLI

```python
from osp import posture
posture.set_encryption_key(client, connection_id, revision, "arn:aws:kms:ap-south-1:123456789012:key/…")
posture.set_residency(client, dataset_id, revision, ["ap-south-1"], require_customer_key=True)
posture.residency(client, dataset_id, version_id)
```

```
osp encryption-key CONN alias/aws/s3 | none
osp residency DATASET --allow ap-south-1,ap-south-2 --require-customer-key
osp residency DATASET [VERSION]         exit 1 when anything is outside the rules
```

## Self-hosting in one country

To keep the data and the control plane in one jurisdiction (the RBI's payment data rule in India, for example):

1. **Run every component in-country.** That means the API, the workers (processing, cleanup, dataset, pack, upload, reconciliation, compaction, outbox, webhook), PostgreSQL and its backups, the queue (SQS in the region, or ElasticMQ), ClamAV, and the console. `docker-compose.yml` in `storage-local` is the reference topology. Each service reads its settings from the environment only.
2. **The platform's own bucket** goes in-country, with `S3_REGION` set to that region: the residency report and placement checks place it there.
3. **Organizations' buckets**: give each connection credentials that may read the bucket's location (`s3:GetBucketLocation`), so its location is confirmed. Set `"residency"` on every regulated dataset, and `"require_customer_key"` where keys must be the customer's.
4. **Keys**: use KMS keys in the same region (an AWS KMS key is regional; a Cloud KMS key ring has a location). Check that key policies allow the platform's credentials `kms:GenerateDataKey` and `kms:Decrypt`.
5. **Nothing leaves by default.** The platform calls no outside service unless configured: webhooks go only to the endpoints organizations register, OIDC and SAML only to their identity providers, and the external content-check hook only where the operator points it (`CONTENT_HOOK_URL`). Keep those in-country too, or turn them off. The platform's outbound network policy refuses private and link-local addresses (outside development, where `STORAGE_ALLOW_PRIVATE_ENDPOINTS` allows them); an egress firewall that only allows in-country endpoints is the stronger control.
6. **Logs and audit.** The audit log is in PostgreSQL. Ship process logs only to an in-country log store.
7. **Prove it.** Run the readiness report with the `residency` rule on every regulated dataset before it is used, and keep the reports. Each refused placement is in the audit log.
