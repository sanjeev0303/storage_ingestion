"""Retention, legal holds and governance.

    print([t["id"] for t in templates(client)])
    impact = set_policy(client, "TENANT", reason="17a-4", template="sec-17a-4", dry_run=True)
    set_policy(client, "TENANT", reason="17a-4", template="sec-17a-4", confirm_compliance=True)

    hold = place_hold(client, "litigation", matter="case-7", query={"path_prefix": "contracts/"})
    proof = hold_proof(client, hold["id"], verify=True)

A change that loosens protection under the two-person rule is not made:
it returns {"status": "PENDING_APPROVAL", "approval_id": ...} and waits for
another member to approve(client, approval_id).

Every permanent deletion leaves a certificate; verify_certificate checks
its SHA-256 and, with the `cryptography` package installed, its Ed25519
signature against the platform's public key.
"""

import base64
import hashlib
import urllib.parse


def _q(value):
    return urllib.parse.quote(value, safe="")


def templates(client):
    """The retention templates: SEC 17a-4, CFTC 1.31, CMS, Ofcom, NARA, MRC..."""
    return client.get("/api/v1/retention/templates")["templates"]


def set_policy(client, scope, reason, scope_key="", template="", lock_mode="", retain_from_key="",
               min_retention="", max_age="", deletion_grace="", confirm_compliance=False, dry_run=False):
    """Sets a retention policy (durations like "720h"), or with dry_run
    returns its impact. Answers the stored policy, the impact, or a
    pending approval."""
    body = {"scope": scope.upper(), "reason": reason}
    for key, value in (("scope_key", scope_key), ("template", template), ("lock_mode", lock_mode.upper()),
                       ("retain_from_key", retain_from_key), ("min_retention", min_retention),
                       ("max_age", max_age), ("deletion_grace", deletion_grace)):
        if value:
            body[key] = value
    if confirm_compliance:
        body["confirm_compliance"] = True
    if dry_run:
        body["dry_run"] = True
    return client.request("PUT", "/api/v1/retention/policies", body)


def holds(client, include_released=False):
    return client.get("/api/v1/legal-holds", include_released="true" if include_released else None)["holds"]


def place_hold(client, reason, scope="QUERY", scope_key="", query=None, matter="", custodians=None):
    """Places a legal hold. A QUERY hold ({"dataset", "path_prefix",
    "content_type_prefix", "metadata", "created_after", "created_before"})
    also covers assets created after it."""
    body = {"scope": scope.upper(), "reason": reason}
    if scope_key:
        body["scope_key"] = scope_key
    if query:
        body["query"] = query
    if matter:
        body["matter"] = matter
    if custodians:
        body["custodians"] = list(custodians)
    return client.post("/api/v1/legal-holds", body)


def release_hold(client, hold_id, reason=""):
    """Releases a hold: None when done, or the pending approval."""
    return client.request("DELETE", f"/api/v1/legal-holds/{_q(hold_id)}", params={"reason": reason})


def hold_proof(client, hold_id, verify=False):
    """The assets a hold covers and when each lock took effect; with verify,
    what the provider holds now (MATCH, or what differs)."""
    return client.get(f"/api/v1/legal-holds/{_q(hold_id)}/proof", verify="true" if verify else None)


def governance(client):
    return client.get("/api/v1/governance")


def set_governance(client, profile="STANDARD", two_person=False, approve_deletes=False, reason=""):
    """profile: STANDARD, GXP_VAULT or SEC_17A4_AUDIT_TRAIL."""
    body = {"profile": profile, "two_person": two_person, "approve_deletes": approve_deletes, "reason": reason}
    return client.request("PUT", "/api/v1/governance", body)


def approvals(client, state="PENDING"):
    return client.get("/api/v1/approvals", state=state or None)["approvals"]


def approve(client, approval_id, note=""):
    """Approves and carries out a change another member asked for."""
    return client.post(f"/api/v1/approvals/{_q(approval_id)}/approve", {"note": note})


def reject(client, approval_id, note=""):
    return client.post(f"/api/v1/approvals/{_q(approval_id)}/reject", {"note": note})


def bulk_delete(client, reason, asset_ids=None, dataset="", path_prefix="", metadata=None, dry_run=True):
    """Deletes many assets; dry_run (the default) only reports what matches
    and what holds, retention or locks would keep."""
    body = {"reason": reason, "dry_run": dry_run}
    if asset_ids:
        body["asset_ids"] = list(asset_ids)
    if dataset:
        body["dataset"] = dataset
    if path_prefix:
        body["path_prefix"] = path_prefix
    if metadata:
        body["metadata"] = metadata
    return client.post("/api/v1/assets/bulk-delete", body)


def certificate(client, deletion_id):
    return client.get(f"/api/v1/deletions/{_q(deletion_id)}/certificate")


def public_key(client):
    return client.get("/api/v1/certificates/public-key")


def verify_certificate(cert, public_key_b64=None):
    """Returns (hash_ok, signature) where signature is "valid", "invalid",
    "unsigned" or "unchecked" (the `cryptography` package is missing)."""
    signed = cert["signed_body"].encode()
    hash_ok = hashlib.sha256(signed).hexdigest() == cert["body_sha256"]
    if not cert.get("signature") or not public_key_b64:
        return hash_ok, "unsigned"
    try:
        from cryptography.exceptions import InvalidSignature
        from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PublicKey
    except ImportError:
        return hash_ok, "unchecked"
    key = Ed25519PublicKey.from_public_bytes(base64.b64decode(public_key_b64))
    try:
        key.verify(base64.b64decode(cert["signature"]), signed)
        return hash_ok, "valid"
    except InvalidSignature:
        return hash_ok, "invalid"
