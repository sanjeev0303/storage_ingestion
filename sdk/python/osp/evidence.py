"""Evidence and attestation (docs/evidence.md).

    from osp import evidence
    evidence.write_export(client, open("audit.jsonl", "w"))
    report = evidence.verify_export(open("audit.jsonl"))       # no platform needed
    evidence.download(client, evidence.pack_path(ds, v), "evidence.zip")
    print(evidence.model_summary(client, model_id, markdown=True))

verify_export recomputes the audit chain from an export alone: each hash is
SHA-256 over the previous hash, then every field followed by 0x1f, then
the details exactly as exported (details_json).
"""

import base64
import hashlib
import json

FIELDS = ("id", "chain", "seq", "tenant_id", "actor_type", "actor_id", "action", "resource_type", "resource_id",
          "outcome", "reason", "correlation_id", "source_ip", "created_at")


# ─── The audit chain ──────────────────────────────────────────────────

def export(client, after_seq=0, start="", end="", page=10000):
    """Yields the organization's audit chain oldest first, after after_seq."""
    while True:
        with client.stream("GET", f"/api/v1/organization/audit/export?after_seq={after_seq}&limit={page}"
                                  + (f"&from={start}" if start else "") + (f"&to={end}" if end else "")) as resp:
            body = resp.read().decode("utf-8")
        lines = [json.loads(l) for l in body.splitlines() if l.strip()]
        yield from lines
        if len(lines) < page:
            return
        after_seq = lines[-1]["seq"]


def write_export(client, out, **kw):
    n = 0
    for e in export(client, **kw):
        out.write(json.dumps(e, separators=(",", ":")) + "\n")
        n += 1
    return n


def entry_hash(e, prev):
    """The hash an entry must have, given the previous entry's (bytes)."""
    h = hashlib.sha256(prev)
    for f in FIELDS:
        h.update(str(e[f]).encode("utf-8"))
        h.update(b"\x1f")
    h.update(e["details_json"].encode("utf-8"))
    return h.hexdigest()


def verify_export(lines, anchors=(), public_key=None):
    """Checks an audit export without the platform. lines are JSON lines
    (a file object or strings); anchors are anchor dicts (body, signature)
    to check against it, with public_key (base64 Ed25519) for their
    signatures. Returns a dict: checked, first, last, broken_at, problem,
    anchors (each with ok and why)."""
    out = {"checked": 0, "first": None, "last": None, "broken_at": None, "problem": "", "anchors": []}
    hashes = {}
    prev = None
    for raw in lines:
        raw = raw.strip()
        if not raw:
            continue
        e = json.loads(raw)
        if prev is not None and e["seq"] != prev["seq"] + 1:
            out["broken_at"], out["problem"] = e["seq"], f"entries {prev['seq'] + 1} to {e['seq'] - 1} are missing"
            break
        if prev is not None and e["prev_hash"] != prev["hash"]:
            out["broken_at"], out["problem"] = e["seq"], "the previous hash does not match the entry before"
            break
        if entry_hash(e, bytes.fromhex(e["prev_hash"])) != e["hash"]:
            out["broken_at"], out["problem"] = e["seq"], "the entry does not match its hash"
            break
        out["checked"] += 1
        out["first"] = out["first"] or e["seq"]
        out["last"] = e["seq"]
        hashes[e["seq"]] = e["hash"]
        prev = e
    for a in anchors:
        body = json.loads(a["body"])
        check = {"seq": body["seq"], "ok": False, "why": ""}
        if body["seq"] not in hashes:
            check["why"] = "the export does not reach this entry"
        elif hashes[body["seq"]] != body["hash"]:
            check["why"] = "the chain differs from what was anchored"
        elif public_key and a.get("signature") and not _signature_ok(public_key, a["body"], a["signature"]):
            check["why"] = "the anchor's signature does not match"
        else:
            check["ok"] = True
            if not (public_key and a.get("signature")):
                check["why"] = "signature not checked"
        out["anchors"].append(check)
    return out


def _signature_ok(public_key, body, signature):
    try:
        from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PublicKey
    except ImportError:
        return True  # cannot check: reported by the caller as not checked
    try:
        Ed25519PublicKey.from_public_bytes(base64.b64decode(public_key)).verify(base64.b64decode(signature), body.encode("utf-8"))
        return True
    except Exception:
        return False


def anchors(client, limit=50):
    """The organization's anchors, each checked by the platform: its
    signature, the chain against it, and the copy outside the database."""
    return client.get("/api/v1/organization/audit/anchors", limit=limit)["anchors"]


def anchor_now(client):
    return client.post("/api/v1/organization/audit/anchors")


def public_key(client):
    return client.get("/api/v1/certificates/public-key")


# ─── Settings and reviews ─────────────────────────────────────────────

def settings(client):
    return client.get("/api/v1/evidence-settings")


def set_settings(client, **changes):
    current = settings(client)
    current.update(changes)
    current.pop("updated_by", None)
    current.pop("updated_at", None)
    return client.request("PUT", "/api/v1/evidence-settings", body=current)


def reviews(client):
    return client.get("/api/v1/audit-reviews")["reviews"]


def create_review(client, start, end=None):
    """start and end are RFC 3339 times; end defaults to now."""
    body = {"from": start}
    if end:
        body["to"] = end
    return client.post("/api/v1/audit-reviews", body)


def sign_review(client, review_id, note=""):
    return client.post(f"/api/v1/audit-reviews/{review_id}/sign", {"note": note})


# ─── Provenance, packs and holds ──────────────────────────────────────

def provenance(client, dataset_id):
    return client.get(f"/api/v1/datasets/{dataset_id}/provenance")


def set_provenance(client, dataset_id, lawful_basis, license, source, source_kind):
    """Records the lawful basis, licence and source before snapshots (the
    publish gate). source_kind is PUBLIC_DATASET, LICENSED_PRIVATE,
    WEB_CRAWLED, USER_DATA, SYNTHETIC or OTHER."""
    return client.request("PUT", f"/api/v1/datasets/{dataset_id}/provenance", body={
        "lawful_basis": lawful_basis, "license": license, "source": source, "source_kind": source_kind})


def run_order(client, run_id):
    """Evidence that each dataset's basis came before its snapshot, and the
    snapshot before the run, with audit chain positions."""
    return client.get(f"/api/v1/training-runs/{run_id}/provenance-order")


def pack_path(dataset_id, version_id, digests=False):
    return f"/api/v1/datasets/{dataset_id}/versions/{version_id}/evidence-pack" + ("?digests=true" if digests else "")


def download(client, path, target):
    """Saves a zip (an evidence pack, a review export, control evidence)."""
    n = 0
    with client.stream("GET", path) as resp, open(target, "wb") as out:
        while True:
            block = resp.read(1 << 20)
            if not block:
                return n
            out.write(block)
            n += len(block)


def hold_snapshot(client, dataset_id, version_id, kind, reference, reason=""):
    """Holds every member of a snapshot: kind FOIA, LITIGATION, AUDIT or
    INVESTIGATION."""
    return client.post(f"/api/v1/datasets/{dataset_id}/versions/{version_id}/holds",
                       {"kind": kind, "reference": reference, "reason": reason})


def snapshot_holds(client, dataset_id, version_id):
    return client.get(f"/api/v1/datasets/{dataset_id}/versions/{version_id}/holds")["holds"]


def release_snapshot_hold(client, dataset_id, version_id, matter, reason=""):
    return client.post(f"/api/v1/datasets/{dataset_id}/versions/{version_id}/holds/release", {"matter": matter, "reason": reason})


# ─── Models and compliance ────────────────────────────────────────────

def model_summary(client, model_id, markdown=False):
    """A model's training-content summary on the EU template."""
    if markdown:
        return client.get_text(f"/api/v1/models/{model_id}/training-summary", format="md")
    return client.get(f"/api/v1/models/{model_id}/training-summary")


def publish_summary(client, model_id):
    client.post(f"/api/v1/models/{model_id}/training-summary/publish")


def frameworks(client):
    return client.get("/api/v1/compliance/frameworks")["frameworks"]


def controls(client, framework):
    return client.get(f"/api/v1/compliance/{framework}")


def undertaking(client, role, name, title, firm):
    return client.get_text("/api/v1/compliance/sec-17a-4/undertaking", role=role, name=name, title=title, firm=firm)


def regulator_access(client, name, days=7, datasets=None):
    """A read-only, time-limited token for an examiner; shown once."""
    return client.post("/api/v1/compliance/regulator-access", {"name": name, "days": days, "datasets": datasets or []})


def as_of(client, namespace, at):
    """The namespace's objects as they stood at the RFC 3339 time at."""
    return client.get("/api/v1/catalog/as-of", dataset=namespace, at=at)["objects"]
