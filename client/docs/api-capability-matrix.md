# API capability matrix

Every console screen, the endpoint behind it, who may use it, how it pages, and whether it was run against the real backend. Paths are under `/api/v1`. "Verified" means a person or agent called the endpoint on the real local stack through the console's proxy and compared the response with the types; "Mocked only" means only msw tests cover it. Contract notes behind this table: [docs/api/](api/).

Permissions are the backend's strings. The console hides actions the user's role lacks and still handles a 403.

## Overview, organization, account

| Screen | Endpoint | Method | Permission | Pagination | Status |
| --- | --- | --- | --- | --- | --- |
| Overview: quota and usage | `/organization/quota` | GET | `quota.read` | none | Verified |
| Overview: recent jobs | `/ingestion/jobs?limit=50` | GET | `ingestion.read` | first page only | Verified |
| Overview: datasets needing attention | `/datasets?status=FAILED\|VALIDATING` | GET | `datasets.read` | first page only | Verified |
| Overview: recent activity | `/organization/audit?limit=8` | GET | `audit.read` | first page only | Verified |
| Overview: dead letters (platform admin) | `/admin/dead-letters?status=OPEN` | GET | platform ADMIN | none | Verified |
| Sign in / out | `/auth/login` (server side only), `/auth/logout` | POST | public / account | n/a | Verified |
| Create an account (new organization, signed in) | `/auth/register` (server side only; 403 `registration_closed` unless `AUTH_SELF_REGISTRATION=true`) | POST | public | n/a | Verified |
| Session restore, memberships | `/auth/me` | GET | account | none | Verified |
| Activate, reset password, accept invitation | `/auth/activate`, `/auth/password/reset`, `/invitations/accept` | POST | public | n/a | Mocked only |
| Account: sessions | `/auth/sessions`, `/auth/sessions/{id}` | GET, DELETE | account | fixed 100, newest first | Verified |
| Account: change password | `/auth/password` | POST | account | n/a | Verified |
| Organization details | `/organization` | GET, PATCH (full replace) | `org.read`, `org.manage` | n/a | Verified |
| Deactivate organization | `/organization/deactivate` | POST | `org.ownership` | n/a | Mocked only |
| Members | `/organization/members`, `/{user_id}/role`, `/suspend`, `/reactivate`, DELETE | GET, PUT, POST, DELETE | `members.read`, `members.manage` | `after` (last user id), no cursor in response | Verified |
| Invitations | `/organization/invitations` | GET, POST, DELETE | `members.manage` | `after` (last id) | Verified |
| Roles | `/organization/roles` | GET, POST, PUT, DELETE | `members.read`, `roles.manage` | none | Verified |
| Platform: organizations, users | `/admin/organizations*`, `/admin/users*` | GET, POST, PUT, DELETE | platform ADMIN | `after` | Verified (reads), Mocked only (writes) |

## Storage connections

| Screen | Endpoint | Method | Permission | Pagination | Status |
| --- | --- | --- | --- | --- | --- |
| Provider catalog and capabilities | `/storage-providers`, `/storage-providers/capabilities` | GET | `storage.read` | none | Verified |
| Connection list, detail | `/storage-connections`, `/{id}` | GET | `storage.read` | none | Verified |
| Create | `/storage-connections` | POST | `storage.manage` | n/a | Verified |
| Edit | `/storage-connections/{id}` | PATCH (revision in body) | `storage.manage` | n/a | Verified |
| Test, enable | `/{id}/test`, `/{id}/enable` | POST | `storage.manage` | n/a | Verified (a failed test is HTTP 200 with `ok:false`) |
| Rotate credentials | `/{id}/rotate-credentials` | POST | `storage.manage` | n/a | Verified |
| Set default, disable | `/{id}/set-default`, `/{id}/disable` | POST | `storage.manage` | n/a | Verified |
| Delete | `/{id}` | DELETE | `storage.manage` | n/a | Verified (refusal counts with real dependants: Mocked only) |

## Uploads and bulk ingestion

| Screen | Endpoint | Method | Permission | Pagination | Status |
| --- | --- | --- | --- | --- | --- |
| Single upload | `/uploads`, `/uploads/{id}/complete`, `/uploads/{id}` | POST, GET | `uploads.write` | n/a | Verified (engine against MinIO) |
| Multipart upload | `/uploads/multipart`, `/{id}/parts/urls`, `/{id}/parts/confirm`, `/{id}/complete`, `/{id}/abort` | POST | `uploads.write` | n/a | Verified (engine against MinIO) |
| Locate the new object | `/catalog/lookup`, `/catalog/objects/{id}/versions`, `/assets/{id}` | GET | `assets.read` | n/a | Verified |
| Ingestion jobs | `/ingestion/jobs`, `/{id}` | GET, POST | `ingestion.read`, `ingestion.write` | `after` string | Verified |
| Manifest (inline, staged) | `/{id}/manifest`, `/manifest/staged-url`, `/manifest/staged` | PUT, POST | `ingestion.write` | n/a | Verified (inline), Mocked only (staged) |
| Finalize, claims, per-entry upload, complete | `/{id}/finalize`, `/claims`, `/entries/{n}/upload-url`, `/parts/urls`, `/parts/confirm`, `/heartbeat`, `/complete` | POST | `ingestion.write` | n/a | Verified (not heartbeat, not 202) |
| Entries, failures | `/{id}/entries`, `/{id}/failures` | GET | `ingestion.read` | `after` position | Verified |
| Retry, cancel | `/{id}/retry`, `/{id}/cancel` | POST | `ingestion.manage` | n/a | Verified |

## Catalog and downloads

| Screen | Endpoint | Method | Permission | Pagination | Status |
| --- | --- | --- | --- | --- | --- |
| Object list, lookup | `/catalog/objects`, `/catalog/lookup` | GET | `assets.read` | opaque `next_cursor`, bound to the filters | Verified (every filter, cursor with a changed filter gives 400) |
| Object, versions | `/catalog/objects/{id}`, `/versions`, `/versions/{n\|current}` | GET | `assets.read` | `before` / `next_before` | Verified |
| Edit metadata, rename, versioning, promote | `/catalog/objects/{id}`, `/rename`, `/versioning`, `/versions/{n}/promote` | PATCH, POST, PUT | `catalog.write` | n/a | Verified (revision in body or `If-Match`; 412, 428, 409 paths) |
| Delete version, object | `/catalog/objects/{id}[/versions/{n}]` | DELETE | `assets.delete` | n/a | Verified (202, tombstones, current version falls back) |
| Download link | `/assets/{id}/download-url`, `/assets/{id}/download-links` | POST, DELETE | `assets.download`, `assets.delete` | n/a | Verified (storage and token modes, revoke) |
| Preview bytes | `/assets/{id}/content` | GET | `assets.download` | n/a | Verified |

## Datasets, validation, readiness, lineage

| Screen | Endpoint | Method | Permission | Pagination | Status |
| --- | --- | --- | --- | --- | --- |
| Dataset list, create, detail, edit | `/datasets`, `/datasets/{id}` | GET, POST, PATCH | `datasets.read`, `datasets.write` | `next_cursor` | Verified |
| Archive | `/datasets/{id}/archive` | POST | `datasets.manage` | n/a | Verified |
| Members | `/datasets/{id}/members` | GET, POST, DELETE | `datasets.read`, `datasets.write` | `next_cursor` | Verified |
| Snapshots, versions | `/datasets/{id}/snapshots`, `/versions`, `/versions/{vid}`, `/versions/{vid}/members` | POST, GET | `datasets.write`, `datasets.read` | `next_before`, `next_after` | Verified |
| Manifest import | `/datasets/{id}/manifests`, `/{job}`, `/errors`, `/submit`, `/cancel` | POST, GET | `datasets.write` | `next_after` line | Verified |
| Validation jobs | `/datasets/{id}/validations`, `/validation-jobs/{id}`, `/cancel` | POST, GET | `datasets.write` | `next_cursor` | Verified |
| Reports and findings | `/validation-reports/{id}`, `/findings` | GET | `datasets.read` | `next_cursor` | Verified (filters by severity and rule) |
| Readiness | `/datasets/{id}/readiness-assessments`, `/readiness-reports` | POST, GET | `datasets.write`, `datasets.read` | `next_cursor` | Verified |
| Profiles | `/validation-profiles`, `/{id}` | GET, POST | `datasets.read`, `datasets.manage` | none | Verified (new version under the same id) |
| Lineage | `/datasets/{id}/lineage`, `/dataset-versions/{vid}/lineage` | GET | `datasets.read` | `next_cursor`, depth 1 to 10 | Verified (both directions, by dataset and by version) |

## Lifecycle, operations, audit

| Screen | Endpoint | Method | Permission | Pagination | Status |
| --- | --- | --- | --- | --- | --- |
| Asset state and lifecycle | `/assets/{id}`, `/assets/{id}/lifecycle` | GET | `assets.read` | n/a | Verified |
| Soft delete, restore | `/assets/{id}`, `/assets/{id}/restore` | DELETE, POST | `assets.delete` | n/a | Verified |
| Quarantine record, release | `/assets/{id}/quarantine`, `/quarantine/release` | GET, POST | `quarantine.read`, `quarantine.release` | n/a | Verified (empty record), release on a real QUARANTINED asset: not run |
| Retention policy, legal holds | `/retention/policies`, `/legal-holds`, `/legal-holds/{id}` | PUT, POST, DELETE | `retention.manage` | n/a | Verified |
| Dead letters | `/admin/dead-letters*` | GET, POST | platform ADMIN | none (limit 100) | Verified (list, inspect, 400 and 404 paths), actions on a real entry: not run |
| Quota reconcile | `/admin/organizations/{id}/quota/reconcile` | POST | platform ADMIN | n/a | Verified |
| Audit log | `/organization/audit`, `/admin/audit` | GET | `audit.read`, platform ADMIN or AUDITOR | `before` / `next_before` | Verified |
| Audit chain verification | `/organization/audit/verify`, `/admin/audit/verify` | GET | same | n/a | Verified (intact chain only) |

## Not available from the API

The console shows these as "not available" rather than inventing them.

| Wanted | Why not | What the console does |
| --- | --- | --- |
| Total objects and bytes beyond the quota ledger | No aggregate endpoint | Uses `usage.asset_count` and `usage.stored_bytes` from the quota ledger and says so |
| Count of active ingestion jobs, manifest and request usage | No usage metric for them | "Not available from the API" |
| List of uploads, list of manifest imports | No list endpoints | Uploads: remembered in this browser (IndexedDB, no secrets). Imports: ids remembered per dataset in `localStorage` |
| Resume of a multipart upload, list of uploaded parts | No such endpoint | The console keeps its own part ETags; resume works only inside the ~30 minute server session |
| Asset id after an upload | Upload endpoints never return it | Found through the catalog by path, version number and checksum |
| Object to datasets reverse lookup | No endpoint | Stated on the object page |
| Read retention policies and legal holds | No GET | Write-only forms; hold ids shown once |
| Error report download for bulk ingestion | No endpoint | Pages `/failures` and builds a CSV in the browser |
| Worker, queue, reconciliation, compaction and cleanup status | No endpoints (logs and metrics only) | Listed as not exposed |
| Cross-dataset validation report list | No endpoint | Reports are reached from each dataset |
| Names for lineage nodes | Nodes are only a kind and an id | Dataset names and version numbers are looked up lazily (up to 24); other nodes show kind and short id |
| Per-dimension readiness remediation or an overall score | Not in the report (only per-finding remediation) | No score is shown |
| Filter findings by object or code, free-text catalog search, date-range audit filters | Not supported by the API | Not offered |
| Browser access to the API | No CORS | Same-origin proxy (see [security.md](security.md)) |

## Backend behaviour the console works around

- Errors come in three shapes, one of them plain text; `src/lib/api/errors.ts` normalises them.
- Refresh tokens are single use and a replay revokes the session; the proxy serialises refreshes.
- `PATCH /organization`, roles and quota are full replaces; the forms send every field.
- A failed connection test is HTTP 200 with `ok:false`; failed updates and rotations are 422 with a body that has no `code`.
- Ingestion grants carry no `upload_headers`; Azure needs `x-ms-blob-type: BlockBlob`, which the driver adds for Azure connections.
- Bucket CORS must expose `ETag` (MinIO does by default).
- A malformed `If-Match` on PATCH or rotate returns 500; the console sends the revision in the body.

## Response details that differ from the contract notes

Found while checking the screens against the real API.

- `readiness.dimensions[].checks` can be `null`, not `[]`.
- A tenant validation profile created without `kind` has `definition.kind` of `""`.
- A finding's `version_id` is the asset id, not the dataset version id.
- Distribution `splits` can have an empty `value`; file extensions come without a dot.
- Findings are ordered by a hash id, not by severity.
- Cancelling an already finished manifest import returns 409 `invalid_state`, not 200.
- Patching an archived dataset answers `dataset_archived` before any revision check.
- The catalog versions endpoint carries no `rejection_reason`; the uploader reads it from `GET /assets/{id}`.
- Retrying an ingestion job that is still VALIDATING returns 409.
- Revisions of a storage connection also change on test, enable, disable and rotate, not only on edits.
- `include=unpublished` lists objects with `versions: 0` and no current version.
- Creating a second invitation for an email that already has a PENDING one returns 201 and marks the earlier one REVOKED (the notes say 409).
