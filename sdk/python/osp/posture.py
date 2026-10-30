"""Storage posture: the health of the buckets behind your connections.

Every connection test checks the bucket's settings: public access, ACLs,
default encryption, whether writes with customer-provided keys (SSE-C)
are refused, versioning, Object Lock, lifecycle rules that would delete
the platform's objects, and how broad the credentials are. A scheduled
scan finds objects changed or added outside the platform.

    report = check(client, connection_id, probe=True)
    for c in report["checks"]:
        print(c["status"], c["name"], c["detail"])

    stage_credentials(client, connection_id, access_key_id=..., secret_access_key=...)
    promote_credentials(client, connection_id)   # tested again first
    rollback_credentials(client, connection_id)  # within 7 days

    for f in drift(client, connection_id):
        print(f["kind"], f["object_key"], f["detail"])
"""


def posture(client, connection_id):
    """The connection's latest posture report."""
    return client.get(f"/api/v1/storage-connections/{connection_id}/posture")


def check(client, connection_id, probe=False):
    """Checks the bucket now. probe also tries a write with a
    customer-provided key, which the bucket must refuse."""
    return client.post(f"/api/v1/storage-connections/{connection_id}/posture", {"probe": probe})


def stage_credentials(client, connection_id, **credentials):
    """Tests new credentials, writing included, and keeps them next to the
    current ones without using them yet."""
    return client.post(f"/api/v1/storage-connections/{connection_id}/credentials/stage", {"credentials": credentials})


def promote_credentials(client, connection_id):
    """Makes the staged credentials current after testing them again."""
    return client.post(f"/api/v1/storage-connections/{connection_id}/credentials/promote")


def rollback_credentials(client, connection_id):
    """Puts back the credentials the last promotion replaced."""
    return client.post(f"/api/v1/storage-connections/{connection_id}/credentials/rollback")


def discard_staged(client, connection_id):
    return client.request("DELETE", f"/api/v1/storage-connections/{connection_id}/credentials/staged")


def set_checksum_mode(client, connection_id, revision, mode):
    """AUTO, WHEN_SUPPORTED or WHEN_REQUIRED."""
    return client.request("PATCH", f"/api/v1/storage-connections/{connection_id}",
                          {"revision": revision, "checksum_mode": mode})


def pin_owner(client, connection_id, revision, owner):
    """Pins the bucket's AWS account ID (or Google project number); every
    request then names it, and a bucket re-created by someone else is
    refused. "" unpins."""
    return client.request("PATCH", f"/api/v1/storage-connections/{connection_id}",
                          {"revision": revision, "expected_owner": owner})


def drift(client, connection_id="", everything=False):
    """Objects changed or added outside the platform: open findings, or
    all with everything=True. connection_id "" is every connection."""
    path = f"/api/v1/storage-connections/{connection_id}/drift" if connection_id else "/api/v1/storage-drift"
    params = {"all": "true"} if everything else {}
    return client.get(path, **params)["findings"]


def resolve(client, finding_id):
    return client.post(f"/api/v1/storage-drift/{finding_id}/resolve")


def integrity(client, dataset_id, version_id):
    """Whether a snapshot's members changed outside the platform."""
    return client.get(f"/api/v1/datasets/{dataset_id}/versions/{version_id}/integrity")


def set_data_class(client, dataset_id, revision, data_class):
    """Marks a dataset as holding PHI, PCI or PII ("" clears it). PHI is
    then refused on connections whose posture is not safe for it."""
    return client.request("PATCH", f"/api/v1/datasets/{dataset_id}", {"revision": revision, "data_class": data_class})


def set_encryption_key(client, connection_id, revision, key):
    """Encrypts every write to the connection with a customer-managed key:
    a KMS key ID, ARN or alias on S3, a Cloud KMS key name on GCS, or an
    encryption scope on Azure. It is tested with a write first. "" removes it."""
    return client.request("PATCH", f"/api/v1/storage-connections/{connection_id}",
                          {"revision": revision, "encryption_key": key})


def set_residency(client, dataset_id, revision, locations=None, require_customer_key=None):
    """Sets where a dataset's data may be stored (provider location codes,
    "eu-*" for a prefix; [] for anywhere), and whether a customer-managed
    key is required."""
    body = {"revision": revision}
    if locations is not None:
        body["residency"] = list(locations)
    if require_customer_key is not None:
        body["require_customer_key"] = require_customer_key
    return client.request("PATCH", f"/api/v1/datasets/{dataset_id}", body)


def residency(client, dataset_id, version_id):
    """Where a snapshot's bytes are, by connection and location, against
    what its dataset allows."""
    return client.get(f"/api/v1/datasets/{dataset_id}/versions/{version_id}/residency")
