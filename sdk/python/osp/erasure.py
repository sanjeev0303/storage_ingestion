"""Erasure, withdrawal and destroy-by-provenance (docs/erasure.md).

    from osp import erasure
    plan = erasure.search(client, subject_ids=["P-1042"])
    e = erasure.create(client, "SUBJECT_WITHDRAWAL", "participant withdrew", subject_ids=["P-1042"],
                       institution="Example University")
    erasure.approve(another_client, e["id"])          # when the organization wants two people
    erasure.execute(client, e["id"])
    e = erasure.wait(client, e["id"])
    cert = erasure.certificate(client, e["id"])
    erasure.verify_certificate(cert)                   # Ed25519, no platform needed

A plan finds every copy of the content (the same SHA-256 on any of the
organization's connections), every snapshot that pins it, every draft that
holds it and every model trained on those snapshots. Executing it makes a
successor of each snapshot without those files, stops reads of the old
one, and deletes the bytes everywhere a legal hold allows; held files wait
until their hold is released.
"""

import base64
import hashlib
import json
import time

KINDS = ("SUBJECT_WITHDRAWAL", "COPYRIGHT", "PII", "CONSENT_EXPIRED", "OTHER")
DONE = ("COMPLETED", "CANCELLED", "FAILED")


def _selector(subject_ids=None, sha256s=None, object_ids=None, subject_key=None):
    body = {"subject_ids": list(subject_ids or []), "sha256s": [s.lower() for s in sha256s or []],
            "object_ids": list(object_ids or [])}
    if subject_key:
        body["subject_key"] = subject_key
    return body


def search(client, subject_ids=None, sha256s=None, object_ids=None, subject_key=None):
    """What an erasure would reach, without recording anything."""
    return client.post("/api/v1/erasures/search", _selector(subject_ids, sha256s, object_ids, subject_key))


def create(client, kind, reason, subject_ids=None, sha256s=None, object_ids=None, subject_key=None,
           reference="", institution=""):
    """Records an erasure request with its plan. kind is one of KINDS."""
    body = _selector(subject_ids, sha256s, object_ids, subject_key)
    body.update({"kind": kind, "reason": reason, "reference": reference, "institution": institution})
    return client.post("/api/v1/erasures", body)


def sha256s_from_manifest(lines):
    """The sha256 of each line of a JSON-lines manifest (a file object or
    strings), as bulk ingestion writes them."""
    out = []
    for raw in lines:
        raw = raw.strip()
        if raw:
            out.append(json.loads(raw)["sha256"].lower())
    return out


def list_erasures(client, state=None):
    return client.get("/api/v1/erasures", state=state)["erasures"]


def get(client, erasure_id):
    return client.get(f"/api/v1/erasures/{erasure_id}")


def approve(client, erasure_id, note=""):
    """The second person's approval, where the organization requires one."""
    return client.post(f"/api/v1/erasures/{erasure_id}/approve", {"note": note})


def execute(client, erasure_id):
    return client.post(f"/api/v1/erasures/{erasure_id}/execute")


def cancel(client, erasure_id, note=""):
    return client.post(f"/api/v1/erasures/{erasure_id}/cancel", {"note": note})


def wait(client, erasure_id, every=10.0, timeout=3600, settle=None):
    """Waits until the erasure is completed, cancelled or failed, or (with
    settle) also while it only waits on a legal hold."""
    deadline = time.monotonic() + timeout
    while True:
        e = get(client, erasure_id)
        if e["state"] in DONE or (settle and e["state"] == "WAITING_ON_HOLD") or time.monotonic() > deadline:
            return e
        time.sleep(every)


def certificate(client, erasure_id):
    """The signed certificate of destruction: body, signature, key_id and
    the platform's public key."""
    return client.get(f"/api/v1/erasures/{erasure_id}/certificate")


def verify_certificate(cert, public_key=None):
    """Checks the certificate's Ed25519 signature over its body, with the
    public key it names or one given. Returns the parsed body; raises
    ValueError when the signature does not match."""
    from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PublicKey
    from cryptography.exceptions import InvalidSignature
    key = public_key or cert["public_key"]
    try:
        Ed25519PublicKey.from_public_bytes(base64.b64decode(key)).verify(
            base64.b64decode(cert["signature"]), cert["body"].encode("utf-8"))
    except InvalidSignature:
        raise ValueError("the certificate's signature does not match its body")
    body = json.loads(cert["body"])
    if cert.get("body_sha256") and hashlib.sha256(cert["body"].encode("utf-8")).hexdigest() != cert["body_sha256"]:
        raise ValueError("the certificate's body does not match its SHA-256")
    return body


def subject_hash(subject_id):
    """How a certificate names a subject: the SHA-256 of the identifier, so
    the certificate can be shared without it."""
    return hashlib.sha256(subject_id.encode("utf-8")).hexdigest()
