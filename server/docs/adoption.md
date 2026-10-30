# Adoption and imports

Data an organization already has in its own bucket can be brought under the platform **without uploading it again and without listing the bucket**: each object named by a source is read once where it is, checked, and published in place. Data elsewhere on the web is **imported** from URL lists and STAC catalogs through the ordinary upload pipeline. Migration 000031; the processing worker runs the jobs.

## Sources

`POST /api/v1/adoptions` (ingestion.write; adopting in place also storage.use):

```json
{"connection_id": "conn-1", "source": "S3_INVENTORY", "source_ref": "inventory/lab/2026-10-30T01-00Z/manifest.json",
 "dataset": "sequencing", "mode": "DETACH", "require_digest": true}
```

| Source | `source_ref` | What it reads |
| --- | --- | --- |
| `MANIFEST` | an object key, or `inline` | JSON Lines: `key`, optional `path`, `size`, `content_type`, `metadata`, and expected digests (`md5`, `sha1`, `sha256`, `sha512`, `crc32c`, `xxh64`, `xxh3`, `xxh128`, `c4`, top-level or under `digests`) |
| `S3_INVENTORY` | the report's `manifest.json` | An S3 Inventory report in CSV: each data file is checked against its MD5, keys are URL-decoded, delete markers and older versions skipped. A single-part object's ETag becomes its expected MD5; a multipart ETag is not a digest and is not used |
| `MHL` | the `.mhl` file | ASC MHL v2 (paths relative to the folder holding `ascmhl/`) or legacy v1 (relative to the list); `md5`, `sha1`, `xxh64`, `xxh3`, `xxh128`, `c4`, and v1's little-endian `xxhash64` |
| `BAGIT` | the bag's folder | RFC 8493: `bagit.txt`, every `manifest-<alg>.txt` present, `bag-info.txt` fields as `bag.<field>` metadata; a file count that disagrees with Payload-Oxum fails the job |
| `URL_LIST` | an object key, or `inline` | JSON Lines with `url` (and `path`, digests...), or bare `URL [path]` lines |
| `STAC` | a URL | A STAC Item, ItemCollection, Collection or Catalog (child links one level deep, up to 10,000 items): each asset under `collection/item/asset.ext`, item properties as `stac.*` metadata, `file:checksum` (SHA-256, SHA-512 or MD5 multihash) as the expected digest |

Inline sources are limited to 16 MiB; larger ones are read from the bucket. Entries a source describes badly (invalid JSON, digests or paths) are kept as `SKIPPED` with a code, so they show in the results.

## Adopting in place

For each entry, on the job's connection (one of the organization's own; the platform's bucket cannot be adopted from):

1. Keys under the prefixes the platform writes (`quarantine/`, the ready prefix, `manifests/`) are refused (`platform_key`).
2. With `require_digest` (the default), an entry with no expected digest is refused (`digest_required`).
3. An object already adopted is skipped (`already_adopted`, with its asset).
4. `HEAD` pins the version: every later read is of exactly that ETag and version.
5. One read streams the bytes into the malware scanner and computes SHA-256, MD5, xxh64 and every expected algorithm at once (objects larger than a scanner segment are scanned in overlapping segments, then read once more for the digests). Any expected digest that differs refuses the entry (`digest_mismatch`), as does malware (`malware_detected`).
6. The content checks (docs/content-checks.md) run under the organization's content policy: a rejecting finding refuses it, flags are recorded.
7. In one transaction: the catalog object at `path`, the asset (`AVAILABLE`, its key, ETag and provider version), an upload record and a finished processing record carrying the checks' evidence (SHA-256 COMPUTED, scanner and version, content-check coverage), its digests, the catalog version and the usage.

Nothing is copied. Downloads, snapshots, validation, retention, holds and Object Lock work on adopted assets like any other; packing never repacks them.

**Deleting an adopted asset.** `mode` decides: `DETACH` (default) removes the platform's record only and never touches the organization's object; `MANAGE` deletes exactly the adopted version, like the platform's own objects.

A failed read is retried, up to three times, on later cycles; the job completes when no entry is pending.

## Imports

A URL entry is fetched through the network guard (no private, loopback or metadata addresses), up to the upload size limit, checked against its expected digests and size, and uploaded through an ordinary upload session. Processing then verifies, checks, scans and publishes it; the entry's result follows its asset.

## Results

- `GET /api/v1/adoptions`, `GET /api/v1/adoptions/{id}`: the job and its counters (entries, adopted, rejected, failed, bytes).
- `GET /api/v1/adoptions/{id}/results`: a JSON page (`?after=&limit=`), or the whole per-entry file with `?format=csv` or `?format=jsonl`: line, key or URL, path, state (`ADOPTED`, `IMPORTED`, `REJECTED`, `SKIPPED`, `FAILED`, `PENDING`), code, detail, asset, upload, SHA-256, size.
- `POST /api/v1/adoptions/{id}/cancel`.

## Exports

- `GET /api/v1/datasets/{id}/versions/{versionID}/mhl`: an ASC MHL v2 list of the snapshot's members with MD5 and xxh64. Digests kept from adoption are reused; others are computed from one read of the pinned bytes (checked against the member's SHA-256) and kept.
- `GET /api/v1/datasets/{id}/versions/{versionID}/manifest-sha256.txt`: the BagIt payload manifest of the snapshot, from its members' SHA-256.

## SDK and CLI

`osp.adoption`: `adopt`, `jobs`, `job`, `results`, `wait`, `cancel`, `export_mhl`, `export_bag_manifest`. CLI: `osp adopt`, `osp adoptions`, `osp adoption ID [--results FILE]`, `osp export-mhl DATASET VERSION`.

## Not covered

- S3 Inventory reports in ORC or Parquet: configure the report as CSV.
- MHL v2 chains (`ascmhl_chain.xml`) are not walked: point the job at the generation to adopt.
- Adoption does not reserve quota up front: usage counts adopted bytes as they are published.
