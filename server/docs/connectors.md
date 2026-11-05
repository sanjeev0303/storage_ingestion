# Connectors

How the platform joins the tools around it. Migration `000041_connectors.sql`. SDKs are in docs/sdks.md.

## Chat shares

Files pasted into chat leak with the chat. A chat share posts a link instead: it opens a console page for signed-in members with the right to download the file, and stops working at its expiry, so a chat export holds only dead links.

`POST /api/v1/chat-integrations {"kind", "name", "url"}` (`org.manage`) records a Slack or Teams incoming webhook. The URL is a credential: it is sealed, only its host is shown, and only Slack's and Microsoft's hosts are accepted. `POST /api/v1/chat-shares {"asset_id", "integration_id", "note", "expires_in"}` (`assets.download`; 1 minute to 30 days, 24 hours by default; not from a confined token) posts the message; if the channel refuses it, no share stands. `POST /api/v1/chat-shares/{id}/open` gives the signed-in member a five-minute download through the usual checks and audit. `DELETE` revokes. The link answers **410** once expired or revoked. The server needs `CONSOLE_PUBLIC_URL`.

## Comms-archive and forensic exports

The platform does not capture messages or collect from devices. It is where those tools' exports are preserved. `osp import-export FORMAT FILE DATASET [--source S] [--retain 61320h] [--lock COMPLIANCE]` reads an export, compares each item with its declared hash (MD5, SHA-1 or SHA-256), applies retention before the first file arrives, uploads it (the platform computes SHA-256 again), and records the collection. Items that differ from what the export declares are still kept, because they are evidence, and are listed.

| Format | Reads |
|---|---|
| `COMMS_JSONL` | One message per line, as Global Relay, Smarsh and LeapXpert exports give them: `id`, `sha256`, and `body` or a `file` beside the export. Each line is kept beside its message. |
| `RSMF` | Relativity Short Message Format: the `.rsmf` kept whole, its `rsmf_manifest.json` checked for every attachment. |
| `UFDR` | Cellebrite reader format: each tagged file in `report.xml` with its local path and hashes. |
| `MANIFEST` | A CSV or JSON-lines hash list beside the files, with `path` and `sha256`, `sha1` or `md5`. |

`GET/POST /api/v1/datasets/{id}/collections` keep the records; the import is an audit chain entry. DAT and OPT load files and per-snapshot chain of custody are in the evidence pack (docs/evidence.md).

## Git LFS

`lfs.url` set to `<api>/lfs/<dataset namespace>` makes a dataset a Git LFS store, using the Batch and File Locking APIs with an API token as the Basic password.

```
git config -f .lfsconfig lfs.url https://storage.example.org/lfs/game
```

Objects are stored under `.lfs/` by their SHA-256, through the same upload path as everything else: verified, scanned, deduplicated. A verify waits for those checks, so a push fails where a file is rejected. One object is at most 5 GiB (the basic transfer is one PUT). Reads go directly to your storage, so CI re-downloads are not billed by a forge. `git lfs lock` works, and a push refuses to overwrite a file someone else holds.

`osp lfs-promote DATASET LABEL [--hold REF]` pins the LFS files at HEAD in the dataset's draft, under their repository paths, snapshots and labels them, and with `--hold` places a legal hold, so what shipped cannot change or go away.

## DICOMweb

A read-only gateway over one snapshot, not a PACS. When a version is profiled, each `.dcm` member's header is indexed, deflated transfer syntax included. At upload every DICOM file is also walked to its end and checked for patient identifiers (docs/content-checks.md).

| Route | Answers |
|---|---|
| `GET /dicomweb/{dataset}/{version}/studies?PatientID=&StudyDate=&ModalitiesInStudy=&limit=&offset=` | QIDO-RS studies |
| `.../studies/{study}/series`, `.../series/{series}/instances` | QIDO-RS series, instances |
| `.../studies/{study}[/series/{series}[/instances/{instance}]]` | WADO-RS: `multipart/related; type="application/dicom"`, the Part 10 files as stored |
| the same with `/metadata` | The attributes as DICOM JSON |

It uses the platform's sign-in (a bearer token, from OIDC or an API token), over TLS: every read is authorized, audited and verified against its SHA-256. Rendered images, bulk data and transcoding are not served; a superseded snapshot answers **409**. Files that cannot be indexed are listed as not profiled in the version's profile.
