"""Python SDK for the verifiable dataset storage platform."""

from .branches import MergeConflict, branches, create_branch, merge
from .client import APIError, Client
from .ingest import ingest_folder, wait_for_job
from .runs import finish_run, record_run, track_run
from .snapshots import (IntegrityError, download, fetch_member, find_dataset, find_version, members,
                        snapshot, verify_local, versions)

__all__ = [
    "APIError", "Client", "MergeConflict", "branches", "create_branch", "merge", "IntegrityError", "download", "fetch_member", "find_dataset", "find_version",
    "ingest_folder", "members", "snapshot", "verify_local", "versions", "wait_for_job",
    "finish_run", "record_run", "track_run",
]
__version__ = "0.1.0"
