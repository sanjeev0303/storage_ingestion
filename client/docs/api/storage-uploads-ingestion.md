# Frontend contract: storage connections, uploads, multipart, downloads, bulk ingestion

Source: `C:\Users\sanjeev.kumar\Music\storage\server` (module `github.com/sanjeev0303/object-storage-platform`), read-only audit. Everything below is taken from code; docs were used only to confirm. All JSON keys are snake_case. All timestamps are RFC 3339 strings (Go `time.Time`; `*time.Time` fields are omitted when null, they are not sent as `null`). `omitempty` fields are ABSENT when empty/zero/false (so model them as optional).

## 0. Global rules

### 0.1 Auth and tenancy (every route except `GET /api/v1/downloads/{token}`)
- `Authorization: Bearer <access token>` required. Missing/bad -> 401 `{"error":"authentication required","code":"unauthenticated"}` + `WWW-Authenticate: Bearer realm="object-storage"`.
- `X-Organization-ID: <org id>` names the organization. Required when the caller belongs to several (or no) orgs; else 400 `{"error":"name the organization with the X-Organization-ID header","code":"organization_required"}`. Not a member -> 403 `code:"not_a_member"`; suspended org -> 403 `code:"organization_inactive"`.
- Permission denied -> 403 `{"error":"permission denied","code":"forbidden"}`.
- Cross-tenant ids always answer 404 (never 403).
- No CORS middleware exists anywhere in the server (see gaps). Requests from a browser origin different from the API need a same-origin proxy or server change.

### 0.2 Permission strings (model/identity.go) and built-in roles
`storage.read`, `storage.use`, `storage.manage`, `uploads.write`, `assets.read`, `assets.download`, `assets.delete`, `datasets.read`, `datasets.write`, `ingestion.read`, `ingestion.write`, `ingestion.manage`.
- owner: all. admin: all except `org.ownership`.
- developer: `storage.read`, `storage.use`, `uploads.write`, `assets.read|download|delete`, `datasets.read|write`, `ingestion.read|write|manage` (NOT `storage.manage`).
- auditor: `storage.read`, `assets.read`, `datasets.read`, `ingestion.read` (cannot download, upload, or manage storage).
- Custom roles can have any subset; do not assume "everyone reads" (docs say that, code checks `storage.read`).

### 0.3 Error body shapes (they differ per area; handle all three)
1. `Problem` (middleware, storage connections, quota, 403/404 security errors): `{"error": string, "code": string}` plus optional quota fields `limit, max, used, requested, remaining` (all numbers, `remaining` is a number).
2. Bare `{"error": string}` (uploads, multipart, ingestion non-security errors, delivery errors).
3. **Plain text, not JSON**: any body that fails JSON decoding (`decodeJSON`) answers 400 with `Content-Type: text/plain`, body `invalid JSON request\n` (unknown field, bad type, empty body where a body is required, trailing JSON, >1 MiB) or `request must contain one JSON value\n`. Frontend must tolerate a non-JSON 400.
- Request bodies: 1 MiB limit, **unknown fields rejected** (`DisallowUnknownFields`), exactly one JSON value. Send only documented fields.
- 429: `{"error":"too many requests; retry after Ns","code":"rate_limited"}` + `Retry-After` seconds (also tenant request quota). 503 `code:"quota_unavailable"` possible.
- Quota refusal (`code:"quota_exceeded"`): `limit` in `max_object_bytes` (HTTP 413), `active_uploads` | `active_multipart` | `pending_processing` (HTTP 429, `Retry-After: 5`), `storage_bytes` | `assets` (HTTP 403).
- Known `code` values used by the shared Problem mapper: `invalid_credentials, refresh_reused, unauthenticated, account_inactive, escalation, forbidden, organization_inactive, organization_required, rate_limited, invalid_token, invalid_request, last_owner, path_exists(409), path_busy(409), revision_conflict(412), not_found(404), connection_not_active(409), connection_in_use(409), conflict(409), endpoint_rejected(400), unsupported(422), credentials_missing(409), byos_disabled(501), query_too_broad(422), quota_exceeded, internal(500, fixed message "the request could not be completed")`.

### 0.4 Idempotency
- **There is no `Idempotency-Key` header on any route in this area.** Idempotency keys are body fields named `idempotency_key` (uploads, multipart start, ingestion job create). (`Idempotency-Key` header exists only for dataset-manifest routes, out of scope.)
- Content-Type: only `POST /api/v1/uploads` enforces it (`Content-Type` must start with `application/json`, else 415 `{"error":"Content-Type must be application/json"}`; note it is checked AFTER body decode). All other JSON routes do not check it; send `application/json` anyway.

### 0.5 Rate-limit buckets (defaults, per org unless noted; env-overridable)
storage GETs 600/min; `/test`, `/rotate-credentials`, `/enable` 10/min; other storage writes 60/min; `POST /uploads` and `/uploads/multipart` 300/min; all other `/uploads/*` POSTs 600/min (this includes part-URL requests); `GET /uploads/{id}` 600/min; download-url, revoke, content 600/min; `/downloads/{token}` 600/min per client IP; ingestion: job create 60/min, manifest routes 60/min, per-entry routes (claims, upload-url, parts/urls, parts/confirm, heartbeat, complete) 3000/min, ingestion GETs 600/min, finalize/retry/cancel 600/min.

---

## 1. Storage providers

### GET /api/v1/storage-providers
- Permission: `storage.read`. 200:
```ts
{ providers: Array<{
    provider: ProviderType; name: string;
    auth_methods: Array<{ method: AuthMethod; secret_fields: string[] }>;   // secret_fields entries are display strings, e.g. "session_token (optional)"
    required_fields: string[];   // display strings, e.g. "bucket (the container)", "account_id (the storage account)"
    notes: string[];
  }>;
  byos_enabled: boolean }  // false => customer connections cannot be created/tested/rotated/enabled (501 byos_disabled)
```
Static catalog (exact):
| provider | name | auth_methods (method: secret_fields) | required_fields |
|---|---|---|---|
| `PLATFORM_MANAGED` | Platform-managed storage | `PLATFORM`: [] | [] (not creatable) |
| `AWS_S3` | Amazon S3 | `ACCESS_KEY`: [access_key_id, secret_access_key, "session_token (optional)"]; `ASSUME_ROLE`: [] | bucket, region |
| `CLOUDFLARE_R2` | Cloudflare R2 | `ACCESS_KEY`: [access_key_id, secret_access_key] | bucket, account_id |
| `S3_COMPATIBLE` | S3-compatible service | `ACCESS_KEY`: [access_key_id, secret_access_key] | bucket, endpoint |
| `GOOGLE_CLOUD_STORAGE` | Google Cloud Storage | `SERVICE_ACCOUNT_KEY`: [service_account_json] | bucket |
| `AZURE_BLOB` | Azure Blob Storage | `ACCOUNT_KEY`: [account_key]; `SAS_TOKEN`: [sas_token] | "bucket (the container)", "account_id (the storage account)" |
- Creatable providers (`CustomerProviders`): AWS_S3, S3_COMPATIBLE, CLOUDFLARE_R2, GOOGLE_CLOUD_STORAGE, AZURE_BLOB. The catalog list also includes PLATFORM_MANAGED (filter it out of a "create" picker).
- Per-provider extras: AWS_S3 + ASSUME_ROLE needs `role_arn` matching `^arn:aws[a-z\-]*:iam::\d{12}:role/[A-Za-z0-9+=,.@_\-/]{1,512}$`; server generates `external_id` (`osp-<hex>`) returned in the connection, customer puts it in the role trust policy. `role_arn` is rejected for any other auth method. AZURE_BLOB + SAS_TOKEN requires `staging_mode:"PLATFORM"` (SAS cannot sign client URLs). R2 endpoint is derived (`https://<account_id>.r2.cloudflarestorage.com`, region "auto"); Azure endpoint derived; `endpoint` optional for AWS_S3/R2/GCS/Azure, must be under provider domain (amazonaws.com / r2.cloudflarestorage.com / storage.googleapis.com / blob.core.windows.net) unless dev policy.

### GET /api/v1/storage-providers/capabilities
- `storage.read`. 200 `{ providers: Array<{ provider: ProviderType; capabilities: Capabilities }> }` (note: `providers` is absent/`null` only if catalog empty; never in practice). Includes PLATFORM_MANAGED.
```ts
Capabilities = {
  presigned_upload: boolean; max_single_upload_bytes: number;
  multipart: boolean; multipart_kind?: "S3"|"GCS_COMPOSE"|"AZURE_BLOCKS"; max_parts?: number; min_part_bytes?: number;
  part_validator?: "CONTENT"|"BLOCK_ID";           // BLOCK_ID (Azure): no ETag is validated
  list_incomplete_uploads: boolean; server_side_copy: boolean; range_reads: boolean; conditional_reads: boolean;
  presigned_download: boolean; list_objects: boolean;
  provider_versioning: boolean; versioning_status?: string /* e.g. "Enabled", "UNKNOWN" */; version_id_kind?: "S3_VERSION_ID"|"GCS_GENERATION"|"AZURE_VERSION_ID";
  checksum_algorithms: string[]   // ["MD5"], ["MD5","CRC32C"], ["MD5","CRC64"]; never null
}
```
Declared values: S3 family = max_single 5 GiB, multipart S3, max_parts 10000, min_part_bytes 5 MiB (5242880), CONTENT. GCS = max_single 5 TiB, GCS_COMPOSE, 10000 parts, no min part, CONTENT. Azure = max_single 5000 MiB (5242880000), AZURE_BLOCKS, max_parts 50000 (but API caps part numbers at 10000), BLOCK_ID, no min part.

---

## 2. Storage connections

Enums:
- `ProviderType`: `PLATFORM_MANAGED | AWS_S3 | S3_COMPATIBLE | CLOUDFLARE_R2 | GOOGLE_CLOUD_STORAGE | AZURE_BLOB`
- `AuthMethod`: `PLATFORM | ACCESS_KEY | ASSUME_ROLE | SERVICE_ACCOUNT_KEY | ACCOUNT_KEY | SAS_TOKEN`
- `ConnectionStatus`: `PENDING | ACTIVE | DISABLED | DELETED` (DELETED never returned by list/get).
- `StagingMode`: `CONNECTION` (upload lands in the connection's own `staging_prefix`, promoted by in-provider copy) | `PLATFORM` (upload lands in platform quarantine; verified bytes are streamed to the connection).

State machine: create -> `PENDING`; passing `test` -> `ACTIVE`; `disable` (from ACTIVE/PENDING) -> `DISABLED`; `enable` (only from DISABLED; tests first) -> `ACTIVE` on pass, stays `DISABLED` on fail; `delete` (only PENDING/DISABLED, nothing depending) -> `DELETED` tombstone, credentials erased. PENDING never serves work. Only ACTIVE (or `platform`) accepts new uploads.

### ConnectionResponse (all connection routes)
```ts
{
  id: string;                 // "sc_<24 hex>", or "platform" for the shared platform connection
  name: string; provider: ProviderType; status: ConnectionStatus; auth_method: AuthMethod;
  bucket?: string; region?: string; endpoint?: string; account_id?: string;
  force_path_style: boolean;          // always present
  role_arn?: string; external_id?: string;   // external_id = trust-policy value, not a secret
  staging_prefix?: string; ready_prefix?: string; staging_mode?: StagingMode;
  is_default: boolean;
  has_credentials: boolean;           // credentials are NEVER returned, only this flag
  credential_rotation: number;        // credential version, 0 = none stored
  credential_rotated_at?: string;
  capabilities?: Capabilities;        // set by last passing test
  capabilities_checked_at?: string;
  last_tested_at?: string; last_test_ok_at?: string;
  last_error_category?: string;       // sanitized category, see TestCategory
  revision: number;                   // optimistic-concurrency counter
  created_by?: string; updated_by?: string;
  created_at: string; updated_at: string; disabled_at?: string; deleted_at?: string;
}
```
- The `platform` connection appears in list/get (id `platform`, name "Platform-managed storage", provider PLATFORM_MANAGED, status ACTIVE, auth PLATFORM, no bucket). It cannot be updated/tested/rotated/disabled/enabled/deleted (400 `invalid_request` "the platform connection cannot be changed"). It CAN be passed to `set-default`.
- Responses to single-connection GET/PATCH/POST-create/set-default/disable carry `ETag: "<revision>"` (quoted) and `Cache-Control: private, no-store`.
- Credentials: submitted once, in `credentials` on create or rotate. Server seals them (AES with tenant/connection/version binding); no response, log or audit ever returns them. Only `has_credentials`/`credential_rotation`/`credential_rotated_at` are exposed.

### StorageCredentials (request only)
```ts
{ access_key_id?: string; secret_access_key?: string; session_token?: string;
  service_account_json?: string /* the whole key file as a string, <=16 KiB */;
  account_key?: string; sas_token?: string }
```
Validation (`ValidateFor(auth_method)`): must contain exactly the method's fields. ACCESS_KEY: `access_key_id`+`secret_access_key` required, `session_token` optional; SERVICE_ACCOUNT_KEY: `service_account_json`; ACCOUNT_KEY: `account_key`; SAS_TOKEN: `sas_token`; PLATFORM/ASSUME_ROLE: none (stores no credentials). Any extra field -> 400. Each string <=4096 chars, no `\r \n \0`. Error: 400 `{"error":"invalid storage connection: ...","code":"invalid_request"}`.

### POST /api/v1/storage-connections  (permission `storage.manage`)
Body `CreateStorageConnectionRequest`:
```ts
{ name: string;                 // trimmed, 1-128 chars, no control chars; unique per org, case-insensitive, among non-deleted (409 code "conflict")
  provider: "AWS_S3"|"S3_COMPATIBLE"|"CLOUDFLARE_R2"|"GOOGLE_CLOUD_STORAGE"|"AZURE_BLOB";
  auth_method: AuthMethod;      // must be one allowed for the provider (see catalog)
  bucket: string;               // regex ^[a-z0-9][a-z0-9.\-_]{1,220}[a-z0-9]$ (3-222 chars), no ".."
  region?: string;              // ^[a-z0-9\-]{0,64}$ ; required for AWS_S3
  endpoint?: string;            // trailing "/" trimmed; absolute URL, no path/query/userinfo, <=512 chars; https only (http only for private/localhost in dev policy); required for S3_COMPATIBLE
  account_id?: string;          // ^[A-Za-z0-9\-]{0,64}$ ; required for CLOUDFLARE_R2 and AZURE_BLOB
  force_path_style?: boolean;
  role_arn?: string;            // ASSUME_ROLE only, required there
  staging_prefix?: string;      // default "quarantine"; ^[a-z0-9][a-z0-9\-_]{0,62}$ ; must differ from ready_prefix
  ready_prefix?: string;        // default "assets"; same pattern
  staging_mode?: "CONNECTION"|"PLATFORM";   // default "CONNECTION"; SAS_TOKEN requires "PLATFORM"
  credentials?: StorageCredentials }
```
- 201 `ConnectionResponse` with `status:"PENDING"`, plus `ETag`. **Create does not test the connection**; call `/test`.
- Errors: 400 `invalid_request` (validation, message in `error`), 400 `endpoint_rejected`, 409 `conflict` (name exists), 501 `byos_disabled` (BYOS off), 403 `forbidden`.

### GET /api/v1/storage-connections  (`storage.read`)
200 `{ connections: ConnectionResponse[]; default_connection_id: string }` (platform first, then org's own by created_at; no pagination; `Cache-Control: private, no-store`).

### GET /api/v1/storage-connections/{id}  (`storage.read`)
200 `ConnectionResponse` + `ETag`. 404 `not_found` (also for other org's / deleted).

### PATCH /api/v1/storage-connections/{id}  (`storage.manage`)
Body (all optional except revision): `{ revision?: number; name?: string; region?: string; endpoint?: string; force_path_style?: boolean; staging_mode?: "CONNECTION"|"PLATFORM" }`.
- Concurrency token REQUIRED: `If-Match: "<revision>"` header (the quoted ETag from GET; `W/` and quotes are stripped) OR body `revision`. If both given and different -> see surprise #4 (returns 500). Missing/<=0 -> 400 `invalid_request` "the connection's revision is required (If-Match)". Stale -> 412 `revision_conflict`.
- Cannot change provider, auth_method, bucket, prefixes, role_arn.
- If the connection is ACTIVE or DISABLED and region/endpoint/force_path_style changes, the server first tests the new settings with stored credentials; on failure nothing is stored and the response is **422 with a `TestResponse` body (no `code` field)**. PENDING connections are edited untested.
- 200 `ConnectionResponse` + new `ETag`. Status never changes here.

### POST /api/v1/storage-connections/{id}/test  (`storage.manage`)
Body optional (may be empty): `{ write_probe?: boolean }` (writes, reads back, deletes one tiny object under `<ready_prefix>/<tenant>/.connection-test/`; also detects versioning). 404 if not found, 409 `connection_not_active` if DISABLED ("enable it to test it"), 501 if BYOS off.
- **A failing test is HTTP 200** with `ok:false`. Always inspect `ok`. A pass activates a PENDING connection. Result is persisted (`last_tested_at`, `last_test_ok_at`, `last_error_category`, `capabilities`).
```ts
TestResponse = {
  ok: boolean;
  category?: TestCategory;        // present when ok=false
  message?: string;               // fixed sanitized text per category, never provider text
  checks: Array<{ name: "endpoint"|"credentials"|"bucket"|"list"|"existing_object"|"write_probe"; ok: boolean; category?: TestCategory }>;  // only checks that ran; stops at first failure; never null
  capabilities?: Capabilities;    // only on ok=true
  tested_at: string;
  connection?: ConnectionResponse // present on /test and /enable 200 responses (not on the 422 responses)
}
TestCategory = "ACCESS_DENIED"|"NOT_FOUND"|"THROTTLED"|"NETWORK"|"ENDPOINT_REJECTED"|"TLS"|"INVALID_CREDENTIALS"|"UNSUPPORTED"|"PROVIDER_ERROR"
```
Messages: ACCESS_DENIED "the credentials are not allowed to do this; check the permissions in docs/storage-connections.md"; NOT_FOUND "the bucket or container does not exist, or the credentials cannot see it"; THROTTLED "the provider is throttling requests; try again later"; NETWORK "the endpoint could not be reached in time"; ENDPOINT_REJECTED "the endpoint is not allowed by the platform's network policy"; TLS "the endpoint's TLS certificate is not trusted"; INVALID_CREDENTIALS "the credentials are malformed or could not be used"; UNSUPPORTED "the provider does not support an operation the platform needs"; PROVIDER_ERROR "the provider returned an unexpected error". Test timeout 20 s default.

### POST /api/v1/storage-connections/{id}/rotate-credentials  (`storage.manage`)
Body: `{ revision?: number; credentials: StorageCredentials }` (credentials required; If-Match or revision required as in PATCH). New credentials are tested first (no probe): pass -> stored, 200 `TestResponse` with `connection`; fail -> **422 `TestResponse`** and old creds stay. Connections with no secret fields (PLATFORM, ASSUME_ROLE) -> 400 "...connections have no stored credentials". Status unchanged (a PENDING connection still needs `/test`).

### POST /api/v1/storage-connections/{id}/set-default  (`storage.manage`)
No body. 200 `ConnectionResponse` (`is_default` is re-read after the change, so `true`). Requires ACTIVE (or `platform`); else 409 `connection_not_active` ("test it before making it the default").

### POST /api/v1/storage-connections/{id}/disable  (`storage.manage`)
No body. 200 `ConnectionResponse` with `status:"DISABLED"`. Refused with 409 `connection_in_use` ("it is the organization's default; set another default first") for the default; 409 `connection_not_active` if already DISABLED/DELETED. Existing data keeps being served/processed through it.

### POST /api/v1/storage-connections/{id}/enable  (`storage.manage`)
No body. Only from DISABLED (else 409 `connection_not_active` "it is X, not disabled"). Tests; pass -> ACTIVE. Always 200 `TestResponse` (+`connection`); on failure `ok:false` and it stays DISABLED.

### DELETE /api/v1/storage-connections/{id}  (`storage.manage`)
204 no body on success. **Why delete is refused** (409, `code:"connection_in_use"`):
1. Connection is ACTIVE -> must `disable` first; body is the generic Problem `{"error":"the storage connection still holds uploads or objects: disable it before deleting it","code":"connection_in_use"}` (message says disable).
2. Dependents exist: body is
```ts
{ error: string; code: "connection_in_use"; active_uploads: number; live_assets: number; datasets: number; is_default: boolean }
```
 - `active_uploads`: upload sessions in PENDING/UPLOADING/COMPLETING/ABORTING (or COMPLETED/VERIFIED with no asset yet) bound to it (as final or staging connection).
 - `live_assets`: non-DELETED assets bound to it (final or staging).
 - `datasets`: datasets whose upload target is this connection.
 - `is_default`: it is the org default.
 The generic (no counts) body is used if counts are all zero. Bucket contents are never touched on delete.

---

## 3. Uploads (single-request)

Upload session `status` strings (`UploadStatus`): `PENDING | UPLOADING | COMPLETING | COMPLETED | VERIFIED | ABORTING | ABORTED | FAILED | EXPIRED`.
Transitions: PENDING -> UPLOADING (first parts/confirm), COMPLETED (single complete), ABORTING, FAILED, EXPIRED. UPLOADING -> COMPLETING, COMPLETED, ABORTING, FAILED, EXPIRED. COMPLETING -> COMPLETED, FAILED. COMPLETED -> VERIFIED (checksum verified by processing worker), FAILED. ABORTING -> ABORTED, EXPIRED, FAILED. Terminal: ABORTED, FAILED, EXPIRED. VERIFIED is the last upload state; after it an *asset* goes `QUARANTINED -> PROCESSING -> AVAILABLE | REJECTED` (asset status enum: QUARANTINED, PROCESSING, AVAILABLE, REJECTED, SOFT_DELETED, DELETED). **Upload status never says AVAILABLE and the upload endpoints never return an asset id** (see gaps).
Session lifetime: `UPLOAD_SESSION_TTL_SECONDS` default 1800 s (30 min) from creation; presigned URL TTL `UPLOAD_URL_TTL_SECONDS` default 900 s (15 min), capped at session expiry. Max object size `UPLOAD_MAX_SIZE_BYTES` default 5 GiB (up to 5 TiB if configured) -> 413.

### POST /api/v1/uploads  (permission `uploads.write`; `storage.use` if `storage_connection_id` is not the org default)
Body:
```ts
{ file_name: string;            // required, non-blank
  content_type: string;         // required, non-blank (MUST be re-sent byte-for-byte on the PUT; it is signed)
  size_bytes: number;           // required, >0, <= server max, and <= connection's max_single_upload_bytes (else 413 "use a multipart upload on this storage connection")
  checksum_sha256?: string;     // optional at the API; if present 64 hex chars (lowercased). Docs/content-security say processing verification fails without a declared checksum (see gaps #9)
  idempotency_key: string;      // required, non-blank
  dataset?: string;             // ^[a-z0-9][a-z0-9._-]{0,63}$ (1-64)
  path?: string;                // logical catalog path, default = file_name; <=1024 bytes, segment <=255, depth <=64; no abs path, "..", backslash, < > : " | ? * , control chars; reserved top-level _system/_ingestion/.well-known
  metadata?: Record<string,string>;   // <=64 keys, <=8192 bytes total, value <=1024 bytes; keys ValidMetadataKey, not reserved names (id, tenant_id, path, status, size_bytes, ...), not "system." prefix
  create_only?: boolean;        // true => 409 if a live object already exists at path
  storage_connection_id?: string }  // "platform" or an ACTIVE own connection id
```
Response 201 (new) or **200 (idempotent replay)** + `Location: /api/v1/uploads/{upload_id}`:
```ts
{ upload_id: string; status: "PENDING"; upload_url: string; expires_at: string;
  upload_headers?: Record<string,string>;   // Azure: {"x-ms-blob-type":"BlockBlob"}; absent for S3/GCS
  storage_connection_id: string }
```
- Idempotency: same `idempotency_key` + same owner + same (file_name, content_type, size_bytes, checksum, dataset, path, create_only, metadata, connection) returns the original session (a fresh presigned URL, 200). Same key with different fields -> 409 `{"error":"idempotency key conflicts with another request"}`. Scope: per tenant + owner (user).
- Client then does `PUT upload_url` with body = raw bytes, header `Content-Type: <content_type exactly>` plus every `upload_headers` entry. Success is provider 200/201 (S3/GCS 200, Azure 201). No Authorization header on that request (credential is in the URL). The PUT does **not** enforce size; the server checks exact size on complete.
- Errors: 400 `{"error":"invalid upload request: <detail>"}`, 413 `upload exceeds the permitted size` / quota, 415 (content type / unsupported media), 409 `path_exists`/`path_busy` (Problem shape), 409 `connection_not_active`, 404 `not_found` (connection), 403 `forbidden` (missing `storage.use`), 422 `unsupported` (connection cannot take direct uploads), 429 quota (`active_uploads`).

### POST /api/v1/uploads/{id}/complete  (`uploads.write`; owner only)
- This one route completes BOTH kinds. **No body (or empty `{}`/`{"parts":[]}`) = single-request upload completion**; non-empty `parts` = multipart completion (section 4).
- Single: server HEADs the staged object. 200 `{ "upload_id": string, "status": "COMPLETED" }` (or `"VERIFIED"` on replay). Repeating is safe.
- 409 `{"error":"the object has not been uploaded"}` -> PUT not finished/visible yet, retry later. 422 `{"error":"uploaded object size does not match expected size"}` -> session FAILED and object deleted (no recovery; start a new upload with a new idempotency key). 404 `{"error":"upload not found"}` for unknown id, another user's upload, or if the upload id is a multipart session called with no parts (surprise).
- 400 `invalid upload request: the upload is <STATUS>` for other states.

### GET /api/v1/uploads/{id}  (`uploads.write` OR `assets.read`; tenant-wide, not owner-restricted)
200:
```ts
{ upload_id: string; status: UploadStatus; file_name: string; size_bytes: number; expires_at: string; multipart: boolean }
```
(no created_at/updated_at, no failure reason, no asset id, no part list). 404 `not_found`. There is **no list-uploads route**.

---

## 4. Multipart uploads (browser flow)

Provider rules (S3 family; GCS/Azure differ as noted): part numbers 1..10000; every part except the last must be >= `min_part_bytes` (5 MiB = 5242880 for S3 family and platform; 0 for GCS/Azure); each part 1 byte..5 GiB; sum of part sizes MUST equal `size_bytes` exactly (else 422 "recorded part sizes do not match expected object size"). Part size is the CLIENT's choice (server does not dictate): use `partSize = max(5 MiB, ceil(size_bytes/10000))` rounded up to a whole MiB. No server-side limit on parallel part PUTs; limits that apply are the per-org request rate (600/min on `/uploads/{id}/parts/*` + complete; 3000/min for ingestion entries) and `active_multipart` quota (429). Reasonable client concurrency 3-6 parts.

### Sequence
1. `POST /api/v1/uploads/multipart` (start)
2. `POST /api/v1/uploads/{id}/parts/urls` (presign; repeatable; 1..10000 part numbers per call)
3. browser `PUT <part url>` per part directly to the provider; capture the `ETag` response header
4. `POST /api/v1/uploads/{id}/parts/confirm` (optional-but-recommended checkpoint, verified against provider; moves PENDING -> UPLOADING)
5. `POST /api/v1/uploads/{id}/complete` with the part list
6. `POST /api/v1/uploads/{id}/abort` to cancel

### 4.1 POST /api/v1/uploads/multipart  (`uploads.write`; `storage.use` for non-default connection)
Body (differences from single: **`checksum_sha256` is REQUIRED, exactly 64 hex chars**; no `Content-Type` enforcement; here 400 validation errors are a generic message):
```ts
{ file_name: string; content_type: string; size_bytes: number /* >0, <= max */; checksum_sha256: string /* 64 hex */;
  idempotency_key: string; dataset?: string; path?: string; metadata?: Record<string,string>; create_only?: boolean; storage_connection_id?: string }
```
Response **201 always** (even replay; check `reused`):
```ts
{ upload_id: string; status: UploadStatus /* "PENDING" */; expires_at: string; reused: boolean; storage_connection_id: string }
```
- Replay with same key+same payload -> same session, `reused:true`. Same key different payload -> 409 `{"error":"idempotency key was already used for a different request"}`.
- 409 `{"error":"multipart upload initialization is pending reconciliation"}` when a previous start's provider call outcome is unknown (retry later). 409 "multipart upload is not active" if the replayed session was closed. 400 `{"error":"invalid upload request"}` (bare message, no detail) for any missing field/bad checksum; 400 with detail only for path/metadata problems. 413 too large. 422 `unsupported` if connection has no multipart.

### 4.2 POST /api/v1/uploads/{id}/parts/urls
Body `{ part_numbers: number[] }` (int32, each 1..10000, no duplicates, 1..10000 entries). Session must be PENDING/UPLOADING, not expired, provider session started.
200:
```ts
{ upload_id: string; parts: Array<{ part_number: number; url: string; expires_at: string }> }   // sorted ascending by part_number
```
- `expires_at` = min(now + `UPLOAD_URL_TTL_SECONDS` default 15 min, session expiry). Re-request URLs any time for missing/expired parts.
- Errors: 400 `{"error":"invalid multipart part"}`, 404 `{"error":"upload not found"}` (also non-owner), 409 `{"error":"multipart upload is not active"}` (expired/aborted/completed) or `...has not started`.

### 4.3 Part PUT (browser -> provider, NOT the API)
- `PUT <url>` with the raw bytes of the part (a `Blob.slice`). Do NOT send `Authorization`. S3/R2/S3-compatible: the presigned part URL does not sign Content-Type, so none is required. GCS: part is a separate object, MD5 ETag. Azure: URL is a SAS "Put Block" URL (`comp=block&blockid=...`); Azure Put Block returns no usable ETag.
- Read the `ETag` response header (S3 returns it quoted, e.g. `"9b2cf535..."`; send it back as-is, quotes included; server compares ignoring quotes). Needs bucket CORS `ExposeHeaders: ETag` (see section 7).

### 4.4 POST /api/v1/uploads/{id}/parts/confirm
Body:
```ts
{ parts: Array<{ part_number: number; etag: string; size_bytes: number; checksum_sha256?: string }> }   // 1..10000 items
```
- Server cross-checks every part against the provider's own part listing (part must exist, size must match, ETag must match unless provider is Azure BLOCK_ID). All-or-nothing: a bad batch records nothing. Idempotent. `etag` required non-blank for S3/GCS; `size_bytes` must be >0 and exactly the bytes PUT.
- 200 `{"status":"parts_recorded"}`. First confirm moves PENDING -> UPLOADING.
- 400 `{"error":"invalid multipart part: part N is not stored at the provider"}` / `...does not match the provider's record` / `duplicate part number N`. 409 not active.

### 4.5 POST /api/v1/uploads/{id}/complete (multipart)
Body: `{ parts: Array<{ part_number: number; etag: string }> }` — non-empty, strictly ascending by `part_number`, must list EVERY uploaded part (omitting one -> 400), ETag must match for CONTENT-validator providers (S3/GCS; for Azure ETag unchecked).
- 200 `{ upload_id: string; status: "COMPLETED"|"VERIFIED"; expires_at: string }` (idempotent on retry).
- **202** `{"error":"multipart operation accepted; outcome pending reconciliation"}` when provider result is unknown or another completion is in flight (session `COMPLETING`). Not an error: poll `GET /api/v1/uploads/{id}` until `COMPLETED`/`VERIFIED`/`FAILED`.
- 400 `{"error":"invalid multipart completion request: <specific>"}` (no parts / out of order / part N was not uploaded / ETag of part N does not match / part N is smaller than 5 MiB and is not the last part / uploaded part N missing from list). 422 `...recorded part sizes do not match expected object size` or `uploaded object size does not match expected size`. 409 `multipart upload is not active` / `has not started`.

### 4.6 POST /api/v1/uploads/{id}/abort
No body. 200 `{ upload_id: string; status: "ABORTED"|"ABORTING"|...; expires_at: string }`. Idempotent on ABORTED/FAILED/EXPIRED. 409 not active if COMPLETING/COMPLETED/VERIFIED. If the provider call fails: 202 pending (session stays `ABORTING`, background reconciliation finishes). Releases the quota reservation.

### 4.7 Resume flow (what actually exists)
- There is **no resume endpoint and no "list uploaded parts" endpoint** (`service/upload/resume.go` is empty; `ResumeUpload` DTO empty). To resume after a page reload the client must itself persist `upload_id`, `idempotency_key`, `size_bytes`, part size, and every part's `{part_number, etag, size_bytes}` (localStorage/IndexedDB), then: re-call `POST /uploads/multipart` with the same body (returns `reused:true`) or just use stored `upload_id`; `GET /uploads/{id}` to check status/expiry (`multipart:true`); request fresh URLs for parts not yet done; PUT; confirm; complete. Server-side recovery only helps for lost *confirmations* (complete re-reads the provider listing), but complete still demands the ETag for each part from the client, so lost ETags cannot be recovered via the API.
- Session expires 30 min after start (default); an expired session cannot be resumed (409 not active); start a new upload.

---

## 5. Downloads

Asset status enum for `AssetResponse.status`: `QUARANTINED | PROCESSING | AVAILABLE | REJECTED | SOFT_DELETED | DELETED`. Only AVAILABLE with matching promoted key is deliverable. Missing/other-tenant/DELETED -> 404 `{"error":"asset not found"}`; own asset not yet/never deliverable -> 409 `{"error":"asset is not available for delivery"}`.

### GET /api/v1/assets/{id}  (`assets.read`)
200 (`Cache-Control: private, no-store`):
```ts
{ id: string; file_name: string; content_type: string; size_bytes: number; checksum_sha256?: string; dataset?: string;
  status: AssetStatus; rejection_reason?: string; deliverable: boolean;
  etag?: string /* sha256-based strong validator, quoted, present when deliverable */;
  published_at?: string; created_at: string; updated_at: string }
```
Also answers HEAD.

### POST /api/v1/assets/{id}/download-url  (`assets.download`)
Body optional (empty body OK; if sent, unknown fields rejected): `{ expires_in_seconds?: number; filename?: string; mode?: "storage"|"token" }`.
- `expires_in_seconds`: must be >=0 (negative -> 400 `{"error":"expires_in_seconds must not be negative"}`); 0/omitted -> server default 5 min (`DELIVERY_URL_TTL`); capped at `DELIVERY_URL_MAX_TTL` (default 15 min; hard cap 1 h). The cap is silent (you get the capped `expires_at`).
- `filename`: sanitized (paths/control/bidi stripped, <=150 runes), used for `Content-Disposition`.
- `mode`: `"storage"` presigned provider URL (not revocable); `"token"` revocable API link; omitted = server default (`DELIVERY_URL_MODE`, default `storage`). Any other value -> 400 `{"error":"invalid delivery request: mode must be storage or token"}`. Token mode off -> 400 "token links are not enabled". If `storage` is requested but the asset's connection cannot sign (Azure SAS) or asset is packed, the server silently switches to `token` (check `delivery`).
200 (`Cache-Control: no-store`):
```ts
{ url: string; expires_at: string; delivery: "storage"|"token"|"cdn"; revocable: boolean }
```
- `delivery:"token"`: `url` is **path-only** `"/api/v1/downloads/<token>"` -> prefix with the API origin. `delivery:"storage"`: absolute provider presigned GET URL (pinned to bucket/key, `Content-Disposition: attachment`, safe content type, `versionId` where versioned). `"cdn"`: signed CDN URL (platform bucket only).
- Errors: 404, 409 not deliverable, 400 invalid, 503 `{"error":"delivery is temporarily unavailable"}`, 403 forbidden.

### DELETE /api/v1/assets/{id}/download-links  (permission **`assets.delete`**, not download)
200 `{ revoked: number }`. Revokes all token links of the asset (presigned storage/CDN URLs cannot be revoked). 400 if token links disabled. (There is no GET/POST on `/download-links`.)

### GET|HEAD /api/v1/assets/{id}/content  (`assets.download`; only registered when `DELIVERY_PROXY_ENABLED` true, default true)
Streams bytes through the API; Bearer required (so not usable as a plain `<a href>`; use `fetch` -> blob, or request a token link). Query: `?filename=` optional. Request headers honored: `Range` (single range `bytes=a-b`, `a-`, `-n`; multi-range/malformed ignored -> 200 full), `If-Range`, `If-Match`, `If-None-Match`, `If-Modified-Since`, `If-Unmodified-Since`.
Responses: 200 whole, 206 range (`Content-Range: bytes first-last/total`), 304 (`ETag`, `Last-Modified`, no body), 412 `{"error":"the asset does not match If-Match"}`, 416 `{"error":"requested range not satisfiable"}` + `Content-Range: bytes */<size>`, 409 not available, 404, 503 (`Retry-After: 1` when `too many downloads in progress`; `Retry-After: 2` when storage throttled).
Headers on success: `ETag: "<sha256>"`, `Last-Modified`, `Content-Type` (html/xhtml/svg/xml/js forced to `application/octet-stream`), `Content-Disposition: attachment; filename*=...` (always attachment, no inline), `X-Content-Type-Options: nosniff`, `Accept-Ranges: bytes`, `Content-Length`, `Cache-Control: private, no-store`, `X-Request-ID`. A failure after headers were sent just truncates the stream (compare bytes received with `Content-Length`).
Resume: keep `ETag`; `Range: bytes=<n>-` + `If-Range: <etag>`; 206 append, 200 restart, 416 `bytes */<size>` with size == have => done.

### GET|HEAD /api/v1/downloads/{token}  (NO Authorization; the token is the credential; rate limited per IP)
Same behavior/headers/ranges as `/content`. Authorization is re-evaluated as the token's issuer on every use. Unknown/expired/revoked token -> 403 `{"error":"download link is invalid or has expired"}` (one answer for all). Only registered when proxy enabled.

---

## 6. Bulk ingestion (datasets + jobs)

All routes need `X-Organization-ID` (if multi-org) and Bearer. Dataset create/get are in this area's handler but are replaced by the datasets handler when it is mounted (same paths; richer response — out of scope here). Job `dataset_id` is a dataset id.

### Enums
- `JobState`: `PENDING_VALIDATION | VALIDATING | READY | RUNNING | PARTIALLY_COMPLETED | COMPLETED | FAILED | CANCELLING | CANCELLED`. Flow: PENDING_VALIDATION --(manifest submitted)--> VALIDATING --(finalize)--> READY --(first claim)--> RUNNING --(last entry ends)--> COMPLETED (all valid published, no invalid line) | PARTIALLY_COMPLETED (some published) | FAILED (none published). Rejected manifest -> CANCELLING w/ `error` -> FAILED. `cancel` from PENDING_VALIDATION/VALIDATING/READY/RUNNING -> CANCELLING -> CANCELLED. A manual retry on a terminal non-CANCELLED job makes it RUNNING again. Finalize with 0 valid entries -> FAILED.
- `EntryState`: `INVALID | PENDING | SCHEDULED | UPLOADING | UPLOADED | PROCESSING | PUBLISHED | FAILED_RETRYABLE | FAILED | CANCELLED`. PENDING -claim-> SCHEDULED -heartbeat-> UPLOADING -complete-> UPLOADED -> PROCESSING -> PUBLISHED (asset AVAILABLE). Slot-holding states: SCHEDULED, UPLOADING. Failures -> FAILED_RETRYABLE (auto re-queued to PENDING after backoff) or FAILED (final unless manual retry). Job cancel -> open entries CANCELLED (already-stored uploads still finish).
- `strategy`: `SINGLE` (size <= 16 MiB, `INGESTION_SMALL_FILE_THRESHOLD`) | `MULTIPART` (larger).
- `completion_policy` (dataset): `ALL | REQUIRED | PARTIAL`. Dataset `status`: `OPEN | INGESTING | READY | PARTIALLY_READY | INCOMPLETE`.
- Entry `error_code` values: validation `invalid_json, line_too_long, invalid_path, duplicate_path, invalid_size, too_large, invalid_checksum, checksum_required, invalid_content_type, invalid_metadata`; runtime `upload_abandoned, object_missing, size_mismatch, upload_failed, storage_unavailable, content_rejected, processing_exhausted, attempts_exhausted, cancelled, quota_exceeded`. Manually retryable codes: `upload_abandoned, object_missing, size_mismatch, upload_failed, storage_unavailable, processing_exhausted, attempts_exhausted, quota_exceeded`. Not retryable: validation codes, `content_rejected`, published entries, entries whose path was taken by another entry.
- `limited_by` (claim): `global_slots | tenant_slots | job_slots | multipart_sessions | storage_quota | processing_backlog | no_pending_entries | job_not_running`.

### Response types
```ts
IngestionJob = {
  id: string; dataset_id: string; state: JobState; max_concurrency: number;
  manifest_source?: "INLINE"|"STAGED"; manifest_bytes: number; manifest_sha256?: string; manifest_complete: boolean; parsed_lines: number;
  progress: { total, valid, invalid, pending, scheduled, uploading, uploaded, processing, published, failed_retryable, failed, cancelled, required_failed, expected_bytes, stored_bytes: number; percent_published: number /* float 0-100 of valid */ };
  error?: string; created_at: string; updated_at: string; finalized_at?: string; completed_at?: string; cancelled_at?: string }
IngestionEntry = {
  position: number /* 1-based manifest line number, entry identity */; path?: string; size_bytes: number; checksum_sha256?: string; content_type?: string;
  metadata?: Record<string,string> /* raw JSON object */; required: boolean; strategy?: "SINGLE"|"MULTIPART"; state: EntryState;
  upload_id?: string; asset_id?: string; attempts: number; error_code?: string; error_detail?: string;
  lease_expires_at?: string; next_attempt_at?: string; updated_at: string }
```
(INVALID entries have no `path`, `strategy`, often `size_bytes:0`.)

### 6.1 POST /api/v1/ingestion/jobs  (`ingestion.write`)
Body `{ dataset_id: string; idempotency_key: string /* trimmed, 1-128 chars */; max_concurrency?: number /* 0..32 (INGESTION_JOB_UPLOAD_SLOTS); 0 = default */ }`. 201 new / **200 same key** -> `IngestionJob` (state `PENDING_VALIDATION`). 404 dataset not found; 409 key reused for a different job; 403 quota (active jobs); 400 invalid_input (`{"error":"invalid ingestion request: ..."}`). Scope of key: per tenant.

### 6.2 GET /api/v1/ingestion/jobs?dataset_id=&after=&limit=  (`ingestion.read`)
Newest first. 200 `{ jobs: IngestionJob[]; next_after?: string }`. `limit` default 100, **max 1000; values <=0 or >1000 silently become 100**. `next_after` (opaque `"<unixnano>:<jobId>"`) present only when the page is full; pass back as `after`. No state filter.

### 6.3 GET /api/v1/ingestion/jobs/{id}  (`ingestion.read`)  -> `IngestionJob`. 404 `{"error":"ingestion job not found"}`.

### 6.4 Manifest
Format: **JSON Lines** (one JSON object per line, UTF-8, `Content-Type: application/x-ndjson` recommended). **No CSV.** Line N = entry `position` N; blank lines skipped but numbered. Unknown fields ignored. Line schema:
```ts
{ path: string;           // required; logical path (same path rules as upload path; unique case-insensitively among live entries of the dataset)
  size: number;           // required integer, 1..INGESTION_MAX_OBJECT_BYTES (default 50 GiB)  (key is "size", NOT size_bytes)
  content_type: string;   // required, valid media type, <=255 bytes
  checksum: string;       // required: "sha256:<64 hex>" (key is "checksum"); missing -> entry INVALID checksum_required
  required?: boolean;     // default true
  metadata?: Record<string,string> }  // flat object of strings; <=32 keys (key 1-64 of [A-Za-z0-9._-]); <=2048 bytes (default)
```
Limits (defaults): line <= 16384 bytes; <= 5,000,000 lines per job; path <= 1024 bytes, segment <= 255, depth <= 64; inline manifest <= 16 MiB; staged <= 2 GiB; org quotas can lower these. Invalid lines do NOT fail the manifest; each becomes an `INVALID` entry with `error_code`/`error_detail`. Whole-manifest failures: too many lines / too large (413), checksum mismatch (422), tenant pending-entry quota (batch rolled back; resend).

**Inline: PUT /api/v1/ingestion/jobs/{id}/manifest**  (`ingestion.write`). Body = the raw JSONL (not JSON-wrapped; no Content-Type check). Optional header `X-Manifest-SHA256: <64 hex>` (declared checksum; bad format -> 400). Allowed only from PENDING_VALIDATION/VALIDATING; job must not be STAGED (409). 200:
```ts
{ lines: number; accepted: number; invalid: number; duplicates: number; replayed: number; complete: boolean; sha256?: string; invalid_samples?: IngestionEntry[] /* first <=20 invalid lines */ }
```
Resend of same bytes resumes/returns the same report (`replayed`); different bytes -> 409 `{"error":"a different manifest was already submitted for this job"}`. Too large/too many lines -> 413 `{"error": string, "report": <same report shape>}` (job failed). Checksum mismatch -> 422.

**Staged (large): POST .../manifest/staged-url** body `{ size_bytes: number }` (1..quota max, default 2 GiB; else 413) -> 200 `{ upload_url: string; expires_at: string }` (presigned PUT to `manifests/<tenant>/<job>.jsonl`; client must `PUT` with `Content-Type: application/x-ndjson`). Only in `PENDING_VALIDATION` (else 409). Then **POST .../manifest/staged** body optional `{ sha256?: string }` -> **202** `IngestionJob` (state VALIDATING; the *worker* reads the manifest asynchronously; poll `GET job` until `manifest_complete: true` or state FAILED/`error`). 409 `{"error":"the manifest object has not been uploaded"}` if the PUT hasn't landed. 413 too large; 400 if empty.

### 6.5 POST /api/v1/ingestion/jobs/{id}/finalize  (`ingestion.write`)
No body. 200 `IngestionJob` (READY, or FAILED if zero valid entries). 409 `{"error":"the manifest has not been fully read yet"}` if `manifest_complete` is false; 403 if expected bytes can't fit tenant/dataset quota. Entries are claimable only after this.

### 6.6 Who drives claim/heartbeat? (answer: the client, i.e. the browser)
Scheduling is **pull-based and client-driven**: the server never receives file bytes and has no push channel. The uploader (browser or CLI such as `ingestion-loadgen`) loops: `claims` -> upload each granted entry -> `complete`. Workers (upload-worker/processing-worker) only run sweeps (lease expiry, retries after backoff, processing outcome sync, staged manifest read, cancel cleanup), not uploads. A browser must therefore implement the loop and respect `limited_by`/`retry_after_seconds`.

**POST /api/v1/ingestion/jobs/{id}/claims** (`ingestion.write`). Body optional `{ max?: number }` (<=0 or >100 -> 100, `INGESTION_MAX_CLAIM`). 200:
```ts
{ grants: Array<{ entry: IngestionEntry /* state SCHEDULED, upload_id set */; upload_url?: string /* SINGLE only */; url_expires_at?: string }>;
  limited_by?: LimitedBy; retry_after_seconds?: number /* >0 only when nothing granted for a limit, = 5 */; failed?: number /* granted but not preparable; will retry later */ }
```
Also sets `Retry-After` header when `retry_after_seconds > 0`. Empty `grants` with `limited_by:"no_pending_entries"` and no retry = nothing left to claim; `job_not_running` = job not READY/RUNNING. Slot lease `INGESTION_SLOT_LEASE` = 15 min; fair-share caps (defaults: global 256, tenant 64, job 32, tenant multipart sessions 16, processing backlog 2000). Granting reserves `size` bytes of quota.
- SINGLE entry: `PUT upload_url` with `Content-Type: <entry.content_type>` exactly (signed), raw bytes; then `POST .../complete`. (Azure connections: also `x-ms-blob-type: BlockBlob` — NOTE ingestion grants do NOT expose `upload_headers`, see gaps.)
- MULTIPART entry: no `upload_url`; use `entry.upload_id` implicitly via the entry endpoints below.
- Entry ownership: any member with `ingestion.write` can drive any entry (uploads are owned internally by the job creator).

**POST .../entries/{position}/upload-url** (`ingestion.write`) no body -> 200 `{ entry, upload_url?, url_expires_at? }` fresh presigned PUT for a SINGLE entry (URL TTL `INGESTION_UPLOAD_URL_TTL` 15 min); also creates the session if the claim response was lost. Entry must be SCHEDULED/UPLOADING (409 `ingestion entry is not in a state that allows this`); job not cancelling (409).
**POST .../entries/{position}/parts/urls** body `{ part_numbers: number[] }` (**1..100 per call**, `INGESTION_MAX_PART_URLS`) -> 200 `{ parts: [{part_number,url,expires_at}] }` (note: no `upload_id` field in this response). 400 `ErrStrategyMismatch` "this operation does not apply to the entry's upload strategy" if SINGLE.
**POST .../entries/{position}/parts/confirm** body `{ parts: [{part_number,etag,size_bytes,checksum_sha256?}] }` (1..100) -> **204 no body**.
**POST .../entries/{position}/heartbeat** no body -> 200 `IngestionEntry` (state UPLOADING, lease renewed 15 min). Call periodically (< every 15 min) during long uploads.
**POST .../entries/{position}/complete** body optional `{ parts?: [{part_number,etag}] }` (parts required for MULTIPART, same ascending/all-parts rules and 5 MiB min as section 4) -> 200 `IngestionEntry` (state UPLOADED/PROCESSING/PUBLISHED; repeat is a harmless no-op) or **202** `IngestionEntry` while completion outcome is still being confirmed (re-poll `GET entry`). Size mismatch -> 200 entry with `state:"FAILED"`, `error_code:"size_mismatch"` (retryable manually). `position` must be a positive integer else 400 `{"error":"position must be a positive integer"}`.
Multipart sizing for ingestion is the same as section 4 (client picks part size; 5 MiB min non-final; sum == entry.size_bytes; <=10000 parts).

### 6.7 Reading entries
- **GET .../entries?state=A,B&after=&limit=** (`ingestion.read`): `state` comma-separated, case-insensitive (uppercased), no validation of names. Ordered by position ascending. 200 `{ entries: IngestionEntry[]; next_after?: number }`; `after` is the last seen position (int); `next_after` only on a full page; limit default 100 (max 1000; out-of-range -> 100).
- **GET .../failures?after=&limit=**: same shape, fixed filter `INVALID, FAILED_RETRYABLE, FAILED, CANCELLED` (ignores `state`). This is the only "error report"; **there is no downloadable error-report endpoint (no CSV/JSONL export)**. Page client-side and export in the browser.
- **GET .../entries/{position}** -> `IngestionEntry`. 404 `{"error":"ingestion entry not found"}`.

### 6.8 Retry / cancel
- **POST .../retry** (`ingestion.manage`): body REQUIRED JSON (empty body -> plain-text 400): `{ positions?: number[] /* 1..1000 */; all?: boolean }`; neither -> 400. 200 `{ retried: number; requeued: number }` (`retried` = back to PENDING for a new upload; `requeued` = processing-held entries re-queued without re-upload). Max 3 manual retries per entry (`INGESTION_MAX_MANUAL_RETRIES`); a terminal job becomes RUNNING again (counts against active-job quota -> 403). At most ~100 pages of `INGESTION_WORKER_BATCH`(500) entries per call; call again if more remain.
- **POST .../cancel** (`ingestion.manage`): no body -> **202** `IngestionJob` (state CANCELLING). Background; poll until CANCELLED. Already-stored uploads still finish and publish. Deleting published assets is separate (`DELETE /api/v1/assets/{id}`).
- Ingestion error mapping: 400 invalid input / strategy mismatch; 404 job/dataset/entry not found; 403 `quota` (bare) or Problem `forbidden`/`quota_exceeded`; 409 state conflict (`ingestion job is not in a state that allows this`, entry state, manifest conflict, manifest incomplete, not staged, upload not attached `the entry's upload has not been created yet`); 413; 422 manifest checksum; 500 `{"error":"ingestion request failed"}`.

### 6.9 Dataset create/get (as served by this handler)
`POST /api/v1/datasets` (`datasets.write`) `{ name: string /* ^[a-z0-9][a-z0-9._-]{0,63}$ */; completion_policy?: "ALL"|"REQUIRED"|"PARTIAL" (default ALL, case-insensitive); max_bytes?: number >=0 }` -> 201 created / 200 existing (409 if same name different settings). `GET /api/v1/datasets/{id}` (`datasets.read`). Response includes `id, name, completion_policy, status, max_bytes, reserved_bytes, stored_bytes, created_at, updated_at, ready_at?` and managed-dataset fields (`description, tags, metadata, lifecycle_status, revision, member_count, current_version_id?, profile_id?, ..., created_by, storage_connection_id?`). When the dataset module is mounted its handler serves these two paths instead.

---

## 7. Browser handling of presigned URLs (CORS and headers)

The API never proxies upload bytes; the browser talks to the storage provider directly for: single upload PUT, multipart part PUTs, staged manifest PUT, and `delivery:"storage"` downloads. The provider bucket (platform or customer) must therefore allow the app origin:
- **CORS on the bucket**: `AllowedOrigins` = app origin; `AllowedMethods` = `PUT` (uploads), `GET`/`HEAD` (downloads); `AllowedHeaders` must include `Content-Type` and, for Azure, `x-ms-blob-type` (and `x-ms-*` / `Range` / `If-Range` for resumable downloads); **`ExposeHeaders` must include `ETag`** or the browser cannot read the part ETag needed for `parts/confirm` and `complete`. For Azure Put Block there is no ETag to expose (not required, validator is BLOCK_ID); GCS needs the same `ETag` exposure (`Access-Control-Expose-Headers`). The server does not configure or document any bucket CORS rule (grep of repo: no CORS anywhere).
- Do not send `Authorization` or cookies to presigned URLs (S3 rejects extra auth). Use `fetch(url,{method:"PUT", body: blob, headers})`; if you need upload progress use `XMLHttpRequest`.
- Single PUT: `Content-Type` must equal the `content_type` sent at create (it is part of the signature) plus all `upload_headers`. Part PUT: no content-type requirement.
- Expiry: single-upload URL 15 min (<= session expiry 30 min); part URLs 15 min each (<= session expiry); download URLs 5 min default / 15 min max. On HTTP 403 `RequestTimeTooSkewed`/`Request has expired` from the provider, request a new URL (`/parts/urls`, `/entries/{n}/upload-url`, new `download-url`) and retry the same part/offset.
- Token download URLs and `/content` go through the API (so API CORS and, for `/content`, Bearer apply). Token links need no Authorization and can be opened in a new tab / `<a href>`.
- Presigned download URLs are bearer secrets: do not log or persist; they cannot be revoked.

---

## 8. Gaps / surprises

1. **No CORS support in the API at all** (no `Access-Control-*` anywhere in Go, docs or config). A browser app on a different origin cannot call it (including preflight for `Authorization`/`X-Organization-ID`) without a same-origin reverse proxy/BFF or server changes. Likewise no bucket-CORS guidance exists; ETag exposure on the bucket is mandatory for multipart.
2. Error bodies are inconsistent: three shapes (`{error,code}`, `{error}`, plain-text `invalid JSON request`). Upload/multipart/ingestion errors mostly have **no `code`**; branch on HTTP status + message or `code` where present. Single-upload and multipart validation messages differ (single gives detail, multipart start gives only "invalid upload request").
3. `Idempotency-Key` HTTP header does **not** exist for uploads/ingestion; keys are body fields (`idempotency_key`). Multipart start returns 201 even on replay (use `reused`), single upload returns 200 on replay.
4. `If-Match` on PATCH/rotate: a malformed `If-Match`, or one disagreeing with body `revision`, produces `catalog.ErrInvalidRequest`, which the storage-connection error mapper does not recognize -> **HTTP 500 `code:"internal"`** instead of 400. Send a correct quoted revision (or only body `revision`).
5. Connection test/enable failures are HTTP **200 with `ok:false`**; update/rotate test failures are **422 with a TestResponse body without `code`**. Create does not test; a PENDING connection takes no uploads and cannot be set default.
6. Upload status API is thin: `GET /uploads/{id}` returns no asset id, no failure reason, no created/updated times, no part list. After `VERIFIED`, there is no upload-to-asset link in these endpoints (the asset id must come from elsewhere, e.g. the catalog by `path`/`dataset`, or ingestion `entry.asset_id`). No list-uploads endpoint.
7. No resume endpoint and no list-parts endpoint; `resume.go` and several DTO files (`complete_upload.go`, `resume_upload.go`, `create_multipart_upload.go`) are empty. A client must persist part ETags itself; `complete` needs them for S3/GCS.
8. `POST /uploads/{id}/complete` with no body on a **multipart** upload returns 404 "upload not found" (it routes to the single-upload completer which treats multipart sessions as not found). Multipart complete requires a non-empty `parts`.
9. Multipart start requires `checksum_sha256` (64 hex) but single `POST /uploads` marks it optional. Processing verification fails without a declared SHA-256 (`checksum_missing`), so treat it as effectively required for single uploads too (compute SHA-256 in the browser, e.g. streaming WebCrypto/hash-wasm).
10. `/uploads/{id}/parts/urls` accepts up to 10000 numbers per call; ingestion per-entry `parts/urls` and `parts/confirm` accept at most 100 per call (and omit `upload_id` in the response).
11. Ingestion `claims`/`upload-url` grants do **not** include `upload_headers`; for an Azure connection the single-PUT needs `x-ms-blob-type: BlockBlob` and the ingestion API never tells the client (the upload service computes it but the grant drops it). Treat Azure + SINGLE ingestion entries as needing that header hard-coded.
12. Manifest key names differ from API names: `size` (not size_bytes), `checksum` as `"sha256:<hex>"` (not checksum_sha256). Manifest `checksum` is mandatory per line (missing => INVALID), contrary to the "optional" Go field.
13. `PageSize`: `limit` > 1000 does not clamp to 1000; it resets to 100. `limit` <=0 -> 100.
14. Job list `next_after` is a string `"<unixnano>:<id>"`; entry list `next_after` is a number; `next_after` is omitted on a short page but a page that is exactly `limit` long always returns a cursor (the following page may be empty).
15. No error-report download: failures are only pageable via `GET .../failures` (JSON). No manifest export, no per-job upload log.
16. Delete refusal reasons (`connection_in_use`) include "still ACTIVE" with no counts; counts body only when dependents exist. `GET` list never returns DELETED connections.
17. `DELETE /download-links` needs `assets.delete` (not `assets.download`); a developer has it, an auditor does not. Auditors have no download/upload/ingestion write at all.
18. Token download URLs are path-only (`/api/v1/downloads/<token>`); mode may silently flip from `storage` to `token` (Azure SAS, packed assets) so always read `delivery`/`revocable` from the response.
19. `/api/v1/assets/{id}/content` and token links always send `Content-Disposition: attachment` and rewrite html/svg/xml/js to `application/octet-stream`; there is no inline preview path through the API.
20. Rate limits are per-org and tight for naive clients: part-URL, confirm and complete calls share the 600/min `upload_finish` bucket; storage `/test`, `/rotate-credentials`, `/enable` are 10/min. Respect `Retry-After`.
21. Data in docs vs code: docs say "everyone reads" connections; code requires `storage.read` (auditor has it, custom roles may not). Docs/DESIGN mention Idempotency-Key header for datasets only.
22. Sessions expire 30 min after start (default) while the single-upload presigned URL lives 15 min; large multipart uploads (>30 min total) will hit session expiry (409 "multipart upload is not active") — there is no session-extend call; choose bigger parts/higher concurrency or expect restarts.
