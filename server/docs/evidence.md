# Evidence and attestation

What auditors, inspectors, regulators and review tools ask for: the audit chain anchored outside the database, exports a SIEM or an inspector can check without the platform, scheduled exception reviews with a reviewer's sign-off, a lawful-basis gate before training, chain-of-custody evidence packs with load files, FOIA and litigation holds on snapshots, model training summaries, and control mappings with live evidence. Migration `000038_evidence.sql`.

## Settings

`GET/PUT /api/v1/evidence-settings` (`audit.read` / `org.manage`):

| Field | Meaning |
|---|---|
| `anchor_connection_id` | Where anchors are written: one of the organization's connections, best a bucket with Object Lock. Empty keeps them in the database only. |
| `anchor_every_minutes` | 5 to 1440 (60). |
| `anchor_lock_days` | COMPLIANCE retention on each anchor object where the bucket has Object Lock (2555, seven years). |
| `review_every_days` | How often an exception review is built (7; 0 stops them). |
| `work_start_hour`, `work_end_hour`, `timezone`, `bulk_read_threshold` | Reads outside working hours by one member, at least the threshold in an hour, are a finding. |
| `require_lawful_basis` | The publish gate (below). |

## Anchors

Every `anchor_every_minutes`, for each organization whose chain moved, the cleanup worker verifies the chain and writes an **anchor**: `{type, organization_id, seq, hash, anchored_at, previous_seq, previous_hash}`, signed Ed25519 with the platform key (`CERTIFICATE_SIGNING_KEY`), to `<ready prefix>/<organization>/.anchors/<seq>.json` on the anchor connection, locked. With `AUDIT_TSA_URL` set, each anchor is also time-stamped by that RFC 3161 service; the token is kept (check it with `openssl ts -verify`). Someone with database access can rewrite the chain and re-hash it, but cannot change the anchors already in a locked bucket, nor the TSA's tokens.

A chain that does not verify is not anchored: a `AUDIT_CHAIN_BROKEN` security alert is raised instead (and sent to the `security.alert` webhook).

`GET /api/v1/organization/audit/anchors` lists anchors, each checked three ways: the signature, the chain still holding that hash at that entry, and the copy in the bucket still being the signed document. `POST` anchors now.

## Exports and the standalone check

`GET /api/v1/organization/audit/export?after_seq=&from=&to=&limit=&format=jsonl|cef` (`audit.read`) pages the chain oldest first (10,000 per page; the next `after_seq` is in `X-Next-After-Seq`). Each JSON line carries exactly what its hash covers: SHA-256 over the previous hash, then `id, chain, seq, tenant_id, actor_type, actor_id, action, resource_type, resource_id, outcome, reason, correlation_id, source_ip, created_at`, each followed by the byte 0x1f, then `details_json`. `format=cef` gives ArcSight CEF lines for a SIEM, with the sequence number and hash.

`osp audit-verify export.jsonl` recomputes the chain from the file alone; with `--anchors` it also checks the export against the organization's anchors and their signatures. A gap, an edited entry, or a regenerated tail that no longer matches an anchor is reported, with where.

## Exception reviews

`POST /api/v1/audit-reviews {"from", "to"}`, `GET /api/v1/audit-reviews[/{id}]` (`audit.read`) list, for a period:
- **deletions** (assets, bulk deletes, catalog objects, expired snapshots, archived datasets),
- **retention changes** (policies, legal holds, governance settings and approvals, tier rules),
- **failed hashes** (scrubs, runs' verification reports, checkpoints that failed their re-read, uploads rejected for a checksum),
- **after-hours bulk reads**,
- **permission changes** (members, roles, invitations, API tokens, SSO, SCIM, access policy, regulator access),

with counts and the report's SHA-256. The cleanup worker builds one every `review_every_days` and sends `audit.review_due`. `POST .../sign {"note"}` records the reviewer's sign-off as an entry in the audit chain naming the review and its report hash, so the sign-off is as tamper-evident as what it reviewed; a review is signed once. `GET .../export` is a zip for an inspector: the review, every chain entry of the period, and a README.

## Lawful basis before training

`GET/PUT /api/v1/datasets/{id}/provenance {"lawful_basis", "license", "source", "source_kind"}` (`datasets.manage` to record): `source_kind` is `PUBLIC_DATASET`, `LICENSED_PRIVATE`, `WEB_CRAWLED`, `USER_DATA`, `SYNTHETIC` or `OTHER`. Recording it is an audit chain entry.

With `require_lawful_basis`, a snapshot of a dataset without all four answers **409** `provenance_required`, and a training run may not record reading a snapshot taken before its dataset's provenance was recorded: **409** `provenance_order`. A basis cannot be applied retrospectively.

`GET /api/v1/training-runs/{id}/provenance-order` is the evidence: for each snapshot the run read, when its dataset's basis was recorded and when the snapshot was taken, then the run, each with its audit chain position, and whether they are in that order.

## Evidence packs

`GET /api/v1/datasets/{id}/versions/{versionID}/evidence-pack[?digests=true&reads=100]` (`audit.read`; `digests` also reads every file, so needs `assets.download`) is a zip:

| File | What it holds |
|---|---|
| `manifest.jsonl` | The canonical manifest; it hashes to the snapshot's manifest SHA-256. |
| `signed-manifest.json` | The snapshot's identity, signed (docs/streaming.md). |
| `custody.jsonl` | Per member: control number, SHA-256 (with `digests`, MD5 and SHA-1 too, kept for next time), who uploaded it and when, where it is stored, its Object Lock, and its reads. |
| `audit-chain.json` | The chain's head when the pack was made, its latest anchor, and the public key. |
| `loadfile.dat`, `loadfile.opt` | Concordance load files (UTF-8, fields in þ separated by 0x14) for review platforms; natives under `NATIVES\`. |
| `verify_pack.py` | Checks the pack with Python alone: SHA256SUMS, the manifest hash, the signatures, and with `--natives FOLDER` every file. |
| `SHA256SUMS` | Every file's SHA-256. |

## Holds on snapshots

`POST /api/v1/datasets/{id}/versions/{versionID}/holds {"kind", "reference", "reason"}` (`retention.manage`): `kind` `FOIA`, `LITIGATION`, `AUDIT` or `INVESTIGATION`. Every member's object version gets a legal hold under the matter `snapshot <version>: <kind> <reference>`, so nothing in the snapshot is purged while it stands. `GET` lists them by matter; `POST .../holds/release {"matter", "reason"}` releases them through the governed release (a second member's approval where required).

## Model training summaries

`GET /api/v1/models/{id}/training-summary[?format=md]` (`datasets.read`) walks the model's lineage (its run, the snapshots it read, their datasets) and fills the European Commission's template for general-purpose AI models (July 2025): the model, training data by modality and size, the data sources grouped by kind with their licence, lawful basis, source and collection period (and the licences and sources members name), and what the provider must still complete. `POST .../publish` records publication; six months later (or a month after a model's creation if never published) the worker sends `model.summary_due`, once each six months.

## Control mappings

`GET /api/v1/compliance/frameworks`; `GET /api/v1/compliance/{framework}` (`audit.read`) for `nist-800-171`, `21-cfr-11` and `sec-17a-4`: each control, how the platform meets it, and the organization's state now (`MET`, `PARTIAL`, `NOT_MET`, or `ORGANIZATION` for a procedure the organization runs), with facts: chain verification, last anchor, last signed review, SSO and SSO-only members, tokens without expiry, connections with customer keys or failing posture, locked and held objects. `GET .../evidence` is a zip of the mapping, members and roles, API tokens (an access review), the chain's verification, anchors, reviews and the last 90 days of the chain.

For SEC 17a-4: `GET /api/v1/compliance/sec-17a-4/undertaking?role=DEO|D3P&name=&title=&firm=` drafts the 17a-4(f)(3)(vii) undertaking letter; `POST /api/v1/compliance/regulator-access {"name", "days", "datasets"}` (`org.manage`) issues an examiner a read-only token (datasets, assets, audit) for 1 to 30 days, shown once, its creation and every read audited; `GET /api/v1/catalog/as-of?dataset=&at=` recreates a namespace as it stood at a time (each object's version then). Retention keyed to schedules uses the retention templates (docs/governance.md); BagIt and MHL exports are in docs/adoption.md.
