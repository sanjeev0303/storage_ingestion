"""A snapshot as a read-only folder (FUSE, Linux and macOS).

    osp mount my-dataset latest /mnt/snapshot

Tools that only read files (a viewer, ffmpeg, a legacy loader) read the
snapshot in place: a file's bytes are fetched as they are read, in ranges,
never the whole snapshot. Needs fusepy (pip install fusepy) and FUSE
(libfuse, or macFUSE). Use shard streams for training: a mount reads file
by file.
"""

import errno
import stat
import time

from .zarr import SnapshotIndex


class SnapshotFS:
    """The file system's answers, apart from FUSE: a tree of the
    snapshot's paths, sizes, and ranged reads."""

    def __init__(self, index):
        self.index = index
        self.files = index.members
        self.dirs = {""}
        for path in self.files:
            parts = path.split("/")
            for i in range(1, len(parts)):
                self.dirs.add("/".join(parts[:i]))
        self.mounted = time.time()

    def attr(self, path):
        key = path.strip("/")
        base = {"st_uid": 0, "st_gid": 0, "st_atime": self.mounted, "st_mtime": self.mounted, "st_ctime": self.mounted}
        if key in self.dirs:
            return dict(base, st_mode=stat.S_IFDIR | 0o555, st_nlink=2, st_size=0)
        m = self.files.get(key)
        if m is None:
            raise OSError(errno.ENOENT, "no such file in the snapshot", path)
        return dict(base, st_mode=stat.S_IFREG | 0o444, st_nlink=1, st_size=m["size_bytes"])

    def list(self, path):
        key = path.strip("/")
        if key not in self.dirs:
            raise OSError(errno.ENOTDIR, "not a folder in the snapshot", path)
        return [".", ".."] + self.index.list_dir(key)

    def read(self, path, size, offset):
        data = self.index.get(path.strip("/"), offset, offset + size)
        if data is None:
            raise OSError(errno.ENOENT, "no such file in the snapshot", path)
        return data


def mount(client, dataset_id, version_id, mountpoint, foreground=True):
    """Mounts the snapshot read-only at mountpoint until unmounted."""
    try:
        from fuse import FUSE, FuseOSError, Operations
    except (ImportError, OSError):
        raise ImportError("osp mount needs fusepy and FUSE: pip install fusepy, and libfuse or macFUSE") from None
    fs = SnapshotFS(SnapshotIndex(client, dataset_id, version_id))

    def answer(fn, *args):
        try:
            return fn(*args)
        except OSError as err:
            raise FuseOSError(err.errno) from None

    class Ops(Operations):
        def getattr(self, path, fh=None):
            return answer(fs.attr, path)

        def readdir(self, path, fh):
            return answer(fs.list, path)

        def open(self, path, flags):
            if flags & 3:  # O_WRONLY or O_RDWR
                raise FuseOSError(errno.EROFS)
            answer(fs.attr, path)
            return 0

        def read(self, path, size, offset, fh):
            return answer(fs.read, path, size, offset)

    FUSE(Ops(), mountpoint, foreground=foreground, ro=True, nothreads=False)
