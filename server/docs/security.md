# Authentication, organizations, permissions, quotas and audit (local runbook)

Every API request is made by a signed-in user, in an organization they are an active member of, with the permissions of their role there. The development identity (`X-Dev-Tenant` / `X-Dev-User`) is gone; nothing authenticates without a token.

| Piece | Where | Role |
|---|---|---|
| Schema | `migrations/000012_identity_tenancy.sql` | users, sessions, tokens, organizations, roles, memberships, invitations, audit log, tenant quotas and usage, cross-tenant foreign keys |
| Model | `internal/model/identity.go`, `principal.go` | permissions, built-in roles, platform roles, principal checks |
| Configuration | `internal/platform/config/security.go` | `AUTH_*`, `RATE_LIMIT_*`, `QUOTA_*`, `AUDIT_RETENTION`; validated at startup |
| Passwords | `internal/platform/password` | Argon2id PHC hashes, bounded concurrency |
| Rate limits | `internal/platform/ratelimit` | in-process sliding-window counters |
| Repositories | `internal/infrastructure/database/postgres/identity_repository.go`, `tenancy_repository.go`, `audit_repository.go`, `quota_repository.go` | every security change with its audit event, in one transaction |
| Services | `internal/service/identity`, `audit`, `quota` | authentication, organizations, members, roles, invitations; the audit log; quota reservations and reconciliation |
| HTTP | `internal/server/middleware/identity.go`, `security.go`; `internal/handler/http/identity_handler.go`, `admin_handler.go` | bearer authentication, organization resolution, rate limits; the account, organization and admin APIs |
| Operator CLI | `cmd/identity-admin` | bootstrap, password recovery, account status, organizations, audit verification and purge, quota reconciliation |

## Authentication

Signing in (`POST /api/v1/auth/login`) checks an Argon2id hash and starts a **session** with two opaque tokens. Each token is `prefix + base64url(32 random bytes)`; the prefix says what it is for, so one kind is never accepted as another.

| Token | Prefix | Lifetime | Use |
|---|---|---|---|
| Access | `sat_` | `AUTH_ACCESS_TOKEN_TTL` (15m) | `Authorization: Bearer` on every request |
| Refresh | `srt_` | the session's, `AUTH_REFRESH_TOKEN_TTL` (30 days) | `POST /api/v1/auth/refresh`, once |
| Activation / reset | `sct_` | `AUTH_ACTIVATION_TTL` (72h) / `AUTH_RESET_TTL` (1h) | sets a password, once |
| Invitation | `sit_` | `AUTH_INVITATION_TTL` (7 days) or the organization's own | joins an organization, once |

Only SHA-256 hashes of tokens are stored. A database copy holds no usable token, and the tokens have 256 bits of entropy, so no salt or slow hash is needed.

**Token lifecycle:**
- **Validation.** The access token must be the session's *current* one, before its expiry, in a session neither expired nor revoked, of an `ACTIVE` account. This is checked on every request.
- **Refresh rotation.** Refreshing spends the refresh token and issues a new pair. The previous access token stops working immediately. The session's own expiry never moves.
- **Replay.** A spent refresh token presented again means two parties hold it. The whole session is revoked, including the legitimate new tokens, the revocation commits, and the refusal is audited (`refresh_reused`).
- **Ending a session.** Logout ends the current session; `DELETE /api/v1/auth/sessions/{id}` ends one of your others.
  - A password change ends every other session.
  - A reset or a deactivation ends all of them.

**Failure throttling:**
- Failed sign-ins count per email and address (`AUTH_LOGIN_ATTEMPTS`) and per address (`AUTH_LOGIN_IP_ATTEMPTS`), within `AUTH_LOGIN_WINDOW`. Over the limit, the answer is 429 with `Retry-After`, even for the right password.
- Every failure is the same 401 `invalid_credentials`. An unknown email costs the same Argon2id work as a wrong password. Only the right password reveals that an account is inactive.

**Passwords:**
- **Policy:** at least `AUTH_PASSWORD_MIN_LENGTH` (12) characters and at most 256 bytes, and not the email address.
- **Hashing:** Argon2id with `AUTH_ARGON2_*`; startup refuses anything below 19 MiB and 2 passes. Hashes are PHC strings, so parameters can be raised later; older hashes are upgraded at the next successful sign-in.
- **Concurrency:** hashing runs at most `NumCPU` at a time, so a burst of sign-ins queues instead of allocating `AUTH_ARGON2_MEMORY_KIB` each without bound.

**Account lifecycle:**
- **Provisioning.** A platform administrator creates an account (`POST /api/v1/admin/users`). It starts `PENDING` and comes with a one-time activation token, which its owner redeems with a password at `POST /api/v1/auth/activate`.
- **Self-provisioning.** Someone invited without an account creates one by accepting the invitation.
- **Self-registration** (`POST /api/v1/auth/register`, off unless `AUTH_SELF_REGISTRATION=true`). Anyone can create an `ACTIVE` account and an organization they own, and are signed in at once. There is no email delivery, so the address is **not verified**: whoever registers an address owns that account. Turn it on only where that is acceptable. It answers 403 `registration_closed` when off, 409 `conflict` for an address already registered, and is limited per source address by `RATE_LIMIT_REGISTER` (default 5/1h). If the organization cannot be created after the account was, the failure is logged and the account is left for an administrator to finish.
- **Recovery.** There is no email delivery. An administrator issues a reset token (`POST /api/v1/admin/users/{id}/password-reset`, or `identity-admin reset-password`) and passes it on; the owner redeems it at `POST /api/v1/auth/password/reset`.
- **Deactivation** (admin API or `identity-admin deactivate`) revokes every session in the same transaction, and stops download links the account issued.
- **Bootstrap.** `identity-admin bootstrap` creates the first platform administrator and works only while none exists. Without `-password-file` it generates a 24-character password, prints it once and stores only its hash. No default credential exists anywhere.

Nothing logs a password, a token, an `Authorization` header or a signed URL. Audit details whose keys look like credentials are dropped (`audit.Scrub`) as a last line of defence.

## Organizations and memberships

An organization is a tenant: its ID is the `tenant_id` of everything it owns. A request names its organization with `X-Organization-ID`. Without the header, a caller with exactly one active membership works in that organization; anyone else gets no tenant, and data routes answer 400 `organization_required`.

The header only names a choice. The tenant comes from the membership, checked on every request:
- **Not a member.** A caller who is not an active member gets 403 `not_a_member`, whether or not the organization exists.
- **Inactive organization.** A suspended or deactivated organization answers 403 `organization_inactive` to all its members.
- **Changes apply at once.** Role changes, suspensions and removals take effect on the member's next request.

| Lifecycle | Who | Effect |
|---|---|---|
| create | platform admin | organization plus its first owner |
| update name and settings | `org.manage` | `allowed_email_domains` restricts invitations; `invitation_ttl_hours` overrides their lifetime |
| deactivate | an owner (`org.ownership`) | every member refused; data, retention and holds untouched |
| suspend / reactivate | platform admin | as deactivation, reversible |
| delete | platform admin, after deactivation | refused while the organization has assets (including soft-deleted ones awaiting purge), uploads in flight, unfinished jobs, legal holds or pending deletions. The row stays as a `DELETED` tombstone; memberships are removed |

**Invitations:**
- An invitation is for an email address. The account accepting it must have that address, or be created for it.
- Invitations expire, can be revoked, and work once.
- Only one can be pending per address and organization.
- The audit record names the role, not the address.

## Roles and permissions

Checks ask for **permissions**, never role names; a role is a set of permissions.

| Permission | Owner | Admin | Developer | Auditor | Guards |
|---|:-:|:-:|:-:|:-:|---|
| `org.read` | ✓ | ✓ | ✓ | ✓ | read the organization |
| `org.manage` | ✓ | ✓ | | | rename, settings |
| `org.ownership` | ✓ | | | | grant or take ownership, deactivate |
| `members.read` | ✓ | ✓ | ✓ | ✓ | list members and roles |
| `members.manage` | ✓ | ✓ | | | invite, change roles, suspend, remove |
| `roles.manage` | ✓ | ✓ | | | custom roles |
| `datasets.read` / `datasets.write` | ✓ / ✓ | ✓ / ✓ | ✓ / ✓ | ✓ / | datasets; with write, members, manifest imports, snapshots, validations and readiness assessments (Phase 10, `docs/datasets.md`) |
| `datasets.manage` | ✓ | ✓ | | | archive datasets, write validation profiles |
| `ingestion.read` / `ingestion.write` | ✓ / ✓ | ✓ / ✓ | ✓ / ✓ | ✓ / | jobs, manifests, claims, uploads of entries |
| `ingestion.manage` | ✓ | ✓ | ✓ | | retry and cancel |
| `uploads.write` | ✓ | ✓ | ✓ | | single and multipart uploads |
| `storage.read` | ✓ | ✓ | ✓ | ✓ | the provider catalog and the organization's storage connections, never credentials (Phase 11, `docs/storage-connections.md`) |
| `storage.use` | ✓ | ✓ | ✓ | | send an upload to a connection other than the organization's default |
| `storage.manage` | ✓ | ✓ | | | create, change, test, rotate, set default, disable, enable and delete connections |
| `assets.read` | ✓ | ✓ | ✓ | ✓ | asset metadata, lifecycle state |
| `assets.download` | ✓ | ✓ | ✓ | | content, download URLs and token links |
| `assets.delete` | ✓ | ✓ | ✓ | | delete, restore, revoke links |
| `quarantine.read` | ✓ | ✓ | ✓ | ✓ | processing and quarantine records |
| `quarantine.release` | ✓ | ✓ | | | requeue a held asset (the scan still decides) |
| `retention.manage` | ✓ | ✓ | | | retention policies, legal holds |
| `quota.read` | ✓ | ✓ | ✓ | ✓ | the organization's quota and usage |
| `audit.read` | ✓ | ✓ | | ✓ | the organization's audit log and its verification |
| `catalog.write` | ✓ | ✓ | ✓ | | catalog objects: user metadata, renames, versioning, the current version (Phase 9, `docs/catalog.md`) |

Assets belong to the organization, not to the member who uploaded them. Upload sessions are the exception: only their creator continues or completes them.

**Custom roles** (`POST /api/v1/organization/roles`) take any permissions but `org.ownership`; unknown names are refused. A role in use by a member or a pending invitation cannot be deleted, and built-in roles cannot be changed.

**Escalation rules,** applied in the service layer to every grant and member change:
1. You grant only a role whose permissions you hold.
2. You change, suspend or remove only a member whose current role's permissions you hold, so an administrator cannot demote an owner.
3. The owner role is granted or taken only by an owner.
4. The **last active owner** cannot be demoted, suspended or removed. Only owners whose accounts are active count.
   - The check runs under the organization's row lock, so two owners demoting each other at once leave exactly one.
5. Anyone may leave, except the last owner.

**Platform roles** are separate from organization roles. `ADMIN` manages accounts, organizations, quotas and dead letters. `AUDITOR` reads every audit chain. A platform role grants no access to tenant data: platform administrators who need data join the organization like anyone else. The last active platform administrator cannot be demoted or deactivated.

## Tenant isolation

Isolation is enforced by the application on every path, and by the database where the data model allows. Object storage is not partitioned per tenant at the provider: one bucket, server-generated keys.

1. **Authentication.** Every route but sign-in, credential redemption, token download links and `/health` passes `middleware.Authenticate`. A nil resolver answers 401 to everything.
2. **Trusted tenant.** The principal's tenant comes only from the checked membership. Request bodies are decoded with `DisallowUnknownFields`, so a `tenant_id` field is a 400.
3. **Permissions in the service layer.** Every `...For(ctx, principal, ...)` service method checks its permission first (`audit.Require`) and records refusals. The HTTP layer only passes the principal through.
4. **Tenant-scoped lookups.** Every query takes `tenant_id = $principal` together with the resource ID. Another tenant's resource is the same `ErrNotFound`, with the same body, as a missing one. There is no existence oracle.
5. **Server-generated storage keys.** Keys are `quarantine/<tenant>/<upload>` for uploads and `<prefix>/<tenant>/<asset>` once promoted (on a tenant's own storage connection, its staging and ready prefixes). Delivery re-derives the promoted key and refuses an asset stored anywhere else. Clients never send a bucket, key or path; they may name a storage connection, which must be their organization's (`docs/storage-connections.md`).
6. **Signed URLs** are issued only for an `AVAILABLE` asset of the caller's organization, to a caller with `assets.download`, and are never logged. **Token links** (`mode: token`) re-check their issuer's current membership and permission on every use, so a removed member's links stop working.
7. **Composite foreign keys** (`(asset_id, tenant_id)` and similar; see the migration) make the database refuse rows that join two tenants:
   - an asset on another tenant's upload;
   - a processing job, quarantine record, download token or deletion of another tenant's asset;
   - an ingestion job or entry of another tenant's dataset or job.
8. **Workers.** Workers take IDs from queue messages and look them up with the message's tenant; a mismatched pair finds nothing. Messages carry IDs, never tokens or URLs. Workers that act for a tenant use `model.ServicePrincipal`, scoped to one tenant.

## Quotas

Limits are per organization. Defaults come from `QUOTA_*`; a platform administrator overrides them per organization (`PUT /api/v1/admin/organizations/{id}/quota`; zero or omitted fields fall back to the defaults).

| Limit | Default | Category |
|---|---|---|
| `max_storage_bytes` | 1 TiB | reserved + stored + pending-deletion bytes |
| `max_assets` | 10M | assets plus reserved uploads |
| `max_object_bytes` | 50 GiB | one object |
| `max_active_uploads` / `max_active_multipart` | 200 / 50 | sessions in flight |
| `max_pending_processing` | 5000 | processing jobs waiting |
| `max_active_jobs`, `max_pending_entries`, `max_manifest_entries`, `max_manifest_bytes` | 10, 10M, 5M, 2 GiB | ingestion (see `docs/ingestion.md`) |
| `max_requests_per_minute` | 6000 | every request of the organization |

**Reservation:** a new upload session stores its quota reservation in the same transaction.
- **Lock.** The tenant's `tenant_usage` row is locked first, so reservations of one tenant run one at a time and each sees the ones before it. Counts of active sessions and pending jobs are read under that lock, which makes concurrent requests race-safe.
- **Session state.** The reservation's state lives on the session row: `quota_state` `RESERVED` → `CONSUMED` or `RELEASED`. Every settlement is therefore a compare-and-set that cannot apply twice, and an idempotent retry of the same request reserves nothing more.
- **Released:** when the session fails (size mismatch, verification), is aborted, or expires, or when its asset is rejected (malware, validation).
- **Consumed:** when its asset becomes `AVAILABLE`, which turns the reserved bytes into stored bytes of the verified size.
- **Deletion:** soft deletion moves stored bytes to pending deletion; the purge removes them.
- **Lock order** is asset → session → usage throughout.

**Refusals:**
- **Response codes.** A refusal is a `quota_exceeded` problem body: `limit`, `max`, `used`, `requested`, `remaining`.
  - 413 for `max_object_bytes`.
  - 429 with `Retry-After` for limits that free up by themselves (active uploads, multipart, pending processing).
  - 403 for the rest.
- **Audit.** Each refusal is audited as `quota.exceeded`.

**Reconciliation** (`quota.Service.Reconcile`) runs:
- in the upload worker every `INGESTION_RECONCILE_INTERVAL`;
- with `upload-worker -reconcile`;
- with `identity-admin quota-reconcile`;
- through `POST /api/v1/admin/organizations/{id}/quota/reconcile`.

Each run does two things:
1. It settles reservations whose session or asset already ended (a crash between the two), and expires single-request sessions past their expiry.
2. It recounts the counters from sessions and assets.

Each repair is audited as `quota.reconcile`.

## Rate limits

| Rule | Key | Routes |
|---|---|---|
| sign-in throttling | email and address, address | `POST /api/v1/auth/login` |
| `refresh` | address | `POST /api/v1/auth/refresh` |
| `credentials` | address / user | activation, reset, invitation acceptance; password change per user |
| `upload_init` | organization | upload and multipart start |
| `upload_finish` | organization | parts, complete, abort, delete, restore; ingestion job finalize, retry and cancel |
| `ingestion_entry` | organization | bulk ingestion's per-entry calls: claims, upload URL refresh, part URLs and confirmation, heartbeat, complete. 3000/min by default, apart from `upload_finish` so a bulk job and interactive uploads never use up each other's budget |
| `manifest`, `job_create` | organization | manifests; jobs and datasets |
| `download_url` | organization / address | download URLs, content, token links |
| `listing` | organization | reads and listings |
| `membership` | organization | organization, member, invitation and role routes |
| `admin` | user / organization | `/api/v1/admin/...`; quarantine, retention and legal-hold routes |
| `max_requests_per_minute` | organization | every tenant route, from its quota |

Over a limit: 429 `rate_limited` with `Retry-After`. If the quota cannot be read, a tenant request is refused with 503 rather than let through unmetered.

**Limits of this design:**
- **Single process.** The counters live in each API process's memory. With N instances a client gets up to N times the limit, and a restart forgets every count. Production would put them in a shared store (Redis, or PostgreSQL for the slow-moving ones).
- **Client addresses.** These come from the connection unless `AUTH_TRUST_PROXY_HEADERS=true`, in which case the **last** `X-Forwarded-For` entry is used: the one the trusted proxy in front of the API wrote. Entries before it are what the client sent and are never used. Set it whenever the API is reached only through the console (which sends each caller's address as that last entry); without it, every user shares the console's address, and the per-address limits (sign-in, refresh at 30 a minute, credentials) apply to all of them together. Never set it on an API that clients can reach directly.

Quotas, unlike rate limits, are authoritative in PostgreSQL.

## Audit log

`audit_events` holds security events: sign-ins and failures, token refresh and replay, account, organization, member, role, invitation, quota, retention and quarantine changes, uploads, download-URL issuance, dead-letter operations, and every refusal.

| Column | Meaning |
|---|---|
| `chain`, `seq` | the organization's ID (or `_platform`), and a gap-free sequence within it |
| `tenant_id` | the organization, or NULL for platform events |
| `actor_type`, `actor_id` | `USER`, `SERVICE` or `ANONYMOUS`, and who |
| `action`, `resource_type`, `resource_id` | what, on what |
| `outcome`, `reason` | `SUCCESS`, `DENIED` or `FAILED`, and why |
| `correlation_id`, `source_ip` | the request's ID and client address |
| `details` | JSON, at most 16 KiB, credentials scrubbed. Changes record before and after |
| `created_at`, `prev_hash`, `hash` | `hash = SHA-256(prev_hash ‖ every column ‖ canonical details)` |

**Atomicity:**
- **Security state changes** write their event in the same transaction: a change and its record commit together or not at all.
- **Other events** are written right after the action: data operations such as uploads and download URLs, refusals, and failed sign-ins. A failure to record them is logged and counted (`security.audit_record_failures`) but does not fail the request.

**Append-only and hash-chained:**
- **Triggers.** They refuse `UPDATE`, `DELETE` and `TRUNCATE` on `audit_events`. The only exception is a `DELETE` in a transaction that sets `storage.audit_purge = on`, which is what the retention purge does.
- **Hash chain.** Each chain is hashed, with its head in `audit_chain_heads`. Verification recomputes every hash and reports the first gap, forgery or head mismatch:
  - `GET /api/v1/organization/audit/verify`;
  - `GET /api/v1/admin/audit/verify?chain=`;
  - `identity-admin audit-verify`.

**Retention:**
- **What is kept.** Events are kept for `AUDIT_RETENTION` (2 years by default; at least 24h).
- **How it is purged.** `identity-admin audit-purge` removes an expired prefix of each chain and records a checkpoint (`audit_checkpoints`) at the last purged event, so the rest still verifies. The purge itself is audited.

**What the hash chain does not prevent:**
- Someone with database owner rights can disable the triggers, or rewrite a whole chain and its head consistently.
- The chain detects edits, deletions and reordering of individual events against the stored head. It does not detect a complete, consistent rewrite.
- Tamper-proofing against the database owner needs the head anchored outside the database: periodic export of `audit_chain_heads` to write-once storage, or signing. That is not built.

Reading the log needs `audit.read` (your organization's chain) or a platform role (`GET /api/v1/admin/audit?chain=`). Reads are themselves audited.

## Asynchronous work

- **Queue messages** carry tenant and resource IDs, never credentials. Workers look everything up again with the message's tenant (see isolation, item 8).
- **Dead-letter administration over the API** needs the platform `ADMIN` role and a reason, and every attempt is audited, allowed or not. This covers list, inspect, retry, replay, resolve and discard. A replay redoes work in some tenant with the platform's authority, so tenant roles are never enough.
- **The `event-admin` CLI** runs with direct database access, which is a platform operator's by definition.

## Health and readiness

Two unauthenticated endpoints, which reveal nothing about the deployment:

- `GET /health` is liveness: the process answers. It checks no dependency, so a database outage never gets a healthy process restarted.
- `GET /ready` is readiness: PostgreSQL answers a ping within two seconds. Otherwise it answers 503 with `{"status":"not_ready","checks":{"database":"unavailable"}}`; the cause is only in the server's logs. Route traffic by this one.

The API serves both on its HTTP port. Every worker started with `METRICS_ADDR` serves both, with `/debug/vars`, on that listener. On SIGINT or SIGTERM the listener shuts down with the worker, letting a request in progress finish.

## Configuration

All settings are in `.env.example`. The API, the workers and `identity-admin` load them with `config.LoadSecurity` and **exit at startup on any invalid value**:
- `AUTH_MODE` other than `token`;
- TTLs out of range;
- Argon2id below the minimum;
- a malformed or missing rate rule;
- negative quotas;
- the Phase 7 names `INGESTION_TENANT_STORAGE_QUOTA`, `INGESTION_MAX_ACTIVE_JOBS` and `INGESTION_MAX_PENDING_ENTRIES`, now `QUOTA_*`.

There is no fallback to a weaker value.

## Running it locally

Commands are bash from `server/`. The scripts use the local Docker database: never the `DATABASE_URL` in `.env`, which may point elsewhere.

```bash
# 1. PostgreSQL, MinIO, ClamAV and ElasticMQ
docker compose up -d

# 2. Migrations (PowerShell), through 000012
#    .\scripts\migrate.ps1                       # object_storage
#    .\scripts\migrate.ps1 -Database object_storage_test -Create

# The API and workers, against the local database, with the S3, scanner and
# queue settings of docs/delivery.md and docs/events.md
export DATABASE_URL="postgres://storage_app:$POSTGRES_PASSWORD_URLENCODED@127.0.0.1:5435/object_storage?sslmode=disable"
go build -o bin/ ./cmd/...
./bin/api &  ./bin/upload-worker &  ./bin/processing-worker &  ./bin/outbox-worker &
API=http://127.0.0.1:8080
json() { python -c "import json,sys; print(json.load(sys.stdin)$1)"; }

# 3. The first platform administrator: the password is printed once
./bin/identity-admin bootstrap -email admin@example.test -name Admin
read -rs ADMIN_PW    # paste it; keeps it out of the shell history
ADMIN=$(curl -s $API/api/v1/auth/login -d "{\"email\":\"admin@example.test\",\"password\":\"$ADMIN_PW\"}" | json "['access_token']")

# 5. Provision accounts; each owner activates with the one-time token
curl -s $API/api/v1/admin/users -H "Authorization: Bearer $ADMIN" -d '{"email":"owner@example.test"}' > owner.json
curl -s $API/api/v1/auth/activate -d "{\"token\":\"$(json "['activation']['token']" < owner.json)\",\"password\":\"owner password 123\"}"
curl -s $API/api/v1/admin/users -H "Authorization: Bearer $ADMIN" -d '{"email":"dev@example.test"}' > dev.json
curl -s $API/api/v1/auth/activate -d "{\"token\":\"$(json "['activation']['token']" < dev.json)\",\"password\":\"dev password 1234\"}"

# 4. An organization with its owner (or: ./bin/identity-admin create-org -name Acme -slug acme -owner-email owner@example.test)
ORG=$(curl -s $API/api/v1/admin/organizations -H "Authorization: Bearer $ADMIN" \
  -d "{\"name\":\"Acme\",\"slug\":\"acme\",\"owner_user_id\":\"$(json "['user']['id']" < owner.json)\"}" | json "['id']")
OWNER=$(curl -s $API/api/v1/auth/login -d '{"email":"owner@example.test","password":"owner password 123"}' | json "['access_token']")
DEV=$(curl -s $API/api/v1/auth/login -d '{"email":"dev@example.test","password":"dev password 1234"}' | json "['access_token']")

# 5. Invite the developer; they accept signed in
INV=$(curl -s $API/api/v1/organization/invitations -H "Authorization: Bearer $OWNER" -d '{"email":"dev@example.test","role_id":"auditor"}' | json "['token']")
curl -s $API/api/v1/invitations/accept -H "Authorization: Bearer $DEV" -d "{\"token\":\"$INV\"}"

# 6. Assign a role
DEV_ID=$(json "['user']['id']" < dev.json)
curl -s -X PUT $API/api/v1/organization/members/$DEV_ID/role -H "Authorization: Bearer $OWNER" -d '{"role_id":"developer"}'

# 7. Authenticate (above) and use the organization; with several, add -H "X-Organization-ID: $ORG"
curl -s $API/api/v1/auth/me -H "Authorization: Bearer $DEV"

# 8. A dataset, and an upload
curl -s $API/api/v1/datasets -H "Authorization: Bearer $DEV" -d '{"name":"photos"}'
printf 'hello\n' > hello.txt
SUM=$(sha256sum hello.txt | cut -d' ' -f1)
curl -s $API/api/v1/uploads -H "Authorization: Bearer $DEV" \
  -d "{\"file_name\":\"hello.txt\",\"content_type\":\"text/plain\",\"size_bytes\":6,\"checksum_sha256\":\"$SUM\",\"idempotency_key\":\"hello-1\"}" > up.json
curl -s -X PUT -H 'Content-Type: text/plain' --data-binary @hello.txt "$(json "['upload_url']" < up.json)"
curl -s -X POST $API/api/v1/uploads/$(json "['upload_id']" < up.json)/complete -H "Authorization: Bearer $DEV" -d '{}'
# once processed: A=<asset ID>; curl -s -X POST $API/api/v1/assets/$A/download-url -H "Authorization: Bearer $DEV"

# 9. Cross-tenant denial: a member of another organization gets 404 for $A, 403 naming $ORG
curl -s -o /dev/null -w '%{http_code}\n' $API/api/v1/assets/$A -H "Authorization: Bearer $OTHER"
curl -s -o /dev/null -w '%{http_code}\n' $API/api/v1/assets/$A -H "Authorization: Bearer $OTHER" -H "X-Organization-ID: $ORG"

# 10. Quota exhaustion: one upload in flight, then a second one is 429 quota_exceeded
curl -s -X PUT $API/api/v1/admin/organizations/$ORG/quota -H "Authorization: Bearer $ADMIN" -d '{"max_active_uploads":1,"max_active_multipart":1}'
curl -s $API/api/v1/uploads -H "Authorization: Bearer $DEV" -d '{"file_name":"a.txt","content_type":"text/plain","size_bytes":3,"idempotency_key":"q1"}'
curl -s $API/api/v1/uploads -H "Authorization: Bearer $DEV" -d '{"file_name":"b.txt","content_type":"text/plain","size_bytes":3,"idempotency_key":"q2"}'
curl -s $API/api/v1/organization/quota -H "Authorization: Bearer $OWNER"

# 11. Audit records and their verification
curl -s "$API/api/v1/organization/audit?limit=20" -H "Authorization: Bearer $OWNER"
curl -s $API/api/v1/organization/audit/verify -H "Authorization: Bearer $OWNER"
./bin/identity-admin audit-verify

# 12. Revoke the membership, then the session
curl -s -X DELETE $API/api/v1/organization/members/$DEV_ID -H "Authorization: Bearer $OWNER"
curl -s -X POST $API/api/v1/auth/logout -H "Authorization: Bearer $DEV"
./bin/identity-admin deactivate -email dev@example.test

# 13. Tests (TEST_DATABASE_URL must name the local object_storage_test database)
go test ./internal/... ./tests/unit/ ./cmd/...
TEST_DATABASE_URL=postgres://storage_app:...@127.0.0.1:5435/object_storage_test?sslmode=disable \
TEST_S3_BUCKET=object-storage-it TEST_S3_CREATE_BUCKET=true TEST_CLAMAV_ADDRESS=127.0.0.1:3310 \
TEST_SQS_ENDPOINT=http://127.0.0.1:9324 go test ./tests/integration/
```

Other operator commands:
- `identity-admin reset-password -email`: local password recovery.
- `identity-admin reactivate -email`.
- `identity-admin audit-purge [-limit]`.
- `identity-admin quota-reconcile [-tenant]`.

## Tests

| What | Where |
|---|---|
| Argon2id, rate limiter, configuration (startup failures), permission model, audit scrubbing, middleware (401/403/400, 429 and `Retry-After`, fail-closed quota) | `internal/platform/password`, `ratelimit`, `config`; `internal/model`; `internal/service/audit`; `tests/unit/security_middleware_test.go` |
| Sign-in and invalid credentials, throttling, expired and revoked tokens, refresh rotation and replay, inactive and pending accounts, password reset, bootstrap | `tests/integration/identity_auth_test.go` |
| Membership validation, organization lifecycle, RBAC in services, escalation, last owner (including concurrent demotion), role and membership revocation, invitations | `tests/integration/tenancy_test.go` |
| Concurrent reservations (slots, bytes, multipart), release on abort, failure and rejection, consumption on publication, reconciliation | `tests/integration/quota_test.go` |
| Audit creation and access control, append-only triggers, tamper detection, retention purge, transactional rollback | `tests/integration/audit_test.go` |
| Over HTTP: sign-in flow, cross-tenant assets, uploads, datasets and manifests, signed URLs, unpublished and quarantined assets, token-link revocation, rate limits, audit access, dead-letter replay, secrets in logs and audit | `tests/integration/security_http_test.go` |
| Worker tenant isolation, cross-tenant foreign keys, credential-free messages | `tests/integration/worker_isolation_test.go` |

## Limitations

**Rate limits and tokens:**
- Rate limits and sign-in throttling are per process and reset on restart (see above).
- Access tokens are checked against the database on every request. There is no cache, so a revocation is immediate, at the cost of one indexed query per request.
- A request authorized at its start finishes with the permissions it began with; changes apply from the next request.

**Recovery:** there is no email delivery. Activation, reset and invitation tokens are handed over by whoever created them. A deployment would mail them.

**Audit:** the chain detects tampering only relative to the stored head (see the audit log section).

**Not built:** SSO/OIDC, MFA, API keys for machines (use a service account's session), and per-resource sharing.
