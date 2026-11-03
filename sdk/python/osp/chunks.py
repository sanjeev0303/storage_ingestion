"""The chunk index (docs/chunks.md): what chunk-level dedup would save, and a new
version fetched as the difference from a copy of an old one.

    from osp import chunks
    chunks.set_enabled(client, dataset_id, True)
    print(chunks.savings(client, dataset_id)["would_save_bytes"])
    chunks.delta_download(client, dataset_id, old_version_id, new_version_id, "build-41", "build-42")

Chunk boundaries come from the server's index, so nothing here needs to agree
with its chunker: a local chunk is reused only if it hashes as the index says,
and every finished file is checked against the SHA-256 the snapshot pinned.
"""

import concurrent.futures
import hashlib
import os
import shutil
import tempfile
import time

from . import fastcdc, ingest, snapshots
from .snapshots import IntegrityError, safe_parts, sha256_file


FILES_AT_ONCE = 4
SENDS_AT_ONCE = 8
READS_AT_ONCE = 6


def set_enabled(client, dataset_id, on=True):
    return client.request("PUT", f"/api/v1/datasets/{dataset_id}/chunking", body={"enabled": on})


def savings(client, dataset_id):
    return client.get(f"/api/v1/datasets/{dataset_id}/chunk-savings")


def status(client, dataset_id, version_id):
    return client.get(f"/api/v1/datasets/{dataset_id}/versions/{version_id}/chunks/status")


def index_version(client, dataset_id, version_id):
    return client.post(f"/api/v1/datasets/{dataset_id}/versions/{version_id}/chunks/index", {})


def delta(client, dataset_id, version_id, from_version_id):
    return client.get(f"/api/v1/datasets/{dataset_id}/versions/{version_id}/delta", **{"from": from_version_id})


def manifest(client, dataset_id, version_id, path):
    """A file's chunks in order, or None if it has not been indexed."""
    url = f"/api/v1/datasets/{dataset_id}/versions/{version_id}/chunks"
    out = []
    after = 0
    while True:
        page = client.request("GET", url, params={"path": path, "after": after, "limit": 10000})
        if not page["indexed"]:
            return None
        out.extend(page["chunks"])
        if len(page["chunks"]) < 10000:
            return out
        after = page["chunks"][-1]["ordinal"] + 1


def plan(wanted, local):
    """Splits a new file's chunks into pieces. local maps a chunk's hash to
    (path, offset) in the old copy. Runs of missing chunks merge into one
    range, so a large change is one read and not thousands."""
    steps = []
    for c in wanted:
        where = local.get(c["sha256"])
        if where:
            steps.append({"from": where, "length": c["length"], "sha256": c["sha256"], "offset": c["offset"]})
        elif steps and "from" not in steps[-1]:
            steps[-1]["length"] += c["length"]
            steps[-1]["chunks"].append(c)
        else:
            steps.append({"offset": c["offset"], "length": c["length"], "chunks": [c]})
    return steps


def _read_local(step):
    path, offset = step["from"]
    try:
        with open(path, "rb") as f:
            f.seek(offset)
            data = f.read(step["length"])
    except OSError:
        return None
    return data if hashlib.sha256(data).hexdigest() == step["sha256"] else None


def _fetch_range(client, url, first, length):
    headers = {"Range": f"bytes={first}-{first + length - 1}"}
    with client.fetch(url, headers=headers) as resp:
        data = resp.read()
    if len(data) != length:
        raise IOError(f"asked for {length} bytes at {first}, got {len(data)}")
    return data


def _build(client, member, wanted, local, target):
    """Writes one file from local chunks and ranged reads. A local chunk that no
    longer hashes as expected is fetched instead. Answers the bytes fetched."""
    url = snapshots.download_url(client, member["version_id"])
    fetched = 0
    whole = hashlib.sha256()
    fd, tmp = tempfile.mkstemp(dir=os.path.dirname(target), prefix=".osp-")
    try:
        with os.fdopen(fd, "wb") as out:
            def get(step):
                data = _read_local(step) if "from" in step else None
                if data is None:
                    return _fetch_range(client, url, step["offset"], step["length"]), True
                return data, False

            # Read ahead, written in order: the pool holds at most a few chunks' worth.
            steps = plan(wanted, local)
            with concurrent.futures.ThreadPoolExecutor(READS_AT_ONCE) as pool:
                for start in range(0, len(steps), READS_AT_ONCE):
                    for data, was_fetched in pool.map(get, steps[start:start + READS_AT_ONCE]):
                        if was_fetched:
                            fetched += len(data)
                        out.write(data)
                        whole.update(data)
        digest = whole.hexdigest()
        if digest != member.get("checksum_sha256"):
            raise IntegrityError(member["path"], member.get("checksum_sha256"), digest)
        os.replace(tmp, target)
    except BaseException:
        if os.path.exists(tmp):
            os.remove(tmp)
        raise
    return fetched


def _fetch_whole(client, member, target):
    data = snapshots.fetch_member(client, member)
    fd, tmp = tempfile.mkstemp(dir=os.path.dirname(target), prefix=".osp-")
    with os.fdopen(fd, "wb") as out:
        out.write(data)
    os.replace(tmp, target)
    return len(data)


def set_storage(client, dataset_id, on=True):
    """Turns storing chunks once on or off. Files sharing enough with others are
    then kept as chunks; off stops new conversions and leaves what is chunked."""
    return client.request("PUT", f"/api/v1/datasets/{dataset_id}/chunk-storage", body={"enabled": on})


def storage(client, dataset_id):
    """What chunk storage holds for a dataset."""
    return client.get(f"/api/v1/datasets/{dataset_id}/chunk-storage")


def rehydrate(client, dataset_id):
    """Makes a dataset's chunked files whole objects again."""
    return client.post(f"/api/v1/datasets/{dataset_id}/chunk-storage/rehydrate", {})


def delta_upload(client, dataset_id, base_version_id, file, path, content_type="application/octet-stream", wait=True, timeout=3600, poll=2):
    """Sends a file as what it does not share with a snapshot you can read. The
    server builds it from what it holds, and it goes through the same SHA-256
    check and malware scan as any upload. Answers the delta upload's state,
    with reused_bytes and sent_bytes."""
    pieces, size, sha = fastcdc.manifest(file)
    created = client.post("/api/v1/delta-uploads", {"dataset_id": dataset_id, "base_version_id": base_version_id, "path": path,
                                                    "size_bytes": size, "sha256": sha, "content_type": content_type, "chunks": pieces})
    url = f"/api/v1/delta-uploads/{created['id']}"

    # Several at once: a chunk is about a megabyte, and a round trip each adds up.
    def send(ordinal):
        c = pieces[ordinal]
        with open(file, "rb") as f:
            f.seek(c["offset"])
            data = f.read(c["length"])
        client.request("PUT", f"{url}/chunks/{ordinal}", raw=data, content_type="application/octet-stream")

    with concurrent.futures.ThreadPoolExecutor(SENDS_AT_ONCE) as pool:
        for _ in pool.map(send, created["missing"]):
            pass
    client.post(f"{url}/complete", {})
    if not wait:
        return client.get(url)
    deadline = time.time() + timeout
    while True:
        status = client.get(url)
        state = status["upload"]["state"]
        if state in ("FAILED", "EXPIRED"):
            raise RuntimeError(f"delta upload of {path} {state}: {status['upload'].get('error')}")
        if state == "DONE" and status.get("asset_status") in ("AVAILABLE", "REJECTED", "DELETED"):
            return status
        if time.time() > deadline:
            raise TimeoutError(f"delta upload of {path} is still {state}")
        time.sleep(poll)


def delta_push(client, dataset_id, base_version_id, folder, progress=None):
    """Makes the dataset's draft hold this folder, sending only what differs from
    the base snapshot. A file the snapshot holds as it is sends nothing. Answers
    counts: files, unchanged, sent, rejected, reused_bytes, sent_bytes."""
    say = progress or (lambda message: None)
    ds = snapshots.find_dataset(client, dataset_id)
    held = {m["path"]: m.get("checksum_sha256") for m in snapshots.members(client, ds["id"], base_version_id)}
    files = ingest.scan_folder(folder)
    ingest.hash_files(files)
    counts = {"files": len(files), "unchanged": 0, "sent": 0, "rejected": 0, "reused_bytes": 0, "sent_bytes": 0}
    changed = []
    to_send = []
    for f in files:
        if held.get(f.path) == f.sha256:
            counts["unchanged"] += 1
        else:
            to_send.append(f)
    with concurrent.futures.ThreadPoolExecutor(FILES_AT_ONCE) as pool:
        statuses = list(pool.map(lambda f: delta_upload(client, ds["id"], base_version_id, f.disk_path, f.path, f.content_type), to_send))
    for f, status in zip(to_send, statuses):
        counts["reused_bytes"] += status["upload"]["reused_bytes"]
        counts["sent_bytes"] += status["upload"]["sent_bytes"]
        say(f"{f.path}: sent {status['upload']['sent_bytes']:,} of {status['upload']['size_bytes']:,} bytes")
        if status.get("asset_status") != "AVAILABLE":
            counts["rejected"] += 1
            say(f"{f.path}: {status.get('asset_status')}, not added")
            continue
        counts["sent"] += 1
        changed.append(f)
    if changed:
        ingest.pin_members(client, ds, changed, progress=say)
    return counts


def delta_download(client, dataset_id, old_version_id, new_version_id, old_folder, out_folder, progress=None):
    """Builds the new version in out_folder from a copy of the old one in
    old_folder, fetching only what differs. The old copy is not changed; it may
    be stale or partial, which only means more is fetched. Every file is checked
    against the SHA-256 the snapshot pinned before it is put in place. Answers
    counts: files, copied, patched, fetched, erased, bytes_fetched, total_bytes."""
    say = progress or (lambda message: None)
    old_root = os.path.abspath(old_folder)
    out_root = os.path.abspath(out_folder)
    if old_root == out_root:
        raise ValueError("write the new version to a folder of its own")
    os.makedirs(out_root, exist_ok=True)
    old_members = list(snapshots.members(client, dataset_id, old_version_id))
    new_members = list(snapshots.members(client, dataset_id, new_version_id))
    new_hashes = {m.get("checksum_sha256") for m in new_members}
    by_hash = {m.get("checksum_sha256"): m for m in old_members}

    # Chunks of old files that are not in the new version as they are: a file
    # renamed and edited still lends its chunks. Unchanged files are copied.
    local = {}
    for m in old_members:
        if m.get("checksum_sha256") in new_hashes or m.get("availability", "AVAILABLE") != "AVAILABLE":
            continue
        path = os.path.join(old_root, *safe_parts(m["path"]))
        if not os.path.exists(path):
            continue
        for c in manifest(client, dataset_id, old_version_id, m["path"]) or []:
            local.setdefault(c["sha256"], (path, c["offset"]))

    counts = {"files": 0, "copied": 0, "patched": 0, "fetched": 0, "erased": 0, "bytes_fetched": 0, "total_bytes": 0}
    for m in new_members:
        counts["files"] += 1
        if m.get("availability", "AVAILABLE") != "AVAILABLE":
            counts["erased"] += 1
            continue
        counts["total_bytes"] += m["size_bytes"]
        target = os.path.join(out_root, *safe_parts(m["path"]))
        os.makedirs(os.path.dirname(target), exist_ok=True)
        twin = by_hash.get(m.get("checksum_sha256"))
        if twin:
            source = os.path.join(old_root, *safe_parts(twin["path"]))
            if os.path.exists(source) and sha256_file(source) == m.get("checksum_sha256"):
                shutil.copyfile(source, target)
                counts["copied"] += 1
                continue
        wanted = manifest(client, dataset_id, new_version_id, m["path"])
        if not wanted:
            counts["bytes_fetched"] += _fetch_whole(client, m, target)
            counts["fetched"] += 1
            say(f"{m['path']}: not indexed, fetched whole")
            continue
        fetched = _build(client, m, wanted, local, target)
        counts["bytes_fetched"] += fetched
        counts["patched"] += 1
        say(f"{m['path']}: fetched {fetched:,} of {m['size_bytes']:,} bytes")
    return counts
