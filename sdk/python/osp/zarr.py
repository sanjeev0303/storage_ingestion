"""A read-only Zarr v3 store over a snapshot (docs/streaming.md).

    import zarr
    from osp.zarr import SnapshotStore
    group = zarr.open_group(SnapshotStore(client, dataset_id, version_id, prefix="scan.zarr"), mode="r")

Each Zarr key is a snapshot member under prefix ("scan.zarr/zarr.json",
"scan.zarr/c/0/0"). Reads ask the platform for just the bytes they need
(HTTP ranges), so a sharded array reads a chunk out of a shard without
fetching the shard, whatever pack the shard is in. A whole read is checked
against the member's pinned SHA-256; a ranged read cannot be, and is
checked by the platform against its pack. The snapshot never changes, so
the store is safe to cache.
"""

from . import snapshots


class SnapshotIndex:
    """The snapshot's members under prefix, by key: what the store reads."""

    def __init__(self, client, dataset_id, version_id, prefix=""):
        self.client = client
        self.prefix = prefix.strip("/") + "/" if prefix.strip("/") else ""
        self.members = {}
        for m in snapshots.members(client, dataset_id, version_id):
            if m.get("availability", "AVAILABLE") == "AVAILABLE" and m["path"].startswith(self.prefix):
                self.members[m["path"][len(self.prefix):]] = m

    def get(self, key, start=None, end=None, suffix=None):
        """The bytes of key, or of bytes start to end (exclusive), or the
        last suffix bytes; None when the snapshot has no such key."""
        m = self.members.get(key)
        if m is None:
            return None
        size = m["size_bytes"]
        if suffix is not None:
            start, end = max(size - suffix, 0), size
        if start is None and end is None:
            return snapshots.fetch_member(self.client, m)
        start, end = start or 0, size if end is None else min(end, size)
        if start >= end:
            return b""
        path = f"/api/v1/assets/{m['version_id']}/content"
        with self.client.stream("GET", path, headers={"Range": f"bytes={start}-{end - 1}"}) as resp:
            data = resp.read()
        if len(data) != end - start:
            raise snapshots.IntegrityError(m["path"], f"{end - start} bytes", f"{len(data)} bytes")
        return data

    def list(self, prefix=""):
        return sorted(k for k in self.members if k.startswith(prefix))

    def list_dir(self, prefix=""):
        """The keys and folders right under prefix, as Zarr lists them."""
        prefix = prefix.strip("/") + "/" if prefix.strip("/") else ""
        seen = set()
        for key in self.members:
            if key.startswith(prefix):
                seen.add(key[len(prefix):].split("/", 1)[0])
        return sorted(seen)


def SnapshotStore(client, dataset_id, version_id, prefix=""):
    """A zarr.abc.store.Store (Zarr 3) reading from the snapshot."""
    try:
        from zarr.abc.store import ByteRequest, OffsetByteRequest, RangeByteRequest, Store, SuffixByteRequest
        from zarr.core.buffer import default_buffer_prototype
    except ImportError:
        raise ImportError("osp.zarr needs Zarr 3: pip install 'zarr>=3'") from None
    index = SnapshotIndex(client, dataset_id, version_id, prefix)

    class _Store(Store):
        supports_writes = False
        supports_deletes = False
        supports_partial_writes = False
        supports_listing = True

        def __init__(self):
            super().__init__(read_only=True)

        def __eq__(self, other):
            return isinstance(other, _Store) and other is self

        async def get(self, key, prototype=None, byte_range: ByteRequest | None = None):
            if isinstance(byte_range, RangeByteRequest):
                data = index.get(key, byte_range.start, byte_range.end)
            elif isinstance(byte_range, OffsetByteRequest):
                data = index.get(key, byte_range.offset, None)
            elif isinstance(byte_range, SuffixByteRequest):
                data = index.get(key, suffix=byte_range.suffix)
            else:
                data = index.get(key)
            if data is None:
                return None
            return (prototype or default_buffer_prototype()).buffer.from_bytes(data)

        async def get_partial_values(self, prototype, key_ranges):
            return [await self.get(key, prototype, r) for key, r in key_ranges]

        async def exists(self, key):
            return key in index.members

        async def set(self, key, value):
            raise PermissionError("a snapshot is read-only")

        async def delete(self, key):
            raise PermissionError("a snapshot is read-only")

        async def list(self):
            for key in index.list():
                yield key

        async def list_prefix(self, prefix):
            for key in index.list(prefix):
                yield key

        async def list_dir(self, prefix):
            for name in index.list_dir(prefix):
                yield name

    return _Store()
