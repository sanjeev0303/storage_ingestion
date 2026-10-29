"""A PyTorch IterableDataset over one pinned dataset snapshot.

Training reads exactly the bytes the snapshot pinned: every file is checked
against its SHA-256 before anything reads it (osp.stream). Each member is
read once per epoch across all DataLoader workers and distributed ranks.

    from osp import Client
    from osp.torch import SnapshotDataset
    ds = SnapshotDataset(client, dataset_id, version_id, cache_dir="/data/cache",
                         transform=lambda sample: decode(sample["data"]))
    loader = torch.utils.data.DataLoader(ds, batch_size=32, num_workers=4)

kind="tar" streams WebDataset shards, kind="records" the rows of CSV, JSON
Lines or Parquet members.
"""

from .stream import SnapshotStream

try:
    import torch.utils.data as _tud
except ImportError:  # the rest of the SDK works without torch
    _tud = None


def _base():
    if _tud is None:
        raise ImportError("osp.torch needs PyTorch: pip install torch")
    return _tud.IterableDataset


class SnapshotDataset(_base() if _tud is not None else object):
    """Yields transform(sample) for every sample of the snapshot, or the
    sample: for kind "files" a dict with the bytes and the member's path,
    label, split and metadata.

    split keeps only members of that split. With shuffle_seed, the order is
    shuffled the same way on every worker, then shared out."""

    def __init__(self, client, dataset_id, version_id, transform=None, cache_dir=None, split=None,
                 shuffle_seed=None, kind="files"):
        _base()
        super().__init__()
        self.stream = SnapshotStream(client, dataset_id, version_id, split=split, cache_dir=cache_dir,
                                     shuffle_seed=shuffle_seed)
        self.transform = transform
        self.kind = kind
        # How many members' bytes were erased and are left out, for the
        # run's record.
        self.erased = self.stream.erased

    def __len__(self):
        """Members, not samples: a tar shard or a table holds many."""
        return len(self.stream)

    def set_epoch(self, epoch):
        """Changes the shuffle between epochs (as DistributedSampler does)."""
        self.stream.set_epoch(epoch)

    def __iter__(self):
        shard, shards = _shard()
        return self.stream.samples(self.kind, shard, shards, self.transform)


def _shard():
    """This process's share: DataLoader worker within distributed rank."""
    worker, workers = 0, 1
    info = _tud.get_worker_info()
    if info is not None:
        worker, workers = info.id, info.num_workers
    rank, world = 0, 1
    try:
        import torch.distributed as dist
        if dist.is_available() and dist.is_initialized():
            rank, world = dist.get_rank(), dist.get_world_size()
    except ImportError:
        pass
    return rank * workers + worker, world * workers
