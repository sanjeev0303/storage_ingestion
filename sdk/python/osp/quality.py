"""Data quality (docs/quality.md): column profiles, drift, leakage and
releases.

    from osp import quality
    p = quality.wait_profile(client, ds, v)
    report = quality.drift(client, ds, v)
    quality.leakage(client, eval_ds, eval_v, train_v)
    quality.ngram_leakage(client, train_ds, train_v, eval_ds, eval_v, n=13)
    quality.release(client, ds, v, "DR2", "reprocessed with baseline 05.00")

ngram_leakage compares text where the text is: it reads both versions'
text files (each checked against its pinned SHA-256), and records the
result on the platform as the caller's.
"""

import hashlib
import re
import time

from . import snapshots

TEXT_TYPES = ("text/", "application/json", "application/x-ndjson", "application/jsonl")
TEXT_EXTENSIONS = (".txt", ".md", ".json", ".jsonl", ".ndjson", ".csv", ".tsv", ".xml", ".html", ".py", ".tex")


def _v(dataset_id, version_id):
    return f"/api/v1/datasets/{dataset_id}/versions/{version_id}"


# ─── Settings, profiles and drift ─────────────────────────────────────

def settings(client, dataset_id):
    return client.get(f"/api/v1/datasets/{dataset_id}/quality-settings")


def set_settings(client, dataset_id, **changes):
    """Changes some of profile_snapshots, max_psi, max_null_change,
    max_row_change, subject_key and subject_pattern."""
    current = settings(client, dataset_id)
    current.update(changes)
    for k in ("dataset_id", "updated_by", "updated_at"):
        current.pop(k, None)
    return client.request("PUT", f"/api/v1/datasets/{dataset_id}/quality-settings", body=current)


def profile(client, dataset_id, version_id):
    return client.get(_v(dataset_id, version_id) + "/profile")


def reprofile(client, dataset_id, version_id):
    return client.post(_v(dataset_id, version_id) + "/profile")


def wait_profile(client, dataset_id, version_id, every=5.0, timeout=1800):
    """Waits for the version's profile to be done (or failed)."""
    deadline = time.monotonic() + timeout
    while True:
        p = profile(client, dataset_id, version_id)
        if p["state"] in ("DONE", "FAILED", "NONE") or time.monotonic() > deadline:
            return p
        time.sleep(every)


def drift(client, dataset_id, version_id, against=None):
    """The version's columns against its baseline (its parent, or the
    version before it), or against another version."""
    return client.get(_v(dataset_id, version_id) + "/drift", against=against)


def drift_lines(report):
    """A drift report as lines of text, breaches first."""
    lines = []
    for t in report["tables"]:
        flag = "  BREACH " + ", ".join(t["breached"]) if t["breached"] else ""
        lines.append(f"{t['table']}: {t['rows_before']:,} -> {t['rows_after']:,} rows ({t['row_change'] * 100:+.1f}%){flag}")
        if t["columns_removed"]:
            lines.append(f"  removed: {', '.join(t['columns_removed'])}")
        if t["columns_added"]:
            lines.append(f"  added: {', '.join(t['columns_added'])}")
        for c in t["columns"]:
            psi = f"PSI {c['psi']:.3f}" if c.get("psi") is not None else (c.get("not_compared") or "")
            nulls = f"nulls {c['null_rate_before'] * 100:.1f}% -> {c['null_rate_after'] * 100:.1f}%"
            flag = "  BREACH " + ", ".join(c["breached"]) if c["breached"] else ""
            lines.append(f"  {c['name']}: {psi}, {nulls}{flag}")
    for name in report.get("not_compared") or []:
        lines.append(f"not compared: {name}")
    return lines


# ─── Leakage ──────────────────────────────────────────────────────────

def leakage(client, eval_dataset_id, eval_version_id, train_version_id):
    """Compares an evaluation version with a training version by SHA-256
    and by subject; the answer lists what was not checked."""
    return client.post(_v(eval_dataset_id, eval_version_id) + "/leakage", {"train_version_id": train_version_id})


def run_leakage(client, run_id, check=False):
    """A run's leakage checks; with check, compares its inputs now."""
    if check:
        return client.post(f"/api/v1/training-runs/{run_id}/leakage")["reports"]
    return client.get(f"/api/v1/training-runs/{run_id}/leakage")["checks"]


_WORD = re.compile(r"\w+", re.UNICODE)


def ngrams(text, n):
    """The set of a text's word n-grams, each as an 8-byte hash."""
    words = _WORD.findall(text.lower())
    out = set()
    for i in range(len(words) - n + 1):
        out.add(hashlib.blake2b(" ".join(words[i:i + n]).encode(), digest_size=8).digest())
    return out


def contaminated(train_grams, text, n, threshold):
    """(share, compared): the share of the text's n-grams found in
    training, and whether the text had any n-grams to compare."""
    grams = ngrams(text, n)
    if not grams:
        return 0.0, False
    return len(grams & train_grams) / len(grams), True


def _is_text(member):
    ct = (member.get("content_type") or "").lower()
    return ct.startswith(TEXT_TYPES) or member["path"].lower().endswith(TEXT_EXTENSIONS)


def _texts(client, dataset_id, version_id):
    for m in snapshots.members(client, dataset_id, version_id):
        if m.get("availability", "AVAILABLE") == "AVAILABLE" and _is_text(m):
            yield m["path"], snapshots.fetch_member(client, m).decode("utf-8", "replace")


def ngram_leakage(client, train_dataset_id, train_version_id, eval_dataset_id, eval_version_id, n=13, threshold=0.5,
                  run_id=None, record=True, progress=None):
    """Finds evaluation texts that share at least threshold of their word
    n-grams with the training texts (13-grams, as GPT-3's contamination
    study used), and records the result. Returns the recorded check, or
    the result when record is False."""
    # ponytail: the training n-grams are held in memory, 8 bytes each plus
    # set overhead; corpora past a few billion words want a Bloom filter.
    train = set()
    for path, text in _texts(client, train_dataset_id, train_version_id):
        train |= ngrams(text, n)
        if progress:
            progress("train", path)
    overlap, members, examples = 0, 0, []
    for path, text in _texts(client, eval_dataset_id, eval_version_id):
        share, compared = contaminated(train, text, n, threshold)
        if progress:
            progress("eval", path)
        if not compared:
            continue
        members += 1
        if share >= threshold:
            overlap += 1
            if len(examples) < 20:
                examples.append({"eval_path": path, "key": f"{share * 100:.0f}% of {n}-grams"})
    result = {"run_id": run_id or "", "train_version_id": train_version_id, "eval_version_id": eval_version_id,
              "n": n, "threshold": threshold, "overlap": overlap, "eval_members": members, "examples": examples}
    if not record:
        return result
    return client.post("/api/v1/leakage-checks", result)


# ─── Releases ─────────────────────────────────────────────────────────

def release(client, dataset_id, version_id, label, notes=""):
    """Names a version a release (DR1, 2026.10), once."""
    return client.request("PUT", _v(dataset_id, version_id) + "/release", body={"label": label, "notes": notes})


def releases(client, dataset_id):
    return client.get(f"/api/v1/datasets/{dataset_id}/releases")["releases"]


def release_diff(client, dataset_id, label, against=None):
    """A release against the one before it: files added, removed and
    changed, and the drift between their profiles."""
    return client.get(f"/api/v1/datasets/{dataset_id}/releases/{label}/diff", against=against)
