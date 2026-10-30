"""The osp command line.

    osp login --url http://localhost:8080 --email you@example.com
    osp ingest ./images --dataset pets
    osp snapshot pets
    osp verify pets latest ./images
    osp verify pets latest --sample 0.1
    osp bench pets latest
    osp download pets 3 ./copy
"""

import argparse
import csv
import errno
import getpass
import json
import os
import socket
import sys
import time

# Names, not the module: osp.branches is also the name of a function.
from . import access, adoption, checks, erasure, evidence, governance, ingest, posture, protection, quality, shards, snapshots, tiering
from .branches import MergeConflict, branches as list_branches, create_branch, merge
from .runs import runs as list_runs
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
    # An API token in the environment (CI, a vendor's script) replaces the
    # saved session.
    token = os.environ.get("OSP_API_TOKEN", "")
    if token:
        url = os.environ.get("OSP_URL") or config.get("url")
        if not url:
            sys.exit("OSP_API_TOKEN is set: set OSP_URL too")
        return Client(url, access_token=token)
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


def money(usd):
    return f"${usd:,.2f}"


def say_estimate(label, est):
    """Prints a cost estimate in one line, best effort."""
    parts = [f"{k} {money(est[k + '_usd'])}" for k in ("storage", "request", "egress", "retrieval") if est.get(k + "_usd")]
    say(f"{label}: about {money(est['total_usd'])}" + (f" ({', '.join(parts)})" if parts else "") +
        f", {est['requests']:,} requests; {est['note']}")


def cmd_ingest(args):
    client = client_from_config()
    files = ingest.scan_folder(args.folder)
    try:
        say_estimate("uploading this folder", tiering.estimate(client, "ingest", files=len(files),
                                                               bytes=sum(f.size for f in files)))
    except APIError:
        pass  # estimates need storage.read; the upload does not
    r = ingest.ingest_folder(client, args.folder, args.dataset,
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
    if not args.folder:
        return verify_remote(client, ds, v, args)
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


def verify_remote(client, ds, v, args):
    """Checks the snapshot itself: its signature, its manifest, and every
    file's bytes (or a sample) read through shard streams."""
    say(f"reading version {v['version']} through shard streams" + (f", checking {args.sample:.0%} of files" if args.sample < 1 else ""))
    r = shards.verify_snapshot(client, ds["id"], v["id"], rate=args.sample, seed=args.seed)
    sig, verifier = r["signature"], r["verifier"]
    print(f"signature: {'ok' if sig['ok'] else 'NOT OK' if sig['checked'] else 'not checked'} ({sig['note']})")
    print(f"manifest: {'the members listed hash to the signed manifest' if r['manifest_ok'] else 'the members listed do NOT match the signed manifest'}")
    print(f"files: {verifier.checked} checked, {verifier.failed} failed, {r['erased']} erased (not read)")
    for f in verifier.failures[:20]:
        print(f"  {f['path']}: {f['reason']}")
    if r["cut"]:
        print(f"a stream ended early: {r['cut']}")
    if args.run:
        verifier.report(client, args.run, v["id"])
        print(f"recorded on training run {args.run}")
    ok = r["manifest_ok"] and not verifier.failed and not r["cut"] and (sig["ok"] or not sig["checked"])
    print("the snapshot reads back exactly as it was frozen" if ok else "the snapshot does not read back as frozen")
    return 0 if ok else 1


def cmd_bench(args):
    client = client_from_config()
    ds = snapshots.find_dataset(client, args.dataset)
    v = snapshots.find_version(client, ds["id"], args.version)
    say(f"finding the fastest number of parallel shard streams, {args.seconds}s a step")
    best, results = shards.autotune(client, ds["id"], v["id"], max_streams=args.max_streams, seconds=args.seconds)
    for r in results:
        print(f"{r['streams']:>3} streams: {r['files_per_second']:>9.1f} files/s  {r['bytes_per_second'] / 1e6:>8.1f} MB/s  "
              f"first byte {r['first_byte_seconds'] * 1000:.0f} ms")
    if results:
        print(f"use {best} parallel streams (DataLoader num_workers={best}, or prefetch={best})")
    if args.compare:
        members = [m for m in snapshots.members(client, ds["id"], v["id"]) if m.get("availability", "AVAILABLE") == "AVAILABLE"]
        sample = members[:args.compare]
        started = time.monotonic()
        for m in sample:
            snapshots.fetch_member(client, m)
        elapsed = time.monotonic() - started
        if sample and elapsed:
            print(f"file by file: {len(sample) / elapsed:.1f} files/s over {len(sample)} files")
    return 0


def cmd_shards(args):
    client = client_from_config()
    ds = snapshots.find_dataset(client, args.dataset)
    v = snapshots.find_version(client, ds["id"], args.version)
    for i, s in enumerate(shards.plan(client, ds["id"], v["id"], args.target_bytes)):
        print(f"{i:>5}  files={s['files']:<6} bytes={s['bytes']:<12} packs={s['packs']:<4} {s['first_path']} .. {s['last_path']}")


def cmd_repack(args):
    client = client_from_config()
    ds = snapshots.find_dataset(client, args.dataset)
    path = f"/api/v1/datasets/{ds['id']}/repacks"
    if args.cancel:
        client.post(f"{path}/{args.cancel}/cancel")
        print(f"repack {args.cancel} cancelled")
        return 0
    if not args.list:
        r = client.post(path)
        print(f"repack {r['id']} queued: the packing worker gathers {ds['name']}'s packed files in path order")
    for r in client.get(path)["repacks"]:
        print(f"{r['id']}  {r['state']:<9} packs={r['packs_planned']} files={r['entries_planned']}  {r['created_at']}")
    return 0


def cmd_packing(args):
    client = client_from_config()
    if args.target_bytes is not None:
        client.request("PUT", "/api/v1/packing-settings", body={"target_pack_bytes": args.target_bytes})
    n = client.get("/api/v1/packing-settings")["target_pack_bytes"]
    print(f"target pack size: {n} bytes" if n else "target pack size: the platform's default")


def cmd_checkpoint(args):
    client = client_from_config()
    base = f"/api/v1/training-runs/{args.run}/checkpoints"
    if args.action == "list":
        answer = client.get(base)
        for c in answer["checkpoints"]:
            state = f"verified by {c['verified_by']}" if c.get("verified_at") else (c.get("verify_error") or "not verified")
            print(f"step {c['step']:>8}  {c['id']}  written by {c['written_by']}  {state}")
        last = answer.get("last_verified")
        print(f"roll back to step {last['step']} ({last['version_id']})" if last else "no verified checkpoint yet")
        return 0
    ds = snapshots.find_dataset(client, args.dataset)
    v = snapshots.find_version(client, ds["id"], args.version)
    host = args.host or socket.gethostname()
    if args.action == "add":
        c = client.post(base, {"step": args.step, "version_id": v["id"], "host": host})
        print(f"checkpoint {c['id']} at step {c['step']}: verify it from another machine with osp checkpoint verify")
        return 0
    # verify: re-read every file here, on a host other than the writer.
    match = [c for c in client.get(base)["checkpoints"] if c["version_id"] == v["id"]]
    if not match:
        raise LookupError(f"run {args.run} has no checkpoint of version {v['id']}")
    r = shards.verify_snapshot(client, ds["id"], v["id"])
    failures = list(r["verifier"].failures)
    if not r["manifest_ok"]:
        failures.insert(0, {"path": "(manifest)", "reason": "the members listed do not match the signed manifest"})
    c = client.post(f"{base}/{match[0]['id']}/verifications", {"host": host, "failures": failures})
    print(f"step {c['step']}: " + ("verified on " + host if c.get("verified_at") else c.get("verify_error", "failed")))
    return 0 if c.get("verified_at") else 1


def cmd_tier_rules(args):
    client = client_from_config()
    if args.delete:
        tiering.delete_rule(client, args.delete)
    if args.add:
        dataset_id = snapshots.find_dataset(client, args.dataset)["id"] if args.dataset else ""
        tiering.add_rule(client, args.add, args.tier, args.min_age, args.unread, dataset_id)
    for r in tiering.rules(client):
        scope = f"dataset {r['dataset_id']}" if r.get("dataset_id") else "every object"
        print(f"{r['id']}  {r['name']}: {scope} to {r['tier']} after {r['min_age_days']} days, unread {r['unread_days']} days"
              + ("" if r["enabled"] else "  (off)"))


def cmd_restore_archived(args):
    client = client_from_config()
    ds = snapshots.find_dataset(client, args.dataset)
    v = snapshots.find_version(client, ds["id"], args.version)
    r = tiering.restore_version(client, ds["id"], v["id"], days=args.days, speed=args.speed)
    print(f"{r['archived']} archived: {r['requested']} restores asked for, {r['already_open']} already under way, "
          f"{r['readable']} readable now")
    if r["requested"]:
        say_estimate("restoring them", r["estimate"])
    if args.wait and r["restores"]:
        done = tiering.wait_restores(client, version_id=v["id"], every=60)
        failed = [x for x in done if x["state"] == "FAILED"]
        print(f"{len(done) - len(failed)} readable, {len(failed)} failed")
        return 1 if failed else 0
    return 0


def cmd_costs(args):
    client = client_from_config()
    if args.dataset:
        ds = snapshots.find_dataset(client, args.dataset)
        c = tiering.dataset_costs(client, ds["id"], years=args.years)
        for line in c["stored"]:
            print(f"{line['provider']:<22} {line['tier']:<13} {line['objects']:>9,} objects {line['bytes']:>16,} bytes "
                  f"{money(line['monthly_usd'])}/month")
        print(f"{money(c['monthly_usd'])} a month; {money(c['retain_usd'])} to keep it {c['years']:g} years as it is, "
              f"{money(c['archived_usd'])} archived")
        print(c["note"])
        return 0
    r = tiering.report(client)
    print(f"{r['month']}: about {money(r['total_usd'])} (storage {money(r['storage_usd'])}, requests {money(r['request_usd'])}, "
          f"egress {money(r['egress_usd'])}, restores {money(r['restore_usd'])})")
    print(f"{r['reads']:,} reads, {r['egress_bytes']:,} bytes out, {r['writes']:,} objects written")
    s = r["savings"]
    print(f"saved: {s['reads_saved_per_pass']:,} requests per full read by packing ({s['packed_files']:,} files in "
          f"{s['packs']:,} packs); {s['shared_bytes']:,} bytes not stored twice ({money(s['dedup_usd_month'])}/month); "
          f"{money(s['tiering_usd_month'])}/month by tiering")
    if r.get("budget"):
        b = r["budget"]
        print(f"budget: {money(b['monthly_usd'])} and {b['monthly_requests']:,} reads a month, {b['action']}"
              + (f"; {r['budget_used']:.0%} used" if b["monthly_usd"] else ""))
    print(r["note"])
    return 0


def cmd_budget(args):
    client = client_from_config()
    if args.usd is not None or args.requests is not None or args.action:
        b = tiering.budget(client)
        tiering.set_budget(client, args.usd if args.usd is not None else b["monthly_usd"],
                           args.requests if args.requests is not None else b["monthly_requests"], args.action or b["action"])
    b = tiering.budget(client)
    print(f"monthly budget: {money(b['monthly_usd']) if b['monthly_usd'] else 'no cost limit'}, "
          f"{b['monthly_requests'] or 'no'} read limit, {b['action']}")


def cmd_estimate(args):
    client = client_from_config()
    ds = snapshots.find_dataset(client, args.dataset)
    v = snapshots.find_version(client, ds["id"], args.version)
    e = tiering.version_estimate(client, ds["id"], v["id"], speed=args.speed)
    print(f"version {v['version']}: {e['files']:,} files, {e['bytes']:,} bytes, {e['packed_files']:,} packed in {e['packs']:,} packs")
    say_estimate("download", e["download"])
    say_estimate("one training epoch", e["epoch"])
    if e["archived_files"]:
        say_estimate(f"restore {e['archived_files']} archived files first ({e['restore']['hours']:g} hours)", e["restore"])


def cmd_expire(args):
    client = client_from_config()
    ds = snapshots.find_dataset(client, args.dataset)
    r = tiering.expire(client, ds["id"], keep_last=args.keep_last, older_than_days=args.older_than, dry_run=not args.yes)
    for v in r["expire"]:
        print(f"expire version {v['version']} ({v['members']:,} files)")
    for v in r["keep"]:
        print(f"keep   version {v['version']}: {v['kept']}")
    print(f"frees {r['freed_objects']:,} objects, {r['freed_bytes']:,} bytes, about {money(r['freed_usd_month'])}/month")
    if r["dry_run"]:
        print("nothing expired yet: run again with --yes")
    else:
        print(f"{r['deletions_requested']} deletions asked for; {len(r.get('refused') or {})} refused (a hold, retention or escrow)")


def cmd_storage_manifest(args):
    client = client_from_config()
    ds = snapshots.find_dataset(client, args.dataset)
    v = snapshots.find_version(client, ds["id"], args.version)
    out = open(args.out, "w", encoding="utf-8") if args.out else sys.stdout
    try:
        n = tiering.write_manifest(client, ds["id"], v["id"], out)
    finally:
        if args.out:
            out.close()
    say(f"{n} members written")


def cmd_audit_export(args):
    client = client_from_config()
    out = open(args.out, "w", encoding="utf-8") if args.out else sys.stdout
    try:
        if args.format == "cef":
            path = f"/api/v1/organization/audit/export?after_seq={args.after}&format=cef"
            with client.stream("GET", path) as resp:
                out.write(resp.read().decode("utf-8"))
            return 0
        n = evidence.write_export(client, out, after_seq=args.after)
    finally:
        if args.out:
            out.close()
    say(f"{n} audit entries written")
    return 0


def cmd_audit_verify(args):
    """Checks an export without the platform; with --anchors, also against
    the organization's anchors (signed, kept outside its database)."""
    anchors, key = [], None
    if args.anchors:
        client = client_from_config()
        anchors = evidence.anchors(client, limit=500)
        try:
            key = evidence.public_key(client)["public_key"]
        except APIError:
            key = None
    with open(args.file, encoding="utf-8") as f:
        r = evidence.verify_export(f, anchors, key)
    if r["broken_at"]:
        print(f"BROKEN at entry {r['broken_at']}: {r['problem']} ({r['checked']} entries checked before it)")
    else:
        print(f"intact: entries {r['first']} to {r['last']} ({r['checked']}) each match their hash and chain to the one before")
    for a in r["anchors"]:
        print(f"anchor at {a['seq']}: {'ok' if a['ok'] else 'NOT OK'}" + (f" ({a['why']})" if a["why"] else ""))
    bad = r["broken_at"] or any(not a["ok"] and a["why"] != "the export does not reach this entry" for a in r["anchors"])
    return 1 if bad else 0


def cmd_anchors(args):
    client = client_from_config()
    if args.now:
        a = evidence.anchor_now(client)
        print(f"anchored entry {a['seq']}" + (f" at {a['object_key']}" if a.get("object_key") else ""))
    for a in evidence.anchors(client):
        stored = "" if a.get("stored_ok") is None else (", bucket copy ok" if a["stored_ok"] else ", bucket copy DIFFERS")
        print(f"{a['created_at']}  entry {a['seq']}  signature {'ok' if a['signature_ok'] else 'not ok'}, "
              f"chain {'ok' if a['chain_ok'] else 'NOT ok'}{stored}" + (f"  {a['problem']}" if a.get("problem") else ""))


def cmd_reviews(args):
    client = client_from_config()
    if args.create:
        import datetime
        start = (datetime.datetime.now(datetime.timezone.utc) - datetime.timedelta(days=args.create)).isoformat(timespec="seconds")
        r = evidence.create_review(client, start)
        print(f"review {r['id']}: {r['counts']}")
    if args.sign:
        r = evidence.sign_review(client, args.sign, args.note or "")
        print(f"review {r['id']} signed by {r['signed_by']} at {r['signed_at']}; report SHA-256 {r['report_sha256']}")
        return 0
    if args.export:
        n = evidence.download(client, f"/api/v1/audit-reviews/{args.export}/export", args.out or f"review-{args.export}.zip")
        say(f"{n} bytes written")
        return 0
    for r in evidence.reviews(client):
        print(f"{r['id']}  {r['period_start'][:10]} to {r['period_end'][:10]}  {r['state']:<6} "
              + ", ".join(f"{k.lower()} {v}" for k, v in sorted(r["counts"].items()) if v))


def cmd_provenance(args):
    client = client_from_config()
    ds = snapshots.find_dataset(client, args.dataset)
    if args.basis:
        evidence.set_provenance(client, ds["id"], args.basis, args.license or "", args.source or "", args.kind or "OTHER")
    p = evidence.provenance(client, ds["id"])
    print(f"lawful basis: {p['lawful_basis'] or 'not recorded'}\nlicence: {p['license'] or 'not recorded'}\n"
          f"source: {p['source'] or 'not recorded'} ({p['source_kind'] or 'kind not recorded'})\n"
          f"recorded: {p.get('recorded_at') or 'never'}")


def cmd_evidence_pack(args):
    client = client_from_config()
    ds = snapshots.find_dataset(client, args.dataset)
    v = snapshots.find_version(client, ds["id"], args.version)
    n = evidence.download(client, evidence.pack_path(ds["id"], v["id"], args.digests), args.out)
    print(f"evidence pack for version {v['version']} written to {args.out} ({n:,} bytes); check it with python verify_pack.py")


def cmd_snapshot_hold(args):
    client = client_from_config()
    ds = snapshots.find_dataset(client, args.dataset)
    v = snapshots.find_version(client, ds["id"], args.version)
    if args.release:
        r = evidence.release_snapshot_hold(client, ds["id"], v["id"], args.release, args.reason or "")
        print(f"{r['released']} holds released")
        return 0
    if args.kind:
        h = evidence.hold_snapshot(client, ds["id"], v["id"], args.kind, args.ref, args.reason or "")
        print(f"{len(h['hold_ids'])} members held: {h['matter']}")
    for h in evidence.snapshot_holds(client, ds["id"], v["id"]):
        print(f"{h['matter']}  {len(h['hold_ids'])} members  {'released' if h['released'] else 'active'}")


def cmd_ai_summary(args):
    client = client_from_config()
    if args.publish:
        evidence.publish_summary(client, args.model)
        print("recorded as published; the next update is due in six months")
        return 0
    text = evidence.model_summary(client, args.model, markdown=True)
    if args.out:
        with open(args.out, "w", encoding="utf-8") as f:
            f.write(text)
        say(f"written to {args.out}")
    else:
        print(text)


def cmd_compliance(args):
    client = client_from_config()
    if not args.framework:
        for f in evidence.frameworks(client):
            print(f"{f['id']:<14} {f['name']}")
        return 0
    if args.evidence:
        n = evidence.download(client, f"/api/v1/compliance/{args.framework}/evidence", args.evidence)
        say(f"evidence written to {args.evidence} ({n:,} bytes)")
        return 0
    fw = evidence.controls(client, args.framework)
    print(fw["name"])
    for c in fw["controls"]:
        print(f"  {c['id']:<14} {c['status']:<12} {c['title']}")
        for e in c.get("evidence") or []:
            print(f"  {'':<14} {'':<12}   {e}")
    print(fw["note"])


def cmd_regulator_access(args):
    client = client_from_config()
    t = evidence.regulator_access(client, args.name, args.days)
    print(f"read-only token for {t['name']}, until {t['expires_at']}; shown once:\n{t['token']}")


def cmd_as_of(args):
    client = client_from_config()
    for o in evidence.as_of(client, args.namespace, args.at):
        print(f"{o['path']}  version {o['version']}  {o['sha256']}  current since {o['current_since']}")


def cmd_profile(args):
    client = client_from_config()
    ds = snapshots.find_dataset(client, args.dataset)
    v = snapshots.find_version(client, ds["id"], args.version)
    if args.again:
        quality.reprofile(client, ds["id"], v["id"])
    p = quality.wait_profile(client, ds["id"], v["id"]) if args.wait or args.again else quality.profile(client, ds["id"], v["id"])
    print(f"version {v['version']}: {p['state'].lower()}, {p.get('files_profiled', 0)} files profiled, {p.get('other_files', 0)} other files")
    for t in p["tables"]:
        print(f"{t['table']} ({t['format']}, {t['files']} files, {t['rows']:,} rows{', stopped at the row limit' if t.get('partial') else ''})")
        for c in t["columns"]:
            parts = [c["kind"], f"{c['nulls']:,} nulls", f"{c['distinct']:,}{'+' if c.get('distinct_capped') else ''} distinct"]
            if c.get("mean") is not None:
                parts.append(f"{c['min']:g} to {c['max']:g}, mean {c['mean']:g}")
            if c.get("top"):
                parts.append("top " + ", ".join(f"{x['value']} ({x['count']})" for x in c["top"][:3]))
            print(f"  {c['name']}: " + "; ".join(parts))
    for s in p["skipped"]:
        print(f"not profiled: {s['path']}: {s['reason']}")
    if p.get("drift"):
        print(f"drift against version {p['baseline_version_id']}: {p['drift']['breaches']} breaches")
    return 0


def cmd_drift(args):
    client = client_from_config()
    ds = snapshots.find_dataset(client, args.dataset)
    v = snapshots.find_version(client, ds["id"], args.version)
    against = snapshots.find_version(client, ds["id"], args.against)["id"] if args.against else None
    r = quality.drift(client, ds["id"], v["id"], against)
    for line in quality.drift_lines(r):
        print(line)
    lim = r["thresholds"]
    print(f"{r['breaches']} breaches (PSI over {lim['max_psi']}, nulls moving over {lim['max_null_change'] * 100:.0f} points, "
          f"rows moving over {lim['max_row_change'] * 100:.0f}%)")
    return 1 if r["breaches"] else 0


def cmd_leakage(args):
    client = client_from_config()
    ev_ds = snapshots.find_dataset(client, args.eval_dataset)
    ev = snapshots.find_version(client, ev_ds["id"], args.eval_version)
    tr_ds = snapshots.find_dataset(client, args.train_dataset)
    tr = snapshots.find_version(client, tr_ds["id"], args.train_version)
    leaked = False
    if args.ngram:
        progress = (lambda side, path: say(f"{side}: {path}")) if args.verbose else None
        c = quality.ngram_leakage(client, tr_ds["id"], tr["id"], ev_ds["id"], ev["id"], n=args.ngram, threshold=args.threshold,
                                  run_id=args.run, progress=progress)
        print(f"NGRAM: {c['state']}: {c['detail']}")
        for x in c["examples"]:
            print(f"  {x['eval_path']} ({x['key']})")
        leaked = c["state"] == "LEAKED"
    r = quality.leakage(client, ev_ds["id"], ev["id"], tr["id"])
    for c in r["checks"]:
        print(f"{c['method']}: {c['state']}: {c['detail']}")
        for x in c["examples"]:
            print(f"  {x['eval_path']} = {x.get('train_path') or '?'} ({x['key'][:16]})")
        leaked = leaked or c["state"] == "LEAKED"
    for n in r["not_checked"]:
        print(f"not checked: {n}")
    return 1 if leaked else 0


def cmd_release(args):
    client = client_from_config()
    ds = snapshots.find_dataset(client, args.dataset)
    v = snapshots.find_version(client, ds["id"], args.version)
    r = quality.release(client, ds["id"], v["id"], args.label, args.notes or "")
    print(f"version {r['version']} released as {r['label']}")
    return 0


def cmd_releases(args):
    client = client_from_config()
    ds = snapshots.find_dataset(client, args.dataset)
    for r in quality.releases(client, ds["id"]):
        print(f"{r['label']:<12} version {r['version']:<4} {r['members']:>9,} files  {r['released_at'][:10]}  {r['notes']}")
    return 0


def cmd_release_diff(args):
    client = client_from_config()
    ds = snapshots.find_dataset(client, args.dataset)
    d = quality.release_diff(client, ds["id"], args.label, args.against)
    m = d["members"]
    print(f"{d['release']['label']} against {d['previous']['label']}: {m['added']:,} added, {m['removed']:,} removed, "
          f"{m['changed']:,} changed, {m['relabeled']:,} relabelled, {m['unchanged']:,} unchanged")
    for e in m["first"]:
        print(f"  {e['kind'].lower():<9} {e['path']}")
    if d.get("drift"):
        for line in quality.drift_lines(d["drift"]):
            print(line)
    elif d.get("drift_note"):
        print(f"drift: {d['drift_note']}")
    return 0


def cmd_quality_settings(args):
    client = client_from_config()
    ds = snapshots.find_dataset(client, args.dataset)
    asked = {"max_psi": args.max_psi, "max_null_change": args.max_null_change, "max_row_change": args.max_row_change,
             "subject_key": args.subject_key, "subject_pattern": args.subject_pattern}
    changes = {k: v for k, v in asked.items() if v is not None}
    if args.profiles is not None:
        changes["profile_snapshots"] = args.profiles == "on"
    s = quality.set_settings(client, ds["id"], **changes) if changes else quality.settings(client, ds["id"])
    subject = s["subject_key"] or s["subject_pattern"] or "not set"
    print(f"profiles: {'on' if s['profile_snapshots'] else 'off'}; drift past PSI {s['max_psi']}, nulls {s['max_null_change']}, "
          f"rows {s['max_row_change']}; subject: {subject}")
    return 0


def _erasure_selector(args):
    hashes = list(args.sha256 or [])
    if args.manifest:
        with open(args.manifest, encoding="utf-8") as f:
            hashes += erasure.sha256s_from_manifest(f)
    return {"subject_ids": args.subject or [], "sha256s": hashes, "object_ids": args.object or [], "subject_key": args.subject_key}


def _print_plan(plan):
    print(f"{len(plan['objects'])} objects ({plan['bytes']:,} bytes) on {len(plan['connections'])} connections; "
          f"{len(plan['versions'])} snapshots pin them, {len(plan['drafts'])} drafts hold them, {len(plan['models'])} models trained on them")
    for o in plan["objects"][:50]:
        held = f"  HELD: {'; '.join(o['held_by'])}" if o.get("held_by") else ""
        print(f"  {o['sha256'][:16]}  {o['size_bytes']:>12,}  {o['connection_id']}  {o['path']}{held}")
    if len(plan["objects"]) > 50:
        print(f"  ... and {len(plan['objects']) - 50} more")
    for v in plan["versions"]:
        print(f"  snapshot {v['dataset_name']} v{v['version']}: {v['members']} members to remove"
              + (f" (superseded by {v['superseded_by']})" if v.get("superseded_by") else ""))
    for m in plan["models"]:
        print(f"  model {m['name']} {m['version']} (run {m['run_name']}) trained on it")
    for n in plan["not_searched"]:
        print(f"not searched: {n}")


def cmd_erasure_search(args):
    client = client_from_config()
    _print_plan(erasure.search(client, **_erasure_selector(args)))
    return 0


def cmd_erasure_request(args):
    client = client_from_config()
    e = erasure.create(client, args.kind, args.reason, reference=args.reference or "", institution=args.institution or "",
                       **_erasure_selector(args))
    print(f"erasure {e['id']}: {e['state'].lower()}")
    _print_plan(e["plan"])
    return 0


def cmd_erasures(args):
    client = client_from_config()
    for e in erasure.list_erasures(client, args.state):
        print(f"{e['id']}  {e['state']:<16} {e['kind']:<18} {e['created_at'][:10]}  {e['institution'] or '-'}  {e['reason'][:60]}")
    return 0


def cmd_erasure(args):
    client = client_from_config()
    if args.approve:
        erasure.approve(client, args.id, args.note or "")
    if args.cancel:
        erasure.cancel(client, args.id, args.note or "")
    if args.execute:
        erasure.execute(client, args.id)
    e = erasure.wait(client, args.id, settle=True) if args.wait else erasure.get(client, args.id)
    print(f"erasure {e['id']}: {e['state'].lower()}; {e['progress']['destroyed']} of {e['progress']['total']} files destroyed, "
          f"{e['progress']['held']} held, {e['progress']['failed']} failed")
    for i in e.get("items") or []:
        if i["state"] != "DESTROYED":
            print(f"  {i['state']:<14} {i['path']}  {i.get('reason') or ''}")
    for s in e.get("successors") or []:
        print(f"  snapshot v{s['version']} -> successor v{s['successor_version']} ({s['removed']} members removed)")
    return 1 if e["state"] == "FAILED" else 0


def cmd_erasure_certificate(args):
    client = client_from_config()
    cert = erasure.certificate(client, args.id)
    if args.out:
        with open(args.out, "w", encoding="utf-8") as f:
            json.dump(cert, f, indent=2)
        say(f"certificate written to {args.out}")
    if args.verify or not args.out:
        body = erasure.verify_certificate(cert)
        print(f"signature ok (key {cert['key_id']}): {len(body['destroyed'])} files destroyed, "
              f"{len(body['snapshots_superseded'])} snapshots superseded, {len(body['models'])} models to retrain; "
              f"issued {body['issued_at']}")
    return 0


def cmd_mount(args):
    from .mount import mount
    client = client_from_config()
    ds = snapshots.find_dataset(client, args.dataset)
    v = snapshots.find_version(client, ds["id"], args.version)
    say(f"mounting version {v['version']} of {ds['name']} read-only at {args.mountpoint}; Ctrl+C to unmount")
    mount(client, ds["id"], v["id"], args.mountpoint)


def cmd_download(args):
    client = client_from_config()
    ds = snapshots.find_dataset(client, args.dataset)
    v = snapshots.find_version(client, ds["id"], args.version)
    try:
        est = tiering.version_estimate(client, ds["id"], v["id"])
        say_estimate("downloading it", est["download"])
        if est["archived_files"]:
            say(f"{est['archived_files']} files are archived: restore them first (osp restore-archived), "
                f"about {money(est['restore']['total_usd'])} and {est['restore']['hours']:g} hours")
    except APIError:
        pass
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
    b = create_branch(client, parent["id"], args.name, from_version)
    print(f"branch {b['name']} ({b['id']}): {b['member_count']} members from version {b['branch']['fork_version_id']}")


def cmd_branches(args):
    client = client_from_config()
    parent = snapshots.find_dataset(client, args.dataset)
    for b in list_branches(client, parent["id"]):
        print(f"{b['branch']['name']:<24} {b['id']}  members={b['member_count']}  merge base {b['branch']['merge_base_version_id']}")


def cmd_runs(args):
    client = client_from_config()
    ds = snapshots.find_dataset(client, args.dataset)
    chosen = [snapshots.find_version(client, ds["id"], args.version)] if args.version else snapshots.versions(client, ds["id"])
    found = False
    for v in chosen:
        for r in list_runs(client, v["id"]):
            found = True
            models = ", ".join(f"{m['name']}:{m['version']}" for m in r["models"]) or "no model"
            metrics = " ".join(f"{k}={value:g}" for k, value in sorted(r["metrics"].items()))
            print(f"v{v['version']:<4} {r['name']:<28} {r['status']:<9} {r['source']:<7} {models}  {metrics}".rstrip())
    if not found:
        print(f"no training runs recorded for {ds['name']}")


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
        r = merge(client, target["id"], source["id"], version, args.strategy, args.dry_run)
    except MergeConflict as conflict:
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


def cmd_findings(args):
    client = client_from_config()
    if args.version:
        ds = snapshots.find_dataset(client, args.target)
        v = snapshots.find_version(client, ds["id"], args.version)
        flagged = checks.version_findings(client, ds["id"], v["id"])
        for path, found in sorted(flagged.items()):
            for f in found:
                print(f"{f['action']:<6} {f['code']:<34} {path}  {f['detail']}")
        print(f"{len(flagged)} of {v['member_count']} members of {ds['name']} v{v['version']} have findings")
        return 1 if flagged else 0
    found = checks.findings(client, args.target)
    for f in found:
        print(f"{f['action']:<6} {f['code']:<34} {f['source']:<10} {f['detail']}")
    if not found:
        print("no findings")
    return 1 if found else 0


def cmd_blast_radius(args):
    r = checks.blast_radius(client_from_config(), args.asset)
    print(f"sha256 {r['checksum_sha256']} ({r['size_bytes']} bytes)")
    print(f"{len(r['assets'])} assets hold these bytes:")
    for a in r["assets"]:
        blocked = "  BLOCKED" if a.get("blocked_reason") else ""
        print(f"  {a['asset_id']}  {a['status']:<10} {a['file_name']}{blocked}")
    print(f"{len(r['snapshots'])} snapshots pin them:")
    for s in r["snapshots"]:
        print(f"  {s['dataset_name']} v{s['version']} ({s['members']} members)")
    print(f"{len(r['downloads'])} identities were given download links:")
    for d in r["downloads"]:
        print(f"  {d['actor_type'].lower()} {d['actor_id']}: {d['links']} links, last {d['last']}")


def cmd_content_policy(args):
    client = client_from_config()
    policy = checks.content_policy(client)
    if args.set or args.clear:
        actions = dict(policy.get("finding_actions") or {})
        for item in args.set or []:
            key, _, action = item.partition("=")
            actions[key] = action
        for key in args.clear or []:
            actions.pop(key, None)
        policy = checks.set_content_policy(client, actions, policy.get("allowed_types"), policy.get("denied_types"),
                                           policy.get("max_size_bytes") or 0, policy.get("unknown_type_action") or "")
    print(json.dumps(policy, indent=2))


def cmd_templates(args):
    for t in governance.templates(client_from_config()):
        print(f"{t['id']:<20} {t['min_retention']:>10} {t['lock_mode']:<10} {t['name']}")


def cmd_holds(args):
    for h in governance.holds(client_from_config(), args.released):
        what = h.get("matter") or h["scope_key"] or "whole organization"
        state = "released" if h.get("released_at") else "active"
        print(f"{h['id']}  {state:<8} {h['scope']:<8} {what}  ({h['reason']})")


def cmd_hold_proof(args):
    proof = governance.hold_proof(client_from_config(), args.hold, args.verify)
    for a in proof["assets"]:
        lock = "no lock"
        if a.get("lock_applied_at"):
            lock = f"lock set {a['lock_applied_at']}" + (" hold" if a["lock_legal_hold"] else "")
            if a.get("lock_retain_until"):
                lock += f" {a.get('lock_mode') or 'GOVERNANCE'} until {a['lock_retain_until']}"
        provider = f"  provider: {a['provider']}" if a.get("provider") else ""
        print(f"{a['asset_id']}  {a['file_name']}  {lock}{provider}")
    if proof["truncated"]:
        print("(the first 1000 assets)")
    mismatched = [a for a in proof["assets"] if (a.get("provider") or "").startswith("MISMATCH")]
    return 1 if mismatched else 0


def cmd_approvals(args):
    for a in governance.approvals(client_from_config(), "" if args.all else "PENDING"):
        print(f"{a['id']}  {a['state']:<8} {a['kind']:<17} {a['summary']}  by {a['requested_by']}: {a['reason']}")


def cmd_decide(args):
    decide = governance.approve if args.command == "approve" else governance.reject
    a = decide(client_from_config(), args.approval, args.note or "")
    print(f"{a['state']}: {a['summary']}" + (f" ({a['result']})" if a.get("result") else ""))
    return 0 if a["state"] in ("EXECUTED", "REJECTED") else 1


def cmd_bulk_delete(args):
    plan = governance.bulk_delete(client_from_config(), args.reason, args.asset, args.dataset or "", args.prefix or "",
                                  dry_run=not args.yes)
    blocked = ", ".join(f"{n} by {k.replace('_', ' ')}" for k, n in plan["blocked"].items()) or "nothing"
    print(f"{plan['matched']} assets, {plan['bytes']} bytes; held back now: {blocked}")
    if plan.get("approval_id"):
        print(f"sent for approval: {plan['approval_id']}")
    elif args.yes:
        print(f"{plan['scheduled']} scheduled for deletion")
    else:
        print("dry run: add --yes to delete")


def cmd_certificate(args):
    client = client_from_config()
    cert = governance.certificate(client, args.deletion)
    print(json.dumps(cert["body"], indent=2))
    if not args.verify:
        return 0
    try:
        key = governance.public_key(client)["public_key"]
    except APIError:
        key = None
    hash_ok, signature = governance.verify_certificate(cert, key)
    print(f"sha256 {'ok' if hash_ok else 'MISMATCH'}; signature {signature}")
    return 0 if hash_ok and signature in ("valid", "unsigned") else 1


def cmd_adopt(args):
    client = client_from_config()
    inline = ""
    if args.file:
        with open(args.file, encoding="utf-8") as f:
            inline = f.read()
    job = adoption.adopt(client, args.source, args.connection or "", args.ref or "", inline, args.dataset or "",
                         args.mode, not args.no_digest)
    say(f"adoption {job['id']} queued")
    if args.wait:
        job = adoption.wait(client, job["id"])
    print(f"{job['id']}  {job['state']}  entries={job['entries']} adopted={job['adopted']} rejected={job['rejected']} failed={job['failed']}")
    return 0 if job["state"] in ("QUEUED", "READING", "RUNNING", "COMPLETED") and not job["failed"] else 1


def cmd_adoptions(args):
    for j in adoption.jobs(client_from_config()):
        print(f"{j['id']}  {j['state']:<9} {j['source']:<12} entries={j['entries']} adopted={j['adopted']} "
              f"rejected={j['rejected']} failed={j['failed']}  {j['source_ref']}")


def cmd_adoption(args):
    client = client_from_config()
    j = adoption.job(client, args.job)
    print(json.dumps(j, indent=2))
    if args.results:
        with open(args.results, "w", encoding="utf-8", newline="") as out:
            writer = csv.writer(out)
            writer.writerow(["line", "object_key", "url", "path", "state", "code", "detail", "asset_id", "sha256"])
            for row in adoption.results(client, args.job):
                writer.writerow([row["line"], row.get("object_key", ""), row.get("url", ""), row["path"], row["state"],
                                 row.get("code", ""), row.get("detail", ""), row.get("asset_id", ""), row.get("sha256", "")])
        say(f"results written to {args.results}")


def cmd_export_mhl(args):
    client = client_from_config()
    ds = snapshots.find_dataset(client, args.dataset)
    v = snapshots.find_version(client, ds["id"], args.version)
    text = adoption.export_bag_manifest(client, ds["id"], v["id"]) if args.bagit else adoption.export_mhl(client, ds["id"], v["id"])
    if args.output:
        with open(args.output, "w", encoding="utf-8") as out:
            out.write(text)
        say(f"written to {args.output}")
    else:
        sys.stdout.write(text)


def cmd_posture(args):
    client = client_from_config()
    report = posture.check(client, args.connection, args.probe) if args.check or args.probe else posture.posture(client, args.connection)
    print(f"{report['worst']}  checked {report['checked_at']}  {'safe' if report['phi_safe'] else 'not safe'} for PHI")
    for c in report["checks"]:
        print(f"  {c['status']:<12} {c['name']:<25} {c['detail']}")
    return 1 if report["worst"] == "FAIL" else 0


def cmd_credentials(args):
    client = client_from_config()
    if args.action == "stage":
        secret = os.environ.get("OSP_STORAGE_SECRET") or getpass.getpass("Secret: ")
        fields = {"access_key_id": args.access_key_id, "secret_access_key": secret} if args.access_key_id else {"account_key": secret}
        out = posture.stage_credentials(client, args.connection, **fields)
        say("staged and tested; promote it when the old key can go")
    elif args.action == "promote":
        out = posture.promote_credentials(client, args.connection)
        say("promoted; the old credentials are kept for a rollback for 7 days")
    elif args.action == "rollback":
        out = posture.rollback_credentials(client, args.connection)
        say("rolled back")
    else:
        out = posture.discard_staged(client, args.connection)
        say("staged credentials discarded")
    c = out.get("connection", out)
    print(f"{c['id']}: rotation {c['credential_rotation']}" + (", one staged" if c.get("staged_credential") else ""))


def cmd_drift(args):
    client = client_from_config()
    if args.resolve:
        posture.resolve(client, args.resolve)
        say("resolved")
        return 0
    findings = posture.drift(client, args.connection or "", args.all)
    for f in findings:
        state = "resolved" if f.get("resolved_at") else "open"
        print(f"{f['id']}  {f['detected_at'][:16]}  {state:<8} {f['kind']:<14} {f['object_key']}: {f['detail']}")
    return 1 if any(not f.get("resolved_at") for f in findings) else 0


def cmd_integrity(args):
    client = client_from_config()
    ds = snapshots.find_dataset(client, args.dataset)
    v = snapshots.find_version(client, ds["id"], args.version)
    report = posture.integrity(client, ds["id"], v["id"])
    if not report["degraded"]:
        print("intact: no member changed outside the platform")
        return 0
    for o in report["objects"]:
        print(f"  {o['kind']:<14} {o['path']}: {o['detail']}")
    return 1


def cmd_data_class(args):
    client = client_from_config()
    ds = snapshots.find_dataset(client, args.dataset)
    out = posture.set_data_class(client, ds["id"], ds["revision"], "" if args.data_class == "none" else args.data_class.upper())
    print(f"{out['name']}: {out.get('data_class') or 'no data class'}")


def cmd_residency(args):
    client = client_from_config()
    ds = snapshots.find_dataset(client, args.dataset)
    if args.allow is not None or args.require_customer_key or args.no_customer_key:
        key = True if args.require_customer_key else (False if args.no_customer_key else None)
        allow = None if args.allow is None else [a for a in args.allow.split(",") if a]
        out = posture.set_residency(client, ds["id"], ds["revision"], allow, key)
        print(f"{out['name']}: {', '.join(out['residency']) or 'anywhere'}"
              + (", customer-managed key required" if out.get("require_customer_key") else ""))
        return 0
    v = snapshots.find_version(client, ds["id"], args.version or "latest")
    r = posture.residency(client, ds["id"], v["id"])
    print(f"allowed: {', '.join(r['allowed']) or 'anywhere'}" + (" (customer key required)" if r["require_customer_key"] else ""))
    for loc in r["locations"]:
        state = "verified" if loc["location_verified"] else "not verified"
        key = f", key {loc['encryption_key']}" if loc.get("encryption_key") else ""
        mark = "ok     " if loc["allowed"] else "OUTSIDE"
        print(f"  {mark} {loc['location'] or 'unknown':<16} ({state}{key})  {loc['members']} members on {loc['connection_id']}")
    return 0 if r["inside"] else 1


def cmd_encryption_key(args):
    client = client_from_config()
    conn = client.get(f"/api/v1/storage-connections/{args.connection}")
    out = posture.set_encryption_key(client, args.connection, conn["revision"], "" if args.key == "none" else args.key)
    print(f"{out['id']}: " + (f"every write encrypted with {out['encryption_key']}" if out.get("encryption_key") else "the bucket's default encryption"))


def cmd_protect(args):
    client = client_from_config()
    ds = snapshots.find_dataset(client, args.dataset)
    if any(v is not None for v in (args.replica, args.escrow, args.vault_days, args.scrub_days)):
        escrow = None if args.escrow is None else args.escrow == "on"
        protection.protect(client, ds["id"], ds["revision"], args.replica, escrow, args.vault_days, args.scrub_days)
    r = protection.replication(client, ds["id"])
    if not r["connection_id"]:
        print("no replica connection")
        return 0
    print(f"replica at {r['connection_id']}: {r['replicated']} of {r['objects']} objects copied, {r['pending']} pending"
          + (f", {r['failing']} failing ({r['last_error']})" if r["failing"] else ""))
    print(f"recovery point: {r['recovery_point_seconds']} seconds behind; escrow {'on' if r['escrow'] else 'off'};"
          f" vault {r['vault_retention_days']} days; scrub every {r['scrub_days'] or '-'} days")
    return 0 if r["pending"] == 0 else 1


def _finish(client, created, wait):
    j = created.get("job", created)
    if not wait:
        print(j["id"])
        return 0
    j = protection.wait(client, j["id"])
    print(f"{j['kind']} {j['state']}: {j['done_items']} of {j['total_items']} objects" + (f"; {j['error']}" if j.get("error") else ""))
    print(json.dumps(j.get("report", {}), indent=2))
    return 0 if j["state"] == "COMPLETED" else 1


def cmd_migrate(args):
    client = client_from_config()
    ds = snapshots.find_dataset(client, args.dataset)
    out = protection.migrate(client, ds["id"], args.target, dry_run=not args.yes)
    plan = out["plan"]
    print(f"{plan['objects']} objects, {plan['bytes']} bytes to copy ({plan['already_there']} already there);"
          f" egress about ${plan['egress_estimate_usd']} ({plan['egress_note']})")
    if not args.yes:
        say("a dry run: add --yes to migrate")
        return 0
    return _finish(client, out, args.wait)


def cmd_restore(args):
    client = client_from_config()
    ds = snapshots.find_dataset(client, args.dataset)
    v = snapshots.find_version(client, ds["id"], args.version)
    return _finish(client, protection.restore(client, ds["id"], v["id"], args.target, args.prefix or ""), args.wait)


def cmd_drill(args):
    client = client_from_config()
    ds = snapshots.find_dataset(client, args.dataset)
    return _finish(client, protection.drill(client, ds["id"], args.sample, args.from_replica), args.wait)


def cmd_export(args):
    client = client_from_config()
    if args.every is not None:
        protection.set_export_schedule(client, args.target, args.every)
        say(f"export bundle every {args.every} hours" if args.every else "scheduled export stopped")
        return 0
    return _finish(client, protection.export(client, args.target), args.wait)


def cmd_clean_point(args):
    client = client_from_config()
    ds = snapshots.find_dataset(client, args.dataset)
    if args.version:
        v = snapshots.find_version(client, ds["id"], args.version)
        protection.mark_clean(client, ds["id"], v["id"])
    diff = protection.changes_since_clean(client, ds["id"])
    for c in diff["changes"]:
        who = f" by {c['uploaded_by']} at {c['uploaded_at']}" if c.get("uploaded_by") else ""
        print(f"  {c['change']:<8} {c['path']}{who}")
    return 1 if diff["changes"] else 0


def cmd_tokens(args):
    for t in access.tokens(client_from_config(), args.all):
        state = "revoked" if t.get("revoked_at") else f"expires {t['expires_at'][:10]}"
        scope = ",".join(t["datasets"]) or "all datasets"
        used = t.get("last_used_at") or "never used"
        print(f"{t['id']}  {t['hint']}  {t['name']:<20} {state:<18} {scope}  {used}")


def cmd_token_create(args):
    t = access.create_token(client_from_config(), args.name, args.permission, args.dataset, args.network,
                            args.expires_in or "", args.allow_links)
    say(f"token {t['id']} for {t['name']}, expires {t['expires_at']}. It is shown once:")
    print(t["token"])


def cmd_token_revoke(args):
    access.revoke_token(client_from_config(), args.token)
    say("revoked")


def cmd_links(args):
    for link in access.links(client_from_config(), args.all, not args.expired):
        state = "revoked" if link.get("revoked_at") else f"until {link['expires_at']}"
        print(f"{link['id']}  {link['file_name']:<30} {state:<34} {link['uses']} uses  by {link['principal_id']}")


def cmd_revoke_links(args):
    if not (args.link or args.asset or args.member or args.all):
        sys.exit("name links to revoke: --link, --asset, --member or --all")
    n = access.revoke_links(client_from_config(), args.link, args.asset, args.member or "", args.all)
    print(f"{n} links revoked")


def cmd_alerts(args):
    client = client_from_config()
    if args.ack:
        access.acknowledge(client, args.ack)
        say("acknowledged")
        return
    for a in access.alerts(client, "" if args.all else "OPEN"):
        action = f"  [{a['action_taken']}]" if a.get("action_taken") else ""
        print(f"{a['id']}  {a['created_at'][:16]}  {a['kind']:<13} {a['principal_id']}: {a['detail']}{action}")


def cmd_egress(args):
    e = access.egress(client_from_config())
    budget = f" of {e['month_budget']}" if e["month_budget"] else ""
    print(f"this month: {e['month_bytes']} bytes{budget} ({e['action']} when over)")
    for m in e["members"]:
        print(f"  {m['principal_id']:<40} {m['bytes']:>16} bytes {m['reads']:>8} reads")


def cmd_classify(args):
    r = access.classify(client_from_config(), args.asset, args.classification.upper())
    print(f"{r['asset_id']}: {r['classification']}")


def cmd_access_policy(args):
    client = client_from_config()
    policy = access.access_policy(client)
    changes = {}
    for pair in args.set or []:
        key, _, value = pair.partition("=")
        if value.lower() in ("true", "false"):
            changes[key] = value.lower() == "true"
        elif value.isdigit():
            changes[key] = int(value)
        else:
            changes[key] = value
    if changes:
        policy = {k: v for k, v in policy.items() if k not in ("updated_by", "updated_at")}
        policy.update(changes)
        policy = access.set_access_policy(client, **policy)
    print(json.dumps(policy, indent=2))


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

    p = sub.add_parser("runs", help="training runs recorded as having used a dataset's versions")
    p.add_argument("dataset")
    p.add_argument("--version", help="one version; every version by default")
    p.set_defaults(run=cmd_runs)

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
                                 ("verify", cmd_verify, "check a version: a local folder against it, or, with no folder, "
                                                        "its signature and every file read back"),
                                 ("download", cmd_download, "download a version, verifying every file")):
        p = sub.add_parser(name, help=help_text)
        p.add_argument("dataset")
        p.add_argument("version", help='"latest", a version number or a version ID')
        if name == "download":
            p.add_argument("folder")
        if name == "verify":
            p.add_argument("folder", nargs="?")
            p.add_argument("--sample", type=float, default=1.0, help="share of files to re-hash, 0 to 1 (default every file)")
            p.add_argument("--seed", type=int, default=0, help="which files a sample picks")
            p.add_argument("--run", help="record the result on this training run")
        if name == "download":
            p.add_argument("--workers", type=int, default=8)
            p.add_argument("--remove-erased", action="store_true",
                           help="delete local copies of files the platform has erased")
        p.set_defaults(run=run)

    p = sub.add_parser("bench", help="measure how fast this machine reads a version, and the best number of streams")
    p.add_argument("dataset")
    p.add_argument("version", nargs="?", default="latest")
    p.add_argument("--seconds", type=float, default=5)
    p.add_argument("--max-streams", type=int, default=16)
    p.add_argument("--compare", type=int, default=0, metavar="N", help="also read N files one by one, to compare")
    p.set_defaults(run=cmd_bench)

    p = sub.add_parser("shards", help="list a version's shards: runs of files read as one stream")
    p.add_argument("dataset")
    p.add_argument("version", nargs="?", default="latest")
    p.add_argument("--target-bytes", type=int)
    p.set_defaults(run=cmd_shards)

    p = sub.add_parser("repack", help="gather a dataset's packed files into new packs in path order")
    p.add_argument("dataset")
    p.add_argument("--list", action="store_true", help="only list its repacks")
    p.add_argument("--cancel", metavar="ID")
    p.set_defaults(run=cmd_repack)

    p = sub.add_parser("packing", help="show or set the organization's target pack size")
    p.add_argument("--target-bytes", type=int, help="1048576 to 10737418240; 0 for the platform's default")
    p.set_defaults(run=cmd_packing)

    p = sub.add_parser("checkpoint", help="record, verify on another host, or list a training run's checkpoints")
    p.add_argument("action", choices=["add", "verify", "list"])
    p.add_argument("run")
    p.add_argument("dataset", nargs="?", help="the dataset holding the checkpoint's files (add, verify)")
    p.add_argument("version", nargs="?", default="latest")
    p.add_argument("--step", type=int, default=0)
    p.add_argument("--host", help="this machine's name (default its hostname)")
    p.set_defaults(run=cmd_checkpoint)

    p = sub.add_parser("tier-rules", help="list, add or delete the rules that move old, unread objects to colder tiers")
    p.add_argument("--add", metavar="NAME")
    p.add_argument("--tier", choices=tiering.TIERS[1:], default="ARCHIVE")
    p.add_argument("--min-age", type=int, default=0, help="days since publication")
    p.add_argument("--unread", type=int, default=0, help="days without a read")
    p.add_argument("--dataset", help="only this dataset's objects")
    p.add_argument("--delete", metavar="RULE_ID")
    p.set_defaults(run=cmd_tier_rules)

    p = sub.add_parser("restore-archived", help="make a version's archived files readable again")
    p.add_argument("dataset")
    p.add_argument("version")
    p.add_argument("--days", type=int, default=7)
    p.add_argument("--speed", choices=tiering.SPEEDS, default="STANDARD")
    p.add_argument("--wait", action="store_true")
    p.set_defaults(run=cmd_restore_archived)

    p = sub.add_parser("costs", help="this month's estimated costs and savings, or a dataset's")
    p.add_argument("dataset", nargs="?")
    p.add_argument("--years", type=float, default=10, help="for a dataset: years to keep it")
    p.set_defaults(run=cmd_costs)

    p = sub.add_parser("budget", help="show or set the monthly cost budget")
    p.add_argument("--usd", type=float)
    p.add_argument("--requests", type=int)
    p.add_argument("--action", choices=["ALERT", "BLOCK"])
    p.set_defaults(run=cmd_budget)

    p = sub.add_parser("estimate", help="what downloading, training on and restoring a version would cost")
    p.add_argument("dataset")
    p.add_argument("version", nargs="?", default="latest")
    p.add_argument("--speed", choices=tiering.SPEEDS, default="STANDARD")
    p.set_defaults(run=cmd_estimate)

    p = sub.add_parser("expire", help="expire a dataset's old snapshots (a preview unless --yes)")
    p.add_argument("dataset")
    p.add_argument("--keep-last", type=int, default=5)
    p.add_argument("--older-than", type=int, default=0, metavar="DAYS")
    p.add_argument("--yes", action="store_true")
    p.set_defaults(run=cmd_expire)

    p = sub.add_parser("storage-manifest", help="a version's members with their storage URIs, as JSON lines")
    p.add_argument("dataset")
    p.add_argument("version")
    p.add_argument("--out")
    p.set_defaults(run=cmd_storage_manifest)

    p = sub.add_parser("audit-export", help="export the organization's audit chain (JSON lines, or CEF for a SIEM)")
    p.add_argument("--after", type=int, default=0, metavar="SEQ")
    p.add_argument("--format", choices=["jsonl", "cef"], default="jsonl")
    p.add_argument("--out")
    p.set_defaults(run=cmd_audit_export)

    p = sub.add_parser("audit-verify", help="check an audit export without the platform, and against the anchors")
    p.add_argument("file")
    p.add_argument("--anchors", action="store_true", help="also check the organization's anchors (signs in)")
    p.set_defaults(run=cmd_audit_verify)

    p = sub.add_parser("anchors", help="list the audit chain's anchors, each checked")
    p.add_argument("--now", action="store_true", help="anchor the chain now")
    p.set_defaults(run=cmd_anchors)

    p = sub.add_parser("reviews", help="list, create, sign or export audit exception reviews")
    p.add_argument("--create", type=int, metavar="DAYS", help="review the last DAYS days now")
    p.add_argument("--sign", metavar="REVIEW_ID")
    p.add_argument("--note")
    p.add_argument("--export", metavar="REVIEW_ID")
    p.add_argument("--out")
    p.set_defaults(run=cmd_reviews)

    p = sub.add_parser("provenance", help="show or record a dataset's lawful basis, licence and source")
    p.add_argument("dataset")
    p.add_argument("--basis")
    p.add_argument("--license")
    p.add_argument("--source")
    p.add_argument("--kind", choices=["PUBLIC_DATASET", "LICENSED_PRIVATE", "WEB_CRAWLED", "USER_DATA", "SYNTHETIC", "OTHER"])
    p.set_defaults(run=cmd_provenance)

    p = sub.add_parser("evidence-pack", help="download a version's evidence pack: custody, signed manifest, load files")
    p.add_argument("dataset")
    p.add_argument("version")
    p.add_argument("out")
    p.add_argument("--digests", action="store_true", help="read every file to add MD5 and SHA-1")
    p.set_defaults(run=cmd_evidence_pack)

    p = sub.add_parser("snapshot-hold", help="hold every member of a version (FOIA, litigation), list or release")
    p.add_argument("dataset")
    p.add_argument("version")
    p.add_argument("--kind", choices=["FOIA", "LITIGATION", "AUDIT", "INVESTIGATION"])
    p.add_argument("--ref")
    p.add_argument("--reason")
    p.add_argument("--release", metavar="MATTER")
    p.set_defaults(run=cmd_snapshot_hold)

    p = sub.add_parser("ai-summary", help="a model's training-content summary on the EU AI Act template")
    p.add_argument("model", help="model ID")
    p.add_argument("--out")
    p.add_argument("--publish", action="store_true", help="record it as published (starts six months)")
    p.set_defaults(run=cmd_ai_summary)

    p = sub.add_parser("compliance", help="control mappings (nist-800-171, 21-cfr-11, sec-17a-4) and their evidence")
    p.add_argument("framework", nargs="?")
    p.add_argument("--evidence", metavar="OUT_ZIP")
    p.set_defaults(run=cmd_compliance)

    p = sub.add_parser("regulator-access", help="issue a read-only, time-limited token for an examiner")
    p.add_argument("name")
    p.add_argument("--days", type=int, default=7)
    p.set_defaults(run=cmd_regulator_access)

    p = sub.add_parser("as-of", help="a namespace's objects as they stood at a time (RFC 3339)")
    p.add_argument("namespace")
    p.add_argument("at")
    p.set_defaults(run=cmd_as_of)

    p = sub.add_parser("profile", help="a version's column profile (tables, columns, what was skipped)")
    p.add_argument("dataset")
    p.add_argument("version")
    p.add_argument("--again", action="store_true", help="profile it again, and wait")
    p.add_argument("--wait", action="store_true", help="wait for the profile")
    p.set_defaults(run=cmd_profile)

    p = sub.add_parser("column-drift", help="a version's columns against its parent's; exit 1 on a breach")
    p.add_argument("dataset")
    p.add_argument("version")
    p.add_argument("--against", metavar="VERSION")
    p.set_defaults(run=cmd_drift)

    p = sub.add_parser("leakage", help="evaluation files also in training (hash, subject, n-grams); exit 1 on leakage")
    p.add_argument("eval_dataset")
    p.add_argument("eval_version")
    p.add_argument("train_dataset")
    p.add_argument("train_version")
    p.add_argument("--ngram", type=int, metavar="N", help="also compare text by word N-grams (13 is usual), and record it")
    p.add_argument("--threshold", type=float, default=0.5, help="share of a text's n-grams found in training (0.5)")
    p.add_argument("--run", metavar="RUN_ID")
    p.add_argument("--verbose", action="store_true")
    p.set_defaults(run=cmd_leakage)

    p = sub.add_parser("release", help="name a version a release (DR1, 2026.10), once")
    p.add_argument("dataset")
    p.add_argument("version")
    p.add_argument("label")
    p.add_argument("--notes")
    p.set_defaults(run=cmd_release)

    p = sub.add_parser("releases", help="a dataset's releases")
    p.add_argument("dataset")
    p.set_defaults(run=cmd_releases)

    p = sub.add_parser("release-diff", help="a release against the one before it: files and drift")
    p.add_argument("dataset")
    p.add_argument("label")
    p.add_argument("--against", metavar="LABEL")
    p.set_defaults(run=cmd_release_diff)

    p = sub.add_parser("quality-settings", help="show or change a dataset's profiling, drift thresholds and subject")
    p.add_argument("dataset")
    p.add_argument("--profiles", choices=["on", "off"])
    p.add_argument("--max-psi", type=float)
    p.add_argument("--max-null-change", type=float)
    p.add_argument("--max-row-change", type=float)
    p.add_argument("--subject-key")
    p.add_argument("--subject-pattern")
    p.set_defaults(run=cmd_quality_settings)

    def erasure_selectors(p):
        p.add_argument("--subject", action="append", metavar="SUBJECT_ID", help="a subject (repeatable)")
        p.add_argument("--sha256", action="append", help="a content hash (repeatable)")
        p.add_argument("--object", action="append", metavar="OBJECT_ID", help="a catalog object, every version (repeatable)")
        p.add_argument("--manifest", help="every sha256 of a JSON-lines manifest")
        p.add_argument("--subject-key", help="the metadata key naming subjects (subject_id)")

    p = sub.add_parser("erasure-search", help="what erasing subjects, hashes or objects would reach: copies, snapshots, models")
    erasure_selectors(p)
    p.set_defaults(run=cmd_erasure_search)

    p = sub.add_parser("erasure-request", help="record an erasure request with its plan")
    p.add_argument("kind", choices=list(erasure.KINDS))
    p.add_argument("reason")
    p.add_argument("--reference", help="a case, ticket or court order")
    p.add_argument("--institution", help="who the certificate is for")
    erasure_selectors(p)
    p.set_defaults(run=cmd_erasure_request)

    p = sub.add_parser("erasures", help="list erasure requests")
    p.add_argument("--state")
    p.set_defaults(run=cmd_erasures)

    p = sub.add_parser("erasure", help="show, approve, execute or cancel an erasure request")
    p.add_argument("id")
    p.add_argument("--approve", action="store_true")
    p.add_argument("--execute", action="store_true")
    p.add_argument("--cancel", action="store_true")
    p.add_argument("--note")
    p.add_argument("--wait", action="store_true", help="wait until it completes (or waits on a hold)")
    p.set_defaults(run=cmd_erasure)

    p = sub.add_parser("erasure-certificate", help="the signed certificate of destruction; checks its signature")
    p.add_argument("id")
    p.add_argument("--out")
    p.add_argument("--verify", action="store_true")
    p.set_defaults(run=cmd_erasure_certificate)

    p = sub.add_parser("mount", help="mount a version read-only as a folder (FUSE)")
    p.add_argument("dataset")
    p.add_argument("version")
    p.add_argument("mountpoint")
    p.set_defaults(run=cmd_mount)

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

    p = sub.add_parser("findings", help="what the content checks found in an asset, or in every member of a version")
    p.add_argument("target", help="an asset id, or a dataset with --version")
    p.add_argument("--version", help="check every member of this version of the dataset")
    p.set_defaults(run=cmd_findings)

    p = sub.add_parser("blast-radius", help="where an asset's bytes went: copies, snapshots, downloads")
    p.add_argument("asset")
    p.set_defaults(run=cmd_blast_radius)

    p = sub.add_parser("content-policy", help="show or change what content check findings do")
    p.add_argument("--set", action="append", metavar="CODE=ACTION", help="e.g. secret=reject, model.pickle_imports=allow")
    p.add_argument("--clear", action="append", metavar="CODE", help="back to the default action")
    p.set_defaults(run=cmd_content_policy)

    sub.add_parser("templates", help="retention templates (SEC 17a-4, CFTC, CMS, Ofcom, NARA, MRC)").set_defaults(run=cmd_templates)

    p = sub.add_parser("holds", help="list legal holds")
    p.add_argument("--released", action="store_true", help="include released holds")
    p.set_defaults(run=cmd_holds)

    p = sub.add_parser("hold-proof", help="what a hold covers and when its locks took effect")
    p.add_argument("hold")
    p.add_argument("--verify", action="store_true", help="read the locks back from the provider")
    p.set_defaults(run=cmd_hold_proof)

    p = sub.add_parser("approvals", help="changes waiting for a second member")
    p.add_argument("--all", action="store_true", help="every state, not only waiting ones")
    p.set_defaults(run=cmd_approvals)
    for name in ("approve", "reject"):
        p = sub.add_parser(name, help=f"{name} a change another member asked for")
        p.add_argument("approval")
        p.add_argument("--note")
        p.set_defaults(run=cmd_decide)

    p = sub.add_parser("bulk-delete", help="delete many assets (a dry run unless --yes)")
    p.add_argument("--reason", required=True)
    p.add_argument("--dataset")
    p.add_argument("--prefix", help="path prefix")
    p.add_argument("--asset", action="append", help="an asset id; repeat for more")
    p.add_argument("--yes", action="store_true", help="really delete")
    p.set_defaults(run=cmd_bulk_delete)

    p = sub.add_parser("certificate", help="a deletion's disposition certificate")
    p.add_argument("deletion")
    p.add_argument("--verify", action="store_true", help="check its SHA-256 and signature")
    p.set_defaults(run=cmd_certificate)

    p = sub.add_parser("adopt", help="adopt objects already in your bucket, or import URLs / a STAC catalog")
    p.add_argument("source", choices=["MANIFEST", "S3_INVENTORY", "MHL", "BAGIT", "URL_LIST", "STAC"], type=str.upper)
    p.add_argument("--connection", help="the storage connection holding the objects")
    p.add_argument("--ref", help="the source's object key on the connection, or the STAC URL")
    p.add_argument("--file", help="a local MANIFEST or URL_LIST file to send inline")
    p.add_argument("--dataset", help="dataset namespace for the catalog paths")
    p.add_argument("--mode", default="DETACH", choices=["DETACH", "MANAGE"], type=str.upper)
    p.add_argument("--no-digest", action="store_true", help="adopt entries without an expected digest")
    p.add_argument("--wait", action="store_true", help="wait for the job to finish")
    p.set_defaults(run=cmd_adopt)

    sub.add_parser("adoptions", help="list adoption jobs").set_defaults(run=cmd_adoptions)
    p = sub.add_parser("adoption", help="one adoption job, and its per-entry results")
    p.add_argument("job")
    p.add_argument("--results", metavar="FILE.csv", help="write every entry's result to a CSV file")
    p.set_defaults(run=cmd_adoption)

    p = sub.add_parser("export-mhl", help="a snapshot as an ASC MHL list (or --bagit manifest)")
    p.add_argument("dataset")
    p.add_argument("version")
    p.add_argument("--bagit", action="store_true", help="the BagIt manifest-sha256.txt instead")
    p.add_argument("-o", "--output")
    p.set_defaults(run=cmd_export_mhl)

    p = sub.add_parser("posture", help="a storage connection's bucket posture")
    p.add_argument("connection")
    p.add_argument("--check", action="store_true", help="check now instead of showing the last report")
    p.add_argument("--probe", action="store_true", help="check now and try an SSE-C write, which must be refused")
    p.set_defaults(run=cmd_posture)
    p = sub.add_parser("credentials", help="stage, promote, roll back or discard a connection's second credential")
    p.add_argument("action", choices=["stage", "promote", "rollback", "discard"])
    p.add_argument("connection")
    p.add_argument("--access-key-id", help="for stage on S3, R2 and S3-compatible (the secret is read from OSP_STORAGE_SECRET or asked)")
    p.set_defaults(run=cmd_credentials)
    p = sub.add_parser("drift", help="objects changed or added outside the platform")
    p.add_argument("connection", nargs="?")
    p.add_argument("--all", action="store_true", help="include resolved findings")
    p.add_argument("--resolve", metavar="FINDING")
    p.set_defaults(run=cmd_drift)
    p = sub.add_parser("integrity", help="whether a snapshot's members changed outside the platform")
    p.add_argument("dataset")
    p.add_argument("version")
    p.set_defaults(run=cmd_integrity)
    p = sub.add_parser("data-class", help="mark a dataset as holding PHI, PCI or PII")
    p.add_argument("dataset")
    p.add_argument("data_class", choices=["phi", "pci", "pii", "none", "PHI", "PCI", "PII"])
    p.set_defaults(run=cmd_data_class)

    p = sub.add_parser("residency", help="where a snapshot's bytes are, or set where a dataset's data may live")
    p.add_argument("dataset")
    p.add_argument("version", nargs="?")
    p.add_argument("--allow", metavar="LOCATIONS", help="comma-separated, such as ap-south-1 or eu-* (empty for anywhere)")
    p.add_argument("--require-customer-key", action="store_true")
    p.add_argument("--no-customer-key", action="store_true")
    p.set_defaults(run=cmd_residency)
    p = sub.add_parser("encryption-key", help="encrypt every write to a connection with a customer-managed key")
    p.add_argument("connection")
    p.add_argument("key", help="KMS key ARN or alias, Cloud KMS key name, Azure encryption scope, or none")
    p.set_defaults(run=cmd_encryption_key)

    p = sub.add_parser("protect", help="a dataset's replica, escrow, vault and scrubs (show, or set)")
    p.add_argument("dataset")
    p.add_argument("--replica", metavar="CONNECTION", help="the replica connection; '' removes it")
    p.add_argument("--escrow", choices=["on", "off"])
    p.add_argument("--vault-days", type=int, help="lock the replica's copies in COMPLIANCE mode this many days")
    p.add_argument("--scrub-days", type=int, help="re-read every member once in this many days (0 off)")
    p.set_defaults(run=cmd_protect)
    p = sub.add_parser("migrate", help="move a dataset to another connection, verified (a dry run unless --yes)")
    p.add_argument("dataset")
    p.add_argument("target")
    p.add_argument("--yes", action="store_true")
    p.add_argument("--wait", action="store_true")
    p.set_defaults(run=cmd_migrate)
    p = sub.add_parser("restore", help="copy a snapshot to a connection, with its SHA-256 manifest")
    p.add_argument("dataset")
    p.add_argument("version")
    p.add_argument("target")
    p.add_argument("--prefix")
    p.add_argument("--wait", action="store_true")
    p.set_defaults(run=cmd_restore)
    p = sub.add_parser("drill", help="re-read a sample and measure how long a restore would take")
    p.add_argument("dataset")
    p.add_argument("--sample", type=int, default=100)
    p.add_argument("--from-replica", action="store_true")
    p.add_argument("--wait", action="store_true")
    p.set_defaults(run=cmd_drill)
    p = sub.add_parser("export", help="write the export-everything bundle to a connection, or schedule it")
    p.add_argument("target")
    p.add_argument("--every", type=int, metavar="HOURS", help="schedule it (0 stops the schedule)")
    p.add_argument("--wait", action="store_true")
    p.set_defaults(run=cmd_export)
    p = sub.add_parser("clean-point", help="what changed since a dataset's clean point (or mark one)")
    p.add_argument("dataset")
    p.add_argument("--version", help="mark this version as the clean point")
    p.set_defaults(run=cmd_clean_point)

    p = sub.add_parser("tokens", help="list API tokens")
    p.add_argument("--all", action="store_true", help="every member's (members.manage)")
    p.set_defaults(run=cmd_tokens)
    p = sub.add_parser("token-create", help="create an API token for a script or a vendor")
    p.add_argument("name")
    p.add_argument("-p", "--permission", action="append", required=True, help="e.g. assets.read; repeat")
    p.add_argument("--dataset", action="append", help="confine it to a dataset; repeat")
    p.add_argument("--network", action="append", help="only from this CIDR; repeat")
    p.add_argument("--expires-in", help="e.g. 720h (default 90 days)")
    p.add_argument("--allow-links", action="store_true", help="may create share links")
    p.set_defaults(run=cmd_token_create)
    p = sub.add_parser("token-revoke", help="revoke an API token")
    p.add_argument("token")
    p.set_defaults(run=cmd_token_revoke)
    p = sub.add_parser("links", help="share links you (or, with --all, anyone) created")
    p.add_argument("--all", action="store_true")
    p.add_argument("--expired", action="store_true", help="include expired and revoked links")
    p.set_defaults(run=cmd_links)
    p = sub.add_parser("revoke-links", help="revoke share links in bulk")
    p.add_argument("--link", action="append")
    p.add_argument("--asset", action="append")
    p.add_argument("--member", help="every link a member created")
    p.add_argument("--all", action="store_true", help="every live link in the organization")
    p.set_defaults(run=cmd_revoke_links)
    p = sub.add_parser("alerts", help="security alerts: read spikes, mass links, new networks, egress")
    p.add_argument("--all", action="store_true", help="include acknowledged alerts")
    p.add_argument("--ack", metavar="ALERT", help="acknowledge an alert")
    p.set_defaults(run=cmd_alerts)
    sub.add_parser("egress", help="this month's downloads against the budget").set_defaults(run=cmd_egress)
    p = sub.add_parser("classify", help="set an asset's classification")
    p.add_argument("asset")
    p.add_argument("classification", choices=["public", "internal", "confidential", "restricted",
                                              "PUBLIC", "INTERNAL", "CONFIDENTIAL", "RESTRICTED"])
    p.set_defaults(run=cmd_classify)
    p = sub.add_parser("access-policy", help="show or change the link, egress and anomaly policy")
    p.add_argument("--set", action="append", metavar="KEY=VALUE", help="e.g. egress_action=BLOCK; repeat")
    p.set_defaults(run=cmd_access_policy)

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
