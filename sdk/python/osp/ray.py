"""Ray Data over snapshots (docs/streaming.md).

    import osp.ray
    ds = osp.ray.read_snapshot(client, dataset_id, version_id, kind="samples")
    out = ds.map(transform)
    version = osp.ray.write_snapshot(out, client, "cleaned", parents=[version_id],
                                     transformation={"name": "dedupe", "version": "1.2"})

read_snapshot makes one read task per shard, so Ray reads shards (and the
packs behind them) in parallel across the cluster, each file checked
against its pinned SHA-256. write_snapshot uploads the rows' files from
the workers and freezes them as a snapshot of a dataset derived from the
parents: its lineage names them and the transformation, without copying
their members.
"""

import json
import os
import tempfile

from . import snapshots
from .ingest import ingest_folder
from .stream import SnapshotStream


def _ray_data():
    try:
        import ray.data
    except ImportError:
        raise ImportError("osp.ray needs Ray Data: pip install 'ray[data]'") from None
    return ray.data


class ShardReader:
    """A read task: the samples of one shard."""

    def __init__(self, stream, kind):
        self.stream, self.kind = stream, kind

    def __call__(self, task):
        out = []
        for sample in self.stream.shard_samples(self.kind, [task]):
            if isinstance(sample.get("metadata"), dict):
                # One schema for every row: metadata keys differ between files.
                sample = dict(sample, metadata=json.dumps(sample["metadata"], sort_keys=True))
            out.append(sample)
        return out


def read_snapshot(client, dataset_id, version_id, kind="files", split=None, **options):
    """A ray.data.Dataset of the snapshot's samples ("files" or "samples",
    as SnapshotStream yields them), one block per shard."""
    data = _ray_data()
    stream = SnapshotStream(client, dataset_id, version_id, split=split, read="shards", **options)
    tasks = [{"ordinals": s["ordinals"]} for s in stream.shard_plan()]
    return data.from_items(tasks, override_num_blocks=max(1, len(tasks))).flat_map(ShardReader(stream, kind))


class BatchWriter:
    """A write task: one batch of rows uploaded as files and pinned as
    members of the dataset."""

    def __init__(self, client, dataset, path_column, data_column, concurrency):
        self.client, self.dataset = client, dataset
        self.path_column, self.data_column, self.concurrency = path_column, data_column, concurrency

    def __call__(self, batch):
        paths, blobs = batch[self.path_column], batch[self.data_column]
        with tempfile.TemporaryDirectory(prefix="osp-ray-") as folder:
            for path, data in zip(paths, blobs):
                parts = snapshots.safe_parts(str(path))
                target = os.path.join(folder, *parts)
                os.makedirs(os.path.dirname(target), exist_ok=True)
                with open(target, "wb") as f:
                    f.write(data if isinstance(data, bytes) else bytes(data))
            summary = ingest_folder(self.client, folder, self.dataset["name"], concurrency=self.concurrency,
                                    into=self.dataset)
        if summary["failed"]:
            raise RuntimeError(f"{len(summary['failed'])} files could not be uploaded: {summary['failed'][:5]}")
        return {"files": [len(paths)]}


def derived_dataset(client, name, parents=(), transformation=None):
    """Creates (or finds) the dataset rows are written to: derived from the
    parent versions, as lineage only, when there are any."""
    body = {"name": name}
    if parents:
        body["derived_from"] = {"version_ids": list(parents), "copy_members": False}
        if transformation:
            body["derived_from"]["transformation"] = transformation
    return client.post("/api/v1/datasets", body)


def write_snapshot(ds, client, dataset, parents=(), transformation=None, path_column="path", data_column="data",
                   batch_size=1000, concurrency=8, idempotency_key=None):
    """Writes the rows of a ray.data.Dataset (a path and bytes per row) as
    files of dataset, then freezes them as a new snapshot and returns it."""
    target = derived_dataset(client, dataset, parents, transformation)
    written = ds.map_batches(BatchWriter(client, target, path_column, data_column, concurrency),
                             batch_size=batch_size, batch_format="numpy")
    written.materialize()
    return snapshots.snapshot(client, target["id"], idempotency_key)
