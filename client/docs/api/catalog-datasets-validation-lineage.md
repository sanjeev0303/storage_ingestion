# Frontend contract: catalog, datasets, validation, readiness, lineage

Source: `C:\Users\sanjeev.kumar\Music\storage\server` (read-only audit). All paths below are under `/api/v1`. Verified against handler/service/model code, not only docs.

## 0. Cross-cutting

- Auth: bearer token + organization (tenant). Another tenant's records answer `404 not_found`, same as missing.
- Error body (JSON): `{ "error": string, "code": string }`. Quota errors add `limit,max,used,requested,remaining`. Rate limit adds `Retry-After` header.
- EXCEPTION: malformed/unknown-field/oversized (>1 MiB) JSON body returns plain text `invalid JSON request` (or `request must contain one JSON value`), status 400, NOT the JSON error shape. Unknown JSON fields are rejected (DisallowUnknownFields) on every JSON body.
- Permissions are checked before the record lookup (403 `forbidden` is the same for every ID).
- Permission strings: `assets.read`, `assets.delete`, `catalog.write`, `datasets.read`, `datasets.write`, `datasets.manage`, `storage.use`.
- Built-in roles: `owner`, `admin` = all datasets/catalog perms. `developer` = `assets.read`, `assets.delete`, `catalog.write`, `datasets.read`, `datasets.write`, `storage.use` (NOT `datasets.manage`). `auditor` = `assets.read`, `datasets.read` only. Custom roles are sets of permission names.
- Rate limit buckets (per tenant): reads `listing` 600/min; catalog changes `upload_finish` 600/min; dataset create/snapshot/validate/readiness `job_create` 60/min; manifest POST + submit `manifest` 60/min; profile POST `admin` 120/min; other dataset changes `upload_finish`. 429 `rate_limited` + `Retry-After`.
- Optimistic concurrency (catalog objects and datasets): response `ETag: "<revision>"` (quoted int). Send revision as `If-Match: "<n>"` header OR body `revision` (int64). If both and different: 400 `invalid_request`. Missing/<=0: `428 revision_required`. Stale: `412 revision_conflict`.
- Timestamps: RFC 3339 strings. `*_at` marked "?" are omitted when null.
- Page sizes: catalog default 100, max 1000 (`CATALOG_*`); datasets default 100, max 1000 (`DATASET_*`), env-configurable (hard caps 10000). `limit` over max or negative: 400 `invalid_request` (not clamped). `limit=0`/absent = default. Non-numeric int param: 400 `invalid_request`.
- Cursor styles differ per endpoint (see each): opaque `next_cursor` string, `next_before` int, `next_after` int. Absent/empty = last page.

## 1. Catalog (`CatalogHandler`)

### Shapes

```ts
CatalogObject {
  id: string; dataset: string /* namespace; "" = root */; path: string;
  versioning: "ENABLED"|"DISABLED"; revision: number;
  metadata: Record<string,string> /* always present, {} if none */;
  current_version?: CurrentVersion /* absent if nothing published or logically deleted */;
  versions: number /* count of versions ever published */;
  deleted_at?: string; created_at: string; updated_at: string;
}
CurrentVersion {
  version: number; asset_id: string; size_bytes: number; content_type: string;
  checksum_sha256?: string; published_at?: string; storage_connection_id?: string;
}
ObjectVersion {
  version: number; asset_id: string; status: AssetStatus; current: boolean;
  file_name: string; size_bytes: number; content_type: string;
  checksum_algorithm?: "sha256" /* present iff checksum_sha256 present */;
  checksum_sha256?: string;
  provider_etag?: string /* store validator, NOT a checksum */;
  provider_version_id?: string /* only on versioned buckets */;
  metadata?: Record<string,string> /* upload-time metadata, immutable */;
  published_at?: string; created_at: string;
}
AssetStatus = "QUARANTINED"|"PROCESSING"|"AVAILABLE"|"REJECTED"|"SOFT_DELETED"|"DELETED"
```

- Checksum: only SHA-256 (`checksum_sha256` hex; `checksum_algorithm` literal `"sha256"` appears only on ObjectVersion, not on CurrentVersion).
- NOT exposed in catalog responses: storage keys, bucket, signed URLs, pack/offset/length (packing is internal; asset bytes are fetched via `/api/v1/assets/{asset_id}/...`), retention, legal hold, lifecycle/deletion-schedule fields. Those live on the asset routes (`GET /assets/{id}/lifecycle`, outside this audit). Catalog gives only `status`, `deleted_at`.
- Version `status` can be any AssetStatus for a numbered version (list returns all rows with a version number, including SOFT_DELETED/DELETED tombstones). Only AVAILABLE can be promoted.
- Object `ETag` header (`"<revision>"`) and `Cache-Control: private, no-store` on all object responses (list responses: no-store, no ETag).

### Routes

| Method + path | Perm | Request | Response / status |
|---|---|---|---|
| `GET /catalog/objects` | `assets.read` | query below | `{objects: CatalogObject[], next_cursor?: string}` 200 |
| `GET /catalog/objects/{id}` | `assets.read` | - | CatalogObject + ETag |
| `GET /catalog/lookup?dataset=&path=` | `assets.read` | `path` required (normalized like uploads); `dataset` optional (""=root) | CatalogObject + ETag; 400 invalid path/dataset; 404 |
| `GET /catalog/objects/{id}/versions?before=&limit=` | `assets.read` | `before` = version number (0/absent = newest); `limit` default 100 max 1000 | `{versions: ObjectVersion[], next_before?: number}` newest first; continue with `before=next_before` |
| `GET /catalog/objects/{id}/versions/{n\|current}` | `assets.read` | `n` positive int or literal `current` | ObjectVersion; 404 if object has no published version and `current` |
| `PATCH /catalog/objects/{id}` | `catalog.write` | `{revision?: number, metadata: Record<string,string\|null>}` merge patch; `null` deletes key | CatalogObject + ETag 200 |
| `POST /catalog/objects/{id}/rename` | `catalog.write` | `{revision?, path: string}` same namespace only; no bytes move | CatalogObject; 409 `path_exists` |
| `PUT /catalog/objects/{id}/versioning` | `catalog.write` | `{revision?, versioning: "ENABLED"\|"DISABLED"}` (case-insensitive input, uppercased) | CatalogObject |
| `POST /catalog/objects/{id}/versions/{n}/promote` | `catalog.write` | `{revision?}` (body may be `{}`; body is required to decode - send `{}`) | CatalogObject; makes AVAILABLE version n current (restore); later versions stay |
| `DELETE /catalog/objects/{id}/versions/{n}` | `assets.delete` | - | 202 ObjectVersion (current:false always in this response); via lifecycle (soft delete, retention/legal hold delay bytes not request); if it was current the newest remaining AVAILABLE becomes current |
| `DELETE /catalog/objects/{id}` | `assets.delete` | - | 202 `{object: CatalogObject, versions_in_processing: number}`; logical delete: published/rejected versions soft-deleted, object gets `deleted_at` and leaves default listings; versions still QUARANTINED/PROCESSING untouched and will publish later; path and history stay claimed (next upload to path = next version, undeletes) |
| `GET /catalog/capabilities` (only if wired) | `assets.read` | - | `{logical_versioning: true, provider_versioning: boolean, provider_versioning_status?: string}`; 503 `storage_unavailable` |

Revision on PATCH/rename/versioning/promote is required (428 if absent). DELETE routes take no revision. `delete version` has no revision check.

### List query params (`GET /catalog/objects`)
- `dataset` (absent = all namespaces; `dataset=` empty = root only; else must match `[a-z0-9][a-z0-9._-]{0,63}`), `prefix` (path prefix, case/NFC-insensitive, may end mid-segment; no leading `/`, `..`, `\`, control chars; <=1024 bytes), `object_id`, `asset_id`, `content_type` (exact, trimmed), `min_size`, `max_size` (bytes, >=0, min<=max when max>0), `created_after|created_before|updated_after|updated_before` (RFC 3339; After inclusive, Before exclusive), `metadata.<key>=<value>` (exact match, repeatable; max 4 keys by default, key must be valid metadata key), `include=unpublished` (also objects without current version, incl. logically deleted), `order` = `path` (default; sorted by dataset then path key) | `updated` (updated_at, id; ascending), `limit`, `cursor`.
- No descending option, no free-text search, no status filter, no versioning filter.
- Cursor: opaque base64url JSON bound to tenant + all filters + order. Changing any filter/order with a cursor: 400 `invalid_request`. Live view (not snapshot).
- Unselective filter timeout (5s): `422 query_too_broad`.
- Metadata rules: <=64 keys, <=8 KiB encoded, key 1-64 chars `[A-Za-z0-9._-]` case-sensitive, value <=1024 bytes UTF-8 no control/bidi chars, `system.` prefix and system names (id, path, status, size_bytes, checksum_sha256, retention, legal_hold, ...) reserved -> 400.
- Path rules: NFC, `/` separators, <=1024 bytes, <=255/segment, <=64 depth; refuse absolute, drive letters, `..`, backslash, control chars, Windows-forbidden chars, segments with edge spaces, first segment `_system|_ingestion|.well-known`. Case-insensitive uniqueness per namespace; path keeps first-upload casing.

### Catalog error codes
400 `invalid_request`; 428 `revision_required`; 412 `revision_conflict`; 404 `not_found`; 409 `path_exists`, `path_busy`, `deletion_blocked`; 403 `forbidden`; 422 `query_too_broad`; 401 `unauthenticated`; 429 `rate_limited`; 503 `storage_unavailable` (capabilities).

## 2. Datasets

### Shapes

```ts
Dataset {  // ETag = revision
  id: string; name: string;
  completion_policy: "ALL"|"REQUIRED"|"PARTIAL";
  status: "OPEN"|"INGESTING"|"READY"|"PARTIALLY_READY"|"INCOMPLETE"  // legacy bulk-ingestion status, NOT the managed lifecycle
  lifecycle_status: "DRAFT"|"VALIDATING"|"READY"|"READY_WITH_WARNINGS"|"FAILED"|"ARCHIVED";
  max_bytes: number; reserved_bytes: number; stored_bytes: number;
  created_at; updated_at; ready_at?;
  description: string; tags: string[] /* always array */; metadata: Record<string,string> /* always object */;
  revision: number; member_count: number; current_version_id?: string;
  profile_id?: string; profile_version?: number; latest_report_id?: string;
  derived_from?: Derivation; created_by: string; updated_by?: string; archived_at?: string;
  storage_connection_id?: string;
}
Derivation { version_ids: string[]; splits?: string[]; labels?: string[]; transformation?: {name:string; version:string; config_sha256?:string} }
```

Draft vs snapshot: the dataset itself is the mutable DRAFT (members, `member_count`, `lifecycle_status`). A snapshot makes an immutable `DatasetVersion` (status always `"FINALIZED"`). Changing the draft or snapshotting moves READY/READY_WITH_WARNINGS/FAILED back to DRAFT. Lifecycle transitions: DRAFT->VALIDATING|ARCHIVED; VALIDATING->READY|READY_WITH_WARNINGS|FAILED|DRAFT; READY*/FAILED->DRAFT|VALIDATING|ARCHIVED. ARCHIVED is final/read-only; VALIDATING cannot be archived (409 `invalid_transition`). Starting a validation sets VALIDATING only when validating the current version and draft still matches it. Dataset name: 1-64 chars `[a-z0-9][a-z0-9._-]*`, unique per tenant (also the catalog namespace label).

### Dataset routes

| Method + path | Perm | Request | Response / status |
|---|---|---|---|
| `POST /datasets` | `datasets.write` | `{name, completion_policy?: "ALL"(default)\|"REQUIRED"\|"PARTIAL", max_bytes?: number>=0, description?: <=2000 chars, tags?: string[]<=32 each `^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$`, metadata?: Record<string,string>, profile_id?, profile_version?, derived_from?: {version_ids: string[1..16], splits?: ("train"\|"validation"\|"test")[], labels?: string[<=100], transformation?: {name:1-128, version:<=64, config?: object<=16KiB}}}` | Dataset + ETag; 201 new, 200 same name+same settings; 409 `idempotency_conflict` if same name different settings. `derived_from` requires non-empty version_ids (400); derived draft is prefilled and members pinned. 422 `snapshot_too_large` possible. |
| `GET /datasets?status=&cursor=&limit=` | `datasets.read` | `status` = lifecycle value (case-insens.); sorted by name asc | `{datasets: Dataset[], next_cursor?}` (cursor `c1.<hex>`) |
| `GET /datasets/{id}` | `datasets.read` | - | Dataset + ETag |
| `PATCH /datasets/{id}` | `datasets.write` (+ `storage.use` when `storage_connection_id` present) | `{revision?, description?, tags?, metadata?: Record<string,string\|null> (merge patch), profile_id?, profile_version?, storage_connection_id?: string ("" = org default)}` | Dataset; 412/428 |
| `POST /datasets/{id}/archive` | `datasets.manage` | no body | Dataset (ARCHIVED); 409 `invalid_transition` while VALIDATING; cancels pending/running imports |
| `GET /datasets/{id}/members?cursor=&limit=` | `datasets.read` | page by member id | `{members: DatasetMember[], next_cursor?}` |
| `POST /datasets/{id}/members` | `datasets.write` | `{on_duplicate?: "REJECT"(default)\|"SKIP"\|"REPLACE", members: MemberRequest[1..1000]}` | 200 `AddMembersResponse` (below); 409 `dataset_archived` |
| `DELETE /datasets/{id}/members/{memberID}` | `datasets.write` | - | Dataset; 404 member |

```ts
MemberRequest { object_id?: string; namespace?: string; path?: string; version_id?: string /*asset id*/; version?: number; label?: string<=256; split?: "train"|"validation"|"test"; metadata?: Record<string,string> /* <=32 keys, 4 KiB, value<=1024 */ }
// rules: exactly one of object_id | path (path may carry namespace, ""=root); not both version_id and version; unpinned = follows object's current version until snapshot
DatasetMember { id; object_id; namespace; path; version_id?: string /*asset id, pinned*/; label?; split?; metadata: Record<string,string>; source: string /* "api" | "import:<id>" | "derived:<version>" */; added_by; created_at; updated_at }
AddMembersResponse { added:number; updated:number; skipped:number; rejected:number; results: {index:number; outcome:"ADDED"|"UPDATED"|"SKIPPED"|"REJECTED"; object_id?; version_id?; code?; error?}[]; dataset: Dataset }
```
Per-member reject codes (also manifest line codes): `missing_reference`, `invalid_reference`, `invalid_path`, `object_not_found`, `version_not_found`, `invalid_split`, `invalid_label`, `invalid_metadata`, `duplicate_member` (REJECT policy, already a member), `duplicate_in_manifest` (repeat within same request/manifest under REJECT). Bad items never fail the request (200 with REJECTED). Under REPLACE the last occurrence wins; otherwise first.

### Manifest imports

Flow (two modes, `POST /datasets/{id}/manifests`, perm `datasets.write`; `Idempotency-Key` header required, 1-128 chars, either mode):
1. Inline: `Content-Type: application/x-ndjson` (also `application/jsonl`, `application/jsonlines`), body = manifest; optional query `duplicate_policy=REJECT|SKIP|REPLACE`, `sha256=<64 hex>`. Max `DATASET_MAX_INLINE_MANIFEST_BYTES` (8 MiB default; 413 `too_large`). Response 202 (new) / 200 (same key+bytes) ManifestImport, state `QUEUED`. 409 `idempotency_conflict` if different bytes.
2. Staged: `Content-Type: application/json` body `{idempotency_key?: string (or header), size_bytes: number (1..max, default max 1 GiB, lowered by tenant quota), sha256?: string, duplicate_policy?}` -> 201 (200 on replay) ManifestImport with `state:"AWAITING_UPLOAD"`, `upload_url` (presigned PUT, expires 15 min; `Cache-Control: no-store`), `upload_expires_at`. Client PUTs bytes to `upload_url`, then `POST /datasets/{id}/manifests/{jobID}/submit` (no body) -> 202 `QUEUED`; 409 `manifest_not_uploaded` if not there; 400 if size differs from declared; 422 `checksum_mismatch` for bad sha256.
3. Poll `GET /datasets/{id}/manifests/{jobID}` (`datasets.read`).
4. `GET .../manifests/{jobID}/errors?after=<line>&limit=` -> `{errors: {line:number; code:string; detail:string}[], next_after?: number}` (`next_after` set when page was full; pass as `after`).
5. `POST .../manifests/{jobID}/cancel` (`datasets.write`, no body): 200 if now terminal, 202 if worker still stopping. Members already added stay.
- Other mode content type: 415 `unsupported_media_type`. 429 `too_many_jobs` when too many unfinished imports (default 10/tenant), with `Retry-After: 60`.

```ts
ManifestImport {
  id; dataset_id; state: "AWAITING_UPLOAD"|"QUEUED"|"RUNNING"|"COMPLETED"|"FAILED"|"CANCELLED";  // terminal: COMPLETED|FAILED|CANCELLED
  source: "INLINE"|"STAGED"; duplicate_policy: "REJECT"|"SKIP"|"REPLACE";
  manifest_bytes: number; manifest_sha256?: string;
  progress: { lines_read; members_added; members_updated; members_skipped; lines_rejected };  // numbers
  attempts: number; cancel_requested: boolean; last_error?: string;
  upload_url?: string; upload_expires_at?: string;  // only on staged create/replay while AWAITING_UPLOAD
  created_at; started_at?; completed_at?;
}
```
Manifest line (JSONL, schema v1, one JSON object per line, blank lines skipped, unknown fields refused):
```
{"object_id":"<id>","version_id":"<asset id>","label":"cat","split":"train","metadata":{"annotator":"a1"}}
{"namespace":"photos","path":"2024/cat-17.jpg","version":3,"label":"cat","schema_version":1}
```
Fields: `object_id` XOR `path` (+optional `namespace`), `version_id` XOR `version`, `label`, `split`, `metadata`, `schema_version` (must be 1). `tenant_id` is refused. Extra line error codes: `invalid_json`, `invalid_utf8`, `line_too_long` (64 KiB default), `tenant_field_not_allowed`, `unsupported_schema_version` plus member codes above. Stored error rows capped (10,000); `lines_rejected` stays exact.

### Snapshots / versions

| Route | Perm | Notes |
|---|---|---|
| `POST /datasets/{id}/snapshots` | `datasets.write` | body optional `{idempotency_key?: <=128}` (or `Idempotency-Key` header; with no body send none or valid JSON). 201 new / 200 same key -> DatasetVersion. Synchronous. 409: `no_members`, `unresolved_members` (member without pin and without current version), `dataset_archived`; 422 `snapshot_too_large` (>5,000,000 members). Can take minutes for huge datasets. |
| `GET /datasets/{id}/versions?before=&limit=` | `datasets.read` | `{versions: DatasetVersion[], next_before?: number}` newest first; `before` = version number |
| `GET /datasets/{id}/versions/{versionID}` | `datasets.read` | versionID is the version UUID (not the number); 404 if not in dataset |
| `GET /datasets/{id}/versions/{versionID}/members?after=&limit=` | `datasets.read` | `{members: VersionMember[], next_after?: number}`; `after` = last ordinal |

```ts
DatasetVersion { id; dataset_id; version: number; status: "FINALIZED";
  validation_status: "NOT_VALIDATED"|"READY"|"READY_WITH_WARNINGS"|"FAILED";  // latest finished validation of THIS version
  member_count; total_bytes; manifest_sha256; manifest_format: "dataset-manifest/v1";
  profile_id?; profile_version?; config: object /* {} if none */; parent_version_id?; derived_from?: Derivation; latest_report_id?; created_by; created_at }
VersionMember { ordinal: number; object_id; version_id /*asset id*/; object_version: number; path; label?; split?; size_bytes; content_type; checksum_sha256?; metadata: Record<string,string> }
```
Canonical manifest line (what `manifest_sha256` hashes, not returned by API): `{"object_id","version_id","version","path","size_bytes","content_type","sha256","label","split","metadata"}` per member sorted by object id.

## 3. Validation profiles

| Route | Perm | Notes |
|---|---|---|
| `GET /validation-profiles` | `datasets.read` | `{profiles: ValidationProfile[], rule_types: string[]}` builtin first, then latest version of each tenant profile |
| `GET /validation-profiles/{profileID}?version=` | `datasets.read` | `version` 0/absent = latest; 404 |
| `POST /validation-profiles` | `datasets.manage` | creates v1 for a new name, next version for same name (new `vp_...` id only for new name per code; same-name versions share id) -> 201 ValidationProfile. 400 `invalid_request` on bad config/rule (strict config decode), >64 KiB definition |

Builtin ids (version 1): `builtin:general`, `builtin:image`, `builtin:audio`, `builtin:video`, `builtin:text`, `builtin:supervised-classification`. Tenant ids: `vp_<id>`.

```ts
ValidationProfile { id; version: number; name; description; built_in: boolean; definition: ProfileDefinition; created_at?: string /* omitted for builtins */ }
ProfileDefinition { kind: string; rules: Rule[]; thresholds: { max_errors: number; max_warnings?: number } }
Rule { id: string; version: number; type: RuleType; severity: "ERROR"|"WARNING"|"INFO"; enabled: boolean; scope: "DATASET"|"OBJECT_METADATA"|"OBJECT_CONTENT"|"SPLIT"; config?: object }
// POST body: { name (1-128), description? (<=2000), kind?, rules: {id, version, type, severity, enabled?: bool (default true), scope? (defaulted from type), config?}[], thresholds: {max_errors: number, max_warnings?: number} }
```
Rule types (`rule_types`, type -> default scope -> readiness dimension -> codes): `required_metadata`(OBJECT_METADATA, COMPLETENESS, `MISSING_METADATA`; cfg `keys`), `governance_metadata`(GOVERNANCE, `MISSING_GOVERNANCE_METADATA`; `keys`), `metadata_schema`(CONSISTENCY, `METADATA_SCHEMA`; `fields{key:{type:string|integer|number|boolean,enum,pattern}}`,`allow_unknown`), `allowed_media_types`(CONSISTENCY, `MEDIA_TYPE_NOT_ALLOWED`; `types`), `allowed_extensions`(CONSISTENCY, `EXTENSION_NOT_ALLOWED`; `extensions`), `object_size`(CONSISTENCY, `OBJECT_TOO_SMALL|OBJECT_TOO_LARGE`; `min_bytes,max_bytes`), `path_pattern`(CONSISTENCY, `PATH_PATTERN_MISMATCH`; `pattern` RE2<=256), `required_label`(COMPLETENESS, `MISSING_LABEL`), `split_assignment`(SPLIT, CONSISTENCY, `MISSING_SPLIT|SPLIT_NOT_ALLOWED`; `allowed`), `object_exists`(COMPLETENESS, `OBJECT_MISSING|VERSION_MISSING|OBJECT_DELETED`), `lifecycle_state`(ACCESSIBILITY, `VERSION_NOT_ACCESSIBLE`; `allowed` default AVAILABLE), `size_consistency`(INTEGRITY, `SIZE_INCONSISTENT|CHECKSUM_INCONSISTENT`), `checksum_present`(INTEGRITY, `CHECKSUM_MISSING`), `checksum_verified`(INTEGRITY, `CHECKSUM_UNVERIFIED|CHECKSUM_VERIFICATION_FAILED|CHECKSUM_NOT_DECLARED`; `require_declared`), `malware_scan`(SECURITY, `MALWARE_DETECTED|SCAN_MISSING|NOT_SCANNED_CLEAN|SCAN_INCOMPLETE`; `policy` clean|scanned), `content_checksum`(OBJECT_CONTENT, INTEGRITY, `CHECKSUM_MISMATCH|SIZE_MISMATCH|CONTENT_MISSING`; `reverify`), `member_count`(DATASET, COMPLETENESS, `EMPTY_DATASET|TOO_FEW_MEMBERS|TOO_MANY_MEMBERS`; `min>=1,max`), `total_bytes`(DATASET, DISTRIBUTION, `TOTAL_BYTES_OUT_OF_RANGE`; `min,max`), `duplicate_references`(DATASET, DUPLICATION, `DUPLICATE_REFERENCE`), `duplicate_content`(DATASET, DUPLICATION, `DUPLICATE_CONTENT`), `split_leakage`(SPLIT, CONSISTENCY, `SPLIT_LEAKAGE`), `split_proportions`(SPLIT, DISTRIBUTION, `SPLIT_PROPORTION`; `proportions{split:[min,max]}`), `required_labels`(DATASET, COMPLETENESS, `LABEL_MISSING`; `labels`), `label_distribution`(DATASET, DISTRIBUTION, `CLASS_TOO_SMALL|CLASS_IMBALANCE`; `min_per_class,max_class_share`), `file_type_distribution`(DATASET, DISTRIBUTION, `UNEXPECTED_FILE_TYPE|FILE_TYPE_DOMINANT`; `expected,max_share`). Authoritative list comes from `rule_types` in the GET response.

Thresholds outcome: ERROR count > `max_errors` (default 0) or WARNING count > `max_warnings` (if set) -> FAILED; else any ERROR/WARNING -> READY_WITH_WARNINGS; else READY. INFO never counts.

## 4. Validation jobs

| Route | Perm | Notes |
|---|---|---|
| `POST /datasets/{id}/validations` | `datasets.write` | request below; 202 new + `Location: /api/v1/validation-jobs/{id}`, 200 same idempotency key |
| `GET /datasets/{id}/validations?cursor=&limit=` | `datasets.read` | `{jobs: ValidationJob[], next_cursor?}` newest first (includes readiness jobs) |
| `GET /validation-jobs/{jobID}` | `datasets.read` | ValidationJob |
| `POST /validation-jobs/{jobID}/cancel` | `datasets.write` | no body; 200 if terminal, 202 while stopping; 409 `invalid_state` if already ended in a way that disallows |

```ts
ValidationRequest { version_id?: string /* default: dataset.current_version_id */; profile_id?: string /* default: dataset profile, then "builtin:general" (validation only) */; profile_version?: number /* 0 = latest */;
  mode?: "METADATA"|"FULL"|"SAMPLED" /* default METADATA, case-insens. */; sample_size?: number /* SAMPLED only: default 100, max 10000; sending with other modes = 400 */; idempotency_key?: string<=128 /* or Idempotency-Key header */ }
ValidationJob { id; dataset_id; version_id; profile_id; profile_version;
  mode: "METADATA"|"FULL"|"SAMPLED"; sample_size?: number; readiness: boolean /* true = readiness assessment */;
  state: "QUEUED"|"RUNNING"|"COMPLETED"|"COMPLETED_WITH_WARNINGS"|"FAILED"|"CANCELLED";   // terminal = all but QUEUED/RUNNING
  progress: { total; processed; passed; warnings; failed; content_examined; content_not_examined; bytes_total; bytes_read };
  attempts; max_attempts; cancel_requested: boolean; last_error?: string; report_id?: string;
  created_by; created_at; started_at?; completed_at? }
```
Mode semantics: METADATA = catalog records only, content never read. FULL = every member content integrity (from processing evidence or reading bytes). SAMPLED = deterministic sample of at most `sample_size`.
State semantics: FAILED with `report_id` = version failed validation; FAILED without `report_id` = could not run (`last_error`). State mapping: report READY -> COMPLETED, READY_WITH_WARNINGS -> COMPLETED_WITH_WARNINGS, FAILED -> FAILED.
Errors: 409 `no_version` (no snapshot yet), 409 `dataset_archived`, 404 (dataset/version/profile), 400 invalid mode/sample/profile, 429 `too_many_jobs` (default 10 active/tenant, `Retry-After: 60`).

## 5. Reports and findings

| Route | Perm | Response |
|---|---|---|
| `GET /validation-reports/{reportID}` | `datasets.read` | ValidationReport with `report` body |
| `GET /validation-reports/{reportID}/findings?severity=&rule_id=&cursor=&limit=` | `datasets.read` | `{findings: Finding[], next_cursor?}` ordered by finding id |
| `GET /datasets/{id}/readiness-reports?all=&cursor=&limit=` | `datasets.read` | `{reports: ValidationReport[] (no `report` body), next_cursor?}` newest first; default only readiness reports; `all=true` (exact string) adds plain validations |

```ts
ValidationReport { id; job_id; dataset_id; version_id; profile_id; profile_version; mode; status: "READY"|"READY_WITH_WARNINGS"|"FAILED";
  readiness: boolean; schema_version: 1; summary: string; started_at; completed_at; report?: ReportBody /* only on single GET */ }
Finding { id; rule_id; rule_version; severity: "ERROR"|"WARNING"|"INFO"; code: string /* e.g. SPLIT_LEAKAGE */;
  object_id?; version_id?; message; expected?; observed?; evidence: string; remediation?: string; created_at }
```
- Findings filters: ONLY `severity` (ERROR|WARNING|INFO, case-insens.; bad -> 400) and `rule_id`. No filter by object, code, or version. Stored findings capped per rule per job (10,000); counts in report remain exact.
- Reports/findings never contain storage keys, URLs, content, or metadata values.

`ReportBody` (schema_version 1; field names exact):
```ts
{ schema_version; report_id; job_id; dataset_id; dataset_version_id; dataset_version: number; manifest_sha256;
  profile: {id; version; kind}; thresholds: {max_errors; max_warnings?}; mode; sample_size?: number;
  status: "READY"|"READY_WITH_WARNINGS"|"FAILED"; started_at; completed_at;
  members: {total; examined; passed; warnings; failed};
  content: { coverage: "NONE"|"SAMPLED"|"FULL"; examined; read; not_examined; bytes_represented; bytes_read; note: string };
  checks: {passed; failed; partial; not_assessed};   // counts of rules
  results: CheckResult[];
  findings: {by_severity: Record<string,number>; by_rule: Record<string,number>; stored: number};
  evidence: {verified_checksums: number; checksum_verification: Count[]; malware_scans: Count[]; version_states: Count[]};
  distributions: {splits: Count[]; labels: Count[]; distinct_labels; unlabeled; content_types: Count[]; extensions: Count[]; sizes: {total_bytes; min_bytes; max_bytes}};  // top 50 each
  readiness?: ReadinessReport;  // only when job.readiness
  summary: string }
Count { value: string; count: number }
CheckResult { rule_id; rule_version; type; severity; scope; dimension: Dimension; status: "PASSED"|"FAILED"|"PARTIAL"|"NOT_ASSESSED";
  checked; failed; not_assessed; read?; findings_stored; note? }
```
Check statuses are `PASSED|FAILED|PARTIAL|NOT_ASSESSED` (no WARN/SAMPLED/unavailable; sampled content check passing = `PARTIAL`, `content.coverage="SAMPLED"`; unreadable members count as not assessed).

## 6. Readiness

Start: `POST /datasets/{id}/readiness-assessments` (`datasets.write`), same body as ValidationRequest, same response/job (ValidationJob with `readiness:true`), 202. Differences: requires a profile (request `profile_id` or dataset's) else 400 `invalid_request`; no builtin:general fallback. Counts toward `job_create`.

```ts
ReadinessReport { profile: string /* "<profile_id>@<version>" */; dimensions: DimensionResult[] /* always 9, in this order */; disclaimer: string }
DimensionResult { dimension: Dimension; status: "PASS"|"WARN"|"FAIL"|"NOT_ASSESSED"; checks: string[] /* rule ids */; coverage: "FULL"|"SAMPLED"|"PARTIAL"|"NONE"; notes?: string[] }
Dimension = "COMPLETENESS"|"INTEGRITY"|"SECURITY"|"CONSISTENCY"|"DUPLICATION"|"DISTRIBUTION"|"REPRODUCIBILITY"|"GOVERNANCE"|"ACCESSIBILITY"
```
No overall score. Status: FAIL if an ERROR-severity check of the dimension failed; WARN if a WARNING check failed or a check was only PARTIAL; NOT_ASSESSED if no check judged it (coverage NONE); else PASS. REPRODUCIBILITY is computed from the version (FAIL if not finalized/hash, WARN if member versions no longer AVAILABLE) and has `checks: []`, coverage FULL.
Remediation: there is NO per-dimension remediation field. Remediation text only exists per `Finding.remediation` (optional string); dimension `notes` are plain strings.

## 7. Lineage

| Route | Perm |
|---|---|
| `GET /datasets/{id}/lineage` | `datasets.read` |
| `GET /dataset-versions/{versionID}/lineage` | `datasets.read` |

Query: `direction` = `upstream` (default; what a node contains/came from/was checked by) | `downstream` (what contains/imports/derives from it); `depth` (default 3, 1..10 (env `DATASET_LINEAGE_MAX_DEPTH`), 0=default); `limit` (page size, default 100, max 1000); `cursor` (opaque string). Walk capped at 10,000 edges and 5s (`422 query_too_broad`). Bad direction/depth: 400.

```ts
LineageResponse { start: Ref; direction: "upstream"|"downstream"; edges: Edge[]; next_cursor?: string; truncated: boolean }
Ref { kind: "DATASET"|"DATASET_VERSION"|"MANIFEST_IMPORT"|"VALIDATION_REPORT"|"TRANSFORMATION"|"OBJECT_VERSION"; id: string }
Edge { from: Ref; to: Ref; relation: "CONTAINS"|"DERIVED_FROM"|"IMPORTED_FROM"|"VALIDATED_BY"|"CREATED_FROM_SNAPSHOT"|"TRANSFORMED_BY";
       depth: number /* distance of the far end from start; edges ordered by depth */; actor: string; job_id?: string; details?: object; created_at }
```
There is no node list: only edges; derive nodes from `from`/`to` refs (nodes carry just kind+id, no labels/names). Edge reads "from RELATION to". Valid pairs: CONTAINS DATASET->DATASET_VERSION; CREATED_FROM_SNAPSHOT DATASET->DATASET_VERSION(source); DERIVED_FROM DATASET_VERSION->DATASET_VERSION(source); TRANSFORMED_BY DATASET|DATASET_VERSION->TRANSFORMATION; IMPORTED_FROM DATASET->MANIFEST_IMPORT; VALIDATED_BY DATASET_VERSION->VALIDATION_REPORT. `OBJECT_VERSION` nodes are reserved and never appear; object versions are fetched via `GET /datasets/{id}/versions/{vid}/members`. Edge `details` is free-form (e.g. manifest hashes, transformation, import counts). Read-only (no API to add edges).

## 8. Dataset error codes (JSON `code`)

400 `invalid_request`; 401 `unauthenticated`; 403 `forbidden`; 404 `not_found`; 409 `dataset_archived`, `invalid_transition`, `no_members`, `unresolved_members`, `no_version`, `idempotency_conflict`, `invalid_state`, `manifest_not_uploaded`, `lineage_cycle`, `conflict`; 412 `revision_conflict`; 413 `too_large`; 415 `unsupported_media_type`; 422 `checksum_mismatch`, `snapshot_too_large`, `query_too_broad`; 428 `revision_required`; 429 `too_many_jobs`, `rate_limited`.

## 9. Gaps / surprises

1. Two "status" fields on Dataset: legacy bulk-ingestion `status` (OPEN/INGESTING/READY/PARTIALLY_READY/INCOMPLETE) and managed `lifecycle_status`. UI should key off `lifecycle_status`.
2. `POST /datasets` and `GET /datasets/{id}` are served by the dataset handler but accept the old ingestion request; ingestion-era fields (`completion_policy`, `max_bytes`, `reserved_bytes`, `stored_bytes`) still appear.
3. Plain-text 400 for malformed JSON bodies (not `{error,code}`); client error parser must handle non-JSON bodies. Also `http.Error` adds trailing newline.
4. Catalog versions do not expose retention, legal hold, deletion schedule, scan result, or pack info. Needs `/assets/{id}/lifecycle` (not audited here). Pack offsets/keys/connection internals are never exposed; only `storage_connection_id` appears (current version + dataset). `checksum_algorithm` appears only on ObjectVersion, not CurrentVersion.
5. Catalog version list includes SOFT_DELETED/DELETED tombstoned versions (any numbered asset); consumers must filter by `status`. `DELETE .../versions/{n}` response always has `current:false`.
6. No catalog sort desc, no search, no filter by versioning/deleted/status. `include=unpublished` is the only way to see logically deleted objects (check `deleted_at`).
7. Catalog `promote` body must be valid JSON (send `{}`) because the body is decoded unconditionally; whereas snapshot/validation POST tolerate empty body (ContentLength 0). `archive`, `cancel`, `submit` take no body.
8. Mixed pagination styles: catalog `next_cursor` (base64url JSON, filter-bound), catalog/dataset versions `next_before` (version number), version members and import errors `next_after` (ordinal/line), datasets/members/validations/findings/reports `next_cursor` (`c1.<hex>` or opaque). Dataset-version `next_before` is last version on the page when full and >1.
9. Findings filter only by severity and rule_id (no object filter, no code filter). Finding `evidence` is a required string (may be empty); `expected`/`observed` omitted when empty.
10. Check status vocabulary (PASSED/FAILED/PARTIAL/NOT_ASSESSED) differs from dimension vocabulary (PASS/WARN/FAIL/NOT_ASSESSED) and from lifecycle vocabulary (READY/READY_WITH_WARNINGS/FAILED). No "unavailable" status; unreadable content = counted as `content.not_examined`/`not_assessed`.
11. Readiness has no remediation field and no score; only dimension `notes` strings and per-finding `remediation`.
12. Lineage has no node objects, no truncation of depth reporting: model comment says `truncated` means depth limit, docs say edge cap (10,000). Treat `truncated:true` as "more exists / narrow query"; `next_cursor` pages within the walk.
13. Lineage `depth` max is configuration (default 10); `limit` uses the shared dataset page-size rules (default 100, max 1000), not a separate edge limit param.
14. `DatasetVersion.validation_status` is `NOT_VALIDATED` or the report status; `DatasetVersion.status` is always `FINALIZED`.
15. Profile creation: same `name` makes a new version under the same id (per docs); the server picks the id (`vp_...`) - the client cannot choose ids or edit existing versions. No list-all-versions or delete route; `GET /validation-profiles` returns only the latest version of tenant profiles.
16. Idempotency: manifest import REQUIRES a key (header or body field in staged mode); snapshot/validation keys optional. Validation `sample_size` with non-SAMPLED mode is a 400.
17. Archived datasets: reads work; any write returns 409 `dataset_archived`; validation start also 409.
18. Snapshot runs inside the HTTP request (can be slow for large datasets; proxy timeouts cancel it with nothing created).
19. `DELETE /datasets/{id}/members/{memberID}` returns the Dataset (not 204); member id is the DatasetMember `id`, not object id.
20. `catalog/capabilities` route is registered only if the capability provider is wired; may 404 in some deployments.
