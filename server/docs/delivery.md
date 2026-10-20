# Object delivery (local runbook)

Published assets (status AVAILABLE, so verified, validated, scanned and
promoted by the Phase 5.4 pipeline) are delivered three ways:

| Way | Route | Authorized | Revocable | Bytes flow through |
|---|---|---|---|---|
| Proxy stream | `GET/HEAD /api/v1/assets/{id}/content` | on every request | yes: access is re-checked on each request | the API |
| Token link | `GET/HEAD /api/v1/downloads/{token}` | on every request, as the issuer | yes, at once | the API |
| Presigned URL | issued by `POST .../download-url` | once, at issue time | **no**, works until it expires | MinIO / S3 directly |

## Routes

| Route | Success | Errors |
|---|---|---|
| `GET /api/v1/assets/{id}` | 200 metadata: status, `deliverable`, `etag`, `published_at` (never bucket or key) | 401, 404 |
| `GET /api/v1/assets/{id}/content` | 200 whole object, 206 one range, 304 not modified | 401, 404, 409 not deliverable, 412, 416, 503 |
| `HEAD /api/v1/assets/{id}/content` | the GET's status and headers; no bytes read from storage | as GET |
| `POST /api/v1/assets/{id}/download-url` body `{"expires_in_seconds", "filename", "mode": "storage"\|"token"}` | 200 `{url, expires_at, delivery, revocable}`, `Cache-Control: no-store` | 400, 401, 404, 409, 503 |
| `DELETE /api/v1/assets/{id}/download-links` | 200 `{"revoked": n}`: every token link for the asset stops at once | 401, 404 |
| `GET/HEAD /api/v1/downloads/{token}` | as `/content` | 403 for unknown, expired or revoked links (one answer for all) |

Other tenants' assets, other owners' assets and missing assets all answer
the same 404 body. 409 is only ever returned to the asset's owner (still
QUARANTINED/PROCESSING, or REJECTED). 503 covers storage outages,
throttling (`Retry-After: 2`), the concurrent-stream cap (`Retry-After: 1`),
a failed audit write, and a stored object that no longer matches its
published version. Provider error text never reaches the client.

## One authorization path

Every route goes through `delivery.Service.deliverable` (token links do so
as the token's issuer):

1. a valid principal (the `Authenticate` middleware; 401 otherwise);
2. a tenant-scoped lookup `WHERE tenant_id = $principal AND id = $asset`;
3. the caller's role in the organization has the permission: `assets.read` for metadata, `assets.download` for content and URLs, `assets.delete` to revoke links (assets belong to the organization, see `docs/security.md`);
4. status AVAILABLE, which is reachable only through verification, validation, a clean scan and promotion;
5. the object key equals the derived `PREFIX/tenant/asset`. A client never supplies a bucket, key, tenant or path.

The decision is read from the database on every request, so deletion or
rejection takes effect on the next one. A missing object is re-checked
against the asset: a concurrent deletion answers 404, while an AVAILABLE
asset without its object is logged as an incident and answers 503.

Requests are authenticated with bearer access tokens and resolved to a
membership by `middleware.BearerResolver` (`docs/security.md`); the
development identity no longer exists. Token links are authorized as their
issuer, whose current membership and permission are checked on every use.

## Headers

| Header | Value |
|---|---|
| `ETag` | `"<sha256 of the content>"`: the checksum verified at upload and again on the promoted copy. Strong and stable. The provider's ETag is never exposed; for multipart objects it is not a content digest. |
| `Last-Modified` | publication time (`published_at`) |
| `Content-Type` | the declared type, re-parsed with parameters dropped. HTML, XHTML, SVG, XML and JavaScript are sent as `application/octet-stream`. |
| `Content-Disposition` | always `attachment`, filename stripped of paths, control and bidi characters, RFC 2231-encoded |
| `X-Content-Type-Options` | `nosniff` |
| `Cache-Control` | `private, no-store` on content and metadata; `no-store` on URL responses |
| `Accept-Ranges` | `bytes` |
| `X-Request-ID` | the caller's ID when it is 1-64 chars of `[A-Za-z0-9._-]`, otherwise a fresh one; also written to the audit |

## Ranges, conditionals and resuming

- **Single ranges:** `bytes=a-b`, `bytes=a-` and `bytes=-n` give 206 with `Content-Range` and `Content-Length`. The end is clamped to the object size. A start past the end, `bytes=-0`, or any range of an empty object gives 416 with `Content-Range: bytes */<size>`. Bounds must be plain digits.
- **Unusual ranges:** multiple ranges (`bytes=0-1,5-6`) and malformed or non-`bytes` units are ignored, and the whole object is sent with 200, as RFC 9110 permits. multipart/byteranges is not implemented.
- **Order of checks:** they follow RFC 9110 section 13.2.2.
  - `If-Match` (strong), or `If-Unmodified-Since` when there is no `If-Match`: a mismatch gives 412.
  - `If-None-Match` (weak), or `If-Modified-Since` when there is no `If-None-Match`: a match gives 304.
  - `If-Range` takes an ETag (strong match only) or the exact `Last-Modified` date. On a mismatch the range is dropped and the whole object is sent, so a client never joins bytes from two versions.
- **Range checks:** ranges are validated against the published size before any storage request.

Resuming a download (any HTTP client):

1. Keep the `ETag` from the first response.
2. After an interruption, request `Range: bytes=<bytes already saved>-` with `If-Range: <that ETag>`.
3. On 206, check that `Content-Range` starts at your offset, then append.
4. On 200, the asset changed: discard what you have and keep the new body.
5. On 416 with `bytes */<size>`, where size equals what you have, you are done.

A presigned URL supports the same resume against MinIO/S3, using the provider's own ETag. If it expired (403 from storage), ask the API for a new URL and continue with the same offset and validator.

## Version consistency

Promotion records the delivery copy's provider ETag (and version ID, where
the bucket is versioned) in `assets.storage_etag` / `storage_version_id`
before the asset becomes AVAILABLE, and sets `published_at`. Every proxied
read is a conditional GET:

- `If-Match: <storage_etag>`, plus `versionId` where one was recorded.
- If the stored object was replaced out of band, the provider answers 412.
  Delivery then returns 503 and audits `version_mismatch`. Bytes of another
  version are never streamed under the asset's metadata, also not to a
  resuming client.
- Assets published before migration 000008 have no recorded ETag. Each
  request HEADs the object, requires the published size, and pins the GET
  to that ETag, so one response never mixes versions.
- Presigned URLs carry `versionId` when the bucket is versioned. On an
  unversioned bucket (local MinIO) an overwrite between issue and use is
  visible to the URL holder. This is one reason the proxy is the default
  path for version-sensitive clients.

## Signed URLs and revocation

- Presigned URLs are signed by the AWS SDK inside the S3 adapter. Each one is pinned to one bucket and key, one method (GET), a TTL of at most `DELIVERY_URL_MAX_TTL` (hard cap 1 hour), and response headers (`attachment`, safe content type).
- Issuing a URL requires the full authorization path. The URL is returned only after the decision is audited, and it is never logged or stored. The API checks at startup that signing works.
- A presigned URL is a bearer credential. Anyone holding it can download until it expires, even after the asset is deleted or the user loses access. It cannot be revoked individually; rotating the signing credentials is the only blunt instrument.
- When revocation matters, use `"mode": "token"` (or `DELIVERY_URL_MODE=token`). The link is 32 random bytes (base64url), and only its SHA-256 is stored. Every use re-runs authorization as the issuer, so deletion, rejection and `DELETE .../download-links` take effect on the next request. Token links need the proxy routes (`DELIVERY_PROXY_ENABLED=true`).

## Streaming

- One 32 KiB buffer per download; nothing is buffered in full in memory or on disk. A test serves 256 MiB with under 8 MiB allocated in total.
- The request context reaches the storage read. A client disconnect cancels the provider request and closes the body.
- `DELIVERY_STALL_TIMEOUT` is a watchdog reset on every chunk. It cancels a stuck provider read (audited `stalled`), and per-chunk write deadlines cut off a client that stopped reading (`client_stalled`).
- `DELIVERY_MAX_STREAMS` caps concurrent proxied downloads. More get 503 with `Retry-After: 1`, so goroutines and connections are not unbounded.
- Every check (authorization, preconditions, range, version, audit) runs before the status line. A failure after headers were sent cannot become an error response: the connection ends short of `Content-Length`, the failure is audited (`client_disconnected`, `client_stalled`, `stalled`, `upstream_failed`, `short_read`, with `bytes_sent`), and nothing is retried mid-stream.

**Proxy or presigned?**

| | Proxy | Presigned |
|---|---|---|
| Access check | every request; revocation is immediate | once at issue; valid until expiry |
| Version safety | conditional reads, never mixed | only with a versioned bucket |
| API load | every byte passes through the API (CPU, bandwidth, connections) | none after issue; scales with the object store |
| Audit | every request, with range and failures | issuance only; downloads appear only in provider access logs |
| Cacheability | private, no-store | private object; a CDN only with signed URLs |

## Audit and metrics

`delivery_audit_events` gets one row per decision:

- ALLOWED for each download (with `byte_range` when partial) and each issued URL (`reason` `via_storage`, `via_token` or `via_cdn`);
- DENIED with a reason (`not_found`, `quarantined`, `processing`, `rejected`, `key_mismatch`, `token_revoked`, `token_expired`, `deleted`);
- FAILED with a reason (`version_mismatch`, `object_missing`, `throttled`, `storage_error`, transfer failures with `bytes_sent`).

Rows never hold URLs, tokens or content, and never one row per chunk.

Metrics (expvar `/debug/vars` under `delivery`, with `METRICS_ADDR`):

| Group | Metrics |
|---|---|
| Downloads | `downloads_full`, `downloads_partial`, `downloads_completed`, `downloads_interrupted`, `bytes_sent` |
| Conditionals and denials | `not_modified`, `range_not_satisfiable`, `denied` |
| URLs and tokens | `urls_issued_storage`, `urls_issued_token`, `urls_issued_cdn`, `tokens_invalid`, `tokens_revoked` |
| Storage | `version_mismatch`, `storage_throttled`, `storage_errors` |
| Streams | `streams_in_flight`, `streams_rejected_busy` |

## Running locally

```powershell
docker compose up -d                       # postgres, minio, clamav
.\scripts\migrate.ps1                      # pending migrations, through 000008
.\scripts\migrate.ps1 -Database object_storage_test
```

```bash
go build -o bin/ ./cmd/...
# API (metrics on 9103); sign in as shown in docs/security.md
APP_ENV=development HTTP_PORT=8080 METRICS_ADDR=127.0.0.1:9103 ./bin/api.exe
# Promote completed uploads (scan with ClamAV)
SCANNER=clamav ./bin/processing-worker.exe -once
```

Sample requests (`A` is an AVAILABLE asset ID; `auth.hdr` holds
`Authorization: Bearer <access token>`, so the token stays off command lines):

```bash
curl() { command curl -H @auth.hdr "$@"; }   # every request below sends it
curl -s localhost:8080/api/v1/assets/$A                                    # metadata
curl -sI localhost:8080/api/v1/assets/$A/content                           # HEAD
curl -s -o file.pdf localhost:8080/api/v1/assets/$A/content                # whole object
curl -s -H 'Range: bytes=0-1048575' -o part1 localhost:8080/api/v1/assets/$A/content
ETAG=$(curl -sI localhost:8080/api/v1/assets/$A/content | tr -d '\r' | sed -n 's/^ETag: //Ip')
curl -s -C - -H "If-Range: $ETAG" -o file.pdf localhost:8080/api/v1/assets/$A/content   # resume
curl -s -X POST -d '{"expires_in_seconds":120}' localhost:8080/api/v1/assets/$A/download-url
curl -s -X POST -d '{"mode":"token"}' localhost:8080/api/v1/assets/$A/download-url
curl -s -X DELETE localhost:8080/api/v1/assets/$A/download-links
# Denied
curl -s -H "Authorization: Bearer $OTHER_ORG_TOKEN" localhost:8080/api/v1/assets/$A/content   # 404
curl -s -H 'Range: bytes=999999999-' -D - localhost:8080/api/v1/assets/$A/content   # 416
```

Tests:

```bash
go test ./internal/service/delivery/ ./tests/unit/            # service and HTTP behaviour
# Postgres + MinIO + ClamAV (see tests/integration):
TEST_DATABASE_URL=... TEST_S3_BUCKET=object-storage-it TEST_S3_CREATE_BUCKET=true \
TEST_CLAMAV_ADDRESS=127.0.0.1:3310 go test ./tests/integration/
```

## Limitations

- Access is per organization and role; there is no sharing of single assets across organizations.
- Presigned URLs cannot be revoked and, on an unversioned bucket, are not pinned to a version.
- Range support stops at single ranges; If-Range by date requires the exact `Last-Modified`.
- Expired and revoked `download_tokens` rows are kept (small, useful for investigation); purge them with SQL if they ever matter.
