"""Dataset branches and three-way merges.

A branch is a dataset of its own that starts with the members of one
snapshot of its parent. Merges go between a branch and its parent, in
either direction, from their last common version:

    exp = create_branch(client, "cats", "relabel-2026")
    ...change the branch's members, snapshot it...
    result = merge(client, "cats", "cats@relabel-2026")   # dry_run=True to preview
"""

import json
import uuid

from .client import APIError


class MergeConflict(Exception):
    """Both sides changed the same objects since their common version; with
    strategy FAIL nothing was changed. result holds the counts and the
    conflicts."""

    def __init__(self, result):
        super().__init__(f"{result['conflicts']} objects were changed on both sides; nothing was merged")
        self.result = result


def branches(client, dataset_id):
    return client.get(f"/api/v1/datasets/{dataset_id}/branches").get("branches") or []


def find_branch(client, parent, name):
    """The parent's branch of that name, or None."""
    for b in branches(client, parent["id"]):
        if b["branch"]["name"] == name:
            return b
    return None


def create_branch(client, parent_id, name, from_version_id=None):
    """Branches the dataset from from_version_id, or its latest version."""
    body = {"name": name}
    if from_version_id:
        body["from_version_id"] = from_version_id
    return client.post(f"/api/v1/datasets/{parent_id}/branches", body)


def branch_of(client, parent, name):
    """The parent's branch of that name, created from its latest version
    when it does not exist yet."""
    return find_branch(client, parent, name) or create_branch(client, parent["id"], name)


def merge(client, target_id, source_id, source_version_id=None, strategy="FAIL", dry_run=False, idempotency_key=None):
    """Merges a version of source (its latest by default) into target's
    draft. Raises MergeConflict when strategy FAIL meets conflicts.
    Snapshot the target afterwards to keep the result as a version."""
    body = {"source_dataset_id": source_id, "strategy": strategy.upper(), "dry_run": dry_run,
            "idempotency_key": idempotency_key or ("" if dry_run else str(uuid.uuid4()))}
    if source_version_id:
        body["source_version_id"] = source_version_id
    try:
        return client.post(f"/api/v1/datasets/{target_id}/merges", body)
    except APIError as err:
        if err.status == 409:
            try:
                answer = json.loads(err.body)
            except ValueError:
                raise err from None
            if answer.get("code") == "merge_conflict":
                raise MergeConflict(answer) from None
        raise


def merges(client, dataset_id):
    return client.get(f"/api/v1/datasets/{dataset_id}/merges").get("merges") or []
