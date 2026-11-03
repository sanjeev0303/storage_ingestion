# Domain metadata and publishing

Geospatial teams need STAC, robotics teams need to find a moment inside a terabyte log, and researchers need a citable, persistent record that outlives the grant. All of this sits on top of snapshots and leaves their manifests unchanged. Migration `000043_domain.sql`.

## STAC

STAC in is the `STAC` adoption source (docs/adoption.md): it mirrors a Catalog, Collection, Item or ItemCollection, keeps each item's properties, bounding box and short geometries as `stac.*` member metadata, and checks a `file:checksum` when the item has one.

STAC out needs `datasets.read`:

- `GET /api/v1/datasets/{id}/versions/{versionID}/stac/collection.json`: a STAC 1.0.0 Collection whose extent comes from the members. A licence that is not a single SPDX-style word is exported as `proprietary`.
- `GET .../stac/items.json`: a FeatureCollection, streamed. Members imported from STAC group back into their items, with `eo:cloud_cover` and the like restored; any other file is an item of its own. Each asset has `file:size` and `file:checksum` (a SHA-256 multihash). Hrefs are relative, to `/api/v1/assets/{id}/content`, and need a credential.

A value such as a Sentinel-2 processing baseline of `04.00` stays a string, so nothing is lost on the way through. To catch a snapshot that mixes baselines, add a `uniform_metadata` rule on `stac.s2.processing_baseline` (docs/quality.md).

## Recordings

When a version is profiled (docs/quality.md), each `.mcap` file and each rosbag2 `metadata.yaml` is indexed: start and end time, message count, and each topic's schema, encoding and count. For MCAP only the footer and summary section are read, a few ranged reads however large the file. A file that cannot be read, or an MCAP without a summary section or statistics, is listed under the profile's `skipped` with the reason.

`GET .../versions/{versionID}/recordings[?topic=/imu&from_ns=&to_ns=]` lists them.

Not read: messages themselves, so nothing here can search on what a message contains, and rosbag2 `.db3` files. Use segments and embeddings for that.

## Segments

A segment is a time range inside one file: `POST .../versions/{versionID}/segments {"segments": [{"path", "start_ns", "end_ns", "label", "note", "attributes"}]}` (`datasets.write`, up to 1000 a call, all or none). The path must be in the snapshot; a range outside what its recording holds is refused. `GET .../segments[?path=&label=&from_ns=&to_ns=&after=&limit=]` lists them, keeping what overlaps the range. `DELETE .../segments/{segmentID}` removes one and the vectors computed over it. Segments add to a snapshot and do not change its manifest hash.

## Embedding indexes

Bring your own vectors, from your own model; the platform computes none.

- `POST .../versions/{versionID}/embedding-indexes {"name", "model", "dimensions", "metric"}` with `COSINE` (the default), `DOT` or `EUCLIDEAN`.
- `POST /api/v1/embedding-indexes/{id}/vectors {"vectors": [{"path", "segment_id", "vector"}]}`, up to 1000 a call, all or none. The same path and segment again replaces the vector.
- `POST /api/v1/embedding-indexes/{id}/query {"vector", "top", "path"}` answers the nearest, the highest `score` first. For Euclidean the score is the negative distance.

Queries read every vector, so results are exact and take in the order of a second at the ceiling of 1,000,000 vectors an index. Reading needs `datasets.read` on the snapshot.

## Upstream sources

Whatever a URL or STAC import brought in is watched. Each source is asked with a `HEAD` once a day, through the network guard, and shown at `GET /api/v1/upstream-sources[?dataset_id=&state=]`:

| State | Meaning |
|---|---|
| `WATCHING` | Same size, and the same ETag or modified time as first seen |
| `CHANGED` | Another size, ETag or modified time. Sends `upstream.changed` |
| `GONE` | 404 or 410. Sends `upstream.gone`; not asked again |

An answer that says nothing (a refusal, a redirect, a server error, no connection) changes no state, and the note says what happened. A `HEAD` cannot prove the bytes are the same: a server that keeps its ETag and size while changing a file is not caught. Your own copy is unaffected in every case.

Each snapshot holding imported bytes is linked in the lineage to an `UPSTREAM_SOURCE` node by `IMPORTED_FROM`, with the SHA-256.

## Publishing

`POST .../versions/{versionID}/publish {"title", "description", "creators": [{"name", "affiliation", "orcid"}], "publisher", "license", "mint_doi"}` (`datasets.manage`). The licence comes from the dataset's provenance (docs/evidence.md) if left out, and without one the snapshot is not published. A snapshot is published once.

The landing page is public at `<CONSOLE_PUBLIC_URL>/published/<slug>`, from `GET /api/v1/public/publications/{slug}`, which needs no credential and is limited by address. It shows the metadata, the file count and size, the manifest's SHA-256, a citation, BibTeX, RIS, the DataCite metadata, and schema.org JSON-LD for dataset search engines. **File names and data stay private**; readers ask for access. A published version is kept by tiering's expiry (docs/tiering.md).

`GET .../versions/{versionID}/publication` answers `{"publication", "landing_url"}`, with `publication` null for a snapshot not published yet.

`CONSOLE_PUBLIC_URL` must be set: without it a citation would point nowhere, and publishing answers **409** `publishing_not_configured`.

**DOIs.** Set `DATACITE_API` (`https://api.datacite.org`, or `https://api.test.datacite.org` to try it), `DATACITE_REPOSITORY`, `DATACITE_PASSWORD`, `DATACITE_PREFIX` and `CONSOLE_PUBLIC_URL`. Without them `mint_doi` answers **409** `doi_not_configured`. The DOI is `<prefix>/osp.<slug>`, registered with a `PUT`, so trying again cannot make a second one. A failure leaves the publication standing with the error in `doi_error`, and `POST .../publication/doi` tries again.

**Withdrawing.** `POST .../publication/withdraw {"reason"}`. The page stays and says it was withdrawn and why, because a DOI must keep resolving. The bytes are untouched: to delete them, use erasure (docs/erasure.md).

## Retention cost

`GET .../versions/{versionID}/retention-cost?years=10[&price_per_gb_month=]` answers bytes × years × price per tier, at AWS S3 list prices in US dollars unless you give your own. It is storage only: retrieval, requests, egress and extra copies are not counted.
