# Security notes for the console

What the console does about sessions, tenancy and secrets, and what it leaves to the backend. The backend stays authoritative for tenant isolation, RBAC, quotas and resource access; nothing here replaces it.

## Session model

The API issues opaque bearer tokens (`sat_` access, 15 minutes; `srt_` refresh, 30 days fixed session cap, rotating, single use). It has no cookies and no CORS.

| Concern | What happens |
| --- | --- |
| Where tokens live | HttpOnly, SameSite=Strict cookies set by `src/app/api/session/login`. Never in `localStorage`, `sessionStorage`, IndexedDB, URLs, logs or JS-visible state. |
| Access token expiry | The access cookie expires 20 s early. The proxy then refreshes before sending, and also refreshes and retries once on a 401. |
| Refresh replay | A spent refresh token revokes the whole session, so `src/server/auth.ts` shares one refresh between concurrent requests and answers late requests still carrying the old cookie from a 30 s memory cache. Single Node instance, or sticky sessions. |
| Sign-out | `POST /api/session/logout` ends the session on the API (best effort) and clears both cookies; the client drops every cached query. |
| Expired or revoked session | A 401 clears the cookies and the client redirects to `/login?next=...`. The route guard (`src/proxy.ts`) only checks that a session cookie exists, and `/login` is not redirected away when signed out, so a stale cookie cannot loop. |
| Redirects | `next` is accepted only if it is a `/app...` path with no scheme, `//` or backslash (`src/lib/auth/redirect.ts`). |

## Request forgery and the proxy

- Cookies are SameSite=Strict. The proxy additionally rejects state-changing requests whose `Origin` differs from the console origin or whose `Sec-Fetch-Site` is cross-site.
- The proxy forwards a fixed list of request headers and a fixed list of response headers. Cookies are never sent to the API, and `Set-Cookie` from the API is never relayed.
- **Caller addresses.** The proxy sends the API exactly one `X-Forwarded-For` entry, on every call including token refresh: the caller's address, taken `TRUSTED_PROXY_HOPS` entries from the end of the header it received (the entries the proxies in front of the console appended; anything before them is what the caller wrote, and is dropped). The API, run with `AUTH_TRUST_PROXY_HEADERS=true`, trusts only that last entry, so sign-in, refresh and credential limits count each caller apart. With `TRUSTED_PROXY_HOPS=0` (no proxy, local development) Next.js fills the header with the connection's address only when the request has none, so a caller that reaches the console directly can still choose its own address: in production the console runs behind a proxy that appends `X-Forwarded-For`, and `TRUSTED_PROXY_HOPS` counts those proxies.
- **Request bodies.** A request with no session cookie is refused before its body is read, and bodies are capped at 17 MiB (an inline manifest is at most 16 MiB; files go straight to storage). Path segments are encoded again before the API URL is built.
- `auth/login` and `auth/refresh` are not reachable through `/api/proxy`, so script cannot obtain a token.
- Public token-redemption routes (`auth/activate`, `auth/password/reset`, `invitations/accept`, `downloads/{token}`) pass through without a session; the API enforces their own rules.

## Tenancy

- The active organization is an id held per tab (`sessionStorage`, not a secret) and sent as `X-Organization-ID`. The API re-checks membership on every request.
- Account routes (`/auth/me`, sessions, password) are called without the header because the API rejects one that names an organization the user is not an active member of.
- Every organization-scoped query key starts with `["org", orgId, ...]`. Switching organization cancels and removes everything under `"org"`; signing out clears the whole cache. Covered by `src/lib/auth/session.test.tsx`.

## Authorization in the UI

Navigation and buttons are filtered with `can(permission)`, derived from the user's role via `GET /organization/roles`. This is convenience only: every mutation still handles a 403 and shows it. When roles cannot be read (a custom role without `members.read`), the UI shows everything and relies on the API's answers.

## Secrets

- Storage provider credentials are typed into a password field, sent once to the API, and never returned by it (only `has_credentials` and a rotation counter). Forms clear secret fields after a successful submit and never prefill from a GET. Credentials are never put in URLs, query keys, storage or logs.
- Presigned upload and download URLs are bearer secrets: they are used immediately and are not logged or persisted. They cannot be revoked (only token-mode links can).
- Invitation, activation and reset tokens come back in API responses because no email is sent. They are shown once with a copy button and are not stored.

## Browser hardening

- Content-Security-Policy, `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY`, `Referrer-Policy: same-origin` and a restrictive `Permissions-Policy` are set in `next.config.ts`. `connect-src` allows any `https:` origin because uploads and downloads go straight to whatever provider the organization connected; a provider reached over plain http (local MinIO) has to be listed in `CSP_STORAGE_ORIGINS`. `script-src` includes `'wasm-unsafe-eval'` so the streaming SHA-256 module (WebAssembly) can load; it does not allow `eval()` of strings. Scripts allow `'unsafe-inline'` because of Next.js hydration; a nonce-based policy would need dynamic rendering of every page.
- File previews are limited to raster images (not SVG) and short plain text or JSON, rendered as an `<img>` of a blob URL or as text nodes. Uploaded HTML, SVG, XML and script are never rendered inline. The API already serves every download as an attachment with `nosniff`.
- No user content is injected as HTML anywhere.

## Known limits

- The CSP cannot pin provider hosts (see above).
- The refresh de-duplication is in-process.
- The API's error bodies come in three shapes; the client normalises them, but a proxy or load balancer error page is shown as a generic message.
