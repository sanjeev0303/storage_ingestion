# Client conventions

Read this before adding to `client/`. It is short on purpose.

## What this is

A Next.js 16 (App Router, React 19, TypeScript strict, Tailwind v4) management console for the Go API in `../server`. Product context: `../PRODUCT.md`. API contract notes, written from the Go handlers: `docs/api/*.md`. When a doc and the Go code disagree, the Go code wins; say so in your report.

Only routes registered in `../server/internal/server/router.go` exist. Never invent an endpoint, never fake a success. If the UI would benefit from something the API lacks, show an honest "not available" state and list the gap in `docs/api-capability-matrix.md`.

Next.js 16 differs from older versions. The docs are in `node_modules/next/dist/docs/`. Route params are async (`await ctx.params`), `middleware` is `proxy.ts`, `LayoutProps<"/route">` and `RouteContext<...>` are global helper types (run `npx next typegen` after adding routes).

## Layout of the code

```
src/app/                    routes (thin server components that render feature components)
src/app/app/<area>/         the console, behind sign-in. URLs start with /app
src/components/ui/          shared primitives (Button, Field, Panel, Table, Modal, ConfirmDialog, ...)
src/components/shell/       app shell, navigation
src/features/<name>/        one folder per product area
    types.ts                DTOs, snake_case exactly as the API sends them
    api.ts                  functions that call api() from @/lib/api/client
    hooks.ts                react-query hooks
    components/             screens and parts, plus *.test.tsx next to what they test
src/lib/api/                client, errors, shared types
src/lib/auth/session.tsx    useSession(): user, organizationId, can(permission)
src/lib/query/              orgKey(), usePagedQuery()
src/lib/utils/              cn, format (formatBytes, formatDateTime, shortId)
src/server/                 server-only code (cookies, token refresh)
src/test/                   vitest setup, msw server, renderWithProviders
```

Own your feature folder and your routes. Add new shared components as new files in `src/components/ui/`. Do not rewrite shared files; if one needs a change, make the smallest additive edit and mention it in your report. Do not add npm dependencies; ask in your report if you need one.

## Talking to the API

- Always `api<T>(path, opts)` from `@/lib/api/client`. Paths are API paths without `/api/v1` (e.g. `/catalog/objects`). The client sends the active organization header; use `accountApi` for account routes.
- Errors are `ApiError` (`status`, `code`, `message`, `retryAfter`). Show `describeError(error)`. The backend has three error shapes (JSON with code, JSON without code, plain text); the client already flattens them.
- Query keys: `orgKey(organizationId, "area", ...)` from `@/lib/query/keys`, and `enabled: Boolean(organizationId)`. Everything under `"org"` is thrown away on organization switch and sign-out. Never put organization data under any other key.
- Lists use cursors: `usePagedQuery` with a `fetchPage(cursor)` that adapts that endpoint's cursor style. Show `LoadMore`. Never fetch a whole collection.
- Mutations: `useMutation`, then `invalidateQueries({ queryKey: orgKey(orgId, "area") })`. No automatic retries. Idempotency keys are body fields (`newIdempotencyKey()`), generated once per user attempt and reused if that attempt is retried.
- Never log request bodies. Never put secrets in URLs, storage, or query keys. Secret form fields are cleared after a successful submit and are never filled from a GET.
- Success is shown only after the backend confirms it. Long operations show the backend's state, not a guess.

## Permissions

`const { can } = useSession()`; `can("storage.manage")`. Use it to hide or disable actions. The backend is the authority: every mutation must still handle 403 and show it. When `can` cannot know (roles unreadable) it returns true.

## Screens

Every list and detail needs all of: loading (`SkeletonRows`), empty (`EmptyState` that teaches what goes here and how to start), error with retry (`ErrorState`), and permission-denied (`ErrorState` renders it on 403). Times use `<Time>`, sizes `formatBytes`, ids `mono-data` with `CopyButton` where people will paste them. Page title via `PageHeader` with breadcrumbs.

Destructive or availability-affecting actions use `ConfirmDialog`, say what they affect, and use `confirmText` when irreversible.

## Design rules (pinned by the owner)

Palette only: ground `#DDE3E8` (60%), plum `#3A0F34` (30%), amber `#EAAA00` (10%). Use the tokens (`bg-ground`, `text-ink`, `text-muted`, `bg-faint`, `bg-accent text-on-accent`). Amber is a fill with plum text on it, never text on the ground. No other colours, no raw hex in components.

The console is neomorphic: `neo` (raised), `neo-sm`, `neo-in` (recessed), `neo-in-sm` utilities, one light edge and one shade edge, always the same ground. Use `Panel`. Corners stay crisp: 2px or 4px (`rounded-sm`, `rounded-md`), never rounder. Inputs are recessed; actions are raised or solid plum. Pressed state is inset.

Layout follows the F pattern: what matters most top left, scanning down the left edge. Rule of thirds for page composition (a 2/3 main area and a 1/3 side area work well). Keep 60/30/10 in mind: mostly ground, plum for text and solid actions, amber rarely.

Status is told apart by shape and word, not hue: use `Outcome` (passed, partial, warning, failed, not_assessed) for check results and `StateBadge` for states. "Not checked" must never look like "passed".

Banned: gradients, drop shadows other than the neomorphic pairs, glass or blur, radial orbs, neon, rainbow colouring, dot grids, emoji, em dashes anywhere in copy, sparkle icons, Lucide icons, checkmark bullets, coloured left stripes on cards, rows of three identical feature cards, terminal windows, fake testimonials, a privacy policy. Do not write "it is not X, it is Y" copy. Fonts are Hanken Grotesk (UI) and JetBrains Mono (ids, checksums, code only); never Inter, Geist or Space Grotesk. Icons: `@phosphor-icons/react` only, regular weight, `aria-hidden` when decorative.

Copy: plain, specific, names the action ("Rotate credentials", not "Submit"); errors say the problem and the recovery.

Motion: GSAP (`useGSAP` from `@gsap/react`, scoped, `gsap.matchMedia` for reduced motion) or CSS transitions of 150 to 250 ms. Motion conveys state change only. At most one authored moment per page. Lenis smooth scroll is already mounted at the root; add `data-lenis-prevent` to any inner scroll area (the `Table` and `Modal` already do).

Accessibility: labelled controls (`Field`), visible focus (already themed), keyboard operable, dialogs via `Modal`/`ConfirmDialog`, no information by colour alone, tables have a real header row, graphs get a table or list alternative.

## Tests

`npm test` runs vitest (jsdom). Use `renderWithProviders` from `@/test/render` and mock HTTP with msw (`server.use(http.get(`${API}/path`, ...))` from `@/test/server`). Mocked tests only prove the component copes with the shape you gave it. Write tests for behaviour that can break: validation, pagination, permission gating, error states, retry and cancellation logic. Add an axe check (`expect(await axe(container)).toHaveNoViolations()`) for critical forms. Real-backend coverage lives in `e2e/` (Playwright).

Before reporting done: `npx tsc --noEmit`, `npx eslint <your files>`, `npx vitest run <your folder>`.

## Git

Do not run `git add`, `git commit` or `git push`. Leave changes in the working tree.

## Running it

- Client dev server: `npm run dev` serves http://localhost:3100 (already running while the team works; do not start another). Port 3000 belongs to a different project.
- Go API: http://localhost:8080 (`BACKEND_URL`). Browser calls go to `/api/proxy/*` on the client origin; the Next server adds the Bearer token from HttpOnly cookies.
- Integration check without a browser: `curl -c jar -H 'Content-Type: application/json' -d '{"email":"...","password":"..."}' http://localhost:3100/api/session/login`, then `curl -b jar -H 'X-Organization-ID: <org>' http://localhost:3100/api/proxy/catalog/objects`. POST/PUT/PATCH/DELETE also need `-H 'Origin: http://localhost:3100'`.
- Shared upload primitives for anything that talks to a presigned URL: `src/lib/upload/` (`sha256Blob`, `putBlob` with progress, `withRetry`, `choosePartSize`, `planParts`, `runPool`).
