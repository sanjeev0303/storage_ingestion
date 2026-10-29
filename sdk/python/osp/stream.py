"""Streaming one snapshot into training, whatever the framework.

SnapshotStream is the core the adapters wrap (osp.torch, osp.tf, osp.hf,
osp.jax). It holds the members of one dataset version: one split if asked,
members whose bytes were erased left out. It shares them out between
workers and machines, and reads each file only after checking its bytes
against the SHA-256 the snapshot pinned.

Three ways to read, by kind:
  "files"   - one sample per member: its bytes, path, label, split, metadata;
  "tar"     - WebDataset shards: each member is a tar file, and every group
              of entries sharing a name before the first dot is one sample;
  "records" - tabular members (CSV, TSV, JSON Lines, Parquet): one sample per
              row.

A file larger than memory is never held whole: it is streamed to local
disk in 1 MiB blocks while it is hashed, checked, and only then read. With
cache_dir, files are kept by their SHA-256, so later epochs read from disk
and a cached file can never be stale.
"""

import csv
import hashlib
import json
import os
import random
import shutil
import tarfile
import tempfile
import urllib.request

from . import snapshots

BLOCK = 1024 * 1024
KINDS = ("files", "tar", "records")


class SnapshotStream:
    def __init__(self, client, dataset_id, version_id, split=None, cache_dir=None, shuffle_seed=None):
        self.client = client
        # Kept so a training run can name the version it read (osp.runs).
        self.dataset_id, self.version_id = dataset_id, version_id
        self.cache_dir = cache_dir
        self.shuffle_seed = shuffle_seed
        self.epoch = 0
        chosen = [m for m in snapshots.members(client, dataset_id, version_id)
                  if split is None or m.get("split") == split]
        # Members whose bytes were erased (a deletion request) are left out;
        # erased says how many, so a training run can record it.
        self.members = [m for m in chosen if m.get("availability", "AVAILABLE") == "AVAILABLE"]
        self.erased = len(chosen) - len(self.members)

    def __len__(self):
        return len(self.members)

    def set_epoch(self, epoch):
        """Changes the shuffle between epochs, the same way on every worker."""
        self.epoch = epoch

    def share(self, shard=0, shards=1):
        """This worker's members: the same shuffle everywhere, then every
        shards-th member from shard, so each member is read once per epoch
        across all workers."""
        order = list(range(len(self.members)))
        if self.shuffle_seed is not None:
            random.Random(self.shuffle_seed + self.epoch).shuffle(order)
        return [self.members[i] for i in order[shard::shards]]

    def samples(self, kind="files", shard=0, shards=1, transform=None):
        """Yields this worker's samples, transform(sample) when given."""
        if kind not in KINDS:
            raise ValueError(f"kind must be one of {KINDS}")
        for member in self.share(shard, shards):
            for sample in self.samples_of(member, kind):
                yield transform(sample) if transform else sample

    def samples_of(self, member, kind):
        if kind == "files":
            yield {"data": self.read(member), "path": member["path"], "label": member.get("label", ""),
                   "split": member.get("split", ""), "metadata": member.get("metadata") or {}}
        elif kind == "tar":
            with self.local(member) as path:
                yield from tar_samples(path, member)
        else:
            with self.local(member) as path:
                yield from records(path, member)

    def read(self, member):
        """A member's bytes, checked."""
        with self.local(member) as path:
            with open(path, "rb") as f:
                return f.read()

    def local(self, member):
        """A context giving the path of a local, checked copy of the member.
        Without cache_dir the copy is removed afterwards."""
        return _LocalCopy(self, member)

    def _cache_path(self, digest):
        if not self.cache_dir or len(digest) != 64:
            return None
        return os.path.join(self.cache_dir, digest[:2], digest)

    def _fetch_to(self, member, target):
        """Streams the member into target, hashing as it goes; replaces
        target only with bytes that match the pinned SHA-256."""
        os.makedirs(os.path.dirname(target), exist_ok=True)
        url = snapshots.download_url(self.client, member["version_id"])
        h = hashlib.sha256()
        fd, tmp = tempfile.mkstemp(dir=os.path.dirname(target), prefix=".osp-")
        try:
            with os.fdopen(fd, "wb") as out, urllib.request.urlopen(url, timeout=600) as resp:
                while True:
                    block = resp.read(BLOCK)
                    if not block:
                        break
                    h.update(block)
                    out.write(block)
            if h.hexdigest() != member.get("checksum_sha256"):
                raise snapshots.IntegrityError(member["path"], member.get("checksum_sha256"), h.hexdigest())
            os.replace(tmp, target)
        finally:
            if os.path.exists(tmp):
                os.remove(tmp)


class _LocalCopy:
    def __init__(self, stream, member):
        self.stream, self.member, self.tmpdir = stream, member, None

    def __enter__(self):
        digest = self.member.get("checksum_sha256", "")
        cached = self.stream._cache_path(digest)
        if cached:
            if os.path.exists(cached) and sha256_of(cached) == digest:
                return cached
            self.stream._fetch_to(self.member, cached)
            return cached
        self.tmpdir = tempfile.mkdtemp(prefix="osp-stream-")
        path = os.path.join(self.tmpdir, "member")
        self.stream._fetch_to(self.member, path)
        return path

    def __exit__(self, *exc):
        if self.tmpdir:
            shutil.rmtree(self.tmpdir, ignore_errors=True)
        return False


def sha256_of(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        while True:
            block = f.read(BLOCK)
            if not block:
                return h.hexdigest()
            h.update(block)


def tar_samples(path, member):
    """WebDataset samples of one checked tar shard: entries grouped by the
    part of their name before the first dot ("0001.jpg" and "0001.cls" make
    one sample with keys "jpg" and "cls"). Read in streaming mode, one sample
    at a time."""
    current, sample = None, {}
    with tarfile.open(path, mode="r|*") as tar:
        for entry in tar:
            if not entry.isfile():
                continue
            base = os.path.basename(entry.name)
            if base.startswith("."):
                continue
            key, _, ext = base.partition(".")
            key = os.path.join(os.path.dirname(entry.name), key)
            if key != current and sample:
                yield sample
                sample = {}
            current = key
            if not sample:
                sample = {"__key__": key, "__shard__": member["path"]}
            sample[ext] = tar.extractfile(entry).read()
    if sample:
        yield sample


def records(path, member):
    """Rows of one checked tabular member, by its extension: CSV, TSV, JSON
    Lines (or a JSON array), or Parquet (needs pyarrow), read in batches."""
    name = member["path"].lower()
    if name.endswith(".parquet"):
        try:
            import pyarrow.parquet as pq
        except ImportError:
            raise ImportError("reading Parquet needs pyarrow: pip install pyarrow") from None
        for batch in pq.ParquetFile(path).iter_batches(batch_size=4096):
            yield from batch.to_pylist()
    elif name.endswith((".csv", ".tsv")):
        with open(path, newline="", encoding="utf-8") as f:
            yield from csv.DictReader(f, delimiter="\t" if name.endswith(".tsv") else ",")
    elif name.endswith((".jsonl", ".ndjson")):
        with open(path, encoding="utf-8") as f:
            for line in f:
                if line.strip():
                    yield json.loads(line)
    elif name.endswith(".json"):
        with open(path, encoding="utf-8") as f:
            data = json.load(f)
        yield from data if isinstance(data, list) else [data]
    else:
        raise ValueError(f"{member['path']}: not a CSV, TSV, JSON Lines, JSON or Parquet file")

