# Erasure, withdrawal and destroy-by-provenance

A participant withdraws from a study, a court orders pirated works destroyed "and any copies", a privacy audit finds passports in a training set. Each needs the same thing: find the content wherever it is (every bucket, every snapshot, every model trained on it), take it out of the snapshots without rewriting history, delete the bytes where no legal hold forbids it, and prove it. Migration `000040_erasure.sql`.

## Finding it

`POST /api/v1/erasures/search {"subject_ids", "sha256s", "object_ids", "subject_key"}` (`retention.manage`) answers what an erasure would reach, recording nothing:

- **Subjects**: members and objects whose metadata names the subject, under `subject_id` (indexed), or `subject_key` when the organization uses another key. Manifests carry it per file (`"metadata": {"subject_id": "P-1042"}`), and member metadata counts as well as object metadata.
- **Content**: objects with a SHA-256 in `sha256s` (a takedown list, or every hash of a manifest: `osp erasure ... --manifest`).
- **Objects**: every version of each catalog object in `object_ids` (a content check's finding, a PII detector's hit).

Then every **exact copy**: every object with the same SHA-256, on any of the organization's storage connections (the platform's, and each bring-your-own bucket), at any path. Near copies (re-encoded, cropped, paraphrased) are not searched, and the plan says so.

The plan lists the objects (connection, path, size, SHA-256), what would hold each back now (legal holds, retention, Object Lock), the snapshots that pin them (and whether they are superseded already), the drafts that hold them, and the models trained on those snapshots, through their runs.

## The request

`POST /api/v1/erasures {"kind", "reason", "reference", "institution", ...the search}` records a request with its plan. `kind` is `SUBJECT_WITHDRAWAL`, `COPYRIGHT`, `PII`, `CONSENT_EXPIRED` or `OTHER`; `institution` names who the certificate is for (one request per institution for a withdrawal that must be attested to each). `GET /api/v1/erasures[?state=]`, `GET /api/v1/erasures/{id}`.

Where the organization's governance requires two people for deletions (docs/governance.md), `POST .../approve {"note"}` by a second member with `retention.manage` comes first. `POST .../cancel` drops a request not yet executed.

## Executing it

`POST /api/v1/erasures/{id}/execute` searches again (the plan is never stale), then:

1. **Successor snapshots.** For each snapshot that pins an erased file, oldest first, a new version of its dataset with every other member as it was, the same order, its own manifest SHA-256, lineage `derived_from` the old one, and `superseded_by` set on the old one. A successor of the dataset's current version becomes current. The old snapshot keeps its record; its erased members show as erased once their bytes are gone.
2. **Reads stop.** A superseded snapshot cannot be streamed for training or recorded as a training run's input any more: **409** `version_superseded`, naming its successor.
3. **Drafts.** Erased files leave every dataset's draft, so no later snapshot takes them back.
4. **Deletion.** Every matched object, each copy included, is deleted through the lifecycle (docs/lifecycle.md) at once, without the grace period: all its stored objects, its replicas, its entry in any pack (the pack is rewritten), on its own connection. A legal hold, a retention period or an Object Lock still in force holds a file back: the request waits (`WAITING_ON_HOLD`) and finishes on its own when the hold is released. Erasure overrides snapshots; it never overrides a hold.
5. **Tombstone.** An `erasure.executed` entry in the audit chain records the request, the content hashes, the subjects as SHA-256 hashes, and the counts. After completion the request keeps the subjects only as those hashes.

The cleanup worker follows each file's deletion. When every file is gone the request is `COMPLETED`, `erasure.completed` is sent (with the models to retrain), and the certificate is issued.

## The certificate of destruction

`GET /api/v1/erasures/{id}/certificate` (`retention.manage` or `audit.read`) is a JSON body signed with the platform's Ed25519 key (`CERTIFICATE_SIGNING_KEY`): the request, the institution, who asked and who approved, each destroyed file (SHA-256, size, connection, path, when, and its own disposition certificate's ID and hash), each superseded snapshot with its successor, the models trained on the content, the audit chain position of the tombstone, and what was not searched (near copies; subjects named only in file contents rather than metadata). `osp erasure certificate ID --verify` checks the signature without the platform.

Models are listed, not changed: retraining is the organization's work, and the certificate says which models it concerns.
