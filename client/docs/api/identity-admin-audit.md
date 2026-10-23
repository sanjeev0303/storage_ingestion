# Frontend contract: identity / org / admin / audit / lifecycle / governance

Source: `C:\Users\sanjeev.kumar\Music\storage\server` (Go, net/http ServeMux). All facts below were read from code. Base path `/api/v1`. All JSON is **snake_case**. Timestamps are Go `time.Time` -> RFC 3339 (UTC, nanosecond precision, e.g. `2026-10-22T10:00:00.123456Z`). `*time.Time` fields are `string | null` unless tagged `omitempty` (then absent when null).

## 0. Cross-cutting

### 0.1 Auth classes
| Class | Middleware | Header requirements |
|---|---|---|
| public | none | none |
| account | `Authenticate` | `Authorization: Bearer sat_...`. `X-Organization-ID` is still *resolved if sent* (see 0.2), tenant not required |
| tenant | `Authenticate` + `RequireTenant` + per-tenant request-rate quota | Bearer + org resolved (single membership auto-picked, or `X-Organization-ID`) |
| admin | same as account (`Authenticate` only) + rate limit `admin` per user | Bearer; platform role checked inside services |

No CORS handling exists anywhere in the server (grep for `Access-Control`/`cors` is empty): a browser frontend must go through a same-origin proxy/rewrite (or CORS must be added server-side). Server echoes `X-Request-ID` (accepts caller's if 1-64 chars `[A-Za-z0-9._-]`).

### 0.2 How X-Organization-ID is chosen/used
- Header `X-Organization-ID: <org uuid>` (trimmed). Only *names a choice*; membership is re-checked on every request.
- Header present: caller must be an ACTIVE member of that org; org must be ACTIVE. Else 403 `not_a_member` (same for non-existent org) or 403 `organization_inactive` (suspended/deactivated/deleted).
- Header absent: if the user has **exactly one** membership that is ACTIVE in an ACTIVE org, that org is used automatically. Otherwise (0 or >1) principal has no tenant: account/admin routes work; tenant routes answer 400 `organization_required`.
- Applies to account routes too: sending a wrong/foreign `X-Organization-ID` to `/auth/me`, `/auth/logout`, etc. returns 403 `not_a_member`. Frontend should omit the header on account/admin calls.
- Frontend rule: after `GET /auth/me`, filter `memberships` by `status==="ACTIVE"` (and org status not exposed there, see 3.2), store selected `organization_id`, send as header on all tenant calls.

### 0.3 Error body shapes
Two shapes in this area:
1. **Standard (middleware, identity/admin/governance handlers):** `{"error": string, "code": string}`. Quota refusals add `limit?: string, max?: number, used?: number, requested?: number, remaining?: number`.
2. **Lifecycle + asset-metadata handlers (`DELETE/POST restore/GET lifecycle`, `GET /assets/{id}`):** mostly **`{"error": string}` with NO `code`** (401, 404, 409, 500, and delivery errors). Only 403/400 org-required/rate-limit paths there use shape 1 (they call `writeSecurityError`).
3. **Malformed JSON body:** `decodeJSON` answers via `http.Error`: **plain text** `invalid JSON request\n` (400, `text/plain`) or `request must contain one JSON value\n`. Not JSON. Applies to every route with a body. Body limit 1 MiB (over -> same plain 400). **Unknown JSON fields are rejected** (400) - never send extra fields (e.g. no `tenant_id`).
4. Empty/`Content-Length: 0` body on a route that decodes JSON -> 400 plain (EOF). Routes without body (suspend, reactivate, deactivate, restore, delete, logout, etc.) ignore bodies.

Error code table (`code` -> HTTP). From `securityStatus` unless noted:
| code | status | meaning |
|---|---|---|
| `invalid_credentials` | 401 | bad email/password; also wrong current_password on change |
| `refresh_reused` | 401 | spent refresh token replayed; whole session revoked |
| `unauthenticated` | 401 | missing/invalid/expired access token, revoked session (also middleware). Has `WWW-Authenticate: Bearer realm="object-storage"` |
| `account_inactive` | 403 | account PENDING/DEACTIVATED (login with right password, refresh, authenticate) |
| `escalation` | 403 | grant/modify beyond own permissions |
| `forbidden` | 403 | missing permission / platform role (message `permission denied`; some carry custom text e.g. "you cannot change your own membership status") |
| `not_a_member` | 403 | (middleware) not an active member of named org |
| `organization_inactive` | 403 | org not ACTIVE |
| `organization_required` | 400 | tenant route with no resolvable org |
| `rate_limited` | 429 | has `Retry-After` seconds header |
| `invalid_token` | 400 | activation/reset/invitation token invalid, used, expired |
| `invalid_request` | 400 | bad input (service message is safe to display), weak password, missing reason, bad retention/hold input |
| `last_owner` | 409 | cannot demote/suspend/remove last active owner |
| `conflict` | 409 | email taken, already a member, slug exists, role name exists, role in use, org state wrong, org not empty, bootstrap done, dead letter not replayable/closed, invalid asset state |
| `not_found` | 404 | user/org/membership/invitation/role/dead letter/asset/upload/hold not found; `DELETE /auth/sessions/{id}` unknown session |
| `quota_exceeded` | 403 / 413 / 429 | 413 when `limit==="max_object_bytes"`; 429 (+`Retry-After: 5`) for `active_uploads`,`active_multipart`,`pending_processing`; else 403 |
| `quota_unavailable` | 503 | (middleware) per-tenant request-rate policy unreadable |
| `internal` | 500 | message fixed: `the request could not be completed` |
Other codes defined for other areas (storage): `path_exists`, `path_busy`(409), `revision_conflict`(412), `connection_not_active`, `connection_in_use`, `credentials_missing`(409), `endpoint_rejected`(400), `unsupported`(422), `byos_disabled`(501), `query_too_broad`(422).

401 for the lifecycle/asset handlers is `{"error":"authentication required"}` (no code) when raised inside the service; middleware 401 has code `unauthenticated`.

### 0.4 Pagination
There is **no cursor in responses** for member/invitation/user/org/dead-letter lists, and no total.
| Endpoint | Query | Default / max | Cursor semantics |
|---|---|---|---|
| members | `after` (=last `user_id`), `limit` | 100 / 500 (out of range -> 100) | ascending by `user_id`; `user_id > after` |
| invitations | `after` (=last invitation `id`), `limit` | 100 / 500 | newest first (`created_at DESC, id DESC`) keyed by that row |
| admin users | `after` (=last `id`), `limit` | 100 / 500 | ascending `id` |
| admin organizations | `after`, `limit` | 100 / 500 | ascending `id` |
| dead-letters | `status`, `limit` | 100 / 500; **no `after`** | newest first; cannot page past `limit` |
| audit (both) | `before` (seq, int64), `limit` | 100 / **1000** | newest first (`seq DESC`); response `next_before` |
| sessions | none | fixed 100, newest first | - |
Client logic: "has more" = `items.length === limit` (request explicit `limit`; server default 100). `limit<=0` or non-numeric -> default; over max -> **default 100** (not clamped to max).
Audit: `next_before` is set (=`seq` of the last event) only when `events.length === limit` (effective limit); absent/0 otherwise. Pass it as `before`.

### 0.5 Rate limits (defaults, per op, sliding window; configurable)
tenant identity routes: op `membership` 120/1m per tenant; admin routes: op `admin` 120/1m per **user**; governance routes: op `admin` per tenant; asset GET + lifecycle GET: `listing` 600/1m; asset DELETE/restore: `upload_finish` 600/1m; refresh 30/1m per IP; credential redemption (activate/reset/accept/change-password) 10/15m per IP (change password: per user); login: 5 failures/15m per email+IP, 50/15m per IP. Excess -> 429 `rate_limited` + `Retry-After`. Also org request-rate quota (`max_requests_per_minute` default 6000) on all tenant routes.

## 1. Tokens / sessions / login

### 1.1 Login
`POST /api/v1/auth/login` (public). Headers: none required. Response has `Cache-Control: no-store`.
Request: `{ "email": string, "password": string }` (no explicit length validation; email trimmed, matched case-insensitively; empty/unknown -> 401 `invalid_credentials`).
200 response `TokensResponse`:
```
{
  token_type: "Bearer",
  access_token: string,        // "sat_" + 43 chars base64url, opaque
  access_expires_at: string,   // default now+15m (AUTH_ACCESS_TOKEN_TTL, allowed 1m..24h)
  refresh_token: string,       // "srt_" + 43 chars
  refresh_expires_at: string,  // session expiry, default now+720h = 30d (AUTH_REFRESH_TOKEN_TTL, max 90d). FIXED: never extended by refresh
  session_id: string,
  user: UserResponse
}
```
No expires_in seconds; compute from `*_expires_at`. Login response does **not** include memberships/permissions: call `/auth/me` next. Errors: 401 `invalid_credentials` (same for unknown email), 403 `account_inactive` (only after correct password; status PENDING/DEACTIVATED), 429 `rate_limited`.

### 1.2 Refresh
`POST /api/v1/auth/refresh` (public). Body `{ "refresh_token": string }`. 200: same `TokensResponse` (new pair, `refresh_expires_at` unchanged = session's). `no-store`. The old access token dies immediately; the old refresh token is single-use. Re-presenting a spent refresh token -> 401 `refresh_reused` and the **entire session (including newly issued tokens) is revoked** -> user must log in again. Malformed/unknown/expired/revoked -> 401 `unauthenticated`. Account inactive -> 403 `account_inactive`. Frontend must serialize refreshes (single-flight; concurrent refresh with same token triggers the replay kill-switch).
Access token validity is also tied to session being the "current access token" of that session.

### 1.3 Account routes
| Route | Class | Body | Success | Notes |
|---|---|---|---|---|
| `POST /auth/logout` | account | none | 204 | ends current session |
| `GET /auth/me` | account | - | 200 `MeResponse` | below |
| `GET /auth/sessions` | account | - | 200 `{ sessions: SessionResponse[] }` | live sessions of caller, max 100, newest first. No "current" flag: compare `id` with login `session_id` |
| `DELETE /auth/sessions/{id}` | account | - | 204 | 404 `{"error":"session not found","code":"not_found"}` if not caller's live session |
| `POST /auth/password` | account | `{current_password, new_password}` | 204 | ends all *other* sessions. 401 `invalid_credentials` if current wrong; 400 `invalid_request` weak (min 12 chars [AUTH_PASSWORD_MIN_LENGTH, 8..128], max 256 **bytes**, must not equal email, must differ from current) |
| `POST /auth/activate` | public | `{token, password}` | 200 `UserResponse` | token is `sct_...` from admin create-user (72h). Does NOT sign in; follow with login. |
| `POST /auth/password/reset` | public | `{token, password}` | 200 `UserResponse` | token `sct_...` from admin (1h). Ends all sessions. 400 `invalid_token` / `invalid_request` |
| `POST /invitations/accept` | public (optional Bearer) | `{token, password?, display_name?}` | 201 `MembershipResponse` (no org name/slug/email) | see below |

Invitation accept: token `sit_...`. If request has `Authorization: Bearer` the caller joins with their account (account email must equal invited address; password/display_name ignored). Without Bearer: `password` required (policy as above), `display_name` optional; creates the account. Neither signs in; new users must then log in. A bad Bearer -> 401 `unauthenticated`. Missing password w/o Bearer -> 400 `invalid_request` ("sign in, or give a password for a new account"). Bad/used/expired token -> 400 `invalid_token`. Already member -> 409 `conflict`.

### 1.4 `/auth/me` (MeResponse)
```
{ user: UserResponse, memberships: MembershipResponse[] }   // memberships never null, [] when none
```
- Lists **all** memberships of the user, any membership status (ACTIVE and SUSPENDED) and in any org status, ordered by org name then id. Each carries `organization_id, organization_name, organization_slug, user_id, role_id, status, created_at` (`email`,`display_name` omitted/empty here).
- It does **NOT** list permissions, does **NOT** carry organization status, and does not say which org is "current". Permissions of a role are only obtainable via `GET /organization/roles` (needs `members.read`; every built-in role has it, a custom role may not). There is no "my permissions" endpoint.
- Platform role is in `user.platform_role`.

### 1.5 Shared types
```ts
UserResponse { id: string; email: string; display_name?: string; status: "PENDING"|"ACTIVE"|"DEACTIVATED";
  platform_role?: "ADMIN"|"AUDITOR"; last_login_at?: string; created_at: string }
MembershipResponse { organization_id: string; organization_name?: string; organization_slug?: string;
  user_id: string; email?: string; display_name?: string; role_id: string; status: "ACTIVE"|"SUSPENDED"; created_at: string }
SessionResponse { id; source_ip?: string; user_agent?: string; created_at; last_used_at; expires_at; access_expires_at }  // all times ISO strings
OrganizationResponse { id; name; slug; status: "ACTIVE"|"SUSPENDED"|"DEACTIVATED"|"DELETED";
  allowed_email_domains?: string[]; invitation_ttl_hours?: number; created_at; deactivated_at?: string }
InvitationResponse { id; email; role_id; status: "PENDING"|"ACCEPTED"|"REVOKED"; created_by: string; created_at; expires_at; accepted_at?: string; token?: string /* only on create */ }
RoleResponse { id; name; description?: string; permissions: string[]; built_in: boolean }
CredentialTokenResponse { user_id; token; expires_at }
```
(`display_name`, `platform_role`, `last_login_at`, `allowed_email_domains`, `invitation_ttl_hours`, `deactivated_at`, `accepted_at`, `token`, `description`, `source_ip`, `user_agent` are omitted when empty/null.)

## 2. Roles and permissions

### 2.1 Permission strings (exact, tenant scope)
`org.read, org.manage, members.read, members.manage, roles.manage, datasets.read, datasets.write, ingestion.read, ingestion.write, ingestion.manage, uploads.write, assets.read, assets.download, assets.delete, quarantine.read, quarantine.release, retention.manage, quota.read, audit.read, catalog.write, datasets.manage, org.ownership, storage.read, storage.use, storage.manage`  (25).

### 2.2 Built-in roles (ids are the lowercase strings; `built_in:true`; names shown)
| id | name | description | permissions |
|---|---|---|---|
| `owner` | Owner | Everything, including granting ownership and deactivating the organization | all 25 (includes `org.ownership`) |
| `admin` | Administrator | Manages members, roles, settings and all data; cannot grant ownership | all except `org.ownership` |
| `developer` | Developer | Uploads, ingests, reads and deletes data | org.read, members.read, datasets.read, datasets.write, ingestion.read, ingestion.write, ingestion.manage, uploads.write, assets.read, assets.download, assets.delete, quarantine.read, quota.read, catalog.write, storage.read, storage.use |
| `auditor` | Auditor | Reads metadata, usage and the audit log; cannot download or change anything | org.read, members.read, datasets.read, ingestion.read, assets.read, quarantine.read, quota.read, audit.read, storage.read |
Custom role ids: `"role_" + id`. `permissions` array order follows the canonical list in 2.1. Roles are not in `/auth/me`.

Platform roles (separate, on user): `ADMIN` (everything under `/admin/*`), `AUDITOR` (only `GET /admin/audit`, `GET /admin/audit/verify`). Platform roles give no tenant data access.

### 2.3 Escalation rules (server enforces; UI should pre-filter)
Grant only roles whose permissions you hold; manage only members whose role perms you hold; owner role only by an owner; last active owner protected; cannot suspend/reactivate self; anyone may leave (DELETE own membership; no `members.manage` needed) except last owner. Custom roles: name 1-64 chars (trimmed), description <=500, >=1 permission, never `org.ownership`, unknown names -> 400, built-ins immutable (400 `invalid_request`).

## 3. Organization (tenant routes, header `X-Organization-ID` as per 0.2)
Permission is per row. All go through op rate limit `membership`.

| Method + path | Permission | Request body | Success |
|---|---|---|---|
| `GET /organization` | `org.read` | - | 200 `OrganizationResponse` |
| `PATCH /organization` | `org.manage` | `{ name: string (required, trimmed, 1-200), allowed_email_domains?: string[] (each lowercased, needs a ".", no "@" or space), invitation_ttl_hours?: int (0-720; 0=server default 168h) }` | 200 `OrganizationResponse`. **Full replace of settings**: omitting `allowed_email_domains`/`invitation_ttl_hours` clears them. 400 `invalid_request`. Org must be ACTIVE or SUSPENDED else 409 `conflict` |
| `POST /organization/deactivate` | `org.ownership` (owner only) | none | 200 `OrganizationResponse` (status DEACTIVATED). Afterwards all member requests -> 403 `organization_inactive`; only platform admin can reactivate |
| `GET /organization/members?after&limit` | `members.read` | - | 200 `{ members: MembershipResponse[] }` (includes `email`,`display_name`; no org name) |
| `PUT /organization/members/{user_id}/role` | `members.manage` | `{ role_id: string }` | 200 `MembershipResponse`. 400 `invalid_request` unknown role; 403 `escalation`/`forbidden`; 409 `last_owner`; 404 |
| `POST /organization/members/{user_id}/suspend` | `members.manage` | none | 200 `MembershipResponse` (status SUSPENDED); cannot target self (403 `forbidden`) |
| `POST /organization/members/{user_id}/reactivate` | `members.manage` | none | 200 `MembershipResponse` |
| `DELETE /organization/members/{user_id}` | `members.manage` (not needed for self) | - | 204 |
| `GET /organization/invitations?after&limit` | `members.manage` | - | 200 `{ invitations: InvitationResponse[] }` (never includes `token`; includes all statuses) |
| `POST /organization/invitations` | `members.manage` | `{ email: string (<=254, one "@", domain has ".", no spaces; must match org's allowed domains if set), role_id: string }` | 201 `InvitationResponse` **with `token`** (`sit_...`, shown once; `Cache-Control: no-store`). No email is sent; UI must show/copy token (or build an accept link). Only one PENDING per email+org (409 `conflict`). Default expiry 168h or org `invitation_ttl_hours`. 400 `invalid_request` bad email/role/domain |
| `DELETE /organization/invitations/{id}` | `members.manage` | - | 204 |
| `GET /organization/roles` | `members.read` | - | 200 `{ roles: RoleResponse[] }` (4 built-ins first, then custom) |
| `POST /organization/roles` | `roles.manage` | `{ name: string; description?: string; permissions: string[] }` | 201 `RoleResponse`. 409 `conflict` duplicate name |
| `PUT /organization/roles/{id}` | `roles.manage` | same as create (full replace) | 200 `RoleResponse` |
| `DELETE /organization/roles/{id}` | `roles.manage` | - | 204. 409 `conflict` if used by a member or pending invitation |
| `GET /organization/quota` | `quota.read` | - | 200 `QuotaResponse` |
| `GET /organization/audit` | `audit.read` | query below | 200 `AuditPageResponse` |
| `GET /organization/audit/verify` | `audit.read` (org chain) | - | 200 `AuditVerificationResponse` |

Note `GET /organization/audit` itself writes an `audit.read` audit event. Route-level 403 body: `{"error":"permission denied","code":"forbidden"}`, and the denial is audited.

## 4. Quota
`GET /organization/quota` (perm `quota.read`) and `GET /admin/organizations/{id}/quota` (platform ADMIN) return:
```ts
QuotaResponse {
  limits: {   // all numbers (int64)
    max_storage_bytes, max_assets, max_object_bytes, max_active_uploads, max_active_multipart, max_active_jobs,
    max_pending_processing, max_pending_entries, max_manifest_entries, max_manifest_bytes, max_requests_per_minute }
  usage: {
    reserved_bytes, reserved_uploads, stored_bytes, asset_count, pending_deletion_bytes, quarantined_bytes,
    incomplete_multipart_bytes, active_uploads, active_multipart, pending_processing,
    remaining_storage_bytes /* = max(max_storage_bytes - reserved_bytes - stored_bytes - pending_deletion_bytes, 0) */ }
}
```
Fixed key sets (maps, but keys are stable). Defaults: max_storage_bytes 1099511627776 (1 TiB), max_assets 10000000, max_object_bytes 53687091200 (50 GiB), max_active_uploads 200, max_active_multipart 50, max_active_jobs 10, max_pending_processing 5000, max_pending_entries 10000000, max_manifest_entries 5000000, max_manifest_bytes 2147483648, max_requests_per_minute 6000 (env-configurable). No usage metric exists for jobs, manifest entries, pending entries or requests, even though limits exist. Limits shown are effective (override else default).

## 5. Audit
Query (both org and admin lists): `before` (int seq; non-numeric -> 0 = from newest), `limit` (see 0.4), `action` (exact name, or prefix when ending in `.` e.g. `member.`; the value is silently ignored (treated as no filter) if it contains anything other than `a-z`, `.`, `_` - so digits/uppercase/hyphens disable the filter), `outcome` (case-insensitive input, uppercased: `SUCCESS|DENIED|FAILED`; any other value yields empty result), `actor` (exact `actor_id`; user id, or `anonymous`, `system`, `bootstrap`, `operator-cli`). No date-range, resource or text filters. Order: newest first by `seq`.
```ts
AuditPageResponse { events: AuditEventResponse[]; next_before?: number }
AuditEventResponse {
  id: string; seq: number /* per-chain, int64 */; tenant_id?: string /* absent for platform chain */;
  actor_type: "USER"|"SERVICE"|"ANONYMOUS"; actor_id: string; action: string; resource_type: string; resource_id?: string;
  outcome: "SUCCESS"|"DENIED"|"FAILED"; reason?: string; correlation_id?: string; source_ip?: string;
  details?: Record<string, unknown>; created_at: string; hash: string /* hex */ }
```
Action names seen in code (dot-separated, lowercase/underscore): `auth.login, auth.logout, auth.refresh, auth.session_revoke, auth.password_change, auth.activate, auth.password_reset, user.create, user.deactivate, user.reactivate, user.platform_role, user.activation_issue, user.password_reset_issue, user.list, platform.bootstrap, organization.create|update|deactivate|delete|active|suspended|read|list, member.list|role_change|suspend|reactivate|remove, invitation.create|list|revoke|accept, role.create|update|delete|list, quota.update|read|exceeded|reconcile, audit.read, audit.verify, audit.purge, asset.delete, asset.restore, asset.access, retention.policy_set|hold_place|hold_release, quarantine.read|release, dead_letter.list|inspect|retry|replay|resolve|discard` (non-exhaustive: other services add upload/download/storage actions). Details never contain keys containing `password,secret,token,authorization,cookie,url,signature,credential`.

Verification: `GET /organization/audit/verify` and `GET /admin/audit/verify[?chain=]`
```ts
AuditVerificationResponse { chain: string; checked: number; first_seq: number; last_seq: number; intact: boolean; broken_at?: number; problem?: string }
```
(`intact === (broken_at==0)`). Org verify: chain = org id, needs `audit.read`.

Admin audit: `GET /admin/audit?chain=<org id | "_platform">` default `_platform`; platform ADMIN or AUDITOR. Same filters/response. `GET /admin/audit/verify?chain=` same roles. Org-chain events about a tenant include `tenant_id`.

## 6. Admin (platform) routes
Class: account + rate limit; permission = platform role. 403 `forbidden` for everyone else (including normal org owners). `ADMIN` required unless noted.

| Method + path | Role | Body | Success |
|---|---|---|---|
| `GET /admin/users?after&limit` | ADMIN | - | 200 `{ users: UserResponse[] }` |
| `POST /admin/users` | ADMIN | `{ email: string; display_name?: string; platform_role?: ""|"ADMIN"|"AUDITOR" (case-insensitive, uppercased) }` | 201 `{ user: UserResponse /*status PENDING*/, activation: CredentialTokenResponse }` (`no-store`; token valid 72h, shown once). 409 `conflict` email taken; 400 bad role/email |
| `POST /admin/users/{id}/deactivate` | ADMIN | none | 200 `UserResponse`; revokes all sessions; cannot self-deactivate (403 `forbidden`); cannot remove last active platform admin |
| `POST /admin/users/{id}/reactivate` | ADMIN | none | 200 `UserResponse` (becomes PENDING if user never set a password) |
| `PUT /admin/users/{id}/platform-role` | ADMIN | `{ platform_role: ""|"ADMIN"|"AUDITOR" }` | 204 (empty string removes role). Last-admin protection -> 403 `forbidden` |
| `POST /admin/users/{id}/password-reset` | ADMIN | none | 201 `CredentialTokenResponse` (`no-store`; PENDING user gets an activation token instead; 1h for reset; 403 `account_inactive` if deactivated) |
| `GET /admin/organizations?after&limit` | ADMIN | - | 200 `{ organizations: OrganizationResponse[] }` |
| `POST /admin/organizations` | ADMIN | `{ name: string (1-200); slug: string (lowercased; regex `^[a-z0-9][a-z0-9-]{1,62}[a-z0-9]$`); owner_user_id: string }` | 201 `OrganizationResponse`. 404 owner unknown, 409 slug exists |
| `PUT /admin/organizations/{id}/status` | ADMIN | `{ status: "ACTIVE"|"SUSPENDED"|"DEACTIVATED" }` (case-insensitive; `DELETED` -> 400) | 200 `OrganizationResponse` |
| `DELETE /admin/organizations/{id}` | ADMIN | - | 200 `OrganizationResponse` (status DELETED). Org must be DEACTIVATED first (409 `conflict`); 409 `conflict` while it has assets/uploads/jobs/holds/pending deletions (message lists them) |
| `GET /admin/organizations/{id}/quota` | ADMIN | - | 200 `QuotaResponse` |
| `PUT /admin/organizations/{id}/quota` | ADMIN | `QuotaRequest` below | 200 `QuotaResponse` |
| `POST /admin/organizations/{id}/quota/reconcile` | ADMIN | none | 200 `{ tenant_id, settled_reservations: number, repaired: boolean, reserved_bytes_before, reserved_bytes_after, stored_bytes_before, stored_bytes_after }` |
| `GET /admin/audit`, `GET /admin/audit/verify` | ADMIN or AUDITOR | see 5 | see 5 |

`QuotaRequest` (all optional ints; **replaces** overrides: omitted/0 -> falls back to default): `max_storage_bytes, max_assets, max_object_bytes, max_active_uploads, max_active_multipart, max_active_jobs, max_pending_processing, max_pending_entries, max_manifest_entries, max_manifest_bytes, max_requests_per_minute`. Negative values yield **500 `internal`** (see gaps), not 400.

### 6.1 Dead letters (only mounted if the dead-letter service is wired; otherwise these paths 404 from the mux)
Platform ADMIN. Shape:
```ts
DeadLetterResponse { id: string; event_id: string; event_type?: string; tenant_id?: string; consumer?: string;
  status: "OPEN"|"RETRIED"|"REPLAYED"|"RESOLVED"|"DISCARDED";
  category?: "PERMANENT"|"EXHAUSTED"|"MALFORMED"|"UNKNOWN_TYPE"|"UNSUPPORTED_VERSION"|"REDRIVE"|"UNCONSUMED";
  last_error?: string; created_at: string /* = dead_lettered_at */; replay_event_id?: string; stale?: string }
```
- `GET /admin/dead-letters?status&limit` -> `{ dead_letters: DeadLetterResponse[] }`. `status` case-insensitive, `""` = all. Unknown status -> empty list.
- `GET /admin/dead-letters/{id}` -> `DeadLetterResponse` (+`stale` message if source event gone/changed). Inspect returns no payload/attempt counts/inbox detail despite internal detail existing.
- `POST /admin/dead-letters/{id}/retry` body `{reason}` -> 204.
- `POST /admin/dead-letters/{id}/replay` body `{reason}` -> 200 `{ replay_event_id: string }`.
- `POST /admin/dead-letters/{id}/resolve` body `{reason}` -> 204.
- `POST /admin/dead-letters/{id}/discard` body `{reason}` -> 204.
`reason`: required, non-blank, <=500 bytes, else 400 `invalid_request` (message "a reason of at most 500 characters is required"). Non-OPEN / not replayable -> 409 `conflict`; unknown id 404 `not_found`. `replay_event_id` in the list item is never populated by the mapper (field is always absent; only the replay response carries it).

## 7. Assets: metadata, lifecycle (tenant class)

### 7.1 `GET /assets/{id}` - permission `assets.read`, op `listing`
200 `Cache-Control: private, no-store`:
```ts
AssetResponse { id; file_name; content_type; size_bytes: number; checksum_sha256?: string; dataset?: string;
  status: "QUARANTINED"|"PROCESSING"|"AVAILABLE"|"REJECTED"|"SOFT_DELETED";   // tombstoned DELETED -> 404
  rejection_reason?: string; deliverable: boolean; etag?: string; published_at?: string; created_at; updated_at }
```
Errors (shape `{error}` only, no `code`, except 403/400-org which have code): 401 `authentication required`; 403 `forbidden`; 400 `organization_required`; 404 `asset not found` (also other tenant's asset); 500 `delivery failed`. (Delivery-only errors 409/412/416/503 belong to download endpoints.)

### 7.2 `DELETE /assets/{id}` - permission `assets.delete`, op `upload_finish`
Soft delete, idempotent. 202, `no-store`:
```ts
DeleteAssetResponse { asset_id: string; status: "SOFT_DELETED"; deletion: DeletionResponse; created: boolean /* false when already deleted */ }
DeletionResponse { id: string; state: "SCHEDULED"|"IN_PROGRESS"|"RETRY_WAIT"|"STORAGE_DELETED"|"FAILED"|"COMPLETED"|"CANCELLED"; purge_after: string; restorable: boolean }
```
`PROTECTED` internal state (legal hold/retention) is **masked as `SCHEDULED`** to owners; `restorable` is true only while the internal state is SCHEDULED/PROTECTED. Grace default 168h (`RETENTION_DEFAULT_DELETION_GRACE`, floor 1h, tenant/dataset/asset policy can change it); `purge_after` = now + grace.
Errors (no `code` field except 403/400): 401 `authentication required`; 403 `forbidden`; 404 `asset not found`; 409 `asset is being processed; delete it once processing has finished` (QUARANTINED/PROCESSING with PENDING job); 409 `the asset changed during the request; try again`; 500 `lifecycle request failed`.

### 7.3 `POST /assets/{id}/restore` - permission `assets.delete`, op `upload_finish`
No body. 200 `no-store`: `RestoreAssetResponse { asset_id: string; status: AssetStatus /* the status it returns to, e.g. "AVAILABLE" */ }`. Errors: 404 `asset not found`; 409 `nothing to restore: no deletion, or its objects are already being removed`; 409 `the asset changed during the request; try again`; 403; 401.

### 7.4 `GET /assets/{id}/lifecycle` - permission `assets.read`, op `listing`
200 `private, no-store`:
```ts
AssetLifecycleResponse { asset_id: string; status: AssetStatus (incl. "SOFT_DELETED"); dataset?: string;
  retain_until: string;            // created_at + effective min_retention (always present)
  expires_at?: string;             // only when effective max_age > 0
  deletion_grace_seconds: number;  // effective grace
  deletion?: DeletionResponse }    // only if a live deletion (SCHEDULED/PROTECTED->SCHEDULED/IN_PROGRESS/RETRY_WAIT/STORAGE_DELETED) or a FAILED one exists
```
Legal holds and lifecycle events are intentionally hidden from owners (never shown, not even existence). 404 for other tenant's/tombstoned assets.

## 8. Governance (tenant class, op `admin`, shown only if services are wired)

### 8.1 `GET /assets/{id}/quarantine` - permission `quarantine.read`
200:
```ts
ProcessingRecordResponse { asset_id; asset_status: AssetStatus; job_state: "PENDING"|"SUCCEEDED"|"FAILED"|"EXHAUSTED";
  job_stage: "VERIFY"|"SCAN"|"PROMOTE"|"CLEANUP"; attempts: number; last_error?: string;
  quarantine: { id: string; category: "VERIFICATION_FAILED"|"VALIDATION_REJECTED"|"MALWARE_DETECTED"|"SCAN_INCOMPLETE"|"PROCESSING_EXHAUSTED";
     reason: string; scan_result: string; status: "HELD"|"RELEASED"|"REJECTED"|"PURGED"; created_at: string; retain_until: string|null }[] }
```
`quarantine` is always an array (maybe empty). `scan_result` values: `CLEAN`, `NOT_SCANNED`, `UNSUPPORTED`, `TIMEOUT`, `ERROR`, plus infected result strings (from code comments; not exhaustively verified). Errors: 404 `not_found` (also when asset has no processing job, e.g. batch-ingested assets), 403.

### 8.2 `POST /assets/{id}/quarantine/release` - permission `quarantine.release`
No body. 202 `{ asset_id: string, job_state: string, stage: string }` (note key is `stage` here, `job_stage` above). Requeues an EXHAUSTED job; the scan still decides. 409 `conflict` if asset not QUARANTINED/PROCESSING or job not requeueable.

### 8.3 `PUT /retention/policies` - permission `retention.manage`
Body `RetentionPolicyRequest`:
```ts
{ scope: "TENANT"|"DATASET"|"ASSET" (case-insensitive, uppercased);
  scope_key?: string;   // "" for TENANT; dataset name (1-64 chars [a-z0-9._-], start alnum) for DATASET; asset id (<=200) for ASSET
  min_retention?: string; max_age?: string; deletion_grace?: string; quarantine_retention?: string;  // Go durations: "720h", "1h30m", "0s"; units ns,us,ms,s,m,h ONLY (no "d"/"w"); omitted/"" = inherit
  reason: string }      // required, non-empty
```
Each duration: >=0 and <=100 years (876000h); `min_retention` >= platform floor (default 0s), `deletion_grace` >= 1h (default floor), `quarantine_retention` > 0 and >= platform `QUARANTINE_RETENTION` (default 720h). Versions are append-only; effective immediately (no `effective_from` field in API).
200 `{ id: string, scope: string, scope_key: string, version: number, effective_from: string }`. Bad duration string -> 400 `{"error":"durations must be like 720h","code":"invalid_request"}`; other validation -> 400 `invalid_request` with service message; ASSET scope unknown id -> 404 `not_found`. **There is no GET for policies** (cannot read current tenant/dataset policies; only `GET /assets/{id}/lifecycle` shows effective results).

### 8.4 Legal holds
- `POST /legal-holds` - `retention.manage`. Body `{ scope: "TENANT"|"DATASET"|"ASSET"; scope_key?: string; reason: string }` (rules as above; reason required). 201 `{ id: string, scope: string, scope_key: string, placed_at: string }`. Errors 400 `invalid_request` / 404 `not_found`.
- `DELETE /legal-holds/{id}` - `retention.manage`. 204; unknown id 404 `{"error":"not found","code":"not_found"}`.
- **No list/get endpoint for holds.** The frontend has to remember hold ids it created (or cannot show them).

## 9. Gaps / surprises
1. No CORS anywhere; browser app needs same-origin proxy or server change.
2. Error shape is inconsistent: lifecycle and asset-metadata handlers return `{error}` without `code` (409s distinguishable only by message text); malformed JSON returns **plain-text** 400 (not JSON). A TS client needs to tolerate non-JSON error bodies.
3. `/auth/me` returns role ids only: no permissions, no org status, no "current org". Own permissions require `GET /organization/roles` (needs `members.read`) + matching `role_id`; a custom role without `members.read` leaves the UI blind. Suspended memberships and inactive orgs are included in `memberships`.
4. No cursors in list responses; derive from last item (`user_id` for members, `id` for others). Out-of-range `limit` falls back to 100, not the max. Dead letters have no paging. Invitations list is newest-first but members/users/orgs ascending.
5. Invitation/activation/reset tokens are returned in API responses (no email delivery) and are shown once; UI must display/copy them. Accept/activate/reset do not sign the user in.
6. `PATCH /organization` is a full replace of settings (omitting fields clears them). `PUT .../roles/{id}` and `PUT .../quota` are full replaces too. Admin quota negative values currently map to 500 `internal` (plain `errors.New` not in status map).
7. Refresh replay kills the session; refresh must be single-flight. `refresh_expires_at` never moves (hard 30-day session cap by default).
8. Retention: no GET for policies or holds; durations are Go strings (no `d`); `PROTECTED` deletions are shown as `SCHEDULED`; holds invisible to owners by design.
9. `DeadLetterResponse.replay_event_id` is never filled by list/inspect; `ProcessingRecordResponse` uses `job_stage`, release response uses `stage`.
10. Audit `action` filter silently ignored if it has digits/uppercase/hyphen; invalid `outcome` returns empty list; `next_before` omitted (0) when last page is short; pass `limit` explicitly to detect more pages.
11. `X-Organization-ID` is validated even on account routes (`/auth/me`, `/auth/logout`): a stale/foreign id yields 403 `not_a_member`. Omit it there.
12. Admin dead-letter routes only exist when the service is wired (404 otherwise). Governance routes likewise conditional (quarantine vs retention/holds mounted independently).
13. Org-level `GET /organization/audit/verify` allows only the org's own chain; arbitrary chains need platform role through `/admin/audit/verify?chain=`.
14. Login does not lowercase client-side; server matches email case-insensitively. Emails in invitations preserve typed case.
