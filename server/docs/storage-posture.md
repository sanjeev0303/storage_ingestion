# Storage posture and connection health

What the buckets behind bring-your-own-storage connections allow, kept under watch: their security settings, the platform's own write path, a second credential with rollback, and objects changed outside the platform. Migration `000033_storage_posture.sql`.

## Posture checks

Every passing connection test checks the bucket's posture, and the cleanup worker checks each connection again every `STORAGE_POSTURE_INTERVAL` (24h). Nothing is changed in the bucket: every check reads a setting. A check the provider or the credentials cannot answer is `NOT_CHECKED`, never a pass.

| Check | AWS S3 | R2, S3-compatible | GCS | Azure |
|---|---|---|---|---|
| `public_access_block` | Block Public Access, all four settings | where supported | public access prevention | container access level |
| `public_policy` | bucket policy status (or the policy itself) | the policy itself | IAM `allUsers`, `allAuthenticatedUsers` | account level, not checked |
| `public_acl` | grants to AllUsers, AuthenticatedUsers | same | uniform access, ACL entries | account level, not checked |
| `object_ownership` | "bucket owner enforced" | | | |
| `default_encryption` | SSE-S3 or SSE-KMS, with the key | where supported | Google or Cloud KMS key | always on |
| `sse_c_denied` | bucket's `BlockedEncryptionTypes`, a policy deny, or the probe | the probe | the probe (CSEK) | not checked |
| `versioning` | | R2 has none: a warning | object versioning | immutable storage with versioning |
| `object_lock` | Object Lock | | locked retention policy, object retention | immutability policy |
| `lifecycle_rules` | expiry of current or non-current versions under the platform's prefixes fails; archive tiers warn | same | delete rules under the prefixes | account level, not checked |
| `incomplete_upload_expiry` | an abort-incomplete-upload rule | same | the platform cleans its own | blocks expire after 7 days |
| `credential_scope` | root user keys fail; listing every bucket warns | listing every bucket warns | not checked | |
| `bucket_owner` | the pinned account answers `HeadBucket` | | the pinned project number | |
| `guessable_name` | names without a random part warn | scoped to the account | same as AWS | scoped to the account |
| `self_test` | PUT, a multipart upload through a presigned part URL, GET and DELETE | | | |
| `lifecycle_drift` | the rules' fingerprint against the last check | | | |

**The SSE-C probe.** Stolen write keys have been used to re-encrypt objects with a customer-provided key and hold them to ransom. With `probe` (the connection test's write probe, or `POST .../posture {"probe":true}`), the platform writes one small object with a random customer key under its own prefix: the bucket must refuse it, and an accepted one is deleted at once.

`GET /api/v1/storage-connections/{id}/posture` (`storage.read`) returns the latest report: each check's `status` (PASS, WARN, FAIL, NOT_CHECKED) and `detail`, `worst`, and `phi_safe`. `POST` with `{"probe": bool}` checks now (`storage.manage`). The connection test's response carries `posture` as well.

A connection is **safe for PHI** when `public_access_block`, `public_policy`, `public_acl`, `default_encryption`, `versioning` and `sse_c_denied` all pass.

**Alerts** (security alerts, and the `security.alert` webhook): `STORAGE_POSTURE` when a connection starts failing, or stops being safe for PHI; `LIFECYCLE_CHANGED` when its lifecycle rules change.

## Regulated data

`PATCH /api/v1/datasets/{id}` with `"data_class": "PHI" | "PCI" | "PII" | ""` (`datasets.manage`). A PHI dataset is stored only on connections safe for PHI, or on the platform's own storage:

- binding the dataset to a connection, or marking it PHI while it is bound to one, is refused (409 `placement_refused`) unless that connection's last posture check found it safe;
- every upload into the dataset is checked the same way when its connection is chosen.

## Explicit checksum mode

`PATCH /api/v1/storage-connections/{id}` with `"checksum_mode"`:

- `AUTO`: the newer CRC request checksums on AWS, only when an operation requires one elsewhere. Several S3-compatible stores broke when SDKs made them the default.
- `WHEN_SUPPORTED`: always send them.
- `WHEN_REQUIRED`: only when an operation requires one.

The change is tested against the bucket first and refused (422, with the test result) if it fails.

## Bucket owner pinning

`"expected_owner": "<AWS account ID or Google project number>"` (empty unpins). On AWS every request then carries `x-amz-expected-bucket-owner`, so a bucket deleted and re-created under the same name by someone else refuses the platform instead of receiving its data. Presigned URLs are signed without it, since a browser could not send the header. The posture report suggests the account to pin when the bucket belongs to the credentials' own account.

## Two credentials

1. `POST .../credentials/stage {"credentials": {...}}`: the new keys are tested, writing included, and kept beside the current ones, unused.
2. `POST .../credentials/promote`: they are tested again (the keys may have been revoked since), then put in use. The replaced ones are kept for **7 days**.
3. `POST .../credentials/rollback`: puts the replaced ones back while they are kept.
4. `DELETE .../credentials/staged` discards staged keys.

A failed test answers 422 with the result; nothing changes. Replaced credentials past their window are erased by the cleanup worker. The connection response shows `staged_credential` and `previous_credential_until`, never a secret.

## Changes outside the platform

The cleanup worker visits each connection every `STORAGE_DRIFT_INTERVAL` (6h), checking up to `STORAGE_DRIFT_BUDGET` (2000) objects per visit and carrying on from where it stopped:

- each published object, or pack, against what was recorded when the platform wrote it: the pinned version deleted (`DELETED`), a newer version written on top (`NEW_VERSION`), the object replaced in an unversioned bucket (`OVERWRITTEN`, by size or ETag), or no longer readable without a key the platform does not have (`UNREADABLE`, as after an SSE-C re-encryption);
- the next page of the platform's prefix, for objects it never wrote (`UNKNOWN_OBJECT`).

A provider that does not answer is not a finding; the scan stops and tries again next time. Each finding is raised once while open.

- `GET /api/v1/storage-connections/{id}/drift` and `GET /api/v1/storage-drift` (`storage.read`, `?all=true` for resolved ones); `POST /api/v1/storage-drift/{id}/resolve` (`storage.manage`).
- `GET /api/v1/datasets/{id}/versions/{versionID}/integrity` (`datasets.read`): whether the snapshot is **degraded**, and which member paths changed how. Object keys never leave through it.
- Readiness: version 3 of the built-in profiles adds the `storage-integrity` rule (ERROR): `STORAGE_CHANGED_OUTSIDE` for members with an open finding, and `STORAGE_POSTURE_FAILING` for members on a connection failing its posture (`allow_posture_fail` turns that off). Versions 1 and 2 stay resolvable.
- An `OUT_OF_BAND_WRITE` security alert is raised per connection and hour with findings.

## Removing a connection

Deleting a connection that nothing depends on writes a tombstone, `<ready prefix>/<organization>/.osp-tombstone.json`, saying the prefix is no longer managed. The connection's row is kept as a tombstone too. The delete still goes ahead if the tombstone cannot be written (the bucket is gone, or the keys revoked).

## Downloads carry the content hash

Proxied and share-link downloads send `Repr-Digest: sha-256=:<base64>:` (RFC 9530) with the whole object's SHA-256, ranges included, so a client can pin and check the content it was promised.

## SDK and CLI

```python
from osp import posture
report = posture.check(client, connection_id, probe=True)
posture.stage_credentials(client, connection_id, access_key_id="AKIA...", secret_access_key=secret)
posture.promote_credentials(client, connection_id)
posture.integrity(client, dataset_id, version_id)
```

```
osp posture CONN [--check | --probe]       exit 1 when a check fails
osp credentials stage CONN --access-key-id AKIA...   (secret from OSP_STORAGE_SECRET, or asked)
osp credentials promote|rollback|discard CONN
osp drift [CONN] [--all] [--resolve FINDING]  exit 1 while findings are open
osp integrity DATASET VERSION               exit 1 when degraded
osp data-class DATASET phi|pci|pii|none
```

The console shows the posture, the integrity settings, the second credential and the changes found on each connection's page; the data class on the dataset's overview; and a degraded notice on a snapshot's page.
