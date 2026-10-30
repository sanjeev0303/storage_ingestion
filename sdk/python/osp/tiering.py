"""Tiering, restores, costs and snapshot expiry (docs/tiering.md).

    from osp import tiering
    tiering.add_rule(client, "cold after 90 days", "ARCHIVE", min_age_days=90, unread_days=30)
    est = tiering.version_estimate(client, dataset_id, version_id)
    tiering.restore_version(client, dataset_id, version_id, days=7, speed="STANDARD", wait=True)
    preview = tiering.expire(client, dataset_id, keep_last=5, dry_run=True)

Estimates use the providers' list prices; real bills depend on region,
contract and discounts.
"""

import json
import time

TIERS = ("STANDARD", "INFREQUENT", "ARCHIVE", "DEEP_ARCHIVE")
SPEEDS = ("FAST", "STANDARD", "BULK")


def _ds(dataset_id):
    return f"/api/v1/datasets/{dataset_id}"


# ─── Rules ────────────────────────────────────────────────────────────

def rules(client):
    return client.get("/api/v1/tier-rules")["rules"]


def add_rule(client, name, tier, min_age_days=0, unread_days=0, dataset_id=""):
    """A rule moving objects to tier once they are min_age_days old and
    unread for unread_days. Held, locked, packed, shared and in-use objects
    are skipped."""
    return client.post("/api/v1/tier-rules", {"name": name, "tier": tier, "min_age_days": min_age_days,
                                              "unread_days": unread_days, "dataset_id": dataset_id})


def set_rule(client, rule_id, name, tier, min_age_days=0, unread_days=0, enabled=True):
    return client.request("PUT", f"/api/v1/tier-rules/{rule_id}", body={
        "name": name, "tier": tier, "min_age_days": min_age_days, "unread_days": unread_days, "enabled": enabled})


def delete_rule(client, rule_id):
    client.request("DELETE", f"/api/v1/tier-rules/{rule_id}")


def moves(client):
    return client.get("/api/v1/tier-moves")["moves"]


# ─── Restores ─────────────────────────────────────────────────────────

def restore_asset(client, asset_id, days=7, speed="STANDARD"):
    return client.post(f"/api/v1/assets/{asset_id}/archive-restores", {"days": days, "speed": speed})


def restore_version(client, dataset_id, version_id, days=7, speed="STANDARD", wait=False, every=60, timeout=None):
    """Asks for every archived member of a snapshot to be readable. With
    wait, returns once all are (or one failed). The answer has archived,
    requested, already_open, readable and the estimate."""
    out = client.post(f"{_ds(dataset_id)}/versions/{version_id}/archive-restores", {"days": days, "speed": speed})
    if wait and out["restores"]:
        out["restores"] = wait_restores(client, version_id=version_id, every=every, timeout=timeout)
    return out


def restores(client, asset_id="", version_id=""):
    return client.get("/api/v1/archive-restores", asset_id=asset_id, version_id=version_id)["restores"]


def wait_restores(client, asset_id="", version_id="", every=60, timeout=None):
    """Waits until no restore of the asset or version is pending."""
    deadline = time.monotonic() + timeout if timeout else None
    while True:
        rs = restores(client, asset_id, version_id)
        if not any(r["state"] in ("PENDING", "RESTORING") for r in rs):
            return rs
        if deadline and time.monotonic() > deadline:
            raise TimeoutError("restores still running")
        time.sleep(every)


# ─── Costs ────────────────────────────────────────────────────────────

def estimate(client, operation, files=0, bytes=0, connection_id="", tier="", years=0, epochs=0, speed="", requests=0):
    """Prices work: operation is ingest, download, epoch, retain or restore."""
    body = {"operation": operation, "files": files, "bytes": bytes, "connection_id": connection_id, "tier": tier,
            "years": years, "epochs": epochs, "speed": speed, "requests": requests}
    return client.post("/api/v1/cost-estimates", body)


def version_estimate(client, dataset_id, version_id, speed=""):
    """What downloading a snapshot, one epoch over it, and restoring its
    archived files first would cost and take."""
    return client.get(f"{_ds(dataset_id)}/versions/{version_id}/cost-estimate", speed=speed)


def dataset_costs(client, dataset_id, years=10):
    return client.get(f"{_ds(dataset_id)}/costs", years=years)


def report(client):
    return client.get("/api/v1/cost-report")


def prices(client):
    return client.get("/api/v1/cost-prices")["prices"]


def budget(client):
    return client.get("/api/v1/cost-budget")


def set_budget(client, monthly_usd=0, monthly_requests=0, action="ALERT"):
    """0 is no limit. BLOCK refuses downloads past the budget; ALERT only
    raises a COST_BUDGET alert."""
    return client.request("PUT", "/api/v1/cost-budget", body={
        "monthly_usd": monthly_usd, "monthly_requests": monthly_requests, "action": action})


# ─── Snapshot expiry ──────────────────────────────────────────────────

def expire(client, dataset_id, keep_last=1, older_than_days=0, dry_run=True):
    """Previews (dry_run) or expires a dataset's old snapshots: all but the
    newest keep_last, older than older_than_days, and none still in use.
    Objects only they held are deleted through the lifecycle."""
    return client.post(f"{_ds(dataset_id)}/version-expiry",
                       {"keep_last": keep_last, "older_than_days": older_than_days, "dry_run": dry_run})


# ─── Storage manifests and Iceberg ────────────────────────────────────

def storage_manifest(client, dataset_id, version_id):
    """Yields a snapshot's members with where their bytes are on your own
    storage (uri: s3://, gs:// or az://), in path order. Members on the
    platform's own storage, or packed, have no uri."""
    after = ""
    path = f"{_ds(dataset_id)}/versions/{version_id}/storage-manifest"
    while True:
        page = client.get(path, after=after)
        yield from page["lines"]
        after = page.get("next_after")
        if not after:
            return


def write_manifest(client, dataset_id, version_id, out):
    """Writes the storage manifest as JSON Lines to the file object out."""
    n = 0
    for line in storage_manifest(client, dataset_id, version_id):
        out.write(json.dumps(line) + "\n")
        n += 1
    return n


def register_iceberg(client, dataset_id, version_id, catalog, table_name, schema=None):
    """Registers a snapshot's Parquet files as an Iceberg table with
    pyiceberg, without copying them: the table's data files are the
    snapshot's pinned objects. catalog is a pyiceberg catalog that can read
    your bucket. schema (a pyarrow schema) defaults to the first file's,
    read with pyarrow's file systems (s3:// and gs://). Returns the table."""
    lines = [l for l in storage_manifest(client, dataset_id, version_id) if l["path"].lower().endswith(".parquet")]
    files = [l["uri"] for l in lines if l.get("uri")]
    if not files:
        raise ValueError("the snapshot has no Parquet files on your own storage to register")
    if len(files) < len(lines):
        raise ValueError(f"{len(lines) - len(files)} Parquet files are packed or on the platform's storage; "
                         "they cannot be registered in place")
    if schema is None:
        try:
            import pyarrow.fs as pafs
            import pyarrow.parquet as pq
        except ImportError:
            raise ImportError("registering an Iceberg table needs pyiceberg and pyarrow: pip install 'pyiceberg[pyarrow]'") from None
        fs, path = pafs.FileSystem.from_uri(files[0])
        with fs.open_input_file(path) as f:
            schema = pq.read_schema(f)
    table = catalog.create_table_if_not_exists(table_name, schema=schema)
    table.add_files(file_paths=files)
    return table
