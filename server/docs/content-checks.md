# Content checks

Every upload is checked inside the quarantine gate before it is published, after its SHA-256 is verified and its type is decided by its own bytes, and before the malware scan. The checks look for what the type alone does not show. Each thing they find is a **finding**; the organization's policy decides whether a finding **flags** the file (it is published, with the finding on its record) or **rejects** it (it is not).

## What is checked

The content inspector (`ContentInspector`, validator `content`) opens each object once and runs the check its format calls for:

| Format (by its bytes, or its name where bytes are ambiguous) | Check | Codes |
| --- | --- | --- |
| PDF | JavaScript, launch actions, embedded files, rich media, XFA, including names escaped as `#xx` and inside compressed object streams | `active_content.pdf_javascript`, `pdf_launch`, `pdf_embedded_file`, `pdf_rich_media`, `pdf_xfa` |
| Office (zip) | VBA projects, ActiveX, embedded OLE objects | `active_content.ooxml_macro`, `ooxml_activex`, `ooxml_ole_object` |
| Office (legacy OLE) | VBA projects, embedded packages | `active_content.ole_macro`, `ole_package` |
| Pickle, PyTorch (`.pt`, `.pth`, `.ckpt`, zip or legacy), joblib | Opcode walk without executing: every GLOBAL and STACK_GLOBAL import judged against a safe list (tensors, arrays, containers) and a dangerous list (os, subprocess, builtins.eval...) | `model.unsafe_pickle`, `model.pickle_imports`, `model.invalid_pickle` |
| NumPy `.npy`, `.npz` | Object arrays, whose pickles are walked too | `model.npy_object_array` (and the pickle codes) |
| safetensors | Header length, JSON, dtypes, offsets inside the file, shape matching the byte range | `model.invalid_safetensors` |
| gzip (any member count) | Read to the end, so every member's CRC-32 and length are checked; expansion ratio | `format.corrupt_gzip`, `format.archive_bomb` |
| BGZF / BAM | EOF block present; BAM header, references and every record's length and reference ids | `format.truncated_bgzf`, `format.invalid_bam` |
| FASTQ (plain or gzipped) | Every record: `@` header, sequence, `+`, quality as long as the sequence, printable | `format.invalid_fastq` |
| MCAP | Magic, every record's length, footer and closing magic | `format.invalid_mcap` |
| tar (plain or gzipped) | Entry count, paths and links that leave the extraction folder, device files | `format.unsafe_archive_entry`, `format.archive_too_many_entries`, `format.corrupt_tar` |
| Text (detected text, JSON, CSV, and configuration and notebook files) | Credentials: AWS keys, private keys, GitHub, Slack, Stripe, Google, Azure and AI provider keys, signed JWTs. A finding names the kind, the byte offset and the count, never the value. | `secret.*` |

A read failure of the object is retried, never taken for a broken file. A check that read only part of an object (larger than `CONTENT_CHECK_MAX_BYTES`, a text file past `SECRET_SCAN_MAX_BYTES`, a gzip past `ARCHIVE_MAX_UNCOMPRESSED_BYTES`) says so in the processing history (`content_checks_partial`), and the job's coverage is recorded as `PARTIAL`.

### External engine

`CONTENT_HOOK_URL` sends every upload (up to `CONTENT_HOOK_MAX_BYTES`) to an engine the operator runs: a PII or de-identification service, YARA, a second antivirus. The request is a POST of the bytes with `Authorization: Bearer $CONTENT_HOOK_TOKEN` and `X-OSP-Asset-Id`, `X-OSP-File-Name`, `X-OSP-Content-Type` and `X-OSP-Size`. The answer:

```json
{"findings": [{"code": "pii.ssn", "action": "reject", "detail": "3 US social security numbers"}]}
```

Codes are recorded as `external.<code>`; the engine's action stands unless the organization overrides it. A failed or malformed answer is an error: the job is retried and in the end held, so the hook never passes a file it did not judge.

## What a finding does

Defaults, by family and where a code differs:

| Family | Default | Except |
| --- | --- | --- |
| `active_content` | flag | `pdf_launch` rejects |
| `model` | reject | `pickle_imports` and `npy_object_array` flag |
| `secret` | flag | |
| `format` | reject | |
| `external` | the engine's action, else flag | |
| `malware` | reject, always | cannot be changed |

`PUT /api/v1/content-policy` (org.manage) sets the organization's own policy, `GET` (org.read) reads it:

```json
{"finding_actions": {"secret": "reject", "model.pickle_imports": "allow"},
 "allowed_types": [], "denied_types": ["application/x-tar"], "max_size_bytes": 0, "unknown_type_action": ""}
```

`allow` records nothing. The types and size only narrow the platform's rules.

## Re-scan on new signatures

A CLEAN verdict holds for the signatures that gave it. Each processing cycle that is not busy with uploads claims up to `RESCAN_BATCH_SIZE` (default 20, 0 turns it off) published assets whose clean verdict came from other signatures than the scanner's current ones, and scans their published bytes again (the standalone object, or its range of a pack). A clean re-scan records the new version, so the next upload of the same bytes reuses it. An infected one:

- **blocks** the asset: it keeps its lifecycle state (its catalog version and usage are unchanged) but every download is refused (`blocked_reason: malware_detected`);
- records the finding `malware.detected_on_rescan` and the processing event `rescan_infected`;
- sets the job's scan result to INFECTED, so the readiness rule `malware-scan` fails the snapshots that pin it;
- fires the `object.flagged` webhook.

A claim lasts an hour; a re-scan that fails is tried again after it.

## Where findings show

- `GET /api/v1/assets/{id}/findings` (assets.read), and in `GET /api/v1/assets/{id}/quarantine`.
- `GET /api/v1/assets/{id}/blast-radius` (audit.read, itself audited): every asset with the same SHA-256 and size, the snapshots that pin them, and who was given download links.
- Readiness: version 2 of the built-in profiles adds the `content-checks` rule (WARNING) wherever malware scans are checked. It reports `CONTENT_FLAGGED`, `CONTENT_REJECTED`, and `CONTENT_NOT_CHECKED` for files the checks never ran on (uploaded before they existed). Its config takes `ignore` (families or codes) and `allow_unchecked`. Version 1 of each profile stays resolvable.
- The webhook event `object.flagged`, one per finding, queued in the transaction that recorded it.
- SDK: `osp.findings`, `osp.version_findings`, `osp.blast_radius`, `osp.content_policy`, `osp.set_content_policy`; CLI: `osp findings`, `osp findings DATASET --version N`, `osp blast-radius`, `osp content-policy --set secret=reject`.
- Console: the asset screen's Content checks and Where these bytes went panels, and Settings, Content checks.

## Settings

| Variable | Default | |
| --- | --- | --- |
| `CONTENT_CHECKS_ENABLED` | `true` | The content inspector |
| `CONTENT_CHECK_MAX_BYTES` | 10 GiB | Larger objects are not inspected (noted) |
| `SECRET_SCAN_MAX_BYTES` | 64 MiB | Of each text file |
| `CONTENT_HOOK_URL`, `CONTENT_HOOK_TOKEN` | unset | External engine |
| `CONTENT_HOOK_MAX_BYTES`, `CONTENT_HOOK_TIMEOUT` | 1 GiB, 5m | |
| `RESCAN_BATCH_SIZE` | 20 | Per cycle; 0 turns re-scans off |

Archive limits (`ARCHIVE_MAX_ENTRIES`, `ARCHIVE_MAX_UNCOMPRESSED_BYTES`, `ARCHIVE_MAX_RATIO`) also bound the content inspector's zip, tar and gzip reading.

## Not covered

- Secrets inside archives and compressed text are not scanned.
- PDF streams with filters other than Flate are not decompressed.
- Blocking does not delete the bytes: retention, legal holds and Object Lock still decide that, through the asset's normal deletion.
