"""Batches for JAX from one pinned snapshot (osp.stream).

JAX reads NumPy arrays; this yields batches of them, each process of a
multi-host job reading its own share of the members (jax.process_index()
of jax.process_count()).

    from osp.stream import SnapshotStream
    from osp.jax import batches
    stream = SnapshotStream(client, dataset_id, version_id, split="train", shuffle_seed=0)
    for epoch in range(10):
        stream.set_epoch(epoch)
        for batch in batches(stream, 64, transform=lambda s: {"x": decode(s["data"]), "y": label_id(s["label"])}):
            state = train_step(state, batch)
"""


def _process():
    try:
        import jax
    except ImportError:
        return 0, 1
    return jax.process_index(), jax.process_count()


def batches(stream, batch_size, transform=None, kind="files", drop_last=True, shard=None, shards=None):
    """Yields dicts of stacked NumPy arrays (one per key of the transformed
    samples). drop_last keeps every batch the same shape, which jit wants."""
    import numpy as np

    if shard is None:
        shard, shards = _process()
    batch = []
    for sample in stream.samples(kind, shard, shards, transform):
        batch.append(sample)
        if len(batch) == batch_size:
            yield _stack(np, batch)
            batch = []
    if batch and not drop_last:
        yield _stack(np, batch)


def _stack(np, batch):
    return {key: np.stack([np.asarray(sample[key]) for sample in batch]) for key in batch[0]}
