# Object Storage console

The management console for the Go API in `../server`: storage connections (bring your own storage), uploads and bulk ingestion, the object catalog, datasets with immutable snapshots, validation and readiness reports, lineage, lifecycle and audit.

Stack: Next.js 16 (App Router, React 19), TypeScript (strict), Tailwind CSS 4, TanStack Query, React Hook Form with Zod, GSAP and Lenis for motion, Radix primitives, Phosphor icons. Design and code conventions: [CONVENTIONS.md](CONVENTIONS.md). Product context: [../PRODUCT.md](../PRODUCT.md).

## How it talks to the API

The Go API has no CORS support and issues bearer tokens, not cookies. The browser therefore never calls it directly:

- `POST /api/session/login` signs in against the API and keeps the access and refresh tokens in **HttpOnly, SameSite=Strict cookies**. Script never sees a token.
- `/api/proxy/*` forwards to `${BACKEND_URL}/api/v1/*` with the bearer token added. It refreshes an expired access token (single flight, because a refresh token can be used once and replaying one revokes the session) and retries the request once on a 401.
- The proxy refuses `auth/login` and `auth/refresh`, checks `Origin` on state-changing requests, forwards only a fixed list of headers, and never forwards cookies to the API.
- The active organization is chosen per browser tab and sent as `X-Organization-ID`. Every cached query about organization data lives under `["org", orgId, ...]` and is discarded on organization switch and sign-out.
- Uploads and downloads go **directly** between the browser and the storage provider using presigned URLs. The provider's bucket CORS must allow the console origin and expose the `ETag` response header (multipart parts cannot be confirmed without it).

Cookies are `Secure` in production builds. For a production build served over plain http (for example a local smoke test) set `COOKIE_SECURE=false`. A storage provider reached over plain http (the local MinIO) must also be allowed in the Content-Security-Policy with `CSP_STORAGE_ORIGINS=http://127.0.0.1:9000` (read at build time, so set it when running `npm run build`); https providers need nothing.

The token refresh de-duplication lives in process memory, so run one Node instance of the console or use sticky sessions.

## Run it locally

Requirements: Node 22.22.2 or later on the 22 line, 24.15 or later, or 26+ (the locked `jsdom`, `vite`, `msw` and `undici` versions require it; 22.16 is too old), and the backend running. The backend needs PostgreSQL, MinIO and the API with its workers; follow `../server/docs/security.md` ("starting the stack") and the runbooks in `../server/docs/`. Neon or any other Postgres configuration stays in the backend's own environment; the console has no database.

```bash
cd client
npm install
cp .env.example .env.local     # BACKEND_URL defaults to http://localhost:8080
npm run dev                    # http://localhost:3100
```

Sign in with an account created through the API or `identity-admin` (see the backend docs). No cloud credentials are needed: the platform-managed storage connection works against local MinIO.

## Commands

| Command | What it does |
| --- | --- |
| `npm run dev` | Dev server on port 3100 |
| `npm run build` / `npm start` | Production build and server on port 3100 |
| `npm run typecheck` | `tsc --noEmit` |
| `npm run lint` | ESLint over `src` |
| `npm test` | Vitest unit and component tests (mocked HTTP with msw) |
| `npm run e2e` | Playwright journeys against the **real** API (needs the backend and the dev server) |

Mocked tests prove a component copes with the response shapes it was given. They do not prove the backend supports an operation: that is what `npm run e2e` and the real-API checks in [docs/api-capability-matrix.md](docs/api-capability-matrix.md) are for.

### End-to-end tests

The journeys sign in as real users and call the real API. Provide the logins through environment variables (never commit them), for example in `.env.e2e`:

```
E2E_OWNER_EMAIL=...
E2E_OWNER_PASSWORD=...
E2E_AUDITOR_EMAIL=...
E2E_AUDITOR_PASSWORD=...
```

They use the Chrome installed on the machine; set `E2E_BROWSER=chromium` after `npx playwright install chromium` to use the bundled browser instead.

## Documentation

- [docs/api-capability-matrix.md](docs/api-capability-matrix.md): every screen, the endpoint behind it, permission, pagination, and what is not available from the API.
- [docs/api/](docs/api): contract notes read from the Go handlers (request and response shapes, error codes, surprises).
- [docs/security.md](docs/security.md): how sessions, CSRF, CSP and secrets are handled.
