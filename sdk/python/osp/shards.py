"""Shard streams: a snapshot read a shard at a time (docs/streaming.md).

A shard is a run of a snapshot's files in path order, served as one tar:
files packed side by side come from one read of their pack, so a data
loader pays one request's time to first byte per shard, not per file.
A shard never splits a sample (files sharing a path stem).

Every file is checked against the SHA-256 the snapshot pinned as it is
read, or a deterministic sample of them (verify="sample"); what failed is
kept, with a reason, and can be recorded on a training run.
"""

import base64
import concurrent.futures
import hashlib
import json
import random
import socket
import tarfile
import time

from .client import APIError


def plan(client, dataset_id, version_id, target_bytes=None):
    """Yields the snapshot's shards, in path order. Each has ordinals (the
    members, in order), files, bytes, packs (how many packs it reads),
    first_path and last_path."""
    path = f"/api/v1/datasets/{dataset_id}/versions/{version_id}/shards"
    yield from client.pages(path, "shards", target_bytes=target_bytes)


class Verifier:
    """Checks what a stream reads. rate 1.0 checks every file; below that,
    a deterministic sample by path (the same files on every run with the
    same seed), so the cost is a choice."""

    def __init__(self, rate=1.0, seed=0, keep=100, strict=False):
        if not 0 < rate <= 1:
            raise ValueError("rate is above 0 and at most 1")
        # strict raises IntegrityError on the first failure instead of
        # leaving the file out.
        self.rate, self.seed, self.keep, self.strict = rate, seed, keep, strict
        self.checked = self.failed = 0
        self.failures = []

    def wants(self, path):
        if self.rate >= 1:
            return True
        h = hashlib.sha256(f"{self.seed}:{path}".encode()).digest()
        return int.from_bytes(h[:8], "big") / 2**64 < self.rate

    def check(self, path, data, want_sha, want_size=None):
        """True when the bytes are the ones pinned, or were not sampled."""
        if not self.wants(path):
            return True
        self.checked += 1
        reason = ""
        if want_size is not None and len(data) != want_size:
            reason = f"size {len(data)}, pinned {want_size}"
        elif not want_sha:
            reason = "no SHA-256 pinned"
        elif hashlib.sha256(data).hexdigest() != want_sha:
            reason = "sha256 mismatch"
        if reason:
            self.fail(path, reason)
            return False
        return True

    def fail(self, path, reason):
        self.failed += 1
        if len(self.failures) < self.keep:
            self.failures.append({"path": path, "reason": reason})

    def report(self, client, run_id, version_id="", host=None):
        """Records what was checked on a training run, with the host that
        read the bytes."""
        return client.post(f"/api/v1/training-runs/{run_id}/verifications", {
            "version_id": version_id, "host": host or socket.gethostname(), "sample_rate": self.rate,
            "checked": self.checked, "failed": self.failed, "failures": self.failures})


class StreamCut(Exception):
    """The server ended a stream early: a file did not read back as
    published, or the connection dropped. Nothing after it was served."""


def stream(client, dataset_id, version_id, ordinals, pinned=None, verifier=None):
    """Yields (path, data, ordinal) for the members at ordinals, in that
    order, from one tar stream. pinned maps ordinal to the member as the
    snapshot pinned it; without it, the SHA-256 the stream carries is
    used. A file that does not match raises IntegrityError, or with a
    verifier that is not strict, is left out and recorded."""
    from .snapshots import IntegrityError
    verifier = verifier or Verifier(strict=True)
    path = f"/api/v1/datasets/{dataset_id}/versions/{version_id}/stream"
    with client.stream("POST", path, {"ordinals": list(ordinals)}) as resp:
        try:
            with tarfile.open(fileobj=resp, mode="r|") as tar:
                for entry in tar:
                    if not entry.isfile():
                        continue
                    ordinal = int(entry.pax_headers.get("OSP.ordinal", "-1"))
                    member = (pinned or {}).get(ordinal) or {}
                    want = member.get("checksum_sha256") or entry.pax_headers.get("OSP.sha256", "")
                    data = tar.extractfile(entry).read()
                    if not verifier.check(entry.name, data, want, member.get("size_bytes")):
                        if verifier.strict:
                            raise IntegrityError(entry.name, want, hashlib.sha256(data).hexdigest())
                        continue
                    yield entry.name, data, ordinal
        except (tarfile.ReadError, EOFError, OSError) as err:
            raise StreamCut(f"the stream ended early: {err}") from err



def read_shards(client, dataset_id, version_id, shards, pinned=None, verifier=None, prefetch=2):
    """Yields (path, data, ordinal) for each shard in order, with up to
    prefetch shards read ahead on threads."""
    if prefetch <= 1:
        for shard in shards:
            yield from stream(client, dataset_id, version_id, shard["ordinals"], pinned, verifier)
        return

    def whole(shard):
        return list(stream(client, dataset_id, version_id, shard["ordinals"], pinned, verifier))

    shards = list(shards)
    with concurrent.futures.ThreadPoolExecutor(prefetch) as pool:
        ahead = [pool.submit(whole, s) for s in shards[:prefetch]]
        for i in range(len(shards)):
            files = ahead[i].result()
            if i + prefetch < len(shards):
                ahead.append(pool.submit(whole, shards[i + prefetch]))
            ahead[i] = None
            yield from files


def shuffled(items, buffer, rng):
    """A streaming shuffle: holds up to buffer items and yields one at
    random as each new one arrives."""
    held = []
    for item in items:
        held.append(item)
        if len(held) >= buffer:
            yield held.pop(rng.randrange(len(held)))
    rng.shuffle(held)
    yield from held


# ─── Signed manifests ─────────────────────────────────────────────────

def canonical_line(m):
    """A member's line in its snapshot's canonical manifest, byte for byte
    as the server writes it (Go's encoding/json): keys in this order,
    metadata keys sorted, <, > and & escaped."""
    line = {"object_id": m["object_id"], "version_id": m["version_id"], "version": m.get("object_version", 0),
            "path": m["path"], "size_bytes": m["size_bytes"], "content_type": m.get("content_type", ""),
            "sha256": m.get("checksum_sha256", ""), "label": m.get("label", ""), "split": m.get("split", ""),
            "metadata": dict(sorted((m.get("metadata") or {}).items()))}
    text = json.dumps(line, ensure_ascii=False, separators=(",", ":"))
    for raw, escaped in (("<", "\\u003c"), (">", "\\u003e"), ("&", "\\u0026"), (" ", "\\u2028"), (" ", "\\u2029")):
        text = text.replace(raw, escaped)
    return text.encode("utf-8") + b"\n"


def manifest_sha256(members):
    """The SHA-256 of the canonical manifest of members in ordinal order."""
    h = hashlib.sha256()
    for m in sorted(members, key=lambda m: m["ordinal"]):
        h.update(canonical_line(m))
    return h.hexdigest()


def check_signature(client, signed):
    """Checks a signed manifest against the platform's published key.
    Returns (checked, ok, note): checked is False when it could not be
    checked, and note says why."""
    if not signed.get("signature"):
        return False, False, "the platform does not sign manifests (no signing key is set)"
    try:
        from cryptography.exceptions import InvalidSignature
        from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PublicKey
    except ImportError:
        return False, False, "the signature was not checked: pip install cryptography"
    try:
        key = client.get("/api/v1/certificates/public-key")
    except APIError as err:
        return False, False, f"the signing key could not be read: {err}"
    if key.get("key_id") != signed.get("key_id"):
        return True, False, f"signed with key {signed.get('key_id')}, the platform publishes {key.get('key_id')}"
    try:
        Ed25519PublicKey.from_public_bytes(base64.b64decode(key["public_key"])).verify(
            base64.b64decode(signed["signature"]), signed["body"].encode("utf-8"))
    except InvalidSignature:
        return True, False, "the signature does not match the manifest"
    return True, True, "signed by the platform"


def verify_snapshot(client, dataset_id, version_id, rate=1.0, seed=0, target_bytes=None, prefetch=2):
    """Checks a snapshot end to end: the signed manifest's signature, that
    the members listed hash to the manifest it signs, and the bytes of
    every file (or a sample) read through shard streams. Returns a dict:
    signature (checked, ok, note), manifest_ok, verifier, missing (members
    no stream served), cut (a stream that ended early)."""
    from . import snapshots
    signed = client.get(f"/api/v1/datasets/{dataset_id}/versions/{version_id}/signed-manifest")
    body = json.loads(signed["body"])
    checked, ok, note = check_signature(client, signed)
    listed = list(snapshots.members(client, dataset_id, version_id))
    manifest_ok = manifest_sha256(listed) == body["manifest_sha256"] and len(listed) == body["member_count"]
    pinned = {m["ordinal"]: m for m in listed if m.get("availability", "AVAILABLE") == "AVAILABLE"}
    verifier = Verifier(rate, seed)
    seen, cut = set(), ""
    try:
        for _, _, ordinal in read_shards(client, dataset_id, version_id, plan(client, dataset_id, version_id, target_bytes),
                                         pinned, verifier, prefetch):
            seen.add(ordinal)
    except StreamCut as err:
        cut = str(err)
    missing = sorted(pinned[o]["path"] for o in pinned if o not in seen)
    for path in missing[:verifier.keep]:
        verifier.fail(path, "not served")
    return {"signature": {"checked": checked, "ok": ok, "note": note}, "manifest_ok": manifest_ok,
            "verifier": verifier, "missing": missing, "cut": cut, "erased": len(listed) - len(pinned)}


# ─── Throughput ───────────────────────────────────────────────────────

def measure(client, dataset_id, version_id, shards, streams, seconds):
    """Reads shards with streams parallel streams for about seconds.
    Returns (files, bytes, elapsed, first_byte_seconds)."""
    files = size = 0
    first = []
    started = time.monotonic()
    deadline = started + seconds
    verifier = Verifier()

    def one(shard):
        t0 = time.monotonic()
        n = b = 0
        for _, data, _ in stream(client, dataset_id, version_id, shard["ordinals"], None, verifier):
            if n == 0:
                first.append(time.monotonic() - t0)
            n, b = n + 1, b + len(data)
            if time.monotonic() > deadline:
                break
        return n, b

    queue = list(shards)
    with concurrent.futures.ThreadPoolExecutor(streams) as pool:
        running = {pool.submit(one, queue.pop(0)) for _ in range(min(streams, len(queue)))}
        while running:
            done, running = concurrent.futures.wait(running, return_when=concurrent.futures.FIRST_COMPLETED)
            for f in done:
                n, b = f.result()
                files, size = files + n, size + b
                if queue and time.monotonic() < deadline:
                    running.add(pool.submit(one, queue.pop(0)))
    elapsed = time.monotonic() - started
    return files, size, elapsed, (sum(first) / len(first) if first else 0.0)


def autotune(client, dataset_id, version_id, max_streams=16, seconds=5, target_bytes=None):
    """Finds how many parallel shard streams this machine reads fastest
    with: doubles them while that adds at least 10% throughput. Returns
    (best_streams, results), results a list of dicts per step."""
    shards = list(plan(client, dataset_id, version_id, target_bytes))
    if not shards:
        return 1, []
    rng = random.Random(0)
    results, best, best_rate = [], 1, 0.0
    streams = 1
    while streams <= max_streams:
        sample = rng.sample(shards, len(shards))
        files, size, elapsed, ttfb = measure(client, dataset_id, version_id, sample, streams, seconds)
        rate = size / elapsed if elapsed else 0.0
        results.append({"streams": streams, "files_per_second": files / elapsed if elapsed else 0.0,
                        "bytes_per_second": rate, "first_byte_seconds": ttfb})
        if rate < best_rate * 1.1:
            break
        best, best_rate = streams, rate
        streams *= 2
    return best, results
