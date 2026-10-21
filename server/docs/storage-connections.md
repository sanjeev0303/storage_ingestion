# Storage connections and bring your own storage (Phase 11)

An organization's bytes live on a **storage connection**:

- **Platform-managed** (`PLATFORM_MANAGED`, ID `platform`): the deployment's own buckets (`S3_BUCKET`, `PROMOTION_BUCKET`). It is every organization's initial default, and the only connection while BYOS is off.
- **Customer-managed** (bring your own storage): the organization's own bucket or container on AWS S3, Cloudflare R2, Google Cloud Storage, Azure Blob Storage or another S3-compatible service.

Each upload, asset and object version records the connection it was written to. Every later step uses that binding and nothing else: verification, scanning, promotion, download, deletion and reconciliation. Changing the default, or disabling a connection, never moves an object or reroutes an old one.

## Contents

1. [Dependency map: what assumed one global bucket](#dependency-map)
2. [Providers and capabilities](#providers-and-capabilities)
3. [Required provider permissions](#required-provider-permissions)
4. [Credential encryption and rotation](#credential-encryption-and-rotation)
5. [Endpoint and network policy](#endpoint-and-network-policy)
6. [Connection API](#connection-api)
7. [Choosing a connection](#choosing-a-connection)
8. [Upload, verification, promotion and download](#upload-verification-promotion-and-download)
9. [Lifecycle, cleanup and reconciliation](#lifecycle-cleanup-and-reconciliation)
10. [Security responsibilities on customer-owned storage](#security-responsibilities-on-customer-owned-storage)
11. [Migration, backfill and rollback](#migration-backfill-and-rollback)
12. [Local setup and tests](#local-setup-and-tests)
13. [Limitations](#limitations)

## Dependency map

Before Phase 11, every service took one `ObjectStorage` client and one or two bucket names at startup. This is where that assumption lived and how it is resolved now.

| Component | Assumed before | Now |
|---|---|---|
| `upload.Service` (single uploads) | presign, head and delete in `S3_BUCKET` under `quarantine/` | the session's **staging connection**; a new session is bound by the chooser |
| `upload.MultipartService` | begin, presign, list parts, complete and abort in `S3_BUCKET`; S3 part rules | the session's staging connection; part rules (minimum size, ETag or block-ID validator) from that provider |
| Multipart recovery (`Resolve*`, `Expire`, `VerifyIntegrity`) | the same bucket | the session's staging connection; the expected key is re-derived from it |
| Prefix scans (`ScanOrphanSessions`, `ScanOrphanObjects`) | `quarantine/` in `S3_BUCKET` | still the platform bucket only; a session bound elsewhere is never touched there. `AbortLeftoverSessions` follows each session's binding |
| `processing.Service` | read, scan and purge in `S3_BUCKET` | the asset's staging connection |
| `promotion.Service` | copy `S3_BUCKET` → `PROMOTION_BUCKET` | staging connection → storage connection: a provider copy on one connection, a bounded stream across two |
| `delivery.Service` | presign, head and range reads in the delivery bucket; CDN | the asset's storage connection; the CDN only fronts the platform's bucket |
| `asset.Service` (deletion, retention, holds) | delete exact objects in both buckets | each target on the connection that holds it, with version IDs where recorded |
| `cleanup.Service` (orphans, missing objects) | scan both platform prefixes | the platform's prefixes and every tenant connection's staging and ready prefixes, each through its own client; scans and candidates are per connection |
| `catalog.Reconciler` | head versions in the delivery bucket | each version through its connection; the unreferenced-object listing stays on the platform (tenant buckets are swept by cleanup) |
| `dataset` validation content reads | read members from the delivery bucket | each member through its version's connection |
| Manifests (ingestion, dataset imports) | `S3_BUCKET` | unchanged: manifests are platform-owned working files, not customer data |
| Events, outbox, queues | IDs only | unchanged: messages carry IDs, never credentials or connection settings. Workers resolve connections from the database |
| `compaction` | database only | unchanged |
| Commands | one S3 client from `S3_*` | the same client for the platform, plus a registry for tenant connections: `cmd/api`, `processing-worker`, `upload-worker`, `cleanup-worker`, `reconciliation-worker`, `dataset-worker`, `catalog-admin reconcile`, `lifecycle-admin` |

A record bound to a connection that cannot be reached (an outage, BYOS turned off, an unreadable credential) fails and is retried later. It is never served from, or written to, another connection.

## Providers and capabilities

`GET /api/v1/storage-providers/capabilities` returns what each adapter implements. A connection test records what the actual bucket allows, for example whether versioning is on.

| | AWS S3 | Cloudflare R2 | S3-compatible | Google Cloud Storage | Azure Blob |
|---|---|---|---|---|---|
| Adapter | AWS SDK v2 | AWS SDK v2: region `auto`, `https://<account>.r2.cloudflarestorage.com`, path style | AWS SDK v2, custom endpoint | official `cloud.google.com/go/storage` | official `azblob` |
| Auth methods | `ACCESS_KEY` (+ optional session token), `ASSUME_ROLE` | `ACCESS_KEY` | `ACCESS_KEY` | `SERVICE_ACCOUNT_KEY` | `ACCOUNT_KEY`, `SAS_TOKEN` |
| Direct (presigned) upload | ✓ (≤ 5 GiB) | ✓ | ✓ | ✓ V4 signed URL | ✓ SAS (`ACCOUNT_KEY` only; send `x-ms-blob-type: BlockBlob`) |
| Large uploads | S3 multipart (5 MiB min part, ≤ 10 000 parts) | S3 multipart | S3 multipart | parallel composite: parts are objects, composed 32 at a time, in levels | block blob: `Put Block` per part, `Put Block List` to complete |
| Part check | ETag (content) | ETag | ETag | MD5 (content) | block ID + size (no content validator; SHA-256 after upload proves the bytes) |
| Copy inside the provider | ✓ | ✓ | ✓ | ✓ (rewrite) | ✓ (`Copy Blob`, waited for) |
| Range and conditional reads | ✓ | ✓ | ✓ | ✓ (generation-pinned) | ✓ (ETag / version) |
| Presigned download | ✓ | ✓ | ✓ | ✓ | ✓ with `ACCOUNT_KEY`; a `SAS_TOKEN` connection serves downloads as API token links |
| Version identifiers | S3 version ID (versioned buckets) | none | S3 version ID if supported | **generation**, kept as the provider's own decimal ID, never mapped to an S3 version | Azure version ID when account versioning is on |
| Provider checksums | MD5 | MD5 | MD5 | MD5, CRC32C (composites: CRC32C only) | MD5, CRC64 |

The platform never trusts a provider checksum or ETag as content proof. Every upload's SHA-256 is computed from the stored bytes before it can be promoted.

## Required provider permissions

Grant the platform these, scoped to the one bucket or container (and ideally to the staging and ready prefixes).

**AWS S3**: `s3:ListBucket`, `s3:GetObject`, `s3:PutObject`, `s3:DeleteObject`, `s3:AbortMultipartUpload`, `s3:ListMultipartUploadParts`, `s3:ListBucketMultipartUploads`. Optionally:
- `s3:GetBucketVersioning`: without it versioning is reported `UNKNOWN`;
- `s3:GetObjectVersion` and `s3:DeleteObjectVersion`: for versioned buckets.

For `ASSUME_ROLE`, the role's trust policy must allow the platform's AWS principal and require the connection's `external_id`:

```json
{"Effect": "Allow", "Principal": {"AWS": "<platform role ARN>"}, "Action": "sts:AssumeRole",
 "Condition": {"StringEquals": {"sts:ExternalId": "<external_id from the connection>"}}}
```

**Cloudflare R2**: an R2 API token with *Object Read & Write* on the bucket.

**S3-compatible**: the same operations as AWS S3, for the access key.

**Google Cloud Storage**: on the bucket:
- `roles/storage.objectAdmin`: create, get, list, delete, compose, rewrite;
- `storage.buckets.get`: for the capability check (versioning).

Signing uses the key's private key locally; no `iam.serviceAccounts.signBlob` is needed.

**Azure Blob**:
- `ACCOUNT_KEY` uses the storage account key.
- A `SAS_TOKEN` must be a container SAS with `r`, `a`, `c`, `w`, `d` and `l` permissions, valid for as long as the connection is used.

## Credential encryption and rotation

- **Encryption.** Credentials are accepted only on create and rotate, and stored only as AES-256-GCM ciphertext (`internal/platform/secretbox`). The ciphertext is sealed with a dedicated 32-byte key from `STORAGE_CREDENTIAL_KEYS`. With BYOS on, a missing, malformed, all-zero or reused key (the database password, `S3_SECRET_ACCESS_KEY` and so on) stops the process at startup.
- **Associated data.** Each ciphertext's associated data is `storage-credential/v1|<tenant>|<connection>|<provider>|<credential version>`. A ciphertext copied to another row, tenant, provider or version does not open.
- **Where plaintext lives.** Plaintext exists only while a provider client is built; the buffer is cleared after decoding. `model.StorageSecrets` prints, logs and JSON-encodes as `[redacted credentials]`. No API response, audit event, log line, error, metric or queue message carries a credential. Provider error messages are reduced to categories.
- **Credential rotation.** `POST /{id}/rotate-credentials` tests the new credentials first and stores them only if the test passes. It then bumps the credential version and drops cached clients in this process; other processes rebuild when their connection cache expires (`STORAGE_CONNECTION_CACHE_TTL`, 10 s). Only the newest credential is stored, so a process that needs a new client while its cached row still names the old version re-reads the row instead of failing; clients it already holds keep working with the old secret until they are rebuilt. The old secret is never returned.
- **Key rotation.**
  1. Add a new key version (`STORAGE_CREDENTIAL_KEYS=1:<old>,2:<new>`, `STORAGE_CREDENTIAL_KEY_VERSION=2`) on every process. New credentials are then sealed with version 2, and old ciphertexts still open with version 1.
  2. Rotate each connection's credentials to re-seal them.
  3. Remove version 1 once no row uses it:
     ```sql
     SELECT count(*) FROM storage_connections WHERE credential_key_version = 1;
     ```
- **KMS.** `secretbox.Sealer` (`Seal` / `Open` with associated data) is the seam for a KMS-backed envelope implementation. None is deployed in this phase.

## Endpoint and network policy

Endpoints are a security boundary (`internal/platform/netguard`, `registry.Endpoint`).

- **Accepted URLs.** Only absolute `https` URLs without userinfo, path, query or fragment, with a valid port and host name.
- **Plain http.** Only with `STORAGE_ALLOW_PRIVATE_ENDPOINTS=true`, which only `APP_ENV=development` accepts, and then only to `localhost` or a loopback or private address: MinIO, fake-gcs-server, Azurite.
- **Known providers.** These must stay on their own domains:
  - AWS: `amazonaws.com`
  - R2: `r2.cloudflarestorage.com`
  - GCS: `storage.googleapis.com`
  - Azure: `blob.core.windows.net`
- **Generic S3-compatible endpoints.** Any public host.
- **Checked on every connection.** Every dial checks the address it actually connects to, so a name that later resolves to loopback, link-local (`169.254.169.254`), private, carrier-grade NAT or reserved space is refused when the connection is made. This defeats DNS rebinding. A connection test also resolves the name first and reports `ENDPOINT_REJECTED`.
- **HTTP client.** No redirects are followed and no proxy is used. TLS is 1.2 or newer. Dial, TLS-handshake and response-header timeouts apply, and each metadata call is bounded by `STORAGE_OPERATION_TIMEOUT`.
- **Abuse limits.** Connection tests and rotations are rate limited per organization (`RATE_LIMIT_STORAGE_TEST`, 10/min by default) and need `storage.manage`, so a test cannot be used as a network scanner.

## Connection API

Every route needs a signed-in member of the organization. The `storage.*` permissions are in `docs/security.md`: everyone reads, developers may *use* a non-default connection, owners and admins *manage*.

| Method and path | Permission | Notes |
|---|---|---|
| `GET /api/v1/storage-providers` | `storage.read` | auth methods, required fields, notes; `byos_enabled` |
| `GET /api/v1/storage-providers/capabilities` | `storage.read` | declared capabilities per provider |
| `POST /api/v1/storage-connections` | `storage.manage` | creates `PENDING`; credentials in `credentials` |
| `GET /api/v1/storage-connections` | `storage.read` | the platform connection, the organization's own, `default_connection_id` |
| `GET /api/v1/storage-connections/{id}` | `storage.read` | `ETag` is the revision |
| `PATCH /api/v1/storage-connections/{id}` | `storage.manage` | name, region, endpoint, path style, staging mode; `If-Match` or `revision`; a new endpoint, region or path style returns it to `PENDING` |
| `POST /api/v1/storage-connections/{id}/test` | `storage.manage` | `{"write_probe": true}` also writes, reads back and deletes one small object of its own under the ready prefix; a pass activates a `PENDING` connection |
| `POST /api/v1/storage-connections/{id}/rotate-credentials` | `storage.manage` | tested before stored; 422 with the test result when refused |
| `POST /api/v1/storage-connections/{id}/set-default` | `storage.manage` | `ACTIVE` connections or `platform` |
| `POST /api/v1/storage-connections/{id}/disable` | `storage.manage` | not the default; existing work keeps draining |
| `POST /api/v1/storage-connections/{id}/enable` | `storage.manage` | back to `PENDING`, then tested |
| `DELETE /api/v1/storage-connections/{id}` | `storage.manage` | only disabled or pending, with nothing depending on it; 409 lists active uploads, live assets, datasets and default; erases the credential |

Statuses: `PENDING` (created or repointed, not yet tested) → `ACTIVE` (tested; takes new uploads) → `DISABLED` (no new uploads; reads, processing and deletion continue) → `DELETED` (tombstone, credential erased; nothing can bind to it).

A test answers a category, never the provider's message. The categories are `ACCESS_DENIED`, `NOT_FOUND`, `THROTTLED`, `NETWORK`, `ENDPOINT_REJECTED`, `TLS`, `INVALID_CREDENTIALS`, `UNSUPPORTED` and `PROVIDER_ERROR`. A test never creates or deletes customer data; the optional write probe touches only `<ready>/<tenant>/.connection-test/<random>` and removes it, including its version where versioning is on.

Audit events, all without credentials: `storage_connection.create`, `.update`, `.test`, `.rotate`, `.set_default`, `.disable`, `.enable`, `.delete`, and denied attempts.

Example with the local MinIO (the development policy must allow private endpoints):

```bash
curl -s -X POST localhost:8080/api/v1/storage-connections \
  -H "Authorization: Bearer $TOKEN" -H "X-Organization-ID: $ORG" -H 'Content-Type: application/json' -d '{
  "name": "Team bucket", "provider": "S3_COMPATIBLE", "auth_method": "ACCESS_KEY",
  "bucket": "team-bucket", "endpoint": "http://127.0.0.1:9000", "force_path_style": true,
  "staging_prefix": "quarantine", "ready_prefix": "assets", "staging_mode": "CONNECTION",
  "credentials": {"access_key_id": "...", "secret_access_key": "..."}}'
curl -s -X POST localhost:8080/api/v1/storage-connections/$ID/test -d '{"write_probe": true}' ...
curl -s -X POST localhost:8080/api/v1/storage-connections/$ID/set-default ...
```

Other providers' bodies:

```text
AWS_S3        "region": "eu-west-1", "auth_method": "ASSUME_ROLE", "role_arn": "arn:aws:iam::123456789012:role/storage"
CLOUDFLARE_R2 "account_id": "<cloudflare account id>"
GOOGLE_CLOUD_STORAGE  "auth_method": "SERVICE_ACCOUNT_KEY", "credentials": {"service_account_json": "<the key file, as a string>"}
AZURE_BLOB    "account_id": "<storage account>", "bucket": "<container>", "auth_method": "ACCOUNT_KEY", "credentials": {"account_key": "..."}
AZURE_BLOB    "auth_method": "SAS_TOKEN", "staging_mode": "PLATFORM", "credentials": {"sas_token": "sv=...&sig=..."}
```

## Choosing a connection

A new upload is bound to the first connection that applies:

1. **The upload's request.** `storage_connection_id` on `POST /api/v1/uploads` or `/uploads/multipart`. A connection other than the organization's default needs `storage.use`.
2. **The upload's dataset.** Set with `PATCH /api/v1/datasets/{id}` `{"storage_connection_id": ...}`, which needs `storage.use`; `""` clears it. Bulk ingestion into the dataset follows it too.
3. **The organization's default**: initially `platform`.

The chosen connection must be the organization's own (or `platform`) and `ACTIVE`. A database trigger enforces this for every binding:
- upload sessions and assets: a connection of the same tenant, never a deleted one;
- new uploads, dataset choices and defaults: an `ACTIVE` connection.

Upload and asset bindings can never change once written.

A binding is never empty: the columns are `NOT NULL` and records older than connections were bound to `platform` by the migration. Code that meets an empty binding treats it as lost, not as the platform's: resolving it fails, and nothing is read, written or deleted on the platform's bucket in its place.

The upload response names `storage_connection_id`. A single upload also returns `upload_headers` the client must send (Azure needs `x-ms-blob-type: BlockBlob`). Idempotent retries replay the bound session; asking for a different connection with the same key is a conflict.

## Upload, verification, promotion and download

- **Staging modes.** `staging_mode` decides where an upload lands before it is verified:
  - `CONNECTION` (default): under the connection's `staging_prefix/<tenant>/<upload>`; promotion is a copy inside the provider to `ready_prefix/<tenant>/<asset>`.
  - `PLATFORM`: in the platform's quarantine; only verified, validated and scanned bytes are streamed to the connection's ready prefix. Unverified content never reaches the customer's bucket. This mode is required for `SAS_TOKEN` connections.
- **Verification.** Verification, validation and malware scanning read the bytes through the staging connection. The content-security policy is unchanged, and nothing unverified becomes `AVAILABLE`.
- **Cross-connection promotion.** It streams from one provider into the other with `WriteObject`, holding one part (`STORAGE_TRANSFER_PART_BYTES`) in memory. A failed stream leaves no object, or one that the copy check refuses, so promotion simply runs again. The copy is verified (size, then SHA-256) before the asset becomes `AVAILABLE`; the source is removed only afterwards.
- **Downloads** use the asset's storage connection:
  - a signed URL from that provider, or a token link through the API when the connection cannot sign;
  - range requests where the provider supports them;
  - the CDN only for assets on the platform's bucket.

  An asset is never signed with the organization's current default.

## Lifecycle, cleanup and reconciliation

- **Deletion and retention** delete each target on the connection that holds it, and delete by version ID where one was recorded. Legal holds and the deletion state machine are unchanged.
- **Orphan cleanup** scans every connection that still holds data:
  - On a tenant connection, an object that **no record claims** is only reported, whatever `LIFECYCLE_ORPHAN_UNRECORDED` says: it may be the customer's own.
  - An object whose record proves it unowned (a deleted asset's copy, a rejected promotion copy) is deleted through that connection, after the usual observations and safety interval.
  - A record bound to another connection neither claims nor disowns an object, so the same key on two connections is never confused.
- **Missing-object checks** head each asset's object where its binding says.
- **Multipart reconciliation** inspects, completes and aborts each session through its own staging connection.
- **Connections and deletion.**
  - Deleting a connection is refused while anything depends on it: active uploads, assets that are not deleted (including soft-deleted ones awaiting purge), datasets that chose it, or being the default.
  - There is no reassignment workflow. An organization drains a connection by deleting or letting expire what is on it; the objects themselves stay in the customer's bucket.

## Security responsibilities on customer-owned storage

The platform controls its own keys and records. On customer-owned storage it cannot enforce:

- **Privacy of the bucket.** The platform never makes objects public, but cannot stop the customer doing so, and does not verify the bucket policy beyond what the connection test exercises.
- **Integrity at rest.** Someone with access to the bucket can overwrite or delete objects. Delivery pins reads to the stored validator or version (If-Match, version ID or generation), so an overwritten object answers "changed" instead of serving other bytes.
- **Content security in `CONNECTION` staging mode.** Unverified uploads sit in the customer's bucket until verification. A customer who needs the platform's quarantine guarantee should use `PLATFORM` staging.
- **Retention and legal holds.** These are enforced by the platform's records, not by provider object locks; the customer can delete objects out from under them.
- **Credentials and network.** Credential hygiene on the customer side, such as least privilege and expiring SAS tokens, and the reachability of their endpoint.

## Migration, backfill and rollback

**Forward migration.** `migrations/000015_storage_connections.sql`:

- creates `storage_connections` and the `platform` row;
- adds `organizations.default_storage_connection_id` (default `platform`) and `datasets.storage_connection_id` (nullable);
- adds `upload_sessions` / `assets.storage_connection_id` and `staging_connection_id`, `processing_jobs.storage_connection_id` and `catalog_objects.storage_connection_id`, all with foreign keys;
- adds `storage_connection_id` to `inventory_scans` and `orphan_candidates`, and rebuilds their unique indexes to include it;
- adds the `storage_binding_tenant_check` trigger.

**Backfill.** Deterministic and without a table rewrite. Every existing upload, asset, job, scan, candidate and organization takes the column default `platform`, which is where its bytes always were. No plaintext credential exists to migrate.

Apply with:

```powershell
.\scripts\migrate.ps1 -Database object_storage
```

Check the backfill:

```sql
SELECT storage_connection_id, count(*) FROM assets GROUP BY 1;
```

**Rollback.** There is no down migration. To roll back by hand, before any organization uses its own storage:

```sql
BEGIN;
DROP TRIGGER IF EXISTS tr_upload_sessions_storage_tenant ON upload_sessions;
DROP TRIGGER IF EXISTS tr_assets_storage_tenant ON assets;
DROP TRIGGER IF EXISTS tr_datasets_storage_tenant ON datasets;
DROP TRIGGER IF EXISTS tr_organizations_storage_tenant ON organizations;
DROP FUNCTION IF EXISTS storage_binding_tenant_check();
DROP INDEX IF EXISTS ux_inventory_scans_running;
CREATE UNIQUE INDEX ux_inventory_scans_running ON inventory_scans (bucket, prefix) WHERE status = 'RUNNING';
DROP INDEX IF EXISTS ux_orphan_candidates_open;
CREATE UNIQUE INDEX ux_orphan_candidates_open ON orphan_candidates (bucket, object_key) WHERE status IN ('MARKED', 'FAILED');
ALTER TABLE upload_sessions DROP COLUMN storage_connection_id, DROP COLUMN staging_connection_id;
ALTER TABLE assets DROP COLUMN storage_connection_id, DROP COLUMN staging_connection_id;
ALTER TABLE processing_jobs DROP COLUMN storage_connection_id;
ALTER TABLE catalog_objects DROP COLUMN storage_connection_id;
ALTER TABLE inventory_scans DROP COLUMN storage_connection_id;
ALTER TABLE orphan_candidates DROP COLUMN storage_connection_id;
ALTER TABLE datasets DROP COLUMN storage_connection_id;
ALTER TABLE organizations DROP COLUMN default_storage_connection_id;
DROP TABLE storage_connections;
DELETE FROM schema_migrations WHERE version = '000015_storage_connections.sql';
COMMIT;
```

Once any record is bound to a tenant connection, rolling back would orphan those bytes: older code reads every object from the platform's bucket. Disable BYOS instead (`BYOS_ENABLED=false`). Records bound to tenant connections then fail to resolve, and are never served from the wrong place, until BYOS is turned back on.

## Local setup and tests

```bash
# The default stack (unchanged): PostgreSQL, MinIO, ClamAV, ElasticMQ
docker compose up -d
.\scripts\migrate.ps1 -Database object_storage

# BYOS locally, against the compose MinIO as an S3-compatible provider
BYOS_ENABLED=true
STORAGE_CREDENTIAL_KEYS=1:$(openssl rand -base64 32)
STORAGE_ALLOW_PRIVATE_ENDPOINTS=true    # APP_ENV=development only

# Emulators for the GCS and Azure adapter tests (opt-in profile)
docker compose --profile emulators up -d fake-gcs azurite
```

Tests:

```bash
# Unit (no services): registry, sealing, connection service, routing of
# upload, processing, promotion, delivery, cleanup; endpoint policy
go test ./internal/... ./cmd/... ./tests/unit/...

# Adapters against MinIO and the emulators
TEST_S3_COMPATIBLE_ENDPOINT=http://127.0.0.1:9000 TEST_S3_COMPATIBLE_ACCESS_KEY=minioadmin \
TEST_S3_COMPATIBLE_SECRET_KEY=minioadmin go test ./internal/infrastructure/storage/registry/
TEST_GCS_ENDPOINT=http://127.0.0.1:4443 go test ./internal/infrastructure/storage/gcs/
TEST_AZURITE_ENDPOINT=http://127.0.0.1:10000 TEST_AZURITE_KEY=<Azurite's well-known devstoreaccount1 key> \
go test ./internal/infrastructure/storage/azure/

# PostgreSQL and the end-to-end BYOS flow over HTTP (with the TEST_S3_COMPATIBLE_* above)
TEST_DATABASE_URL=postgres://storage_app:...@127.0.0.1:5435/object_storage_test?sslmode=disable \
go test ./tests/integration/ -run 'StorageConnections|BringYourOwnStorage'
```

Without their variables the adapter and end-to-end tests skip and say so.

**Live cloud tests.** None exist yet. Live AWS, R2, GCS and Azure accounts were not used in this phase.

## Limitations

- **Not supported yet:**
  - GCS workload identity (only service account keys);
  - Azure Microsoft Entra ID (identity-based) auth (only account keys and SAS tokens);
  - the AWS default credential chain for a tenant connection (it would hand tenants the platform's identity; `ASSUME_ROLE` with an external ID is the way to use roles).
- **Versioning status.** Azure versioning is an account setting the data plane does not report. Its status is `UNKNOWN` until a write probe sees a version ID.
- **Provider-specific limits:**
  - GCS composite objects carry no MD5; R2 has no versioning.
  - GCS multipart cleanup relies on the parts being under the upload's own prefix; aborted uploads are cleaned by abort and reconciliation.
  - Azure uncommitted blocks of an abandoned upload expire after seven days if abort never ran.
- **No reassignment.** Objects are not moved between connections, and a connection with live assets cannot be deleted.
- **Platform-only scans.** The catalog reconciler's unreferenced-object report and the multipart prefix scans cover the platform's bucket only. Tenant buckets are swept by the cleanup worker, which only reports unrecorded objects there.
- **No management UI.** `client/` holds no frontend, so the screens for choosing, testing and rotating connections are a separate product task. This phase delivers the API, DTOs, documentation and end-to-end API tests.
