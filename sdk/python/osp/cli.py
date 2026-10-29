"""The osp command line.

    osp login --url http://localhost:8080 --email you@example.com
    osp ingest ./images --dataset pets
    osp snapshot pets
    osp verify pets latest ./images
    osp download pets 3 ./copy
"""

import argparse
import errno
import getpass
import json
import os
import sys

from . import branches, ingest, snapshots
from .client import APIError, Client


def config_path():
    base = os.environ.get("OSP_CONFIG_DIR") or os.path.join(os.path.expanduser("~"), ".osp")
    return os.path.join(base, "credentials.json")


def load_config():
    try:
        with open(config_path(), encoding="utf-8") as f:
            return json.load(f)
    except FileNotFoundError:
        return {}


def save_config(config):
    path = config_path()
    os.makedirs(os.path.dirname(path), exist_ok=True)
    # Only the user may read the saved session.
    fd = os.open(path + ".tmp", os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(fd, "w", encoding="utf-8") as f:
        json.dump(config, f)
    os.replace(path + ".tmp", path)


def client_from_config():
    config = load_config()
    if not config.get("url") or not config.get("refresh_token"):
        sys.exit("not signed in: run osp login first")

    def keep(tokens):
        config["access_token"] = tokens["access_token"]
        config["refresh_token"] = tokens["refresh_token"]
        save_config(config)

    return Client(config["url"], access_token=config.get("access_token", ""),
                  refresh_token=config["refresh_token"], organization_id=config.get("organization_id", ""),
                  on_tokens=keep)


def say(message):
    print(message, file=sys.stderr)


def cmd_login(args):
    password = os.environ.get("OSP_PASSWORD") or getpass.getpass("Password: ")
    config = {"url": args.url, "organization_id": args.org or ""}
    client = Client(args.url, organization_id=config["organization_id"])
    user = client.login(args.email, password)
    config.update(access_token=client.access_token, refresh_token=client.refresh_token)
    save_config(config)
    print(f"signed in as {user['email']}")


def cmd_logout(args):
    client = client_from_config()
    try:
        client.logout()
    finally:
        os.remove(config_path())
    print("signed out")


def cmd_whoami(args):
    print(json.dumps(client_from_config().get("/api/v1/auth/me"), indent=2))


def cmd_datasets(args):
    for ds in client_from_config().pages("/api/v1/datasets", "datasets"):
        print(f"{ds['id']}  {ds['name']}  members={ds.get('member_count', 0)}  {ds.get('lifecycle_status', '')}")


def cmd_ingest(args):
    r = ingest.ingest_folder(client_from_config(), args.folder, args.dataset,
                             concurrency=args.concurrency, wait=not args.no_wait, progress=say, branch=args.branch)
    where = f"{args.dataset}@{args.branch}" if args.branch else args.dataset
    print(f"{r['new']} new, {r['changed']} changed (new versions), {r['unchanged']} unchanged; "
          f"{r['members']} files pinned as members of {where}")
    job = r["job"]
    if job:
        p = job.get("progress", {})
        print(f"bulk job {job['id']} {job['state']}: {p.get('published', 0)} published, {p.get('failed', 0)} failed")
    for problem in r["failed"]:
        print(f"failed: {problem}")
    bad_job = job is not None and job["state"] in ("FAILED", "CANCELLED")
    return 1 if r["failed"] or bad_job else 0


def cmd_snapshot(args):
    client = client_from_config()
    ds = snapshots.find_dataset(client, args.dataset)
    v = snapshots.snapshot(client, ds["id"], args.key)
    print(f"version {v['version']} ({v['id']}): {v['member_count']} files, manifest SHA-256 {v['manifest_sha256']}")


def cmd_versions(args):
    client = client_from_config()
    ds = snapshots.find_dataset(client, args.dataset)
    for v in snapshots.versions(client, ds["id"]):
        print(f"{v['version']:>5}  {v['id']}  files={v['member_count']}  {v['validation_status']}  {v['created_at']}")


def cmd_members(args):
    client = client_from_config()
    ds = snapshots.find_dataset(client, args.dataset)
    v = snapshots.find_version(client, ds["id"], args.version)
    for m in snapshots.members(client, ds["id"], v["id"]):
        print(json.dumps(m))


def cmd_verify(args):
    client = client_from_config()
    ds = snapshots.find_dataset(client, args.dataset)
    v = snapshots.find_version(client, ds["id"], args.version)
    report = snapshots.verify_local(client, ds["id"], v["id"], args.folder)
    exact = not (report["changed"] or report["missing"] or report["extra"] or report["erased_but_present"])
    print(f"version {v['version']}: {len(report['matched'])} match, {len(report['changed'])} changed, "
          f"{len(report['missing'])} missing, {len(report['extra'])} not in the snapshot, "
          f"{len(report['erased'])} erased")
    if report["erased_but_present"]:
        print(f"{len(report['erased_but_present'])} files here were erased from the platform; delete them "
              f"(osp download --remove-erased) to complete the erasure")
    for kind in ("changed", "missing", "extra", "erased_but_present"):
        for path in report[kind][:20]:
            print(f"  {kind}: {path}")
    print("the folder is exactly this snapshot" if exact else "the folder differs from this snapshot")
    return 0 if exact else 1


def cmd_download(args):
    client = client_from_config()
    ds = snapshots.find_dataset(client, args.dataset)
    v = snapshots.find_version(client, ds["id"], args.version)
    c = snapshots.download(client, ds["id"], v["id"], args.folder, workers=args.workers,
                           remove_erased=args.remove_erased, progress=say)
    print(f"version {v['version']}: {c['downloaded']} downloaded, {c['kept']} already present; every file matches its SHA-256")
    if c["erased"] or c["removed"]:
        print(f"{c['erased'] + c['removed']} files were erased from the platform and are not downloaded; "
              f"{c['removed']} local copies of them were deleted")


def cmd_diff(args):
    client = client_from_config()
    ds = snapshots.find_dataset(client, args.dataset)
    base = snapshots.find_version(client, ds["id"], args.against)
    target = snapshots.find_version(client, ds["id"], args.version)
    result = snapshots.diff(client, ds["id"], base["id"], target["id"])
    s = result["summary"]
    print(f"version {base['version']} -> {target['version']}: {s['added']} added, {s['removed']} removed, "
          f"{s['changed']} changed, {s['relabeled']} relabeled, {s['unchanged']} unchanged")
    for d in result["differences"]:
        side = d.get("target") or d.get("base")
        detail = ""
        if d["kind"] == "RELABELED":
            detail = f"  {d['base'].get('label', '')!r} -> {d['target'].get('label', '')!r}"
        print(f"  {d['kind']:<9} {side['path']}{detail}")


def cmd_validate(args):
    client = client_from_config()
    ds = snapshots.find_dataset(client, args.dataset)
    v = snapshots.find_version(client, ds["id"], args.version)
    say(f"validating version {v['version']} with {args.profile} ({args.mode})")
    report, findings = snapshots.validate(client, ds["id"], v["id"], args.profile, args.mode)
    print(f"{report['status']}: {report['summary']}")
    for f in findings[:30]:
        print(f"  {f['severity']:<7} {f['code']:<26} {f['message']}")
    return 0 if report["status"] in ("READY", "READY_WITH_WARNINGS") else 1


def cmd_branch(args):
    client = client_from_config()
    parent = snapshots.find_dataset(client, args.dataset)
    from_version = None
    if args.version:
        from_version = snapshots.find_version(client, parent["id"], args.version)["id"]
    b = branches.create_branch(client, parent["id"], args.name, from_version)
    print(f"branch {b['name']} ({b['id']}): {b['member_count']} members from version {b['branch']['fork_version_id']}")


def cmd_branches(args):
    client = client_from_config()
    parent = snapshots.find_dataset(client, args.dataset)
    for b in branches.branches(client, parent["id"]):
        print(f"{b['branch']['name']:<24} {b['id']}  members={b['member_count']}  merge base {b['branch']['merge_base_version_id']}")


def side(member):
    """One side of a merge conflict, short."""
    if not member["present"]:
        return "absent"
    return f"{member.get('label', '')!r} version {member.get('version_id', '')[:8]}"


def cmd_merge(args):
    client = client_from_config()
    target = snapshots.find_dataset(client, args.target)
    source = snapshots.find_dataset(client, args.source)
    version = None
    if args.version:
        version = snapshots.find_version(client, source["id"], args.version)["id"]
    try:
        r = branches.merge(client, target["id"], source["id"], version, args.strategy, args.dry_run)
    except branches.MergeConflict as conflict:
        r = conflict.result
    verb = "would change" if not r["applied"] else "changed"
    print(f"{source['name']} -> {target['name']}: {verb} {r['added']} added, {r['updated']} updated, "
          f"{r['removed']} removed; {r['already_same']} already the same; {r['conflicts']} conflicts")
    for c in r["conflict_list"][:30]:
        print(f"  conflict {c['path']}: base {side(c['base'])}, {source['name']} {side(c['source'])}, "
              f"{target['name']} {side(c['target'])}")
    if not r["applied"] and r["conflicts"] and not args.dry_run:
        print("nothing was merged; rerun with --strategy source or target to resolve the conflicts")
        return 1
    if r["applied"]:
        print(f"snapshot {target['name']} to keep the result as a version")
    return 0


def cmd_uploads(args):
    for u in client_from_config().pages("/api/v1/uploads", "uploads", status=args.status):
        print(f"{u['upload_id']}  {u['status']:<10}  {u['size_bytes']:>12}  {u['file_name']}")


def cmd_status(args):
    print(json.dumps(client_from_config().get("/api/v1/operations/status"), indent=2))


def main(argv=None):
    parser = argparse.ArgumentParser(prog="osp", description="Verifiable dataset storage from the command line.")
    sub = parser.add_subparsers(dest="command", required=True)

    p = sub.add_parser("login", help="sign in and save the session")
    p.add_argument("--url", required=True, help="API address, for example http://localhost:8080")
    p.add_argument("--email", required=True)
    p.add_argument("--org", help="organization ID, when you belong to several")
    p.set_defaults(run=cmd_login)

    sub.add_parser("logout", help="end the session").set_defaults(run=cmd_logout)
    sub.add_parser("whoami", help="show the signed-in user").set_defaults(run=cmd_whoami)
    sub.add_parser("datasets", help="list datasets").set_defaults(run=cmd_datasets)

    p = sub.add_parser("ingest", help="upload a folder into a dataset")
    p.add_argument("folder")
    p.add_argument("--dataset", required=True, help="dataset name; created when it does not exist")
    p.add_argument("--concurrency", type=int, default=8)
    p.add_argument("--no-wait", action="store_true", help="return after the uploads, before processing ends")
    p.add_argument("--branch", help="pin the files in this branch of the dataset (created when missing) instead of the dataset")
    p.set_defaults(run=cmd_ingest)

    p = sub.add_parser("branch", help="branch a dataset from one of its versions")
    p.add_argument("dataset")
    p.add_argument("name")
    p.add_argument("--version", help="the version to branch from; the latest by default")
    p.set_defaults(run=cmd_branch)

    p = sub.add_parser("branches", help="list a dataset's branches")
    p.add_argument("dataset")
    p.set_defaults(run=cmd_branches)

    p = sub.add_parser("merge", help="merge a branch into its dataset, or the dataset into a branch (three-way)")
    p.add_argument("target", help="the dataset or branch (name@branch) that receives the changes")
    p.add_argument("source", help="the branch or dataset whose version is merged")
    p.add_argument("--version", help="the source version to merge; its latest by default")
    p.add_argument("--strategy", default="FAIL", choices=["FAIL", "SOURCE", "TARGET", "fail", "source", "target"],
                   help="what to do with objects both sides changed: stop (default), take the source's, keep the target's")
    p.add_argument("--dry-run", action="store_true", help="show what would change, change nothing")
    p.set_defaults(run=cmd_merge)

    p = sub.add_parser("snapshot", help="freeze a dataset's members as a new version")
    p.add_argument("dataset", help="dataset name or ID")
    p.add_argument("--key", help="idempotency key: the same key never makes two versions")
    p.set_defaults(run=cmd_snapshot)

    p = sub.add_parser("versions", help="list a dataset's versions")
    p.add_argument("dataset")
    p.set_defaults(run=cmd_versions)

    for name, run, help_text in (("members", cmd_members, "list a version's pinned files as JSON lines"),
                                 ("verify", cmd_verify, "check a local folder against a version, by SHA-256"),
                                 ("download", cmd_download, "download a version, verifying every file")):
        p = sub.add_parser(name, help=help_text)
        p.add_argument("dataset")
        p.add_argument("version", help='"latest", a version number or a version ID')
        if name != "members":
            p.add_argument("folder")
        if name == "download":
            p.add_argument("--workers", type=int, default=8)
            p.add_argument("--remove-erased", action="store_true",
                           help="delete local copies of files the platform has erased")
        p.set_defaults(run=run)

    p = sub.add_parser("diff", help="compare two versions of a dataset by SHA-256")
    p.add_argument("dataset")
    p.add_argument("version", help="the newer version")
    p.add_argument("--against", required=True, help="the version to compare with")
    p.set_defaults(run=cmd_diff)

    p = sub.add_parser("validate", help="validate a version and print its report")
    p.add_argument("dataset")
    p.add_argument("version")
    p.add_argument("--profile", default="builtin:general")
    p.add_argument("--mode", default="METADATA", choices=["METADATA", "FULL", "SAMPLED"])
    p.set_defaults(run=cmd_validate)

    p = sub.add_parser("audit", help="re-read every file of a version from storage and check its SHA-256")
    p.add_argument("dataset")
    p.add_argument("version")
    p.set_defaults(run=cmd_validate, profile="builtin:storage-audit", mode="FULL")

    p = sub.add_parser("uploads", help="list the organization's uploads")
    p.add_argument("--status")
    p.set_defaults(run=cmd_uploads)
    sub.add_parser("status", help="background work: processing, ingestion, imports").set_defaults(run=cmd_status)

    args = parser.parse_args(argv)
    try:
        return args.run(args) or 0
    except APIError as err:
        print(f"error: {err}", file=sys.stderr)
        return 2
    except (LookupError, ValueError, snapshots.IntegrityError) as err:
        print(f"error: {err}", file=sys.stderr)
        return 2
    except OSError as err:
        # Output piped into a reader that stopped early (osp uploads | head);
        # Windows reports that as EINVAL rather than EPIPE.
        if err.errno not in (errno.EPIPE, errno.EINVAL):
            raise
        sys.stdout = open(os.devnull, "w")
        return 0


if __name__ == "__main__":
    sys.exit(main())
