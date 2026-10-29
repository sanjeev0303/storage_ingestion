# Managed datasets: membership, snapshots, validation, readiness and lineage (local runbook)

Phase 10 turns a tenant's catalog objects into datasets for machine-learning and AI work:

- **Curated membership.** A dataset is a curated, tenant-owned collection of catalog objects. Members reference objects, and optionally exact versions; bytes are never copied.
- **Immutable snapshots.** A snapshot fixes the draft as a version of exact object versions, with a canonical manifest hash.
- **Validation and readiness.** Validation jobs check a version against a versioned profile and write a durable report with paginated findings. A readiness assessment adds a dimension-by-dimension judgement against the profile the caller chose.
- **Lineage.** The lineage graph records what each version contains, came from, was imported from and was checked by.

| Piece | Where | Role |
|---|---|---|
| Schema | `migrations/000014_dataset_management.sql` | lifecycle columns on `datasets`; `dataset_members`, `dataset_versions`, `dataset_version_members`, `dataset_manifest_imports`, `dataset_import_errors`, `validation_profiles`, `validation_jobs`, `validation_reports`, `validation_findings`, `lineage_nodes`, `lineage_edges` |
| Model | `internal/model/dataset.go` | statuses and transitions, members, versions and the canonical manifest line, imports, rules, jobs, findings, reports, lineage relations |
| Repository | `internal/infrastructure/database/postgres/dataset_repository.go`, `dataset_import_repository.go`, `validation_repository.go`, `lineage_repository.go` | transactional membership and snapshots, import batches with checkpoints, job claims and batches, aggregates, reports, the graph |
| Service | `internal/service/dataset` | authorization and audit, membership, manifests (`imports.go`), the rules engine (`rules.go`), built-in profiles (`profiles.go`), jobs and reports (`validation.go`), readiness (`readiness.go`) |
| JSONL reader | `internal/platform/jsonl` | the bounded line reader, shared with Phase 7's ingestion manifests |
| HTTP | `internal/handler/http/dataset_handler.go` | `/api/v1/datasets...`, `/api/v1/validation-jobs`, `/api/v1/validation-reports`, `/api/v1/dataset-versions`, `/api/v1/validation-profiles` |
| Events | `internal/dto/event/dataset_event.go`, `internal/service/events/handlers.go` | `dataset.import_queued`, `dataset.validation_queued` on the `datasets` queue |
| Worker | `cmd/dataset-worker`, `internal/worker/dataset` | imports, validations, recovery, gauges, `/health` and `/ready` |
| Fixture | `testdata/datasets` | six small text files, a manifest, a profile and an upload script |

## Repository inspection (what existed, what was added)

Phase 10 was built on the code as it stands, not on what earlier plans said.

| Capability | Before Phase 10 | Phase 10 |
|---|---|---|
| Datasets | Phase 7 `datasets`: name, completion policy, ingestion status (`OPEN` … `INCOMPLETE`), byte quota; `POST /api/v1/datasets`, `GET /api/v1/datasets/{id}` | Same rows, extended. The managed lifecycle (`lifecycle_status`) sits beside the ingestion `status`. Description, tags, metadata, revision, member count, current version, profile, latest report, derivation and archival were added. The two routes moved to the dataset handler and accept the old requests unchanged. |
| Membership | none: catalog `dataset` was only a namespace label | `dataset_members`, separate from storage and from namespaces |
| Versions / snapshots | none | `dataset_versions` and `dataset_version_members`, canonical hash |
| Manifests | Phase 7 JSONL reader (bounded lines, checkpoints, inline and staged), for ingestion entries | The line reader was extracted to `internal/platform/jsonl` and is now used by both. Dataset manifests have their own schema, imports and worker. |
| Validation | Phase 6/7 processing: checksum verification, scanning and content-type checks per upload, recorded on `processing_jobs` | Versioned profiles, a rules engine and jobs per dataset version. Processing's records serve as evidence and are never re-derived. |
| Lineage | none | relational nodes and edges, recursive bounded traversal |
| Async work | transactional outbox, SQS consumers with inbox and dead letters, DB-polled workers with leases | The same: outbox events on a new `datasets` queue, plus a polling worker with leases, checkpoints and backoff |
| AuthN/Z, quotas, audit | Phase 8: tenants, roles as permission sets, rate rules, quotas, hash-chained audit | New permission `datasets.manage`. Dataset routes use the existing rate rules. Manifest limits follow tenant quotas. Every change is audited. |
| Catalog | Phase 9: objects, versions (assets), metadata, cursor listings | Members reference catalog objects and versions; path references use the catalog's normalization |

## Datasets and their lifecycle

`lifecycle_status` is one of `DRAFT`, `VALIDATING`, `READY`, `READY_WITH_WARNINGS`, `FAILED` and `ARCHIVED`. Transitions are explicit (`model.CanTransitionDataset`) and enforced in SQL by conditional updates:

```
DRAFT ──validation of the current version──▶ VALIDATING ──outcome──▶ READY | READY_WITH_WARNINGS | FAILED
  ▲                                              │                         │
  └──── cancelled, failed to run, or draft changed meanwhile ──────────────┘ (a change to the draft or a new snapshot)
DRAFT | READY | READY_WITH_WARNINGS | FAILED ──archive──▶ ARCHIVED (final; VALIDATING cannot be archived)
```

- **Which validation moves the status.** Starting a validation moves the dataset to `VALIDATING` only when it validates the current version and the draft still matches that version (`members_revision` equal). Validating an older version records its outcome on that version only.
- **When a validation ends.** The dataset takes the outcome if the version is still current and the draft still matches; otherwise it returns to `DRAFT`. While another validation of the current version is unfinished, it stays `VALIDATING`.
- **Draft changes.** Any change to the draft, and any new snapshot, moves `READY*` and `FAILED` back to `DRAFT`.
- **Archiving.** An archived dataset is read-only; its versions, reports and lineage stay. Archiving cancels imports that are waiting and asks running ones to stop.
- **Updates.** `PATCH` needs the revision, sent as `If-Match` (the `ETag` of every dataset response) or in the body. None is `428 revision_required`; a stale one is `412 revision_conflict`. Metadata is a merge patch: `null` removes a key.
- **Recovery.** Ending a job settles its dataset in the same transaction. As a safety net, the worker returns any dataset left `VALIDATING` with no unfinished validation to `DRAFT` (`dataset_stuck_released`).

## Membership

- **Identity.** A member is one catalog object (`dataset_members`, unique per dataset and object), named by `object_id` or by `namespace` + `path` (catalog normalization: case- and Unicode-insensitive; `..`, absolute paths and control characters refused).
- **Version.** Optionally pinned to one version by `version_id` (the asset ID) or `version` number; unpinned members follow the object's current version until a snapshot fixes it.
- **Provenance.** Each member records its label, split (`train`, `validation`, `test`), member metadata, `source` (`api`, `import:<id>`, `derived:<version>`) and `added_by`.
- **Tenant boundary.** References are resolved only in the caller's tenant. Another tenant's object is "not found", never an error that reveals it exists. Composite foreign keys make a cross-tenant member impossible even by direct SQL.
- **Duplicates.** The duplicate policy (`on_duplicate` / `duplicate_policy`) is `REJECT` (default), `SKIP` or `REPLACE`; a replacement that changes nothing counts as skipped. Within one request or manifest, `REPLACE` keeps the last occurrence and the others keep the first.
- **Bulk adds.** Bulk adds take up to `DATASET_MAX_BULK_MEMBERS` per request and report each input's outcome (`ADDED`, `UPDATED`, `SKIPPED`, `REJECTED` + code); bad inputs never fail the others.
- **Paging.** Members page by member ID (`ix_dataset_members_page`).

## Versions (snapshots) and the canonical manifest

`POST /api/v1/datasets/{id}/snapshots` runs one transaction under the dataset's row lock (every membership change takes the same lock first, so a snapshot never sees a change halfway through):

1. **Idempotency key.** With an idempotency key (`Idempotency-Key` header or body), an earlier version made with it is returned (`200`), checked before and after taking the lock.
2. **Refusals.** It refuses an archived dataset, an empty one (`409 no_members`), one larger than `DATASET_MAX_SNAPSHOT_MEMBERS`, and one with members that have neither a pin nor a current version (`409 unresolved_members`).
3. **Member rows.** `INSERT … SELECT` copies every member into `dataset_version_members`, resolving the exact asset (pin, else current version) and recording path, label, split, size, content type, checksum and metadata (version metadata with the member's on top). The database does this; no member list exists in the service.
4. **Hash.** The rows are read back in canonical order through SHA-256, as a stream, and the version is sealed with the hash, count and total bytes.
5. **Current version and lineage.** The dataset's current version and next version number move, and the lineage edges are written.

A version is never changed afterwards. Later uploads, renames or member changes affect only later versions, and the version still names the exact asset of each member. Retention still applies to those assets: a version whose object version was deleted under retention keeps its record. Validation then reports the version as missing or inaccessible.

**Canonical manifest (`dataset-manifest/v1`).** One line per member in ordinal order, where ordinal is the member's position when members are sorted by object ID, starting at 1. Each line is a JSON object with exactly these keys in this order, as Go's `encoding/json` writes it (metadata keys sorted; `<`, `>` and `&` escaped as `\u003c`, `\u003e`, `\u0026`), followed by `\n`:

```
{"object_id":"…","version_id":"<asset id>","version":3,"path":"pets/cat-1.txt","size_bytes":29,"content_type":"text/plain","sha256":"…","label":"cat","split":"train","metadata":{"license":"cc0"}}
```

`manifest_sha256` is the SHA-256 of the concatenated lines. The same members, versions, labels, splits and metadata always give the same hash, in any dataset (`TestSnapshotsAreImmutableDeterministicAndIdempotent` checks both).

**Derived datasets.** `POST /api/v1/datasets` with `derived_from` creates a dataset whose draft is the members of up to 16 source versions of the tenant. It can be limited to some splits and labels; the first source to hold an object wins. Every member is pinned to the exact version the source held, in the same transaction as the creation. An optional `transformation` (name, version, configuration) is recorded with the SHA-256 of its configuration, never the configuration itself. The derived dataset's versions name their inputs in the lineage graph.

## Manifest imports

Format: JSON Lines, schema version 1, one member per line (`internal/service/dataset/imports.go`):

```
{"object_id":"<id>","version_id":"<asset id>","label":"cat","split":"train","metadata":{"annotator":"a1"}}
{"namespace":"photos","path":"2024/cat-17.jpg","version":3,"label":"cat","schema_version":1}
```

Fields: `object_id`, or `path` with an optional `namespace`; `version_id` or `version`; `label` (≤ 256), `split`, `metadata` (flat strings, ≤ 32 keys and 4 KiB), `schema_version` (must be 1 when present). Blank lines are skipped. Unknown fields are refused. A `tenant_id` field is refused outright (`tenant_field_not_allowed`): references always resolve in the importing tenant.

- **Inline.** `POST /api/v1/datasets/{id}/manifests` with `Content-Type: application/x-ndjson` (or `application/jsonl`) and an `Idempotency-Key` header. The body is read once, bounded by `DATASET_MAX_INLINE_MANIFEST_BYTES` and the tenant's quota (`413 too_large` beyond). It is hashed, an optional `?sha256=` is checked, and it is kept with the import. Answers `202` with the import, or `200` for the same key and bytes again (`409 idempotency_conflict` for other bytes).
- **Staged.** The same route with `Content-Type: application/json`, `{"idempotency_key","size_bytes","sha256"?,"duplicate_policy"?}`, answers `201` with a presigned `upload_url`. PUT the manifest there, then `POST …/manifests/{jobID}/submit`: the size is checked against storage and the import queued (`409 manifest_not_uploaded` before the upload). A declared SHA-256 is verified over the whole object before any line is stored. A manifest replaced after submission fails the import (its ETag no longer matches).
- **Reading.** Queuing writes `dataset.import_queued` in the import's transaction. The dataset worker claims the import with a lease and reads it from its checkpoint in batches of `DATASET_IMPORT_BATCH` lines through the bounded JSONL reader. Each batch's references are resolved in a few set queries. The batch's members, rejected lines and the new checkpoint (line and byte offset) commit together, only while the worker still holds the lease. A pass yields after `DATASET_IMPORT_BUDGET`, so a huge manifest cannot hold a worker.
- **Errors.** Rejected lines are recorded with their line number and a code: `invalid_json`, `invalid_utf8`, `line_too_long`, `missing_reference`, `invalid_reference`, `invalid_path`, `object_not_found`, `version_not_found`, `invalid_split`, `invalid_label`, `invalid_metadata`, `duplicate_member`, `duplicate_in_manifest`, `tenant_field_not_allowed`, `unsupported_schema_version`. They page by line at `GET …/manifests/{jobID}/errors?after=<line>`; at most `DATASET_MAX_IMPORT_ERRORS` are stored, and all are counted.
- **Failures and retries.** A pass that fails is retried with backoff, from the same checkpoint. `attempts` counts claims without progress, so a manifest that keeps failing ends `FAILED` after `DATASET_MAX_ATTEMPTS` (its dead letter). A worker that dies leaves its lease to expire; the next claim resumes at the checkpoint. Nothing is stored twice.
- **Cancellation.** `POST …/manifests/{jobID}/cancel` cancels an import no worker holds at once. One being read is asked to stop, and stops after its current batch (`202`). Members an import already added stay; the draft is not a snapshot.
- **Lineage.** An import that changed the draft records the edge DATASET `IMPORTED_FROM` MANIFEST_IMPORT, with its counts and manifest hash.
- **Limits.** Manifest size, line size and line count are capped by `DATASET_MAX_MANIFEST_BYTES`, `DATASET_MANIFEST_MAX_LINE_BYTES` and `DATASET_MAX_MANIFEST_ENTRIES`, each lowered by the tenant's quota (`max_manifest_bytes`, `max_manifest_entries`). Unfinished imports per tenant are capped by `DATASET_MAX_ACTIVE_IMPORTS` (`429 too_many_jobs`).
- **No content reads.** Importing never reads object content: references and metadata only.

## Validation

### Profiles

A profile is a versioned list of rules with thresholds.

- **Built-in profiles.** These ship in code, version 1 each: `builtin:general`, `builtin:image`, `builtin:audio`, `builtin:video`, `builtin:text` and `builtin:supervised-classification`.
- **Tenant profiles.** Profiles are written with `POST /api/v1/validation-profiles` (needs `datasets.manage`). A new name gets version 1; the same name gets the next version under the same ID. Written versions never change.
- **The job's copy.** A job stores the exact definition it ran with, so a report always says which rules it applied.
- **Rule fields.** Each rule has a stable `id`, `version`, `type`, `severity` (`ERROR`, `WARNING`, `INFO`), `enabled` (default true), `scope` (set by the type) and `config`. Configuration is decoded strictly: an unknown field or a bad value refuses the profile (`400`) rather than silently disabling a check. `GET /api/v1/validation-profiles` lists the profiles and every rule type.

| Type | Scope | Readiness dimension | Config | Finds |
|---|---|---|---|---|
| `required_metadata` | OBJECT_METADATA | Completeness | `keys` | `MISSING_METADATA` |
| `governance_metadata` | OBJECT_METADATA | Governance | `keys` (e.g. `license`, `source`) | `MISSING_GOVERNANCE_METADATA` |
| `metadata_schema` | OBJECT_METADATA | Consistency | `fields: {key: {type: string\|integer\|number\|boolean, enum, pattern}}`, `allow_unknown` | `METADATA_SCHEMA` (values are never echoed) |
| `allowed_media_types` | OBJECT_METADATA | Consistency | `types` (`image/*` allowed) | `MEDIA_TYPE_NOT_ALLOWED` |
| `allowed_extensions` | OBJECT_METADATA | Consistency | `extensions` | `EXTENSION_NOT_ALLOWED` |
| `object_size` | OBJECT_METADATA | Consistency | `min_bytes`, `max_bytes` | `OBJECT_TOO_SMALL`, `OBJECT_TOO_LARGE` |
| `path_pattern` | OBJECT_METADATA | Consistency | `pattern` (RE2, ≤ 256) | `PATH_PATTERN_MISMATCH` |
| `required_label` | OBJECT_METADATA | Completeness | – | `MISSING_LABEL` |
| `split_assignment` | SPLIT | Consistency | `allowed` | `MISSING_SPLIT`, `SPLIT_NOT_ALLOWED` |
| `object_exists` | OBJECT_METADATA | Completeness | – | `OBJECT_MISSING`, `VERSION_MISSING`, `OBJECT_DELETED` |
| `lifecycle_state` | OBJECT_METADATA | Accessibility | `allowed` (default `AVAILABLE`) | `VERSION_NOT_ACCESSIBLE` |
| `size_consistency` | OBJECT_METADATA | Integrity | – | `SIZE_INCONSISTENT`, `CHECKSUM_INCONSISTENT` (snapshot vs. the version's record) |
| `checksum_present` | OBJECT_METADATA | Integrity | – | `CHECKSUM_MISSING` |
| `checksum_verified` | OBJECT_METADATA | Integrity | `require_declared` | `CHECKSUM_UNVERIFIED`, `CHECKSUM_VERIFICATION_FAILED`, `CHECKSUM_NOT_DECLARED` |
| `malware_scan` | OBJECT_METADATA | Security | `policy`: `clean` (a CLEAN verdict) or `scanned` | `MALWARE_DETECTED`, `SCAN_MISSING`, `NOT_SCANNED_CLEAN`, `SCAN_INCOMPLETE` |
| `content_checksum` | OBJECT_CONTENT | Integrity | `reverify` | `CHECKSUM_MISMATCH`, `SIZE_MISMATCH`, `CONTENT_MISSING` |
| `member_count` | DATASET | Completeness | `min` (≥ 1), `max` | `EMPTY_DATASET`, `TOO_FEW_MEMBERS`, `TOO_MANY_MEMBERS` |
| `total_bytes` | DATASET | Distribution | `min`, `max` | `TOTAL_BYTES_OUT_OF_RANGE` |
| `duplicate_references` | DATASET | Duplication | – | `DUPLICATE_REFERENCE` |
| `duplicate_content` | DATASET | Duplication | – | `DUPLICATE_CONTENT` (per group) |
| `split_leakage` | SPLIT | Consistency | – | `SPLIT_LEAKAGE` (per group) |
| `split_proportions` | SPLIT | Distribution | `proportions: {split: [min, max]}` | `SPLIT_PROPORTION` |
| `required_labels` | DATASET | Completeness | `labels` | `LABEL_MISSING` |
| `label_distribution` | DATASET | Distribution | `min_per_class`, `max_class_share` | `CLASS_TOO_SMALL`, `CLASS_IMBALANCE` |
| `file_type_distribution` | DATASET | Distribution | `expected`, `max_share` | `UNEXPECTED_FILE_TYPE`, `FILE_TYPE_DOMINANT` |

**Thresholds.** More `ERROR` findings than `max_errors` (default 0), or more `WARNING` findings than `max_warnings` when set, make the version `FAILED`. Otherwise any `ERROR` or `WARNING` finding makes it `READY_WITH_WARNINGS`, and none makes it `READY`. `INFO` never counts.

### Evidence, and what "verified" means

- **Integrity records.** Integrity rules read the platform's own records: the version's size and SHA-256, and its processing job's `verification_status`/`checksum_source` and `scan_result`.
- **Records are not bytes.** A catalog record is never taken as proof that the bytes are intact. `checksum_present` and `checksum_verified` say only what the records say. `content_checksum` establishes content integrity in two ways:
  - **Processing evidence.** Processing computed the SHA-256 of the same stored bytes and matched it (`verification_status = PASSED`, and the version's checksum equals the snapshot's). That evidence is used as-is and not recomputed (`reverify: false`, the default).
  - **Reading.** Otherwise, or with `reverify: true`, the validation streams the version from storage, hashes it and compares the hash and size with the snapshot's. A version published to a versioned bucket is read by the provider version ID (GCS: generation) recorded at publication, so bytes written over its key later are neither taken for it nor held against it. A recorded version that no longer exists is `CONTENT_MISSING`. Without a recorded version (an unversioned bucket) the version's own key is read: every version has a key of its own, so only a write to the bucket outside the platform changes it, and that shows as `CHECKSUM_MISMATCH` or `SIZE_MISMATCH`.
- **Unreadable members.** A version that cannot be read (not `AVAILABLE`, timed out, storage error) is counted as not examined, never as a pass.
- **Duplicates and leakage.** These use only checksums that processing verified against the stored bytes. Two members are duplicates when their verified SHA-256 is equal. Leakage is the same verified content in two different assigned splits; the same object cannot be in two splits, since membership is unique. File names and paths are never taken as proof of anything. Members without a verified checksum take no part, and the note says how many did (`compared N of M members with verified checksums`). No perceptual or near-duplicate similarity is attempted.

### Modes

| Mode | Member checks | Content check | Report says |
|---|---|---|---|
| `METADATA` (default) | all members | not run: `NOT_ASSESSED` | `coverage: NONE`, "metadata-only validation: no object content was examined" |
| `FULL` | all members | every member: from processing's verification, or by reading the bytes | `coverage: FULL`, how many were read |
| `SAMPLED` | all members | a deterministic sample of at most `sample_size` (default `DATASET_DEFAULT_SAMPLE_SIZE`, at most `DATASET_MAX_SAMPLE_SIZE`): every ⌈N/sample⌉-th member in canonical order | `coverage: SAMPLED`, "this is not a full integrity scan"; a passing content check is `PARTIAL`, never `PASSED` |

### Jobs

- **Starting a job.** `POST /api/v1/datasets/{id}/validations` (`{"version_id"?, "profile_id"?, "profile_version"?, "mode"?, "sample_size"?, "idempotency_key"?}`) writes the job and `dataset.validation_queued` in one transaction and answers `202` with `Location: /api/v1/validation-jobs/{id}`. The version defaults to the current one and the profile to the dataset's, then `builtin:general`.
- **States.** `QUEUED` → `RUNNING` → `COMPLETED` | `COMPLETED_WITH_WARNINGS` | `FAILED` | `CANCELLED`. `FAILED` with a report means the version failed validation; without one, `last_error` says why the job could not run.
- **The member pass.** The worker claims a job with a lease, then reads the version's members with their evidence from its checkpoint, `DATASET_VALIDATION_BATCH` at a time. One pass over a batch applies every member rule. Content checks run on at most `DATASET_CONTENT_CONCURRENCY` objects at once, each bounded by `DATASET_OBJECT_TIMEOUT`, and the lease is renewed while they run. The batch's findings, counters, per-rule counts and checkpoint commit together, only while the job is still leased to that worker and still at that checkpoint. A batch done twice (a crash, a second delivery) writes nothing new.
- **Finding IDs.** A finding's ID is a hash of the job, rule, code and member, so finding the same problem again is the same row.
- **Finishing.** Dataset-level rules run on aggregates the database computes, not on member lists: counts, splits, labels, types, extensions, verified-checksum groups and evidence counts. The report, the dataset-level findings, the job's end, the version's and dataset's outcome and the `VALIDATED_BY` edge commit in one transaction. Completing twice changes nothing.
- **Recovery, retries and dead letters.**
  - **Dead workers.** A dead worker's job is claimed again once its lease expires and continues from the checkpoint.
  - **Failed passes.** A failing pass is retried with backoff.
  - **Dead letters.** `attempts` counts claims without progress; past `DATASET_MAX_ATTEMPTS` the job ends `FAILED` ("attempts exhausted"). This is its dead letter, and the dataset leaves `VALIDATING`. Event-level dead letters are the existing consumer's: a message that cannot be handled goes to `dead_letters`.
  - **Duration cap.** A job still running after `DATASET_MAX_JOB_DURATION` fails.
- **Cancellation.** `POST /api/v1/validation-jobs/{id}/cancel` cancels a job no worker holds at once. A running one stops after its batch (`202` until then), and the dataset leaves `VALIDATING`.
- **Fairness and limits.**
  - **Claims.** Due jobs are ranked within their tenant, and a tenant gets only as many as fit under `DATASET_RUNNING_PER_TENANT` running ones. One tenant's backlog cannot hold every worker.
  - **Active jobs.** Unfinished validations per tenant are capped by `DATASET_MAX_ACTIVE_VALIDATIONS` (`429 too_many_jobs`).
  - **Stored findings.** Each rule stores at most `DATASET_MAX_FINDINGS_PER_RULE` findings per job; every failure is still counted in the report.

### Reports (schema version 1)

`GET /api/v1/validation-reports/{id}` returns the summary and `report`, the body:

| Field | Meaning |
|---|---|
| `schema_version`, `report_id`, `job_id`, `dataset_id`, `dataset_version_id`, `dataset_version`, `manifest_sha256` | what was validated |
| `profile {id, version, kind}`, `thresholds`, `mode`, `sample_size` | how |
| `status`, `started_at`, `completed_at` | `READY`, `READY_WITH_WARNINGS` or `FAILED` |
| `members {total, examined, passed, warnings, failed}` | per member: its worst failed rule's severity |
| `content {coverage, examined, read, not_examined, bytes_represented, bytes_read, note}` | what content integrity rests on |
| `checks {passed, failed, partial, not_assessed}`, `results[]` | per rule: `status` (`PASSED`, `FAILED`, `PARTIAL`, `NOT_ASSESSED`), `checked`, `failed`, `not_assessed`, `read`, `findings_stored`, `note`, `dimension` |
| `findings {by_severity, by_rule, stored}` | failure counts; `stored` may be lower than the counts |
| `evidence {verified_checksums, checksum_verification[], malware_scans[], version_states[]}` | the processing and lifecycle records, counted |
| `distributions {splits, labels, distinct_labels, unlabeled, content_types, extensions, sizes}` | at most `DATASET_DISTRIBUTION_TOP` values each |
| `readiness` | readiness assessments only (below) |
| `summary` | one human-readable paragraph |

Findings page at `GET /api/v1/validation-reports/{id}/findings?severity=&rule_id=&cursor=&limit=`. Each has a stable ID, rule ID and version, severity, code, object and version IDs, a safe message, expected and observed values when they are safe (sizes, types, checksums, splits, never metadata values or content), evidence source and remediation. Reports and findings never carry storage keys, buckets, URLs or object content.

## Readiness assessments

`POST /api/v1/datasets/{id}/readiness-assessments` runs a validation whose report also judges each dimension against the profile the caller chose. It refuses to run without one (the request's `profile_id`, or the dataset's). `GET /api/v1/datasets/{id}/readiness-reports` lists them (`?all=true` adds plain validations).

Dimensions: `COMPLETENESS`, `INTEGRITY`, `SECURITY`, `CONSISTENCY`, `DUPLICATION`, `DISTRIBUTION`, `REPRODUCIBILITY`, `GOVERNANCE`, `ACCESSIBILITY`. Each has a status, the checks it was judged by, a coverage (`FULL`, `SAMPLED`, `PARTIAL`, `NONE`) and notes:

- **`FAIL`.** An `ERROR` check of the dimension failed.
- **`WARN`.** A `WARNING` check failed, or a check passed only where it was assessed: a sample, or members it could not read. A sampled integrity check is at best `WARN`, labelled `SAMPLED`.
- **`NOT_ASSESSED`.** The profile has no check for the dimension, or none could judge this version (for example per-class checks when there are more distinct labels than a report lists).
- **`PASS`.** None of the above. `INFO` checks never change a status.
- **`REPRODUCIBILITY`.** Judged from the version itself: a finalized snapshot pins every member to an exact version, has a manifest hash, and the job recorded the profile version. It warns when some of those versions are no longer available.

There is no overall score. The report carries a disclaimer: the assessment is against the selected profile, from catalog records, processing evidence and the profile's checks. It does not judge semantic quality, label correctness, legal compliance, bias or suitability for a model.

## Lineage

Lineage lives in two tables (`lineage_nodes`, `lineage_edges`). A node stands for a record that lives elsewhere: `DATASET`, `DATASET_VERSION`, `MANIFEST_IMPORT`, `VALIDATION_REPORT`, `TRANSFORMATION`, `TRAINING_RUN`, `MODEL` (see Training runs), and `OBJECT_VERSION` (reserved: a version's object versions are its member rows, below). Its ID is a hash of the tenant, kind and record ID, so the same record in two tenants is two nodes and traversal never crosses tenants. Edges read "from RELATION to":

| Relation | From → to | Written when |
|---|---|---|
| `CONTAINS` | DATASET → DATASET_VERSION | a snapshot |
| `CREATED_FROM_SNAPSHOT` | DATASET → DATASET_VERSION (a source) | a derived dataset is created |
| `DERIVED_FROM` | DATASET_VERSION → DATASET_VERSION (a source) | a derived dataset's snapshot; details: input and output manifest hashes, transformation |
| `TRANSFORMED_BY` | DATASET / DATASET_VERSION → TRANSFORMATION | a derived dataset with a transformation; details: name, version, config SHA-256 |
| `IMPORTED_FROM` | DATASET → MANIFEST_IMPORT | an import that changed the draft ends |
| `VALIDATED_BY` | DATASET_VERSION → VALIDATION_REPORT | a validation completes |

- **Idempotent writes.** Edges are written in the transaction of the change they record, and are unique per (from, to, relation): writing one twice changes nothing.
- **Validity.** Only the pairs above are valid.
- **No cycles.** `CONTAINS`, `DERIVED_FROM` and `CREATED_FROM_SNAPSHOT` must stay acyclic. Before such an edge is written (under a per-tenant lock), a bounded recursive query checks that its target does not already lead back to its source (`409 lineage_cycle`). A chain longer than 64 edges is refused rather than checked partially.
- **Object versions.** A version's object versions are not edges: they are its member rows, which already index them (`ix_dataset_version_members_asset`), and page at `GET …/versions/{id}/members`.

**Traversal.**

- **Endpoints.** `GET /api/v1/datasets/{id}/lineage` and `GET /api/v1/dataset-versions/{id}/lineage` take `direction`, `depth`, `limit` and `cursor`. `upstream` (default) follows edges: what a node contains, came from or was checked by. `downstream` follows them backwards: what contains, imports or derives from it.
- **Bounds.** `depth` defaults to 3, at most `DATASET_LINEAGE_MAX_DEPTH`. The walk is a recursive CTE capped at `DATASET_LINEAGE_MAX_EDGES` edges and `DATASET_LINEAGE_QUERY_TIMEOUT`. Each edge is listed once at its shortest distance, in order of distance, with keyset cursors; `truncated` says the walk hit its edge cap.
- **Retention.** Lineage is kept when datasets are archived.

## API

All routes need a bearer token and an organization; the service scopes every call to it. Another tenant's records answer exactly like missing ones (`404`). Reads count against the `listing` rate rule. Creating datasets, snapshots, validations and assessments counts against `job_create`, manifests against `manifest`, profile writes against `admin`, and other changes against `upload_finish`.

| Method and path | Permission | Answer |
|---|---|---|
| `POST /api/v1/datasets` | datasets.write | `201` new, `200` same settings, `409 idempotency_conflict` |
| `GET /api/v1/datasets?status=&cursor=&limit=` | datasets.read | page by name |
| `GET /api/v1/datasets/{id}` | datasets.read | dataset, `ETag` = revision |
| `PATCH /api/v1/datasets/{id}` | datasets.write | `If-Match` or `revision`; `428`, `412` |
| `POST /api/v1/datasets/{id}/archive` | datasets.manage | `200`; `409 invalid_transition` while validating |
| `POST /api/v1/datasets/{id}/members` | datasets.write | per-input results |
| `GET /api/v1/datasets/{id}/members?cursor=&limit=` | datasets.read | page by member ID |
| `DELETE /api/v1/datasets/{id}/members/{memberID}` | datasets.write | dataset |
| `POST /api/v1/datasets/{id}/manifests` | datasets.write | inline `202`; staged `201` with `upload_url` |
| `POST /api/v1/datasets/{id}/manifests/{jobID}/submit` | datasets.write | `202` |
| `GET /api/v1/datasets/{id}/manifests/{jobID}` | datasets.read | import and progress |
| `GET /api/v1/datasets/{id}/manifests/{jobID}/errors?after=&limit=` | datasets.read | rejected lines |
| `POST /api/v1/datasets/{id}/manifests/{jobID}/cancel` | datasets.write | `200` cancelled, `202` stopping |
| `POST /api/v1/datasets/{id}/snapshots` | datasets.write | `201` new, `200` same key |
| `GET /api/v1/datasets/{id}/versions?before=&limit=` | datasets.read | newest first |
| `GET /api/v1/datasets/{id}/versions/{versionID}` | datasets.read | version |
| `GET /api/v1/datasets/{id}/versions/{versionID}/members?after=&limit=` | datasets.read | canonical order |
| `POST /api/v1/datasets/{id}/validations` | datasets.write | `202` + `Location` |
| `GET /api/v1/datasets/{id}/validations?cursor=&limit=` | datasets.read | newest first |
| `GET /api/v1/validation-jobs/{jobID}` | datasets.read | job and progress |
| `POST /api/v1/validation-jobs/{jobID}/cancel` | datasets.write | `200` / `202` |
| `GET /api/v1/validation-reports/{reportID}` | datasets.read | summary and body |
| `GET /api/v1/validation-reports/{reportID}/findings?severity=&rule_id=&cursor=&limit=` | datasets.read | page by finding ID |
| `POST /api/v1/datasets/{id}/readiness-assessments` | datasets.write | `202` |
| `GET /api/v1/datasets/{id}/readiness-reports?all=&cursor=&limit=` | datasets.read | summaries, newest first |
| `GET /api/v1/datasets/{id}/lineage`, `GET /api/v1/dataset-versions/{id}/lineage` | datasets.read | edges |
| `GET /api/v1/validation-profiles`, `GET /api/v1/validation-profiles/{id}?version=` | datasets.read | built-in and tenant profiles |
| `POST /api/v1/validation-profiles` | datasets.manage | `201` |

**Errors.** Errors are `{"error": "...", "code": "..."}`. The codes are:

- `400`: `invalid_request` for bad JSON, fields, limits (`limit` above `DATASET_MAX_PAGE_SIZE`), cursors, profiles or lineage.
- `404`: `not_found`.
- `409`: `dataset_archived`, `invalid_transition`, `no_members`, `unresolved_members`, `no_version`, `idempotency_conflict`, `invalid_state`, `manifest_not_uploaded`, `lineage_cycle`.
- `412`: `revision_conflict`.
- `413`: `too_large`.
- `415`: `unsupported_media_type`.
- `422`: `checksum_mismatch`, `snapshot_too_large`, `query_too_broad`.
- `428`: `revision_required`.
- `429`: `too_many_jobs` or `rate_limited`, with `Retry-After`.
- `403`: `forbidden` when a permission is missing. It is checked before the record is looked up, so it is the same for every ID.

**Audit.** These actions are recorded: `dataset.create`, `dataset.update`, `dataset.archive`, `dataset.members_add`, `dataset.members_remove`, `dataset.manifest_import`, `dataset.manifest_submit`, `dataset.manifest_cancel`, `dataset.snapshot`, `dataset.validate`, `dataset.readiness`, `dataset.validation_cancel` and `validation_profile.create`. Every refused attempt is recorded as denied. Reads are not audited, as elsewhere in the platform.

## Observability

- **Metrics.** The API publishes `datasets` and the worker publishes `datasets` at `DATASET_METRICS_ADDR/debug/vars`. Counters include:
  - **Creation and snapshots:** `dataset_created`, `dataset_snapshots_created`, `dataset_snapshot_members`, `dataset_snapshot_ms_total`.
  - **Imports:** `dataset_imports_created`, `dataset_import_lines`, `dataset_import_lines_rejected`, `dataset_imports_completed|failed|cancelled`, `dataset_import_retries`, `dataset_import_dead`.
  - **Validations:** `dataset_validations_created`, `dataset_validation_members`, `dataset_validation_bytes_read`, `dataset_validations_ready|ready_with_warnings|failed`, `dataset_validations_failed_to_run`, `dataset_validation_retries`, `dataset_validation_dead`, `dataset_validation_ms_total`.
  - **Findings:** `dataset_findings_error|warning|info`.
  - **Readiness:** `dataset_readiness_assessments`, `dataset_readiness_ms_total`.
  - **Lineage:** `dataset_lineage_queries`, `dataset_lineage_ms_total`.
  - **Recovery:** `dataset_stuck_released`.
- **Gauges.** `dataset_imports_state_*`, `dataset_validations_state_*`, `dataset_jobs_oldest_due_seconds` (queue age) and `dataset_jobs_stuck` (running jobs whose lease ran out).
- **Logs.** Logs are structured, with `tenant_id`, `dataset_id`, `import_id` / `job_id` and `version_id`; errors are sanitized. Manifests, metadata values, object content and URLs are never logged.
- **Health.** The worker answers `GET /health` (liveness) and `GET /ready` (database ping) on `DATASET_METRICS_ADDR`, like every worker's metrics listener; the API answers both on its HTTP port (`docs/security.md`).

## Configuration

The `DATASET_*` variables (`.env.example`, read by `config.LoadDatasets`) are validated at startup: a limit that would leave a request or job unbounded stops the process. For example, a page size above 10 000, a budget not shorter than the lease, or a lineage depth above 50. The worker also uses the `EVENT_*`/`SQS_*` settings of `docs/events.md` and the S3 settings. It reads published content from `PROMOTION_BUCKET`, or `S3_BUCKET` when that is unset.

## Running it locally

Commands are bash from `server/`, against the local Docker database. Never use the `DATABASE_URL` in `.env`.

```bash
# 1. Dependencies and migrations (PowerShell for the script)
docker compose up -d                       # PostgreSQL, MinIO, ClamAV, ElasticMQ
#   .\scripts\migrate.ps1                  # object_storage, through 000014
#   .\scripts\migrate.ps1 -Database object_storage_test -Create

# 2. API and workers (as in docs/security.md), plus the dataset worker
export DATABASE_URL="postgres://storage_app:$POSTGRES_PASSWORD_URLENCODED@127.0.0.1:5435/object_storage?sslmode=disable"
export SCANNER=clamav DATASET_POLL_INTERVAL=1s DATASET_METRICS_ADDR=127.0.0.1:8091
go build -o bin/ ./cmd/...
./bin/api & ./bin/processing-worker & ./bin/outbox-worker & ./bin/dataset-worker &
API=http://127.0.0.1:8080
# Sign in as in docs/security.md: $DEV is a developer's token, $OWNER the owner's.

# 3. Upload the sample files to pets/… and wait for processing to publish them
API=$API DEV=$DEV testdata/datasets/upload.sh
curl -s "$API/api/v1/catalog/lookup?path=pets/cat-1.txt" -H "Authorization: Bearer $DEV"

# 4. A dataset
curl -s $API/api/v1/datasets -H "Authorization: Bearer $DEV" -d '{"name":"pets","description":"sample","tags":["fixture"]}' > ds.json
D=$(python -c 'import json;print(json.load(open("ds.json"))["id"])')

# 5. Import the JSONL manifest (two lines are refused on purpose)
curl -s $API/api/v1/datasets/$D/manifests -H "Authorization: Bearer $DEV" \
  -H 'Content-Type: application/x-ndjson' -H 'Idempotency-Key: pets-1' --data-binary @testdata/datasets/manifest.jsonl
# → 202 {"id":"<IMP>","state":"QUEUED","source":"INLINE",...}
curl -s $API/api/v1/datasets/$D/manifests/<IMP> -H "Authorization: Bearer $DEV"
# → {"state":"COMPLETED","progress":{"lines_read":8,"members_added":6,"lines_rejected":2,...}}
curl -s $API/api/v1/datasets/$D/manifests/<IMP>/errors -H "Authorization: Bearer $DEV"
# → {"errors":[{"line":7,"code":"invalid_path",...},{"line":8,"code":"object_not_found",...}]}

# 6. A snapshot
curl -s -X POST $API/api/v1/datasets/$D/snapshots -H "Authorization: Bearer $DEV" -H 'Idempotency-Key: pets-v1'
# → 201 {"id":"<V1>","version":1,"status":"FINALIZED","member_count":6,"manifest_sha256":"…",...}

# 7. A profile (owner or administrator), then a FULL validation with it
curl -s $API/api/v1/validation-profiles -H "Authorization: Bearer $OWNER" --data-binary @testdata/datasets/profile.json   # → {"id":"<P>",...}
curl -s $API/api/v1/datasets/$D/validations -H "Authorization: Bearer $DEV" -d '{"profile_id":"<P>","mode":"FULL"}'
# → 202, Location: /api/v1/validation-jobs/<JOB>
curl -s $API/api/v1/validation-jobs/<JOB> -H "Authorization: Bearer $DEV"
# → {"state":"FAILED","report_id":"<R>","progress":{"processed":6,"bytes_read":…}}  (cat-1 leaks into test)

# 8. The report and its findings
curl -s $API/api/v1/validation-reports/<R> -H "Authorization: Bearer $DEV"
curl -s "$API/api/v1/validation-reports/<R>/findings?severity=ERROR&limit=20" -H "Authorization: Bearer $DEV"
# → {"findings":[{"code":"SPLIT_LEAKAGE","observed":"2 members share sha256 … splits: test,train",...}]}

# 9. A readiness assessment and its report
curl -s $API/api/v1/datasets/$D/readiness-assessments -H "Authorization: Bearer $DEV" \
  -d '{"profile_id":"builtin:supervised-classification","mode":"SAMPLED","sample_size":3}'
curl -s $API/api/v1/datasets/$D/readiness-reports -H "Authorization: Bearer $DEV"

# 10. A derived dataset and the lineage
curl -s $API/api/v1/datasets -H "Authorization: Bearer $DEV" -d '{"name":"pets-train","derived_from":{"version_ids":["<V1>"],"splits":["train"],"transformation":{"name":"dedupe","version":"1"}}}'
curl -s "$API/api/v1/dataset-versions/<V1>/lineage?direction=downstream" -H "Authorization: Bearer $DEV"
curl -s "$API/api/v1/datasets/$D/lineage?depth=2" -H "Authorization: Bearer $DEV"

# 11. Tests (unit, then integration against the local services)
go test ./internal/... ./cmd/... ./tests/unit/...
TEST_DATABASE_URL=postgres://storage_app:...@127.0.0.1:5435/object_storage_test?sslmode=disable \
TEST_S3_BUCKET=object-storage-it TEST_S3_CREATE_BUCKET=true TEST_CLAMAV_ADDRESS=127.0.0.1:3310 \
TEST_SQS_ENDPOINT=http://127.0.0.1:9324 go test ./tests/integration/ -run 'Dataset|Manifest|Snapshot|Validation|Readiness|Lineage|Evidence|Content' -v

# 12. The streaming benchmark: a 1,000,000-line generated manifest through the real import code
go test ./internal/service/dataset/ -run TestLargeManifestIsStreamedInBoundedMemory -v
```

`testdata/datasets` holds six small text files, two of them identical (`cat-1.txt` and `cat-1-copy.txt`), `manifest.jsonl` and `profile.json`. The manifest assigns the identical pair to `train` and `test`, leaves the copy without a licence, and has one unsafe and one unknown path. `TestDatasetFixtureWorkflow` runs the sample through the API, so this walkthrough stays true.

## Measured

On this laptop (PostgreSQL 16 in Docker):

- **Streaming.** A generated 1,000,000-line manifest (about 60 MB) streamed through the import code in 3.4 s, in 2,000 batches of 500, with a peak heap growth of 4.1 MiB. The manifest never exists whole in memory (`TestLargeManifestIsStreamedInBoundedMemory`).
- **Resumed import.** A 600-line staged manifest against catalog objects generated by `catalogbench` imported in batches of 100, through a crash and a storage failure, without a duplicate. The snapshot of the 600 members hashed to the canonical value (`TestStagedManifestImportResumesAfterACrash`).
- **Real stack.** API, processing, outbox and dataset workers, MinIO, ClamAV and ElasticMQ ran a smoke of 36 checks: upload to snapshot, FULL validation reading the bytes from MinIO, readiness, derived dataset, lineage, isolation, archive and audit verification.

Snapshot and validation latency for millions of members were not benchmarked. Both run in the database and in batches by design: the member copy is one `INSERT … SELECT`, the hash a streamed read, and validation reads and commits batches of `DATASET_VALIDATION_BATCH`. Measure them before relying on a number.

## Migration and rollback

`000014` is additive: new columns on `datasets` with defaults, and new tables, indexes and foreign keys. Existing rows become `DRAFT` datasets with no members. Apply it with `scripts/migrate.ps1`. To roll back (losing all Phase 10 data):

```sql
BEGIN;
DROP TABLE IF EXISTS lineage_edges, lineage_nodes, validation_findings, validation_reports, validation_jobs,
  validation_profiles, dataset_import_errors, dataset_manifest_imports, dataset_version_members, dataset_versions,
  dataset_members;
ALTER TABLE datasets DROP COLUMN IF EXISTS description, DROP COLUMN IF EXISTS tags, DROP COLUMN IF EXISTS metadata,
  DROP COLUMN IF EXISTS lifecycle_status, DROP COLUMN IF EXISTS revision, DROP COLUMN IF EXISTS members_revision,
  DROP COLUMN IF EXISTS member_count, DROP COLUMN IF EXISTS next_version, DROP COLUMN IF EXISTS current_version_id,
  DROP COLUMN IF EXISTS profile_id, DROP COLUMN IF EXISTS profile_version, DROP COLUMN IF EXISTS latest_report_id,
  DROP COLUMN IF EXISTS derivation, DROP COLUMN IF EXISTS updated_by, DROP COLUMN IF EXISTS archived_at;
DELETE FROM schema_migrations WHERE version = '000014_dataset_management.sql';
COMMIT;
```

Run older binaries only after the rollback. Phase 7 code reads `datasets` through the extended column list.

## Limitations

- **Membership is unique.** A dataset holds an object once; repeated occurrences of one file in a dataset are not modelled.
- **Snapshot size.** A snapshot is one transaction under the dataset's row lock: membership changes and other snapshots of that dataset wait, other datasets do not. It holds no member list in memory. Measured on one laptop (PostgreSQL 17, defaults): 10,000 members in 3.8 s, 100,000 in 11.7 s, 1,000,000 in 160.5 s. At the default cap, `DATASET_MAX_SNAPSHOT_MEMBERS=5000000`, that is roughly 13 minutes (extrapolated). The snapshot runs inside the HTTP request: a client or proxy that gives up first cancels it and nothing is created. Behind a 60-second proxy, keep the cap below about 350,000.
- **Import errors.** Members an import added before it failed or was cancelled stay in the draft; that import's lines can be found by the member's `source`. Error rows stop at `DATASET_MAX_IMPORT_ERRORS`; the count stays exact.
- **Content checks.** Members that cannot be read now (timeouts, storage errors) are reported as not examined, without per-member findings; the check is then partial, never passed.
- **What a snapshot guarantees.** It fixes which version of each object is a member, with that version's size and SHA-256. It guarantees the bytes only as far as storage does: in a versioned bucket the recorded version is read and cannot be overwritten in place, but it can be deleted. In an unversioned bucket, whoever can write to the bucket can replace a version's bytes. Validation then reports the mismatch; it cannot prevent it. Retention and deletion are unchanged: a member's version can still be deleted under its retention, and validation reports it missing.
- **Duplicates and leakage.** Only exact, verified SHA-256 equality counts; there is no near-duplicate or perceptual similarity.
- **Labels.** Per-class rules need the full label list. A version with more distinct labels than `DATASET_DISTRIBUTION_TOP` is not assessed by them.
- **Lineage.** Object-version lineage is the version's member rows, not graph edges, so a walk does not descend into individual objects. Edges are written by the platform; there is no public API to add arbitrary ones.
- **Large exports.** Reports are stored in PostgreSQL and findings are paged; there is no asynchronous export of very large reports to object storage.
- **Readiness.** Readiness is per profile and has no score. It says nothing about semantic quality, label correctness, licensing in law, bias, or training outcomes.

## Training runs

A training run records which dataset versions a model was trained on (migration `000027_training_runs.sql`). It joins lineage, so a version's downstream walk reaches the runs that read it and the models they produced, and a model's upstream walk reaches its training data.

- **Record:** `POST /api/v1/training-runs` (`datasets.write`):
  ```json
  {"name": "resnet50", "source": "MLFLOW", "external_id": "<mlflow run id>", "url": "https://mlflow.example.com/#/...",
   "params": {"lr": "0.01"}, "dataset_version_ids": ["<version>"], "model": {"name": "resnet50", "version": "3", "uri": "s3://..."}}
  ```
  `source` is `SDK` (default), `MLFLOW`, `WANDB` or `OTHER`. A run sent again with the same source and `external_id` answers the recorded one (200 instead of 201), so a retried script never records it twice. `status` starts `RUNNING` unless given.
- **Finish:** `PATCH /api/v1/training-runs/{id}` with `status` (`FINISHED` or `FAILED`), `metrics` (merged in) and a `model`. A finished run keeps its status.
- **Read:** `GET /api/v1/training-runs?dataset_version_id=` (newest first, `next_cursor`), `GET /api/v1/training-runs/{id}`, and `GET /api/v1/training-runs/{id}/lineage`.
- **Lineage:** a run is `TRAINED_ON` each version it read; a model is `PRODUCED_BY` its run. Both relations are acyclic.
- **Limits:** a name of 1 to 200 characters; 1 to 100 versions, each the organization's (404 otherwise); at most 64 params (keys up to 64 bytes, values up to 512) and 64 metrics (finite numbers); an `http` or `https` URL only. A model's name and version are unique in the organization (409 `model_exists`).
- **Audit:** `training_run.create` and `training_run.update`.

## Branches

A branch is a dataset of its own, with a parent: `POST /api/v1/datasets/{id}/branches {"name": "exp", "from_version_id": "…"}`. Without `from_version_id`, it branches from the dataset's latest version.

- **What it starts with:** that snapshot's members, pinned to the same object versions. Rows are copied; bytes are not.
- **Everything works on a branch:** members, manifest imports, snapshots, validation, readiness, diff and lineage. Its responses carry `branch` (parent, name, the version it was branched from, and its current merge base).
- **One level deep:** a branch is never branched again. Branch names follow dataset names. A branch's dataset name is `<parent>@<branch>`, so it is never an upload namespace. `GET /api/v1/datasets` leaves branches out; `GET /api/v1/datasets/{id}/branches` lists them.
- **New files for a branch:** they go into the parent's namespace, as new objects or new versions of existing paths, and the branch pins them. A bulk ingestion job therefore targets the parent; one aimed at a branch is refused. The parent's own snapshots still pin the versions they pinned (`osp ingest --branch` does all of this).
- **Diff across a family:** `GET /datasets/{id}/versions/{vid}/diff?against=` accepts a version of the dataset's parent, or of another branch of it.

### Merges

`POST /api/v1/datasets/{target}/merges {"source_dataset_id": "…", "source_version_id": "…", "strategy": "FAIL", "dry_run": false, "idempotency_key": "…"}` merges a version of a branch into its parent's draft, or a version of the parent into the branch's draft. Without `source_version_id`, it merges the source's latest version. The result lands in the target's draft; take a snapshot to keep it as a version.

**It is a three-way merge.** The common ancestor is the branch's `merge_base_version_id`: the version it was branched from, then the last version merged between the two in either direction. For every object whose membership the source changed since that base (added, removed, a new pinned version, label, split or metadata):

| The target, since the base | Result |
|---|---|
| unchanged | the source's change is applied (`added`, `updated`, `removed`) |
| changed the same way | nothing to do (`already_same`) |
| changed differently | a conflict |

**Strategies for conflicts:**

- **`FAIL`** (the default): changes nothing at all. Answers 409 `merge_conflict` with the conflicts: each object's path and its base, source and target membership. At most 1,000 are listed; all are counted.
- **`SOURCE`:** takes the source's side.
- **`TARGET`:** keeps the target's.

**Dry runs and records:** `dry_run` answers the same counts and conflicts and changes nothing. Applied merges are recorded (`GET /datasets/{id}/merges`) and audited (`dataset.merge`). A repeated `idempotency_key` returns the recorded merge instead of merging again.

**Concurrency:** a merge takes the target's row lock, like any change to a draft, so it never interleaves with another change or a snapshot.
