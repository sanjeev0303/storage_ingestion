# object-storage-platform

A multi-tenant object storage platform in Go: direct-to-storage uploads,
verification and malware scanning, delivery, lifecycle and retention, an
event pipeline, bulk ingestion, identity and tenancy, an object catalog,
managed datasets with validation, readiness reports and lineage, and
storage connections: platform-managed buckets, or an organization's own AWS
S3, Cloudflare R2, Google Cloud Storage, Azure Blob or S3-compatible storage.
It runs locally against Docker (PostgreSQL, MinIO, ClamAV, ElasticMQ; the
fake-gcs-server and Azurite emulators are an opt-in profile).

Runbooks, each with its local commands and limits:

| Topic | Document |
|---|---|
| Delivery, download URLs and the CDN | `docs/delivery.md` |
| Multipart uploads | `docs/multipart-reliability.md` |
| Verification, validation and scanning | `docs/content-security.md` |
| Lifecycle, retention and deletion | `docs/lifecycle.md` |
| Transactional outbox, queues and dead letters | `docs/events.md` |
| Bulk ingestion | `docs/ingestion.md` |
| Identity, tenancy, roles, quotas and audit; starting the stack | `docs/security.md` |
| Object catalog, versions and listing | `docs/catalog.md` |
| Datasets, snapshots, validation, readiness and lineage | `docs/datasets.md` |
| Storage connections, bring your own storage, providers, credentials | `docs/storage-connections.md` |

Migrations: `scripts/migrate.ps1` (see `docs/security.md`). Commands live in
`cmd/`: the API and one binary per worker. Tests: `go test ./internal/...
./cmd/... ./tests/unit/...`; the integration suite in `tests/integration`
needs the local services (`TEST_DATABASE_URL` and friends, see each runbook).
