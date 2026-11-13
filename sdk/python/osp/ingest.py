"""Bulk ingestion of a local folder through a manifest.

Every file is hashed locally first; the manifest declares each file's
SHA-256, and the platform publishes a file only after it has hashed the
uploaded bytes itself and found the same value.
"""

import concurrent.futures
import contextlib
import hashlib
import json
import mimetypes
import os
import threading
import time
import uuid

from .client import APIError, put_bytes, session_gone

PART_SIZE = 8 * 1024 * 1024
# Parts of one file sent at once: one connection to a far bucket is slow.
PARTS_AT_ONCE = 4
# Manifests up to this size go in the request body; larger ones are staged.
INLINE_MANIFEST_LIMIT = 16 * 1024 * 1024
FINISHED_STATES = {"COMPLETED", "PARTIALLY_COMPLETED", "FAILED", "CANCELLED"}
# How long each wait for the platform's checks goes on before it gives up,
# in seconds. Pass timeout=None to wait for ever; a run again picks up where
# a run that gave up stopped.
WAIT_TIMEOUT = 2 * 60 * 60
# Times a wait claims entries again that went back to PENDING after a
# failed upload: only a client's claim sends them.
CLAIM_ROUNDS = 10


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


def ingest_folder(client, folder, dataset, concurrency=8, wait=True, progress=None, branch=None, into=None,
                  resume_file=None, timeout=WAIT_TIMEOUT):
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

    resume_file is a JSON file that remembers the parts each large file
    has confirmed, so a run cut short (a lost connection, a crash) sends
    only the rest the next time.

    With wait, each wait for the platform's checks gives up after timeout
    seconds (WAIT_TIMEOUT, two hours, by default; None waits for ever).
    While it waits it claims and sends again any file whose upload failed
    and went back to PENDING.
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

    resume = ResumeFile(resume_file) if resume_file else None
    if new:
        summary["job"] = _ingest_new(client, ds, new, concurrency, wait, say, resume, timeout)
    if changed:
        summary["failed"] = _upload_new_versions(client, dataset, changed, concurrency, wait, say, resume, timeout)
    if wait:
        target = ds
        if branch:
            from .branches import branch_of
            target = branch_of(client, ds, branch)
        summary["members"] = pin_members(client, target, files, progress=say, namespace=dataset, timeout=timeout)
    return summary


def _ingest_new(client, ds, files, concurrency, wait, say, resume=None, timeout=WAIT_TIMEOUT):
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
    by_path = {f.path: f for f in files}

    def upload_claimed():
        errors = _upload_claimed(client, job_path, by_path, concurrency, say, resume)
        if errors:
            say(f"{len(errors)} files failed to upload; they are tried again once the platform hands them back")

    upload_claimed()
    if not wait:
        return client.get(job_path)
    return wait_for_job(client, job["id"], timeout=timeout, progress=say, claim=upload_claimed)


# A single PUT carries at most this much; larger changed files use parts.
SINGLE_UPLOAD_LIMIT = 64 * 1024 * 1024
# Asset states that do not change by waiting. BLOCKED is an AVAILABLE asset
# whose reads a later check stopped.
UPLOAD_DONE = {"AVAILABLE", "REJECTED", "BLOCKED", "DELETED", "SOFT_DELETED"}


def _upload_new_versions(client, dataset, files, concurrency, wait, say, resume=None, timeout=WAIT_TIMEOUT):
    """Uploads changed files to their paths: each becomes the object's next
    version once processing has verified and scanned it. Returns the paths
    that failed, with why."""

    def single(f, body):
        created = client.post("/api/v1/uploads", body)
        upload_id = created["upload_id"]
        if not created.get("upload_url") and created.get("status") in ("COMPLETED", "VERIFIED"):
            return upload_id  # a start sent again after its upload completed: nothing is left to send
        # The same key hands back the same upload with a new URL.
        put_bytes(created["upload_url"], lambda: open(f.disk_path, "rb"), f.size, f.content_type,
                  headers=created.get("upload_headers"),
                  refresh=lambda: client.post("/api/v1/uploads", body)["upload_url"])
        client.post(f"/api/v1/uploads/{upload_id}/complete", retry=True)
        return upload_id

    def multipart(f, body):
        upload_id = client.post("/api/v1/uploads/multipart", body)["upload_id"]
        _send_parts(client, f"/api/v1/uploads/{upload_id}", f, {"checksum_sha256": f.sha256}, resume)
        return upload_id

    def one(f):
        key = "sdk-version-" + hashlib.sha256(f"{dataset}/{f.path}/{f.sha256}".encode()).hexdigest()[:40]
        body = {"file_name": os.path.basename(f.path), "content_type": f.content_type, "size_bytes": f.size,
                "checksum_sha256": f.sha256, "idempotency_key": key, "dataset": dataset, "path": f.path}
        send = single if f.size <= SINGLE_UPLOAD_LIMIT else multipart
        try:
            upload_id = send(f, body)
        except APIError as err:
            if not session_gone(err):
                raise
            # The session for this key ended, and the same key would hand
            # it back: once more from the start, under a new key.
            upload_id = send(f, {**body, "idempotency_key": key + "-" + uuid.uuid4().hex[:8]})
        if not wait:
            return None
        give_up = None if timeout is None else time.monotonic() + timeout
        while True:
            status = client.get(f"/api/v1/uploads/{upload_id}")
            asset = status.get("asset") or {}
            if asset.get("status") in UPLOAD_DONE:
                if asset["status"] == "AVAILABLE":
                    return None
                return f"{f.path}: {asset['status']} {asset.get('rejection_reason', '')}".strip()
            if status.get("status") in ("FAILED", "ABORTED", "EXPIRED"):
                return f"{f.path}: upload {status['status']}"
            if give_up is not None and time.monotonic() >= give_up:
                state = asset.get("status") or status.get("status")
                return f"{f.path}: still {state} after {timeout}s; run again to check on it"
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


def pin_members(client, ds, files, progress=None, namespace=None, timeout=WAIT_TIMEOUT):
    """Makes each local file a member of the dataset, pinned to the catalog
    version whose SHA-256 equals the local file, so the next snapshot holds
    exactly this folder. Files with no published version of their bytes yet
    (still processing, or rejected) are left out and reported. Raises
    TimeoutError when an import is still running after timeout seconds.
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
        give_up = None if timeout is None else time.monotonic() + timeout
        while imp["state"] not in IMPORT_DONE:
            if give_up is not None and time.monotonic() >= give_up:
                raise TimeoutError(f"pinning members: import {imp['id']} is still {imp['state']}")
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


def _upload_claimed(client, job_path, by_path, concurrency, say, resume=None):
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
                    running.add(pool.submit(_upload_one, client, job_path, by_path, grant, resume))
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


def _upload_one(client, job_path, by_path, grant, resume=None):
    entry = grant["entry"]
    local = by_path.get(entry.get("path", ""))
    if local is None:
        raise FileNotFoundError(f"no local file for {entry.get('path')}")
    entry_path = f"{job_path}/entries/{entry['position']}"

    if entry.get("strategy") == "SINGLE":
        put_bytes(grant["upload_url"], lambda: open(local.disk_path, "rb"), local.size, entry.get("content_type"),
                  headers=grant.get("upload_headers"))
        client.post(entry_path + "/complete", retry=True)
        return

    # An entry claimed again after a failure has a new upload session, which
    # the saved parts of the old one must not complete.
    _send_parts(client, entry_path, local, {}, resume, upload_id=entry.get("upload_id", ""))


class ResumeFile:
    """Remembers, in a JSON file, the parts each unfinished upload has had
    confirmed, so running the same upload again sends only the rest. The
    file is written whole through a temporary copy, so a crash mid-write
    leaves the previous one, and a lock file beside it keeps processes
    sharing it (two `osp ingest` runs) from dropping each other's parts."""

    def __init__(self, path):
        self.path = path
        self._lock = threading.Lock()

    @contextlib.contextmanager
    def _locked(self):
        lock = self.path + ".lock"
        os.makedirs(os.path.dirname(os.path.abspath(self.path)), exist_ok=True)
        with self._lock:
            while True:
                try:
                    fd = os.open(lock, os.O_CREAT | os.O_EXCL | os.O_WRONLY)
                    break
                except (FileExistsError, PermissionError):
                    # A lock left by a process that died holding it is stale
                    # long before this: a write takes milliseconds.
                    try:
                        if time.time() - os.path.getmtime(lock) > 30:
                            os.remove(lock)
                    except OSError:
                        pass
                    time.sleep(0.01)
            try:
                yield
            finally:
                os.close(fd)
                os.remove(lock)

    def _load(self):
        try:
            with open(self.path, encoding="utf-8") as f:
                return json.load(f)
        except (OSError, ValueError):
            return {}

    def _write(self, entries):
        with open(self.path + ".tmp", "w", encoding="utf-8") as f:
            json.dump(entries, f)
        os.replace(self.path + ".tmp", self.path)

    def parts(self, key):
        """Part number (as text) to {"etag", "size"} for one upload."""
        with self._locked():
            return dict(self._load().get(key, {}))

    def save_part(self, key, number, etag, size):
        with self._locked():
            entries = self._load()
            entries.setdefault(key, {})[str(number)] = {"etag": etag, "size": size}
            self._write(entries)

    def forget(self, key):
        with self._locked():
            entries = self._load()
            if entries.pop(key, None) is not None:
                self._write(entries)


def _send_parts(client, base, local, complete_body, resume=None, upload_id=""):
    """Uploads a file in PART_SIZE parts under an upload at base (an
    ingestion entry or a multipart upload), confirms them and completes.
    With resume, parts confirmed by an earlier run are not sent again.
    upload_id names the session when base does not."""
    key = f"{base}:{local.sha256}:{PART_SIZE}"
    if upload_id:
        key = f"{base}:{upload_id}:{local.sha256}:{PART_SIZE}"
    try:
        _send_saved_parts(client, base, local, complete_body, resume, key)
    except APIError as err:
        # The session ended, or the server no longer knows the saved parts:
        # start clean next time instead of failing the same way again.
        if resume and (session_gone(err) or (err.status == 400 and err.path == base + "/complete")):
            resume.forget(key)
        raise
    if resume:
        resume.forget(key)


def _send_saved_parts(client, base, local, complete_body, resume, key):
    done = resume.parts(key) if resume else {}
    count = (local.size + PART_SIZE - 1) // PART_SIZE
    numbers = [n for n in range(1, count + 1) if str(n) not in done]
    urls = {}
    # Part links, confirmations and the completion can be sent again safely.
    for start in range(0, len(numbers), 100):
        answer = client.post(base + "/parts/urls", {"part_numbers": numbers[start:start + 100]}, retry=True)
        for part in answer["parts"]:
            urls[part["part_number"]] = part["url"]

    def send(n):
        offset = (n - 1) * PART_SIZE
        length = min(PART_SIZE, local.size - offset)
        with open(local.disk_path, "rb") as f:
            f.seek(offset)
            data = f.read(length)

        # Links last about 15 minutes; one that expired before its turn is replaced.
        def refresh():
            return client.post(base + "/parts/urls", {"part_numbers": [n]}, retry=True)["parts"][0]["url"]

        etag = put_bytes(urls[n], data, length, refresh=refresh)
        # Confirmed one at a time, so a run cut short keeps every part it sent.
        client.post(base + "/parts/confirm", {"parts": [{"part_number": n, "etag": etag, "size_bytes": length}]},
                    retry=True)
        if resume:
            resume.save_part(key, n, etag, length)
        return n, etag, length

    with concurrent.futures.ThreadPoolExecutor(PARTS_AT_ONCE) as pool:
        futures = [pool.submit(send, n) for n in numbers]
        try:
            for future in concurrent.futures.as_completed(futures):
                n, etag, length = future.result()
                done[str(n)] = {"etag": etag, "size": length}
        except BaseException:
            # The first failure stops the parts not yet started; the ones
            # under way finish before this raises.
            pool.shutdown(cancel_futures=True)
            raise
    completed = [{"part_number": int(n), "etag": done[n]["etag"]} for n in sorted(done, key=int)]
    client.post(base + "/complete", {"parts": completed, **complete_body}, retry=True)


def wait_for_job(client, job_id, poll=2.0, timeout=WAIT_TIMEOUT, progress=None, claim=None):
    """Polls the job until it finishes, and returns it. Raises TimeoutError
    after timeout seconds (WAIT_TIMEOUT by default; None waits for ever).

    claim, when given, is called while the job has PENDING entries, at most
    CLAIM_ROUNDS times: an upload that failed goes back to PENDING once its
    lease or retry delay has passed, and only a client's claim sends it."""
    say = progress or (lambda message: None)
    started = time.monotonic()
    last = None
    rounds = 0
    while True:
        job = client.get("/api/v1/ingestion/jobs/" + job_id)
        p = job.get("progress", {})
        line = f"{job['state']}: {p.get('published', 0)}/{p.get('valid', 0)} published, {p.get('failed', 0)} failed"
        if line != last:
            say(line)
            last = line
        if job["state"] in FINISHED_STATES:
            return job
        if claim and p.get("pending") and rounds < CLAIM_ROUNDS:
            rounds += 1
            claim()
            continue
        if timeout is not None and time.monotonic() - started > timeout:
            raise TimeoutError(f"job {job_id} is still {job['state']}")
        time.sleep(poll)
