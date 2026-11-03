# Chunks: savings, storage, delta downloads and uploads

Checkpoints, Parquet files and game builds change a little between versions, and whole-file dedup treats each as new. This cuts files into content-defined chunks (FastCDC: 256 KiB minimum, 1 MiB average, 8 MiB maximum), so that bytes inserted early leave later chunks unchanged. Migrations `000044_chunks.sql` and `000045_chunk_storage.sql`.

Each step works without the next: measure, then shorten transfers, then store less.

## The index

`PUT /api/v1/datasets/{id}/chunking {"enabled": true}` (`datasets.manage`), or `osp chunking DATASET on`. The dataset worker then reads each file of each version once, which on a bucket of your own costs what reading it costs. `POST .../versions/{versionID}/chunks/index` indexes one version without turning it on.

Files are keyed by SHA-256 inside an organization, so the same bytes are read once however many versions hold them. A file of 256 KiB or less is one chunk, known without reading it. Bytes that do not hash to the SHA-256 the snapshot pinned are not indexed. `GET .../chunks/status` shows the state and each file left out, with the reason. Chunks are never compared across organizations.

The index holds hashes of pieces of files. When no object holds a file any more (after an erasure, say), its chunks are forgotten within the hour.

## The savings report

`GET /api/v1/datasets/{id}/chunk-savings`, or `osp chunk-savings DATASET`:

| Field | Meaning |
|---|---|
| `logical_bytes` | Every unexpired version counted in full |
| `unique_file_bytes` | Each distinct file once: what is stored today |
| `indexed_file_bytes` | The part of that which has been indexed |
| `unique_chunk_bytes` | Each distinct chunk of those once |
| `would_save_bytes` | `indexed_file_bytes - unique_chunk_bytes` |
| `not_indexed_files`, `not_indexed_bytes` | What the report does not cover yet |

## Storing chunks once

`PUT /api/v1/datasets/{id}/chunk-storage {"enabled": true}` (`datasets.manage`), or `osp chunk-storage DATASET on`. This turns the index on too. `GET` on the same path says what it holds. A dataset under a legal hold or a retention policy is refused.

**What is taken.** The pack worker converts files of the dataset a few at a time. A file is taken only if at least a fifth of its bytes are shared with another file of the organization on any connection, so a file that shares nothing stays one object. Left whole, and looked at again later if they were simply not sharing yet:

- files under a legal hold or an Object Lock, and any file on a bucket with Object Lock on
- adopted files, which were yours before they were the platform's
- packed files, files on a colder tier, and files sharing one object with an identical upload

**How it stays safe.** Each chunk is one object at `<ready prefix>/<tenant>/chunks/<aa>/<sha256>` on the file's own connection, so a chunk is stored once per connection and never crosses organizations or leaves its region.

1. The file's chunks are written, each checked against the index as it is read from the original.
2. They are read back through the same reader everything uses, and must hash to the file's SHA-256. Only then do readers move to them.
3. The original stays for `CHUNK_SOURCE_RETIRE_AFTER` (24 hours by default), then goes, unless a hold has arrived in the meantime.

A conversion that cannot finish leaves the object as it was, and says why.

**Reading.** Every read path knows chunks: downloads, ranges, shards and streaming, content validation, malware re-scans, copies and replication, exports and scrubbing. Each chunk is read whole and checked against its SHA-256, even for a few bytes of it, so a damaged chunk is an error, never wrong bytes. A whole-file read is checked against the file's SHA-256 as well.

**What changes.** One object no longer holds the file, so:

- A chunked file gets a link streamed through the API instead of a provider or CDN link, as a packed file does. Reads cost API bandwidth.
- A read takes a request per chunk, about one per MiB. On S3 that is GET requests you pay for, and small files read slower than one object would. Four chunks are fetched ahead at once.
- The provider's own tools (lifecycle rules, console downloads, Object Lock) see chunks, not your file.

So look at the savings report first: storing chunks once pays where versions share most of their bytes and are not read all day.

**Deleting.** A chunk goes when no file needs it. A deleted file's storage counts as deleted once chunks only it needed are gone; `CHUNK_UNUSED_AFTER` (an hour by default) is how long an unused chunk waits. A chunk another file also holds stays, because it is that file's content too. So after an erasure, the pieces an erased file shared with another file remain, as part of that other file: take this into account before storing chunks once on personal data.

**Going back.** `POST /api/v1/datasets/{id}/chunk-storage/rehydrate`, or `osp chunk-storage DATASET rehydrate`, makes each file one object again and stops new conversions. The bytes are checked as they are written, and the file's provider link comes back. A file that needs an Object Lock while kept as chunks becomes one object first, by itself, and is then locked. A chunked file that is damaged and has a replica is not repaired automatically: scrubbing reports it.

## Delta downloads

`GET .../versions/{versionID}/delta?from={versionID}`, or `osp delta DATASET OLD NEW`, says what moving between two versions fetches. To do it:

```
osp delta builds 41 42 ./build-41 ./build-42
```

The new version is written to its own folder and the old copy is not changed. An unchanged file is copied, and checked first. A changed file is built from local chunks and ranged reads, with neighbouring missing chunks merged into one read. Chunks of old files that left the new version count too, so a file renamed and edited still helps. A local chunk that no longer hashes as the index says is fetched instead, so a stale copy only costs more download. A file that is not indexed is fetched whole.

**Every finished file is checked against the SHA-256 the snapshot pinned before it is put in place.**

`GET .../versions/{versionID}/chunks?path=&after=&limit=` is a file's manifest, for other clients. It needs `datasets.read`.

## Delta uploads

```
osp delta-push builds latest ./build-43
```

```python
chunks.delta_upload(client, dataset_id, base_version_id, "game.pak", "game.pak")
```

```go
client.DeltaUpload(ctx, "game.pak", osp.DeltaOptions{DatasetID: id, BaseVersionID: base, Path: "game.pak"})
```

`delta-push` makes the draft hold the folder, sending only what differs from the snapshot, and you then snapshot it. The base version should be indexed: if it is not, everything is sent, which still works. Pure Python cuts files at a few MB/s; the Go SDK is much faster.

**The file goes through the same pipeline as any upload.** The server builds it into an ordinary upload, from the chunks sent and the ones it holds, checking each. It then gets the usual SHA-256 check, malware scan, content checks and publication, and counts against quotas like any other. A file that does not hash to what was promised never becomes an upload.

**Who can reuse what.** Reusing chunks needs `uploads.write` and read access to the base version, and only chunks that snapshot holds are reused. Knowing a hash from a dataset you cannot read gets you nothing: that chunk simply has to be sent. So this cannot be used to read data by knowing its hash.

The API, for other clients:

1. `POST /api/v1/delta-uploads {"dataset_id", "base_version_id", "path", "size_bytes", "sha256", "content_type", "chunks": [{"offset", "length", "sha256"}]}` answers `{"id", "missing": [ordinals], "reused_bytes", "sent_bytes"}`. Chunks must cover the file, are at most 16 MiB, and at most 1,000,000.
2. `PUT /api/v1/delta-uploads/{id}/chunks/{ordinal}` with each missing chunk's bytes. A chunk that is not what was promised is refused.
3. `POST /api/v1/delta-uploads/{id}/complete`; **409** until every chunk has arrived.
4. `GET /api/v1/delta-uploads/{id}` answers `state` (`OPEN`, `QUEUED`, `ASSEMBLING`, `DONE`, `FAILED`, `EXPIRED`), `error`, and once built `asset_id` and `asset_status`.

An upload nobody finishes expires after a day and gives its space back.
