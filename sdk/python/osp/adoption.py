"""Adoption: bring data already in your bucket under the platform without
uploading it again, and import URL lists and STAC catalogs.

    job = adopt(client, "S3_INVENTORY", connection_id="conn-1",
                source_ref="inventory/lab/2026-10-30T01-00Z/manifest.json", dataset="sequencing")
    job = wait(client, job["id"])
    for row in results(client, job["id"]):
        print(row["line"], row["state"], row["code"], row["path"])

Sources: MANIFEST, S3_INVENTORY, MHL, BAGIT (adopted where they are, from
one of your own storage connections), URL_LIST and STAC (imported).
"""

import time
import urllib.parse


def _q(value):
    return urllib.parse.quote(value, safe="")


def adopt(client, source, connection_id="", source_ref="", inline="", dataset="", mode="DETACH", require_digest=True):
    """Starts an adoption job. mode DETACH keeps your objects when the
    assets are deleted; MANAGE lets the platform delete them."""
    body = {"source": source.upper(), "mode": mode.upper(), "require_digest": require_digest}
    for key, value in (("connection_id", connection_id), ("source_ref", source_ref), ("inline", inline), ("dataset", dataset)):
        if value:
            body[key] = value
    return client.post("/api/v1/adoptions", body)


def jobs(client):
    return client.get("/api/v1/adoptions")["adoptions"]


def job(client, job_id):
    return client.get(f"/api/v1/adoptions/{_q(job_id)}")


def results(client, job_id, page_size=1000):
    """Yields each entry's result, in line order."""
    after = 0
    while True:
        page = client.get(f"/api/v1/adoptions/{_q(job_id)}/results", after=after or None, limit=page_size)
        yield from page["entries"]
        after = page.get("next_after")
        if not after:
            return


def cancel(client, job_id):
    client.post(f"/api/v1/adoptions/{_q(job_id)}/cancel")


def wait(client, job_id, timeout=3600, poll=5):
    """Waits for a job to finish and returns it."""
    deadline = time.monotonic() + timeout
    while True:
        j = job(client, job_id)
        if j["state"] in ("COMPLETED", "FAILED", "CANCELLED") or time.monotonic() > deadline:
            return j
        time.sleep(poll)


def export_mhl(client, dataset_id, version_id):
    """The snapshot as an ASC MHL v2 list (XML text)."""
    return client.get_text(f"/api/v1/datasets/{_q(dataset_id)}/versions/{_q(version_id)}/mhl")


def export_bag_manifest(client, dataset_id, version_id):
    """The snapshot's BagIt manifest-sha256.txt."""
    return client.get_text(f"/api/v1/datasets/{_q(dataset_id)}/versions/{_q(version_id)}/manifest-sha256.txt")
