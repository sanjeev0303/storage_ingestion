"""Connectors (docs/connectors.md): bring comms-archive and forensic exports
in with their hashes checked, share files to chat, and use a dataset as a
Git LFS store.

    from osp import connectors
    report = connectors.import_export(client, "COMMS_JSONL", "smarsh.jsonl", "comms-2026", retain="61320h")
    print(report["verified"], report["mismatches"])

The platform does not capture messages or collect from devices: it is where
those tools' exports are preserved. Each item's declared hash is compared
with its bytes here, and the platform computes SHA-256 again when it is
uploaded. A file that differs from what the export declares is still kept
(it is evidence) and is listed as a mismatch.
"""

import csv
import email
import email.policy
import hashlib
import json
import os
import re
import shutil
import subprocess
import tempfile
import xml.etree.ElementTree as ET
import zipfile

from . import ingest, quality, snapshots

FORMATS = ("COMMS_JSONL", "RSMF", "UFDR", "MANIFEST")
HASHES = {32: "md5", 40: "sha1", 64: "sha256"}


def sha256_of(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for block in iter(lambda: f.read(1 << 20), b""):
            h.update(block)
    return h.hexdigest()


def _matches(data_path, declared):
    """Whether a file has the hash an export declares (its length says the
    algorithm: MD5, SHA-1 or SHA-256)."""
    declared = declared.strip().lower().removeprefix("sha256:")
    algorithm = HASHES.get(len(declared))
    if not algorithm:
        return False
    h = hashlib.new(algorithm)
    with open(data_path, "rb") as f:
        for block in iter(lambda: f.read(1 << 20), b""):
            h.update(block)
    return h.hexdigest() == declared


def _safe(path):
    """A path from an export, kept inside the folder being built."""
    parts = [p for p in re.split(r"[\\/]+", path) if p not in ("", ".", "..")]
    if not parts:
        raise ValueError(f"an empty path in the export: {path!r}")
    return os.path.join(*[re.sub(r"[:*?\"<>|]", "_", p) for p in parts])


class Result:
    def __init__(self):
        self.items, self.verified = 0, 0
        self.mismatches = []

    def check(self, name, path, declared):
        self.items += 1
        if not declared:
            self.mismatches.append(f"{name}: no hash declared")
        elif _matches(path, declared):
            self.verified += 1
        else:
            self.mismatches.append(f"{name}: differs from the declared {declared.strip()[:80]}")


def read_comms_jsonl(export, out):
    """One message per line, as vendors' archive exports give them: id,
    sha256, and either the message inline or a file beside the export. The
    line itself is kept, so nothing the vendor recorded is lost."""
    result = Result()
    folder = os.path.dirname(os.path.abspath(export))
    os.makedirs(os.path.join(out, "messages"), exist_ok=True)
    with open(export, encoding="utf-8") as f:
        for number, line in enumerate(f, 1):
            line = line.strip()
            if not line:
                continue
            msg = json.loads(line)
            name = re.sub(r"[^A-Za-z0-9._-]", "_", str(msg.get("id") or msg.get("message_id") or f"line-{number}"))
            if msg.get("file"):
                source = os.path.join(folder, _safe(msg["file"]))
                if not os.path.isfile(source):
                    result.items += 1
                    result.mismatches.append(f"{name}: the export names a file that is not there: {msg['file']}")
                    continue
                target = os.path.join(out, "messages", name + os.path.splitext(source)[1])
                shutil.copyfile(source, target)
            else:
                target = os.path.join(out, "messages", name + ".txt")
                with open(target, "w", encoding="utf-8", newline="") as t:
                    t.write(str(msg.get("body", "")))
            result.check(name, target, str(msg.get("sha256") or msg.get("hash") or ""))
            with open(os.path.join(out, "messages", name + ".meta.json"), "w", encoding="utf-8") as m:
                json.dump(msg, m, ensure_ascii=False)
    return result


def read_rsmf(export, out):
    """Relativity Short Message Format: an email whose rsmf.zip attachment
    holds rsmf_manifest.json and the attachments. The .rsmf is preserved
    whole, and each attachment the manifest names is checked to be there."""
    result = Result()
    with open(export, "rb") as f:
        message = email.message_from_binary_file(f, policy=email.policy.default)
    if not message.get("X-RSMF-Version"):
        raise ValueError("not an RSMF file: no X-RSMF-Version header")
    zipped = None
    for part in message.iter_attachments():
        if (part.get_filename() or "").lower() == "rsmf.zip":
            zipped = part.get_payload(decode=True)
    if zipped is None:
        raise ValueError("not an RSMF file: no rsmf.zip attachment")
    shutil.copyfile(export, os.path.join(out, os.path.basename(export)))
    zip_path = os.path.join(out, ".rsmf.zip")
    with open(zip_path, "wb") as z:
        z.write(zipped)
    with zipfile.ZipFile(zip_path) as z:
        manifest = json.loads(z.read("rsmf_manifest.json"))
        names = set(z.namelist())
        result.items += 1
        result.verified += 1  # the .rsmf itself, kept as it came
        for event in manifest.get("events", []):
            result.items += 1
            missing = [a.get("id", "?") for a in event.get("attachments", []) if a.get("id") not in names]
            if missing:
                result.mismatches.append(f"event {event.get('id', '?')}: attachments not in the file: {', '.join(missing)}")
            else:
                result.verified += 1
        z.extractall(os.path.join(out, "extracted"), [n for n in names if not n.endswith("/") and ".." not in n])
    os.remove(zip_path)
    return result


def read_ufdr(export, out):
    """Cellebrite UFDR: a zip with report.xml, whose tagged files name a
    local path and their hashes."""
    result = Result()
    with zipfile.ZipFile(export) as z:
        names = set(z.namelist())
        report = next((n for n in names if n.lower().endswith("report.xml")), None)
        if report is None:
            raise ValueError("not a UFDR file: no report.xml")
        root = ET.fromstring(z.read(report))
        seen = 0
        for node in root.iter():
            if not node.tag.endswith("file"):
                continue
            values = {}
            for item in node.iter():
                if item.tag.endswith("item") and item.get("name"):
                    values[item.get("name").strip().lower()] = (item.text or "").strip()
            local = values.get("local path")
            if not local:
                continue
            seen += 1
            local = local.replace("\\", "/")
            if local not in names:
                result.items += 1
                result.mismatches.append(f"{local}: the report names a file that is not in the export")
                continue
            target = os.path.join(out, _safe(local))
            os.makedirs(os.path.dirname(target), exist_ok=True)
            with z.open(local) as src, open(target, "wb") as dst:
                shutil.copyfileobj(src, dst)
            result.check(local, target, values.get("sha256") or values.get("sha1") or values.get("md5") or "")
        if seen == 0:
            raise ValueError("no files with a local path in report.xml")
        with open(os.path.join(out, "report.xml"), "wb") as r:
            r.write(z.read(report))
    return result


def read_manifest(export, out):
    """A hash list beside the files: JSON lines or CSV with path and sha256
    (or md5, sha1), paths relative to the list."""
    result = Result()
    folder = os.path.dirname(os.path.abspath(export))
    with open(export, encoding="utf-8", newline="") as f:
        if export.lower().endswith((".jsonl", ".ndjson")):
            rows = [json.loads(line) for line in f if line.strip()]
        else:
            rows = list(csv.DictReader(f))
    for row in rows:
        row = {k.strip().lower(): (v or "") if isinstance(v, str) else v for k, v in row.items()}
        path = row.get("path") or row.get("file") or row.get("filename")
        if not path:
            raise ValueError("a manifest row has no path")
        source = os.path.join(folder, _safe(path))
        if not os.path.isfile(source):
            result.items += 1
            result.mismatches.append(f"{path}: not there")
            continue
        target = os.path.join(out, _safe(path))
        os.makedirs(os.path.dirname(target), exist_ok=True)
        shutil.copyfile(source, target)
        result.check(path, target, str(row.get("sha256") or row.get("sha1") or row.get("md5") or ""))
    return result


READERS = {"COMMS_JSONL": read_comms_jsonl, "RSMF": read_rsmf, "UFDR": read_ufdr, "MANIFEST": read_manifest}


def import_export(client, fmt, export, dataset, source="", retain=None, retain_lock="", progress=None):
    """Reads an export, uploads what it holds to the dataset, applies
    retention (a duration such as 61320h, with lock mode COMPLIANCE or
    GOVERNANCE) before the first file arrives, and records the collection."""
    fmt = fmt.upper()
    if fmt not in READERS:
        raise ValueError(f"format is one of {', '.join(FORMATS)}")
    if retain:
        set_retention(client, dataset, retain, f"collection {os.path.basename(export)}", retain_lock)
    work = tempfile.mkdtemp(prefix="osp-import-")
    try:
        result = READERS[fmt](export, work)
        summary = ingest.ingest_folder(client, work, dataset, progress=progress)
    finally:
        shutil.rmtree(work, ignore_errors=True)
    ds = snapshots.find_dataset(client, dataset)
    record = client.post(f"/api/v1/datasets/{ds['id']}/collections", {
        "format": fmt, "source": source or os.path.basename(export), "export_sha256": sha256_of(export), "items": result.items,
        "verified": result.verified, "mismatches": result.mismatches[:1000], "retention": retain or ""})
    record["uploaded"] = summary
    return record


def set_retention(client, dataset, duration, reason, lock_mode=""):
    body = {"scope": "DATASET", "scope_key": dataset, "min_retention": duration, "reason": reason}
    if lock_mode:
        body["lock_mode"] = lock_mode.upper()
        body["confirm_compliance"] = lock_mode.upper() == "COMPLIANCE"
    return client.request("PUT", "/api/v1/retention/policies", body=body)


# ─── Chat ─────────────────────────────────────────────────────────────

def add_chat(client, kind, name, webhook_url):
    """Records a Slack or Teams incoming webhook; the URL is sealed."""
    return client.post("/api/v1/chat-integrations", {"kind": kind, "name": name, "url": webhook_url})


def chat_integrations(client):
    return client.get("/api/v1/chat-integrations")["integrations"]


def share_to_chat(client, asset_id, integration_id, note="", expires_in=""):
    """Posts a link instead of the file: it needs a signed-in member and
    stops working at its expiry, so a chat export holds only dead links."""
    return client.post("/api/v1/chat-shares", {"asset_id": asset_id, "integration_id": integration_id, "note": note, "expires_in": expires_in})


def revoke_share(client, share_id):
    return client.request("DELETE", f"/api/v1/chat-shares/{share_id}")


# ─── Git LFS ──────────────────────────────────────────────────────────

def lfs_url(base_url, dataset):
    return f"{base_url.rstrip('/')}/lfs/{dataset}"


def lfs_files(repo="."):
    """(path, oid) of each LFS file at HEAD, from git lfs itself."""
    out = subprocess.run(["git", "lfs", "ls-files", "--long"], cwd=repo, capture_output=True, text=True, check=True).stdout
    files = []
    for line in out.splitlines():
        m = re.match(r"^([0-9a-f]{64}) [-*] (.+)$", line)
        if m:
            files.append((m.group(2), m.group(1)))
    return files


def promote(client, dataset, label, repo=".", notes="", hold=None):
    """Promotes the repository's LFS files at HEAD to a release: pins each in
    the dataset's draft, under its repository path, snapshots and labels it,
    and with hold (a matter reference) puts a legal hold on it, so what
    shipped cannot change or go away."""
    ds = snapshots.find_dataset(client, dataset)
    files = lfs_files(repo)
    if not files:
        raise ValueError("no Git LFS files at HEAD")
    members = [{"namespace": dataset, "path": f".lfs/{oid[:2]}/{oid}", "metadata": {"repo_path": path}} for path, oid in files]
    for start in range(0, len(members), 1000):
        client.post(f"/api/v1/datasets/{ds['id']}/members", {"members": members[start:start + 1000], "on_duplicate": "REPLACE"})
    version = snapshots.snapshot(client, ds["id"])
    release = quality.release(client, ds["id"], version["id"], label, notes)
    if hold:
        client.post(f"/api/v1/datasets/{ds['id']}/versions/{version['id']}/holds", {"kind": "AUDIT", "reference": hold, "reason": f"release {label}"})
    return release


def dicomweb_url(base_url, dataset_id, version_id):
    """The DICOMweb root of a snapshot: QIDO-RS and WADO-RS, read-only, with
    the platform's bearer token."""
    return f"{base_url.rstrip('/')}/dicomweb/{dataset_id}/{version_id}"
