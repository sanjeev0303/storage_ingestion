# Tiering, restores and costs

Objects move to colder, cheaper storage classes by rule; archived objects are restored on request; every read, download and retention plan can be priced first; a monthly budget can alert or stop reads; and old snapshots expire with a preview of the space freed. Migration `000037_tiering_costs.sql`.

## Tiers

Each asset has a `storage_tier`: `STANDARD`, `INFREQUENT`, `ARCHIVE` or `DEEP_ARCHIVE`, shown on catalog versions with `readable_until`. Each provider maps them to its own classes:

| Tier | Amazon S3 | Google Cloud Storage | Azure Blob | Cloudflare R2 |
|---|---|---|---|---|
| STANDARD | STANDARD | STANDARD | Hot | Standard |
| INFREQUENT | STANDARD_IA | NEARLINE | Cool | Infrequent Access |
| ARCHIVE | GLACIER (Flexible Retrieval) | COLDLINE | Cold | none |
| DEEP_ARCHIVE | DEEP_ARCHIVE | ARCHIVE | Archive | none |

A connection that tiers needs, beside what it already has, permission to copy an object onto itself (S3 `s3:PutObject` with `s3:GetObject` on the source), to delete the version a move replaces (`s3:DeleteObjectVersion`), and to restore (`s3:RestoreObject`); on GCS `storage.objects.create` and `storage.objects.delete`; on Azure `Microsoft.Storage/storageAccounts/blobServices/containers/blobs/write`. Without restore permission a restore fails with the provider's refusal, and the files stay archived.

S3's two Glacier classes and Azure's Archive tier must be **restored** before a read; GCS classes, Azure Cool and Cold, and Standard-IA read directly (at a retrieval fee). A self-hosted or S3-compatible store does not tier; the platform's own storage tiers only with `PLATFORM_STORAGE_TIERING=true`, when it is Amazon S3.

## Rules

`GET/POST /api/v1/tier-rules`, `PUT/DELETE /api/v1/tier-rules/{id}` (`storage.read` / `storage.manage`): `{"name", "tier", "min_age_days", "unread_days", "dataset_id"}`. A rule moves objects published at least `min_age_days` ago and not read (downloaded, linked or streamed) for `unread_days`, from a warmer tier to `tier`, everywhere or among one dataset's members. One of the two must be set, so new uploads never move at once.

A rule never moves an object that is:
- under a legal hold, or an Object Lock (retention or legal hold),
- sharing its stored bytes with another asset, or adopted,
- a member of a snapshot a **running** training run reads or keeps checkpoints in, or of a dataset's clean point,
- one whose move failed in the last day.

**Packs** (docs/packing.md) move whole: a pack moves once every file served from it qualifies, and its files take its tier. A restore of any of them restores the pack, and all its files read again. Packing does not rewrite or repack an archived pack.

The cleanup worker applies the rules every `TIERING_INTERVAL` (10 minutes), up to 200 objects per rule per pass. A move names the object's ETag, so an object changed meanwhile is never moved. Providers that copy to change class (S3, GCS) write a new version; the asset is pointed at it and the version it replaced is removed, so it is not billed twice. Every move, or why it failed, is in `GET /api/v1/tier-moves`.

Scrubs and replicas (docs/copy-engine.md) skip archived objects until they are restored: re-reading them would need a restore and bill retrieval.

## Restores

- `POST /api/v1/assets/{id}/archive-restores {"days", "speed"}` (`assets.download`): `days` 1 to 365 (7), `speed` `FAST`, `STANDARD` or `BULK`. One restore per object is open at a time; asking again joins it.
- `POST /api/v1/datasets/{id}/versions/{versionID}/archive-restores` restores every archived member of a snapshot, so a training run or an audit can read it whole, and answers how many were archived, asked for, already under way and readable, with the cost.
- `GET /api/v1/archive-restores?asset_id=&version_id=` lists them: `PENDING`, `RESTORING`, `READY` (readable until `readable_until`), `FAILED` or `EXPIRED`.

Until it is restored, reading an archived object answers **409** `restore_required`. The worker starts restores, checks the provider, and records when each is readable; the webhook `object.restored` fires when one is ready or failed. On Azure a rehydrated blob stays Hot.

## Costs

Every figure is an estimate at the providers' list prices (`GET /api/v1/cost-prices`): storage per GB-month by tier, requests per 1,000, egress per GB, retrieval and restore per GB, minimum billed days, restore hours. Real bills depend on region, agreements and discounts.

- `POST /api/v1/cost-estimates {"operation", "connection_id", "files", "bytes", "requests", "tier", "years", "epochs", "speed"}` (`storage.read`) prices `ingest` (PUTs and a first month), `download` (requests, egress, retrieval), `epoch` (reads in-region, `epochs` times), `retain` (storage for `years`, at least the tier's minimum days) or `restore`.
- `GET /api/v1/datasets/{id}/versions/{versionID}/cost-estimate?speed=` prices downloading a snapshot, one training epoch over it (a pack per request for packed files, docs/streaming.md), and restoring its archived files first, with the hours that takes.
- `GET /api/v1/datasets/{id}/costs?years=` is a dataset's (or a branch's) storage by connection and tier, its monthly cost, and keeping it `years` (10) as it is or archived.
- `GET /api/v1/cost-report` (`org.read`) is the month: storage by connection and tier, reads, egress, objects written and restored bytes, their costs, the budget and how much of it is used, and **savings**: requests a full read saves through packing, bytes deduplication keeps from being stored twice, and what tiering saves each month.
- The SDK prints an estimate before `osp ingest` and `osp download`; `osp estimate`, `osp costs` and `osp bench` show the rest.

## Budgets

`GET/PUT /api/v1/cost-budget {"monthly_usd", "monthly_requests", "action"}` (`org.read` / `org.manage`); 0 is no limit. Before each download, link and shard stream, the month's estimated spend (storage, reads, egress, restores) and reads are checked. Past either limit a `COST_BUDGET` alert is raised (once a day, also to the `security.alert` webhook); with `BLOCK`, reads are refused with **429** `cost_budget_exceeded` until the budget is raised or the month turns. The egress budget in bytes (docs/access.md) still applies beside it.

## Snapshot expiry

`POST /api/v1/datasets/{id}/version-expiry {"keep_last", "older_than_days", "dry_run"}` (`datasets.manage`; `dry_run` defaults to true) expires all but the newest `keep_last` (at least 1) snapshots older than `older_than_days`, except any still in use. Each kept version says why: the dataset's current version or clean point, a branch's fork or merge base, a derived dataset's source, a training run's input or checkpoint, a legal hold over the dataset, or the age or count asked for.

The preview lists what expires and what stays, and the **space freed**: the object versions only the expiring snapshots hold (no other live snapshot, no dataset's draft, not the current version of their object), their bytes and their monthly cost. Expiring keeps each version's record (number, manifest hash, counts, `expired_at` and who) and drops its member list; each freed object is then deleted through the lifecycle, so legal holds, retention, escrow and the two-person rule still apply, after the grace period. Those refused are listed with why.

## Storage manifests and Iceberg

`GET /api/v1/datasets/{id}/versions/{versionID}/storage-manifest?after=&limit=` (`storage.read` and `datasets.read`) pages a snapshot's members in path order with where their bytes are on the organization's own storage: `uri` (`s3://`, `gs://`, `az://`), the storage version, size, SHA-256 and tier. Members on the platform's own storage, or packed, have no `uri` and are read through the API.

`osp.tiering.register_iceberg(client, dataset_id, version_id, catalog, "db.table")` registers a snapshot's Parquet files as an Iceberg table with pyiceberg, in place: the table's data files are the snapshot's pinned objects, so query engines read them without a copy, and the snapshot's retention and expiry rules keep them.
