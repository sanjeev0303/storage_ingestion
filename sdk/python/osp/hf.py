"""A Hugging Face datasets.IterableDataset over one pinned snapshot
(osp.stream).

    from osp.stream import SnapshotStream
    from osp.hf import as_hf_dataset
    ds = as_hf_dataset(SnapshotStream(client, dataset_id, version_id, split="train"))
    for row in ds: ...

Each member is one shard of the IterableDataset, so a torch DataLoader with
num_workers, and datasets.distributed.split_dataset_by_node, share the
members out with no file read twice. ds.shuffle(seed=...) shuffles the
member order and a buffer of samples, as for any IterableDataset.
"""

import json


def as_hf_dataset(stream, kind="files", transform=None):
    try:
        from datasets import IterableDataset
    except ImportError:
        raise ImportError("osp.hf needs Hugging Face datasets: pip install datasets") from None

    def generate(members):
        for member in members:
            for sample in stream.samples_of(member, kind):
                if kind == "files":
                    # One schema for every row: metadata keys differ between files.
                    sample = dict(sample, metadata=json.dumps(sample["metadata"], sort_keys=True))
                yield transform(sample) if transform else sample

    return IterableDataset.from_generator(generate, gen_kwargs={"members": stream.share()})
