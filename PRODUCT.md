# Product

<!-- impeccable:product-schema 1 -->

## Platform

web

## Stack

Next.js, React, TypeScript (strict) and Tailwind CSS in `client/`, per the Phase 14 brief. The Go API in `server/` is the only backend; it has no CORS and no cookies, so the client talks to it through a same-origin Next.js proxy.

## Users

Data and ML platform engineers working inside an organization. They connect storage, ingest large datasets, check what was verified, and decide whether a dataset snapshot is fit to train on. Org admins also use it for members, roles, quotas, audit and failed jobs, but the daily job is operating and monitoring data, not administering accounts.

## Product Purpose

A management console for a multi-tenant object storage platform. It exposes real backend capabilities: storage connections (platform-managed, AWS S3, Cloudflare R2, GCS, Azure Blob, S3-compatible), multipart uploads, bulk ingestion by manifest, the object catalog and versions, managed datasets with immutable snapshots, validation and readiness reports, lineage, lifecycle and audit. Success is that an engineer can trust what the screen says: nothing is shown as done until the backend confirms it.

## Positioning

Verifiable, reproducible datasets. Every snapshot pins object versions; every report states what was checked, how (metadata-only, full-integrity, sampled), what was not checked, and what evidence backs it. A generic file manager cannot say that a check was not performed.

## Operating Context

- Long-running work: large multipart uploads, manifests with very many entries, validation jobs. Screens show progress that survives refresh and distinguish upload progress from processing progress.
- Several organizations per user; every cached read is scoped to the active organization.
- Authentication is Bearer access/refresh tokens with an `X-Organization-ID` header chosen by the client; authorization is decided by the backend only.

## Capabilities and Constraints

- Only routes registered in `server/internal/server/router.go` exist. No invented endpoints; gaps are documented, not simulated.
- Never fetch whole catalogs or manifests into the browser; cursor pagination and streaming only.
- Provider secrets never reach browser storage, URLs, logs or GET responses.
- Physical pack offsets and storage keys are never shown; a logical object is one object.
- No invented AI-readiness score; metadata validation does not prove semantic correctness, legal compliance or training success.
- Local mode must work without real cloud credentials (platform-managed storage, local Postgres, MinIO).

## Evidence on Hand

Backend runbooks in `server/docs/` (security, catalog, datasets, ingestion, storage-connections, delivery, lifecycle, events). No customer data, testimonials, benchmarks or pricing exist; none may be invented.

## Product Principles

1. Honest state: pass, warning, failure, not performed, sampled and unavailable are different things and look different.
2. The backend is the authority; the UI never implies a permission or success it has not been told.
3. Scale by design: bounded pages, bounded graphs, bounded concurrency.
4. Reproducibility is visible: a published snapshot is immutable and says so; a draft says it is a draft.
5. Destructive or availability-affecting actions say what they affect before they run.

## Accessibility & Inclusion

WCAG AA contrast, keyboard-first operation, visible focus, accessible dialogs, tables and forms, a table or list alternative to every graph, and reduced-motion support. Light and dark themes.
