"""Protection: copies at a second connection, and moving data between
connections, always verified by SHA-256.

    protect(client, dataset_id, revision, replica_connection_id=conn, escrow=True, scrub_days=30)
    replication(client, dataset_id)          # how much is copied; the recovery point

    plan = migrate(client, dataset_id, target_connection_id, dry_run=True)
    job = migrate(client, dataset_id, target_connection_id)["job"]
    wait(client, job["id"])

    restore(client, dataset_id, version_id, target_connection_id, prefix="incident-42")
    drill(client, dataset_id, sample=200, from_replica=True)
    export(client, target_connection_id)     # the export-everything bundle
"""

import time


def protect(client, dataset_id, revision, replica_connection_id=None, escrow=None, vault_retention_days=None, scrub_days=None):
    """Sets a dataset's protection: a replica connection ("" removes it),
    escrow before deletion, a locked vault's retention, scrubs every N days."""
    body = {"revision": revision}
    for key, value in (("replica_connection_id", replica_connection_id), ("escrow", escrow),
                       ("vault_retention_days", vault_retention_days), ("scrub_days", scrub_days)):
        if value is not None:
            body[key] = value
    return client.request("PATCH", f"/api/v1/datasets/{dataset_id}", body)


def replication(client, dataset_id):
    return client.get(f"/api/v1/datasets/{dataset_id}/replication")


def scrubs(client, dataset_id):
    """Members whose last scrub found them damaged, or repaired them."""
    return client.get(f"/api/v1/datasets/{dataset_id}/scrubs")["scrubs"]


def migrate(client, dataset_id, target_connection_id, dry_run=False, idempotency_key=""):
    body = {"target_connection_id": target_connection_id, "dry_run": dry_run}
    if idempotency_key:
        body["idempotency_key"] = idempotency_key
    return client.post(f"/api/v1/datasets/{dataset_id}/migrations", body)


def restore(client, dataset_id, version_id, target_connection_id, prefix="", idempotency_key=""):
    body = {"version_id": version_id, "target_connection_id": target_connection_id, "prefix": prefix}
    if idempotency_key:
        body["idempotency_key"] = idempotency_key
    return client.post(f"/api/v1/datasets/{dataset_id}/restores", body)


def drill(client, dataset_id, sample=100, from_replica=False):
    return client.post(f"/api/v1/datasets/{dataset_id}/drills", {"sample": sample, "from_replica": from_replica})


def export(client, target_connection_id, idempotency_key=""):
    body = {"target_connection_id": target_connection_id}
    if idempotency_key:
        body["idempotency_key"] = idempotency_key
    return client.post("/api/v1/exports", body)


def set_export_schedule(client, connection_id, interval_hours):
    """Writes the bundle every interval_hours; 0 stops it."""
    return client.request("PUT", "/api/v1/export-schedule", {"connection_id": connection_id, "interval_hours": interval_hours})


def mark_clean(client, dataset_id, version_id):
    """Marks the last snapshot known good; "" clears it."""
    client.request("PUT", f"/api/v1/datasets/{dataset_id}/clean-point", {"version_id": version_id})


def changes_since_clean(client, dataset_id):
    return client.get(f"/api/v1/datasets/{dataset_id}/clean-point/diff")


def jobs(client, dataset_id=""):
    params = {"dataset_id": dataset_id} if dataset_id else {}
    return client.get("/api/v1/copy-jobs", **params)["jobs"]


def job(client, job_id):
    return client.get(f"/api/v1/copy-jobs/{job_id}")


def failed_items(client, job_id):
    return client.get(f"/api/v1/copy-jobs/{job_id}/items", state="FAILED")["items"]


def wait(client, job_id, every=5.0, timeout=24 * 3600):
    """Waits for a job to end and returns it."""
    deadline = time.monotonic() + timeout
    while True:
        j = job(client, job_id)
        if j["state"] in ("COMPLETED", "FAILED", "CANCELLED") or time.monotonic() > deadline:
            return j
        time.sleep(every)
