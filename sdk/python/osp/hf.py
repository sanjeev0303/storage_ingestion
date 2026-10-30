"""A Hugging Face datasets.IterableDataset over one pinned snapshot
(osp.stream).

    from osp.stream import SnapshotStream
    from osp.hf import as_hf_dataset
    ds = as_hf_dataset(SnapshotStream(client, dataset_id, version_id, split="train"))
    for row in ds: ...

Each shard stream (or, reading file by file, each member) is one shard of
the IterableDataset, so a torch DataLoader with num_workers, and
datasets.distributed.split_dataset_by_node, share them out with no file
read twice. ds.shuffle(seed=...) shuffles their order and a buffer of
samples, as for any IterableDataset.
"""

import json


def as_hf_dataset(stream, kind="files", transform=None):
    try:
        from datasets import IterableDataset
    except ImportError:
        raise ImportError("osp.hf needs Hugging Face datasets: pip install datasets") from None

    def finish(sample):
        if isinstance(sample.get("metadata"), dict):
            # One schema for every row: metadata keys differ between files.
            sample = dict(sample, metadata=json.dumps(sample["metadata"], sort_keys=True))
        return transform(sample) if transform else sample

    if kind == "samples" or (kind == "files" and stream.read_mode == "shards"):
        def by_shard(shards):
            for sample in stream.shard_samples(kind, shards):
                yield finish(sample)
        return IterableDataset.from_generator(by_shard, gen_kwargs={"shards": stream.share_shards()})

    def generate(members):
        for member in members:
            for sample in stream.samples_of(member, kind):
                yield finish(sample)

    return IterableDataset.from_generator(generate, gen_kwargs={"members": stream.share()})
