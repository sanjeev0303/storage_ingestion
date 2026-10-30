# Streaming snapshots to training

A data loader that reads small files one by one waits on each request's time to first byte, and the GPUs wait with it. A snapshot is read here a **shard** at a time instead: a run of its files, in path order, served as one response, with files packed side by side read from their pack in one request. Every file is checked against the SHA-256 the snapshot pinned, by the server as it reads and by the SDK as it receives. Migration `000036_pack_streaming.sql`.

## Shards and shard streams

- `GET /api/v1/datasets/{id}/versions/{versionID}/shards?target_bytes=&cursor=` (`datasets.read`) splits the snapshot, in path order, into shards of about `target_bytes` (default 128 MiB, 1 MiB to 4 GiB, at most 10,000 files). A shard never splits a **sample**: files sharing a path before the first dot of the name (`a/0001.jpg`, `a/0001.cls.txt`). Each shard lists its `ordinals`, `files`, `bytes`, how many `packs` it reads, and its first and last path. Erased members are left out.
- `POST /api/v1/datasets/{id}/versions/{versionID}/stream {"ordinals": [...]}` (`datasets.read` and `assets.download`) answers a tar of those members in that order. Each entry's PAX records carry `OSP.sha256`, `OSP.ordinal` and `OSP.asset_id`.
  - Every file is authorized, audited (a read event each) and counted against the egress budget before the first byte. A refusal answers like a download would.
  - Consecutive files that sit in one pack, at most 1 MiB apart, are read with one ranged request on the pack. Other files are read on their own, 8 ahead at a time so their times to first byte overlap (each up to 8 MiB held in memory while it waits its turn), failing over to a verified replica (docs/copy-engine.md). Each stream logs how long authorizing took and how long it ran.
  - Each file is re-hashed as it streams. If one is not the bytes published, or storage fails, the connection is cut: the tar never ends cleanly, so a reader cannot take it as complete.

Ranged reads of one file stay where they were: `GET /api/v1/assets/{id}/content` with `Range` (or a presigned URL, which the provider serves ranges from) reads a slide tile or a Zarr chunk without the whole file. `Repr-Digest` gives the whole file's SHA-256 on every range.

## Packing for streams

- **Path order.** Packing takes an organization's small files in catalog path order, so a sample's files land in one pack next to each other, and a full pack ends between samples.
- **Target pack size.** `GET/PUT /api/v1/packing-settings {"target_pack_bytes"}` (`org.read` / `org.manage`): 1 MiB to 10 GiB, within the platform's largest pack; 0 returns to the platform's default.
- **Repack.** `POST /api/v1/datasets/{id}/repacks` (`datasets.manage`) gathers the packed files of a dataset (its draft and every snapshot) into new packs in path order, of the target size. One runs per dataset at a time (409 otherwise); `GET .../repacks` lists them, `POST .../repacks/{id}/cancel` stops one. The packing worker plans as many packs each pass as the organization's free pack slots allow and resumes from the request's cursor. Each new pack is built and verified like any other. Publishing it moves the files out of their old packs in the same transaction, and a pack left serving nothing retires. Files keep their asset IDs and catalog entries; a chunk already one whole pack in order is left alone.

## Verification

- **Signed manifests.** `GET /api/v1/datasets/{id}/versions/{versionID}/signed-manifest` answers `body` (the exact signed bytes: dataset, version, member count, total bytes and the canonical manifest's SHA-256), `algorithm`, `key_id` and `signature`, an Ed25519 signature with the key that signs disposition certificates (`CERTIFICATE_SIGNING_KEY`, published at `GET /api/v1/certificates/public-key`). Without a key the body comes unsigned. The canonical manifest is each member's line (docs/datasets.md) in ordinal order, so anyone can recompute it from the member listing.
- **Readiness.** Built-in profiles version 5 add `small_files`: a warning when the version's median file is under 1 MiB and fewer than half its files are packed (`median_below`, `packed_share`). The report's sizes carry `median_bytes` and `packed_members`.
- **Runs.** `POST /api/v1/training-runs/{runID}/verifications {"version_id", "host", "sample_rate", "checked", "failed", "failures": [{"path", "reason"}]}` records what a data loader re-hashed and on which host (the first 100 failures are kept). A report with failures is audited as `training_run.verification_failed`. `GET` lists them.
- **Checkpoints.** `POST /api/v1/training-runs/{runID}/checkpoints {"step", "version_id", "host"}` records a checkpoint: a snapshot of the files the run saved at a step, written by `host`. `POST .../checkpoints/{id}/verifications {"host", "failures"}` records a re-read of every file, which must come from **another host** (409 `same_host` otherwise), so a fault in the writer's memory or CPU is not repeated by the check. `GET .../checkpoints` lists them, newest step first, with `last_verified`: the step to roll back to.
- **Derived output.** `derived_from.copy_members: false` on `POST /api/v1/datasets` makes a dataset whose lineage names its source versions and transformation without copying their members, for a transformation's output written back.

## The SDK

- `SnapshotStream` (and the PyTorch, TensorFlow, JAX and Hugging Face adapters over it) reads through shard streams by default. Workers share shards; with `shuffle_seed` the shard order is shuffled the same way on every worker, and a buffer of samples (`shuffle_buffer`, 256) within them. `prefetch` (2) reads shards ahead on threads. `kind="samples"` yields WebDataset-style samples (`{"__key__", "jpg", "cls.txt", ...}`). `read="files"`, or a `cache_dir`, reads file by file as before.
- `verify` sets the share of files re-hashed (1.0 by default; a deterministic sample by path below that). `on_corrupt="raise"` stops at the first bad file; `"skip"` leaves it out and keeps it in `stream.verifier`, and `stream.report(run_id)` records it on the run with this host's name.
- `osp verify <dataset> <version>` with no folder checks the signature (with `cryptography` installed; it says when it could not), that the members listed hash to the signed manifest, and every file read back (`--sample 0.1` for a share), with a reason for each failure. `--run` records it. With a folder it checks the folder, as before.
- `osp bench <dataset> [version]` finds the number of parallel shard streams this machine reads fastest with (doubling while that adds 10%), with files/s, MB/s and time to first byte; `--compare N` reads N files one by one for comparison.
- `osp shards`, `osp repack`, `osp packing --target-bytes`, `osp checkpoint add|verify|list`.
- `osp.ray.read_snapshot(...)` is a Ray Data dataset with one read task per shard; `osp.ray.write_snapshot(ds, client, name, parents=[...], transformation={...})` uploads rows (`path`, `data`) from the workers and freezes them as a snapshot derived from the parents.
- `osp.zarr.SnapshotStore(client, dataset_id, version_id, prefix="scan.zarr")` is a read-only Zarr 3 store: keys are members under the prefix, partial reads are HTTP ranges, so a sharded array reads a chunk without its shard.
- `osp mount <dataset> <version> <folder>` mounts a snapshot read-only with FUSE (Linux, macOS; `pip install fusepy`) for tools that only read files. It reads file by file: use shard streams for training.
