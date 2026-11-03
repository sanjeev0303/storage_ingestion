"""Cuts a file where the server would (docs/chunks.md), so that chunks of a new
build match chunks the server already holds. tests/test_fastcdc.py and the
server's tests check one test vector: the sizes, gear table and masks must not
drift from internal/platform/fastcdc.

With numpy (pip install osp[fast]) large files are cut on all cores. Without it,
it works the same at a few MB/s. The Go SDK is the fastest.
"""

import concurrent.futures
import hashlib
import os

try:
    import numpy
except ImportError:
    numpy = None

MIN_SIZE = 256 << 10
AVG_SIZE = 1 << 20
MAX_SIZE = 8 << 20
_STRICT = 0xFFFFFC0000000000
_LOOSE = 0xFFFC000000000000
_WORD = (1 << 64) - 1
# A block of the loose search: its mask hits about every 256 KiB.
_BLOCK = 256 << 10
# Files larger than two of these are cut on all cores, a segment per worker at a time.
_SEGMENT = 4 << 20
_WORKERS = min(6, os.cpu_count() or 1)

_GEAR = [int.from_bytes(hashlib.sha256(b"gear" + bytes([i])).digest()[:8], "little") for i in range(256)]
_NP_GEAR = numpy.array(_GEAR, dtype=numpy.uint64) if numpy is not None else None


def _cut_python(data):
    n = len(data)
    if n <= MIN_SIZE:
        return n
    n = min(n, MAX_SIZE)
    normal = min(AVG_SIZE, n)
    gear = _GEAR
    h = 0
    i = MIN_SIZE
    while i < normal:
        h = ((h << 1) + gear[data[i]]) & _WORD
        if not h & _STRICT:
            return i + 1
        i += 1
    while i < n:
        h = ((h << 1) + gear[data[i]]) & _WORD
        if not h & _LOOSE:
            return i + 1
        i += 1
    return n


def _hashes(values):
    """The gear hash at every position, from zero at the first. Each hash is the
    last 64 gears, each shifted by its age, so doubling the window six times
    gives them all at once instead of one byte at a time."""
    h = _NP_GEAR[values]
    for shift in (1, 2, 4, 8, 16, 32):
        h[shift:] += h[:-shift] << numpy.uint64(shift)
    return h


def _cut_numpy(data):
    n = len(data)
    if n <= MIN_SIZE:
        return n
    n = min(n, MAX_SIZE)
    normal = min(AVG_SIZE, n)
    values = numpy.frombuffer(data, dtype=numpy.uint8, count=n)
    # The strict mask to the average size, the loose one after. Most chunks end
    # soon after the average, so the rest is looked at a block at a time.
    h = _hashes(values[MIN_SIZE:normal])
    hits = numpy.flatnonzero((h & numpy.uint64(_STRICT)) == 0)
    if hits.size:
        return MIN_SIZE + int(hits[0]) + 1
    start = normal
    while start < n:
        end = min(start + _BLOCK, n)
        # The 63 bytes before the block are still in the window, except that the
        # hash began at MIN_SIZE.
        back = max(start - 63, MIN_SIZE)
        h = _hashes(values[back:end])[start - back:]
        hits = numpy.flatnonzero((h & numpy.uint64(_LOOSE)) == 0)
        if hits.size:
            return start + int(hits[0]) + 1
        start = end
    return n


cut = _cut_numpy if numpy is not None else _cut_python


def chunks(stream):
    """Yields (offset, bytes) for each chunk of a binary stream."""
    buf = b""
    start = 0
    offset = 0
    done = False
    while True:
        if not done and len(buf) - start < MAX_SIZE:
            buf = buf[start:]
            start = 0
            while not done and len(buf) < 2 * MAX_SIZE:
                more = stream.read(2 * MAX_SIZE - len(buf))
                if more:
                    buf += more
                else:
                    done = True
        if start == len(buf):
            return
        n = cut(memoryview(buf)[start:start + MAX_SIZE])
        yield offset, buf[start:start + n]
        start += n
        offset += n


def _hash_file(path):
    whole = hashlib.sha256()
    with open(path, "rb") as f:
        while block := f.read(1 << 20):
            whole.update(block)
    return whole.hexdigest()


def _hits(path, start, end):
    """Where in bytes start to end the hash, with its full window behind it, meets
    the loose mask, and which of those meet the strict one too (it is the stricter
    of the two, so every strict hit is a loose hit)."""
    back = max(start - 63, 0)
    with open(path, "rb") as f:
        f.seek(back)
        raw = f.read(end - back)
    h = _hashes(numpy.frombuffer(raw, dtype=numpy.uint8))[start - back:]
    at = numpy.flatnonzero((h & numpy.uint64(_LOOSE)) == 0)
    return at + start, (h[at] & numpy.uint64(_STRICT)) == 0


def _parallel_cuts(path, size):
    """Every chunk's end, using all the cores. A cut depends only on the 64 bytes
    before it, so the hits can be found in any order and walked afterwards. Only
    the 63 positions just after a chunk's minimum size see a shorter window, and
    those are worked out by hand."""
    spans = [(s, min(s + _SEGMENT, size)) for s in range(0, size, _SEGMENT)]
    with concurrent.futures.ThreadPoolExecutor(_WORKERS) as pool:
        parts = list(pool.map(lambda span: _hits(path, *span), spans))
    loose = numpy.concatenate([p[0] for p in parts])
    strict = loose[numpy.concatenate([p[1] for p in parts])]
    cuts = []
    at = 0
    with open(path, "rb") as f:
        while at < size:
            left = size - at
            if left <= MIN_SIZE:
                cuts.append(size)
                break
            limit = at + min(left, MAX_SIZE)
            normal = at + min(AVG_SIZE, limit - at)
            first = at + MIN_SIZE
            end = None
            # A window that began at the minimum size.
            f.seek(first)
            near = f.read(min(63, limit - first))
            h = 0
            for i, byte in enumerate(near):
                h = ((h << 1) + _GEAR[byte]) & _WORD
                if not h & (_STRICT if first + i < normal else _LOOSE):
                    end = first + i + 1
                    break
            if end is None:
                free = first + len(near)
                i = numpy.searchsorted(strict, free)
                if i < strict.size and strict[i] < normal:
                    end = int(strict[i]) + 1
                else:
                    i = numpy.searchsorted(loose, max(free, normal))
                    end = int(loose[i]) + 1 if i < loose.size and loose[i] < limit else limit
            cuts.append(end)
            at = end
    return cuts


def _hash_span(path, offset, length):
    with open(path, "rb") as f:
        f.seek(offset)
        return hashlib.sha256(f.read(length)).hexdigest()


def manifest(path):
    """A file's chunks as the API wants them, and its size and SHA-256."""
    size = os.path.getsize(path)
    if numpy is not None and size > 2 * _SEGMENT:
        cuts = _parallel_cuts(path, size)
        spans = [(start, end - start) for start, end in zip([0] + cuts[:-1], cuts)]
        with concurrent.futures.ThreadPoolExecutor(_WORKERS) as pool:
            digests = list(pool.map(lambda s: _hash_span(path, *s), spans))
            whole = pool.submit(_hash_file, path).result()
        return [{"offset": o, "length": n, "sha256": d} for (o, n), d in zip(spans, digests)], size, whole
    whole = hashlib.sha256()
    out = []
    size = 0
    with open(path, "rb") as f:
        for offset, data in chunks(f):
            whole.update(data)
            out.append({"offset": offset, "length": len(data), "sha256": hashlib.sha256(data).hexdigest()})
            size += len(data)
    return out, size, whole.hexdigest()
