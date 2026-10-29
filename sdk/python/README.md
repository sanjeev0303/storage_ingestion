# osp: Python SDK and CLI

Upload datasets, freeze them as snapshots, and prove later that what you
train on is exactly what a snapshot pinned. Standard library only;
PyTorch is optional (`pip install osp-sdk[torch]`).

```sh
pip install -e sdk/python
osp login --url http://localhost:8080 --email you@example.com   # asks for the password
osp ingest ./images --dataset pets        # hash, upload, wait, add as members
osp snapshot pets --key release-1         # same key, same version: retries are safe
osp verify pets latest ./images           # exit 0 only if the folder is exactly the snapshot
osp download pets 1 ./copy                # every file checked against its SHA-256; resumes
osp status                                # what processing, ingestion and imports are doing
```

The session is saved in `~/.osp/credentials.json` (readable only by you;
`OSP_CONFIG_DIR` moves it). `OSP_PASSWORD` signs in without a prompt, for
automation.

## In Python

```python
from osp import Client, ingest_folder, find_dataset, snapshot, find_version, verify_local

client = Client("http://localhost:8080")
client.login("you@example.com", password)
ingest_folder(client, "./images", "pets")
ds = find_dataset(client, "pets")
v = snapshot(client, ds["id"], "release-1")
report = verify_local(client, ds["id"], v["id"], "./images")   # matched, changed, missing, extra
```

## Training on a snapshot (PyTorch)

```python
from osp.torch import SnapshotDataset
data = SnapshotDataset(client, ds["id"], v["id"], split="train", cache_dir="/data/osp-cache",
                       transform=lambda data, member: decode(data, member["label"]))
loader = torch.utils.data.DataLoader(data, batch_size=32, num_workers=4)
```

Every file is checked against the SHA-256 the snapshot pinned before it is
yielded, so a run never trains on a file that changed afterwards. The cache
is keyed by SHA-256: later epochs read local disk, and a cached file can
never be stale. Members are shared out across DataLoader workers and
distributed ranks.

The same stream feeds TensorFlow (`osp.tf.as_tf_dataset`), Hugging Face
(`osp.hf.as_hf_dataset`) and JAX (`osp.jax.batches`). `kind="tar"` reads
WebDataset shards, and `kind="records"` reads CSV, TSV, JSON Lines and
Parquet members row by row.

## Branches and merges

```python
from osp import merge, MergeConflict

ingest_folder(client, "./relabelled", "pets", branch="relabel")   # files land in pets, pinned in pets@relabel
branch = find_dataset(client, "pets@relabel")
snapshot(client, branch["id"])                          # a merge brings in a version
try:
    merge(client, ds["id"], branch["id"])               # FAIL: stops on conflicts
except MergeConflict as conflict:
    print(conflict.result["conflict_list"])
merge(client, ds["id"], branch["id"], strategy="SOURCE")  # or TARGET; dry_run=True previews
snapshot(client, ds["id"])                              # keep the merged draft as a version
```

On the command line: `osp branch pets relabel`, `osp branches pets`,
`osp ingest ./relabelled pets --branch relabel`, `osp merge pets pets@relabel --dry-run`.

## What ingestion does

1. Hashes every file locally and writes a manifest declaring each SHA-256.
2. Creates the dataset (or reuses the one with that name) and an ingestion
   job whose idempotency key is the manifest's SHA-256: running the same
   folder again resumes that job instead of uploading twice.
3. Uploads straight to storage through presigned URLs (8 MiB parts for
   large files). The platform publishes a file only after it hashed the
   uploaded bytes itself, found the declared SHA-256 and scanned it for
   malware.
4. Adds every published file as a dataset member pinned to that exact
   version, so the next snapshot holds it.

## Tests

```sh
python -m unittest discover -s sdk/python/tests
```
