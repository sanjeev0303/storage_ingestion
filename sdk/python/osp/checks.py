"""Content checks: what the quarantine gate found inside uploaded files.

Every upload is checked before it is published: active content in
documents, code in model files (pickles), credentials in text, broken
formats (gzip, BAM, FASTQ, MCAP, safetensors), and the verdict of any
external engine the operator runs. A finding either flags the file, which
is still published, or rejects it. A file found infected by a re-scan with
newer signatures after it was published is blocked: nothing is served.

    for f in findings(client, asset_id):
        print(f["code"], f["action"], f["detail"])

    flagged = version_findings(client, dataset_id, version_id)
    set_content_policy(client, finding_actions={"secret": "reject"})
"""

from concurrent.futures import ThreadPoolExecutor

from .snapshots import members


def findings(client, asset_id):
    """The content findings of one object version (an asset)."""
    return client.get(f"/api/v1/assets/{asset_id}/findings")["findings"]


def version_findings(client, dataset_id, version_id, workers=8):
    """{path: [findings]} for the members of a snapshot that have any."""
    rows = list(members(client, dataset_id, version_id))
    with ThreadPoolExecutor(max_workers=workers) as pool:
        found = pool.map(lambda m: findings(client, m["version_id"]), rows)
        return {m["path"]: f for m, f in zip(rows, found) if f}


def blast_radius(client, asset_id):
    """Where an asset's bytes went: every asset with the same SHA-256, the
    snapshots that pin them and who was given download links. Needs the
    audit.read permission."""
    return client.get(f"/api/v1/assets/{asset_id}/blast-radius")


def content_policy(client):
    """The organization's own content policy: types and per-finding actions."""
    return client.get("/api/v1/content-policy")["policy"]


def set_content_policy(client, finding_actions=None, allowed_types=None, denied_types=None,
                       max_size_bytes=0, unknown_type_action=""):
    """Replaces the organization's content policy. finding_actions maps a
    family ("secret") or a code ("model.pickle_imports") to allow, flag or
    reject; malware always rejects."""
    body = {
        "finding_actions": finding_actions or {},
        "allowed_types": allowed_types or [],
        "denied_types": denied_types or [],
        "max_size_bytes": max_size_bytes,
        "unknown_type_action": unknown_type_action,
    }
    return client.request("PUT", "/api/v1/content-policy", body)["policy"]
