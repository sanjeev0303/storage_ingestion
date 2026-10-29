"""Dataset snapshots: create, list, verify a local copy, download."""

import concurrent.futures
import time
import hashlib
import os
import tempfile
import urllib.request
import uuid

from .ingest import sha256_file


def find_dataset(client, name_or_id):
    """Returns the dataset with this ID, or else the one with this name.
    "cats@exp" names the branch exp of the dataset cats."""
    if "@" in name_or_id:
        parent_name, _, branch = name_or_id.partition("@")
        parent = find_dataset(client, parent_name)
        for b in client.get(f"/api/v1/datasets/{parent['id']}/branches").get("branches") or []:
            if b["branch"]["name"] == branch:
                return b
        raise LookupError(f"{parent_name} has no branch {branch!r}")
    for ds in client.pages("/api/v1/datasets", "datasets"):
        if ds["id"] == name_or_id or ds.get("name") == name_or_id:
            return ds
    # A branch is not listed among the datasets but can be named by its ID.
    try:
        return client.get(f"/api/v1/datasets/{name_or_id}")
    except Exception:
        raise LookupError(f"no dataset {name_or_id!r}") from None


def snapshot(client, dataset_id, idempotency_key=None):
    """Freezes the dataset's current members as a new immutable version.
    The same key returns the same version, so a retry never makes two."""
    key = idempotency_key or str(uuid.uuid4())
    return client.post(f"/api/v1/datasets/{dataset_id}/snapshots", headers={"Idempotency-Key": key})


def versions(client, dataset_id):
    """Every version of the dataset, newest first."""
    return list(client.pages(f"/api/v1/datasets/{dataset_id}/versions", "versions",
                             cursor_param="before", cursor_key="next_before"))


def find_version(client, dataset_id, version):
    """version is "latest", a version number or a version ID."""
    found = versions(client, dataset_id)
    if not found:
        raise LookupError("the dataset has no snapshots yet")
    if version in (None, "", "latest"):
        return max(found, key=lambda v: v["version"])
    for v in found:
        if v["id"] == version or str(v["version"]) == str(version):
            return v
    raise LookupError(f"no version {version!r}")


def members(client, dataset_id, version_id, page_size=None):
    """Yields the version's members in their canonical order. Each one pins
    an exact object version: version_id is the asset that holds its bytes."""
    after = 0
    path = f"/api/v1/datasets/{dataset_id}/versions/{version_id}/members"
    while True:
        page = client.get(path, after=after or None, limit=page_size)
        rows = page.get("members") or []
        yield from rows
        after = page.get("next_after")
        if not after or not rows:
            return


def verify_local(client, dataset_id, version_id, folder, workers=8):
    """Checks a local copy against a snapshot, file by file, by SHA-256.

    Returns a dict with lists: matched, changed (different bytes), missing,
    extra (local files the snapshot does not hold), erased (members whose
    bytes the platform erased, absent here as they should be) and
    erased_but_present (erased members this folder still holds: delete
    them to complete the erasure). A copy is exact only when changed,
    missing, extra and erased_but_present are all empty.
    """
    expected = {}
    gone = set()
    for m in members(client, dataset_id, version_id):
        expected[m["path"]] = m
        if m.get("availability", "AVAILABLE") != "AVAILABLE":
            gone.add(m["path"])
    local = set()
    for root, _, names in os.walk(folder):
        for name in names:
            local.add(os.path.relpath(os.path.join(root, name), folder).replace(os.sep, "/"))

    report = {"matched": [], "changed": [], "missing": [], "extra": sorted(local - set(expected)),
              "erased": sorted(gone - local), "erased_but_present": sorted(gone & local)}
    present = [p for p in expected if p in local and p not in gone]
    report["missing"] = sorted(p for p in expected if p not in local and p not in gone)

    def check(path):
        disk = os.path.join(folder, *path.split("/"))
        want = expected[path].get("checksum_sha256", "")
        return path, os.path.getsize(disk) == expected[path]["size_bytes"] and sha256_file(disk) == want

    with concurrent.futures.ThreadPoolExecutor(workers) as pool:
        for path, ok in pool.map(check, present):
            report["matched" if ok else "changed"].append(path)
    report["matched"].sort()
    report["changed"].sort()
    return report


def download_url(client, asset_id):
    answer = client.post(f"/api/v1/assets/{asset_id}/download-url", {"mode": "storage"})
    url = answer["url"]
    if url.startswith("/"):
        url = client.base_url + url
    return url


def fetch_member(client, member, timeout=600):
    """Returns a member's bytes, refusing any that do not hash to the
    SHA-256 the snapshot pinned."""
    url = download_url(client, member["version_id"])
    with urllib.request.urlopen(url, timeout=timeout) as resp:
        data = resp.read()
    digest = hashlib.sha256(data).hexdigest()
    if len(data) != member["size_bytes"] or digest != member.get("checksum_sha256"):
        raise IntegrityError(member["path"], member.get("checksum_sha256"), digest)
    return data


def safe_parts(path):
    """Splits a member path into parts that stay inside the folder. The
    platform already refuses such paths; this is checked again because the
    path decides where a file is written. Checked by its text alone: on
    Windows, resolving paths while other threads create folders is not
    reliable."""
    parts = path.split("/")
    for part in parts:
        if part in ("", ".", "..") or "\\" in part or ":" in part or "\x00" in part:
            raise ValueError(f"refusing an unsafe path: {path!r}")
    return parts


class IntegrityError(Exception):
    def __init__(self, path, expected, got):
        super().__init__(f"{path}: expected SHA-256 {expected}, got {got}")
        self.path = path


def download(client, dataset_id, version_id, folder, workers=8, remove_erased=False, progress=None):
    """Writes the snapshot's files under folder. Files already there with
    the pinned SHA-256 are kept as they are, so an interrupted download
    resumes. Every file is verified before it replaces anything.

    Members whose bytes the platform erased are skipped; with
    remove_erased, local copies of them are deleted too, so the folder
    follows the erasure. Returns counts: downloaded, kept, erased, removed.
    """
    say = progress or (lambda message: None)
    os.makedirs(folder, exist_ok=True)
    root = os.path.abspath(folder)
    rows = list(members(client, dataset_id, version_id))

    def one(member):
        target = os.path.join(root, *safe_parts(member["path"]))
        if member.get("availability", "AVAILABLE") != "AVAILABLE":
            if remove_erased and os.path.exists(target):
                os.remove(target)
                return "removed"
            return "erased"
        if os.path.exists(target) and os.path.getsize(target) == member["size_bytes"] \
                and sha256_file(target) == member.get("checksum_sha256"):
            return "kept"
        os.makedirs(os.path.dirname(target), exist_ok=True)
        url = download_url(client, member["version_id"])
        h = hashlib.sha256()
        fd, tmp = tempfile.mkstemp(dir=os.path.dirname(target), prefix=".osp-")
        try:
            with os.fdopen(fd, "wb") as out, urllib.request.urlopen(url, timeout=600) as resp:
                while True:
                    block = resp.read(1024 * 1024)
                    if not block:
                        break
                    h.update(block)
                    out.write(block)
            if h.hexdigest() != member.get("checksum_sha256"):
                raise IntegrityError(member["path"], member.get("checksum_sha256"), h.hexdigest())
            os.replace(tmp, target)
        finally:
            if os.path.exists(tmp):
                os.remove(tmp)
        return "downloaded"

    counts = {"downloaded": 0, "kept": 0, "erased": 0, "removed": 0}
    with concurrent.futures.ThreadPoolExecutor(workers) as pool:
        for done, outcome in enumerate(pool.map(one, rows), 1):
            counts[outcome] += 1
            if done % 100 == 0:
                say(f"{done}/{len(rows)} files")
    return counts


def diff(client, dataset_id, base_version_id, target_version_id):
    """Compares two versions by object. Returns the summary (added,
    removed, changed, relabeled, unchanged) and every difference."""
    path = f"/api/v1/datasets/{dataset_id}/versions/{target_version_id}/diff"
    first = client.get(path, against=base_version_id)
    differences = list(first.get("differences") or [])
    after = first.get("next_after")
    while after:
        page = client.get(path, against=base_version_id, after=after)
        differences.extend(page.get("differences") or [])
        after = page.get("next_after")
    return {"summary": first["summary"], "differences": differences}


VALIDATION_DONE = {"COMPLETED", "FAILED", "CANCELLED"}


def validate(client, dataset_id, version_id, profile="builtin:general", mode="METADATA", poll=2.0):
    """Runs a validation of one version and waits for its report.

    profile "builtin:storage-audit" with mode "FULL" re-reads every pinned
    file from storage and hashes it again: the check for bytes changed or
    lost after the snapshot. Returns (report, findings), findings being the
    first page."""
    job = client.post(f"/api/v1/datasets/{dataset_id}/validations",
                      {"version_id": version_id, "profile_id": profile, "mode": mode})
    while job["state"] not in VALIDATION_DONE:
        time.sleep(poll)
        job = client.get(f"/api/v1/validation-jobs/{job['id']}")
    if not job.get("report_id"):
        raise RuntimeError(f"validation {job['id']} ended {job['state']}: {job.get('last_error', '')}")
    report = client.get(f"/api/v1/validation-reports/{job['report_id']}")
    findings = client.get(f"/api/v1/validation-reports/{job['report_id']}/findings").get("findings") or []
    return report, findings
