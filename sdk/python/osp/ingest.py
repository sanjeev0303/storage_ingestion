"""Bulk ingestion of a local folder through a manifest.

Every file is hashed locally first; the manifest declares each file's
SHA-256, and the platform publishes a file only after it has hashed the
uploaded bytes itself and found the same value.
"""

import concurrent.futures
import hashlib
import json
import mimetypes
import os
import time

from .client import put_bytes

PART_SIZE = 8 * 1024 * 1024
# Manifests up to this size go in the request body; larger ones are staged.
INLINE_MANIFEST_LIMIT = 16 * 1024 * 1024
FINISHED_STATES = {"COMPLETED", "PARTIALLY_COMPLETED", "FAILED", "CANCELLED"}


# Dataset file types, fixed here: mimetypes reads the Windows registry,
# where .csv is application/vnd.ms-excel, and the platform validates the
# declared type against the content.
KNOWN_TYPES = {
    ".csv": "text/csv", ".tsv": "text/tab-separated-values", ".txt": "text/plain", ".json": "application/json",
    ".jsonl": "application/x-ndjson", ".ndjson": "application/x-ndjson", ".parquet": "application/vnd.apache.parquet",
    ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".png": "image/png", ".gif": "image/gif", ".webp": "image/webp",
    ".wav": "audio/wav", ".mp3": "audio/mpeg", ".flac": "audio/flac", ".pdf": "application/pdf",
}


def content_type_of(name):
    known = KNOWN_TYPES.get(os.path.splitext(name)[1].lower())
    return known or mimetypes.guess_type(name)[0] or "application/octet-stream"


class LocalFile:
    def __init__(self, path, disk_path, size, content_type):
        self.path = path  # relative, with forward slashes
        self.disk_path = disk_path
        self.size = size
        self.content_type = content_type
        self.sha256 = ""


def scan_folder(folder):
    """Lists the non-empty files under folder, by their path inside it."""
    files = []
    for root, dirs, names in os.walk(folder):
        dirs.sort()
        for name in sorted(names):
            disk_path = os.path.join(root, name)
            size = os.path.getsize(disk_path)
            if size == 0:
                continue  # the platform keeps no empty objects
            rel = os.path.relpath(disk_path, folder).replace(os.sep, "/")
            content_type = content_type_of(name)
            files.append(LocalFile(rel, disk_path, size, content_type))
    return files


def sha256_file(path, chunk=1024 * 1024):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        while True:
            block = f.read(chunk)
            if not block:
                return h.hexdigest()
            h.update(block)


def hash_files(files, workers=8):
    with concurrent.futures.ThreadPoolExecutor(workers) as pool:
        for f, digest in zip(files, pool.map(lambda f: sha256_file(f.disk_path), files)):
            f.sha256 = digest


def build_manifest(files):
    lines = []
    for f in files:
        lines.append(json.dumps({
            "path": f.path, "size": f.size, "content_type": f.content_type,
            "checksum": "sha256:" + f.sha256, "required": True,
        }, separators=(",", ":")))
    return ("\n".join(lines) + "\n").encode()


def remote_versions(client, dataset):
    """Maps each path in the dataset's catalog to its current version
    (asset_id, checksum_sha256, size_bytes)."""
    current = {}
    for obj in client.pages("/api/v1/catalog/objects", "objects", dataset=dataset):
        version = obj.get("current_version")
        if version:
            current[obj["path"]] = version
    return current


def ingest_folder(client, folder, dataset, concurrency=8, wait=True, progress=None, branch=None, into=None):
    """Brings the dataset up to date with a local folder, uploading only
    what changed.

    Each local file is compared, by SHA-256, with the current version at
    its path in the dataset's catalog:
      new       - no object at that path: uploaded through bulk ingestion;
      changed   - other bytes: uploaded to the same path, which makes a new
                  version of that object (the old version stays, so
                  earlier snapshots still hold it);
      unchanged - the same bytes: not uploaded at all.
    Then every local file becomes a dataset member pinned to the version
    with exactly its bytes, so the next snapshot is this folder.

    With branch, the files still land in the dataset's namespace (new
    objects, or new versions of existing paths), but only the branch pins
    them: the dataset's own members and snapshots are left as they are. The
    branch is created from the dataset's latest version when it does not
    exist yet.

    Running it again resumes: the bulk job's key is the SHA-256 of its
    manifest, and uploads are keyed by path and content.

    into is the dataset (as the API answers it) when it already exists and
    was made with options a plain create would not match, such as a
    derived one.
    Returns a summary dict (new, changed, unchanged, members, failed, job).
    """
    say = progress or (lambda message: None)
    files = scan_folder(folder)
    if not files:
        raise ValueError(f"no files to ingest under {folder}")
    say(f"hashing {len(files)} files")
    hash_files(files, workers=concurrency)

    ds = into or client.post("/api/v1/datasets", {"name": dataset})
    current = remote_versions(client, dataset)
    new = [f for f in files if f.path not in current]
    changed = [f for f in files if f.path in current and current[f.path].get("checksum_sha256") != f.sha256]
    unchanged = len(files) - len(new) - len(changed)
    say(f"{len(new)} new, {len(changed)} changed, {unchanged} unchanged")
    summary = {"new": len(new), "changed": len(changed), "unchanged": unchanged, "members": 0, "failed": [], "job": None}

    if new:
        summary["job"] = _ingest_new(client, ds, new, concurrency, wait, say)
    if changed:
        summary["failed"] = _upload_new_versions(client, dataset, changed, concurrency, wait, say)
    if wait:
        target = ds
        if branch:
            from .branches import branch_of
            target = branch_of(client, ds, branch)
        summary["members"] = pin_members(client, target, files, progress=say, namespace=dataset)
    return summary


def _ingest_new(client, ds, files, concurrency, wait, say):
    manifest = build_manifest(files)
    manifest_sha = hashlib.sha256(manifest).hexdigest()
    # Keyed by dataset and manifest: a rerun resumes the same job, and the
    # same folder ingested into another dataset is a job of its own.
    key = hashlib.sha256((ds["id"] + ":" + manifest_sha).encode()).hexdigest()
    job = client.post("/api/v1/ingestion/jobs", {
        "dataset_id": ds["id"], "idempotency_key": "sdk-" + key[:40], "max_concurrency": concurrency,
    })
    job_path = "/api/v1/ingestion/jobs/" + job["id"]
    say(f"bulk job {job['id']} is {job['state']}")
    if job["state"] in FINISHED_STATES:
        return job
    if not job.get("manifest_complete"):
        _submit_manifest(client, job_path, manifest, manifest_sha)
        job = client.get(job_path)
    if job["state"] in ("PENDING_VALIDATION", "VALIDATING", "READY"):
        job = client.post(job_path + "/finalize")
    errors = _upload_claimed(client, job_path, {f.path: f for f in files}, concurrency, say)
    if errors:
        say(f"{len(errors)} files failed to upload; run again to retry them")
    if not wait:
        return client.get(job_path)
    return wait_for_job(client, job["id"], progress=say)


# A single PUT carries at most this much; larger changed files use parts.
SINGLE_UPLOAD_LIMIT = 64 * 1024 * 1024
UPLOAD_DONE = {"AVAILABLE", "REJECTED", "DELETED", "SOFT_DELETED"}


def _upload_new_versions(client, dataset, files, concurrency, wait, say):
    """Uploads changed files to their paths: each becomes the object's next
    version once processing has verified and scanned it. Returns the paths
    that failed, with why."""

    def one(f):
        key = "sdk-version-" + hashlib.sha256(f"{dataset}/{f.path}/{f.sha256}".encode()).hexdigest()[:40]
        body = {"file_name": os.path.basename(f.path), "content_type": f.content_type, "size_bytes": f.size,
                "checksum_sha256": f.sha256, "idempotency_key": key, "dataset": dataset, "path": f.path}
        if f.size <= SINGLE_UPLOAD_LIMIT:
            created = client.post("/api/v1/uploads", body)
            upload_id = created["upload_id"]
            put_bytes(created["upload_url"], lambda: open(f.disk_path, "rb"), f.size, f.content_type,
                      headers=created.get("upload_headers"))
            client.post(f"/api/v1/uploads/{upload_id}/complete")
        else:
            upload_id = client.post("/api/v1/uploads/multipart", body)["upload_id"]
            _send_parts(client, f"/api/v1/uploads/{upload_id}", f, {"checksum_sha256": f.sha256})
        if not wait:
            return None
        while True:
            status = client.get(f"/api/v1/uploads/{upload_id}")
            asset = status.get("asset") or {}
            if asset.get("status") in UPLOAD_DONE:
                if asset["status"] == "AVAILABLE":
                    return None
                return f"{f.path}: {asset['status']} {asset.get('rejection_reason', '')}".strip()
            if status.get("status") in ("FAILED", "ABORTED", "EXPIRED"):
                return f"{f.path}: upload {status['status']}"
            time.sleep(1)

    failed = []
    with concurrent.futures.ThreadPoolExecutor(concurrency) as pool:
        for problem in pool.map(one, files):
            if problem:
                failed.append(problem)
    say(f"{len(files) - len(failed)} changed files published as new versions")
    for problem in failed:
        say("  " + problem)
    return failed


MEMBER_LINES_PER_IMPORT = 20000
IMPORT_DONE = {"COMPLETED", "FAILED", "CANCELLED"}


def pin_members(client, ds, files, progress=None, namespace=None):
    """Makes each local file a member of the dataset, pinned to the catalog
    version whose SHA-256 equals the local file, so the next snapshot holds
    exactly this folder. Files with no published version of their bytes yet
    (still processing, or rejected) are left out and reported.
    """
    say = progress or (lambda message: None)
    namespace = namespace or ds["name"]
    current = remote_versions(client, namespace)
    lines, waiting = [], []
    for f in files:
        version = current.get(f.path)
        if version and version.get("checksum_sha256") == f.sha256:
            lines.append(json.dumps({"namespace": namespace, "path": f.path, "version_id": version["asset_id"]},
                                    separators=(",", ":")))
        else:
            waiting.append(f.path)
    if waiting:
        say(f"{len(waiting)} files have no published version with their bytes yet and were not pinned")
    pinned = 0
    for start in range(0, len(lines), MEMBER_LINES_PER_IMPORT):
        chunk = ("\n".join(lines[start:start + MEMBER_LINES_PER_IMPORT]) + "\n").encode()
        key = "sdk-members-" + hashlib.sha256(chunk).hexdigest()[:40]
        imp = client.request("POST", f"/api/v1/datasets/{ds['id']}/manifests", raw=chunk,
                             content_type="application/x-ndjson", params={"duplicate_policy": "REPLACE"},
                             headers={"Idempotency-Key": key})
        while imp["state"] not in IMPORT_DONE:
            time.sleep(1)
            imp = client.get(f"/api/v1/datasets/{ds['id']}/manifests/{imp['id']}")
        if imp["state"] != "COMPLETED":
            raise RuntimeError(f"pinning members failed: import {imp['id']} is {imp['state']}: {imp.get('last_error', '')}")
        p = imp.get("progress", {})
        pinned += p.get("members_added", 0) + p.get("members_updated", 0) + p.get("members_skipped", 0)
        if p.get("lines_rejected"):
            say(f"{p['lines_rejected']} files could not be pinned; see the import's errors")
    say(f"{pinned} files are members of {ds['name']}, each pinned to the version with its bytes")
    return pinned


def _submit_manifest(client, job_path, manifest, manifest_sha):
    if len(manifest) <= INLINE_MANIFEST_LIMIT:
        client.request("PUT", job_path + "/manifest", raw=manifest, content_type="application/x-ndjson",
                       headers={"X-Manifest-SHA256": manifest_sha})
        return
    staged = client.post(job_path + "/manifest/staged-url", {"size_bytes": len(manifest)})
    put_bytes(staged["upload_url"], manifest, len(manifest), "application/x-ndjson")
    client.post(job_path + "/manifest/staged", {"sha256": manifest_sha})


def _upload_claimed(client, job_path, by_path, concurrency, say):
    """Claims entries while there is room and uploads each from disk."""
    errors = []
    done = 0
    with concurrent.futures.ThreadPoolExecutor(concurrency) as pool:
        running = set()
        while True:
            free = concurrency - len(running)
            claim = {"grants": []}
            if free > 0:
                claim = client.post(job_path + "/claims", {"max": min(free, 32)})
                for grant in claim.get("grants") or []:
                    running.add(pool.submit(_upload_one, client, job_path, by_path, grant))
            limited = claim.get("limited_by", "")
            if limited == "job_not_running":
                break
            if limited == "no_pending_entries" and not running:
                break
            if running and (free == 0 or not claim.get("grants")):
                finished, running = concurrent.futures.wait(running, return_when=concurrent.futures.FIRST_COMPLETED)
                for f in finished:
                    if f.exception():
                        errors.append(f.exception())
                    else:
                        done += 1
                        if done % 50 == 0:
                            say(f"{done} files uploaded")
            elif not claim.get("grants"):
                time.sleep(max(claim.get("retry_after_seconds", 1), 1))
        for f in concurrent.futures.as_completed(running):
            if f.exception():
                errors.append(f.exception())
    return errors


def _upload_one(client, job_path, by_path, grant):
    entry = grant["entry"]
    local = by_path.get(entry.get("path", ""))
    if local is None:
        raise FileNotFoundError(f"no local file for {entry.get('path')}")
    entry_path = f"{job_path}/entries/{entry['position']}"

    if entry.get("strategy") == "SINGLE":
        put_bytes(grant["upload_url"], lambda: open(local.disk_path, "rb"), local.size, entry.get("content_type"),
                  headers=grant.get("upload_headers"))
        client.post(entry_path + "/complete")
        return

    _send_parts(client, entry_path, local, {})


def _send_parts(client, base, local, complete_body):
    """Uploads a file in PART_SIZE parts under an upload at base (an
    ingestion entry or a multipart upload), confirms them and completes."""
    count = (local.size + PART_SIZE - 1) // PART_SIZE
    numbers = list(range(1, count + 1))
    urls = []
    for start in range(0, count, 100):
        answer = client.post(base + "/parts/urls", {"part_numbers": numbers[start:start + 100]})
        urls.extend(answer["parts"])
    recorded, completed = [], []
    with open(local.disk_path, "rb") as f:
        for i, part in enumerate(urls):
            offset = i * PART_SIZE
            length = min(PART_SIZE, local.size - offset)
            f.seek(offset)
            data = f.read(length)
            etag = put_bytes(part["url"], data, length)
            recorded.append({"part_number": part["part_number"], "etag": etag, "size_bytes": length})
            completed.append({"part_number": part["part_number"], "etag": etag})
    for start in range(0, len(recorded), 100):
        client.post(base + "/parts/confirm", {"parts": recorded[start:start + 100]})
    client.post(base + "/complete", {"parts": completed, **complete_body})


def wait_for_job(client, job_id, poll=2.0, timeout=None, progress=None):
    """Polls the job until it finishes, and returns it."""
    say = progress or (lambda message: None)
    started = time.monotonic()
    last = None
    while True:
        job = client.get("/api/v1/ingestion/jobs/" + job_id)
        p = job.get("progress", {})
        line = f"{job['state']}: {p.get('published', 0)}/{p.get('valid', 0)} published, {p.get('failed', 0)} failed"
        if line != last:
            say(line)
            last = line
        if job["state"] in FINISHED_STATES:
            return job
        if timeout and time.monotonic() - started > timeout:
            raise TimeoutError(f"job {job_id} is still {job['state']}")
        time.sleep(poll)
