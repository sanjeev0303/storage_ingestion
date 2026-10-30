# Retention, holds and governance

What the platform does beyond a retention period and a legal hold (docs/lifecycle.md): COMPLIANCE locks, retention counted from a record's own date, holds by query, proof that locks took effect, two-person approvals, governance profiles, bulk deletes with a dry run, lock tamper detection and signed disposition certificates. Migration 000030.

## Retention policies

`PUT /api/v1/retention/policies` takes, besides the durations:

| Field | |
| --- | --- |
| `template` | Fills unset settings from a retention template (`GET /api/v1/retention/templates`) |
| `lock_mode` | `GOVERNANCE` (default) or `COMPLIANCE` for the Object Lock the policy sets |
| `retain_from_key` | A metadata date (`2026-01-31` or RFC 3339) retention counts from |
| `confirm_compliance` | Required to make a COMPLIANCE policy |
| `dry_run` | Answers the impact and stores nothing |

The answer carries `impact`: the available assets and bytes the scope covers, the lock mode, whether the change **loosens** the policy it replaces (a shorter minimum, COMPLIANCE dropped, the retain-from date dropped) and whether it needs approval.

- **COMPLIANCE wins:** if any policy in an asset's chain (tenant, dataset, asset) is COMPLIANCE, its lock is COMPLIANCE.
- **COMPLIANCE only gets stricter:** a change that loosens a COMPLIANCE policy is refused with 409 `compliance_locked`, approvals or not.
- **Guardrails:** a COMPLIANCE policy needs a minimum retention, `confirm_compliance`, and at most `RETENTION_COMPLIANCE_MAX` (default 87600h, ten years): nobody can delete what it locks before then, the bucket's root account included.
- **Mode changes at the provider:** S3 and MinIO refuse to change the mode of a lock still in force without the governance bypass, which the platform never sends. A version already locked in GOVERNANCE is extended in GOVERNANCE; COMPLIANCE applies to versions locked from now on and to locks that have run out.
- **Counting from a date:** retention ends at the later of creation and the metadata date, plus the minimum. A missing or earlier date never shortens it.

### Templates

Starting points with their citations; counsel confirms they fit.

| Id | Period | Mode | Counts from |
| --- | --- | --- | --- |
| `sec-17a-4` | 6 years | COMPLIANCE | creation |
| `cftc-1-31` | 5 years | GOVERNANCE | creation |
| `cms-imaging` | 5 years | GOVERNANCE | `majority_date` |
| `medicare-advantage` | 10 years | GOVERNANCE | creation |
| `hipaa-documentation` | 6 years | GOVERNANCE | creation |
| `ofcom-tv`, `ofcom-radio` | 90, 42 days | GOVERNANCE | creation |
| `nara-grs-6-1` | 7 years | GOVERNANCE | creation |
| `mrc-research`, `mrc-clinical` | 10, 20 years | GOVERNANCE | `study_end_date` |

## Legal holds

`POST /api/v1/legal-holds` also takes `scope: "QUERY"` with a `query` (`dataset`, `path_prefix`, `content_type_prefix`, `metadata` that must all match, `created_after`, `created_before`), and `matter` and `custodians`. The query is evaluated whenever an asset is checked (deletion, purge, lock sync), so a QUERY hold also covers assets created after it. Its matter names it.

`GET /api/v1/legal-holds/{id}/proof` lists what a hold covers (up to 1000 assets), with each one's Object Lock: when it was applied, the hold and retention set. With `?verify=true` it reads up to 100 locks back from the provider and says `MATCH` or what differs. Reading a proof is audited.

## Governance and approvals

`GET/PUT /api/v1/governance` (retention.manage):

| Setting | |
| --- | --- |
| `two_person` | Loosening a policy, releasing a hold, bulk deletes and loosening these settings wait for a second member |
| `approve_deletes` | Every asset deletion waits for approval |
| `profile` | `STANDARD`, `GXP_VAULT` (two-person and approved deletes: 21 CFR Part 11, EU Annex 11) or `SEC_17A4_AUDIT_TRAIL` (two-person and a six-year retention floor: the audit-trail alternative to WORM in Rule 17a-4(f)(2)) |

A change that waits answers **202** `{"status": "PENDING_APPROVAL", "approval_id": ...}` and is not made. `GET /api/v1/approvals` lists them (pending first; unexpired ones last seven days). `POST /api/v1/approvals/{id}/approve` carries the change out as asked; the member who asked cannot approve it (403 `self_approval`), and an approval is decided once (409). `POST .../reject` closes it.

Every attempt to loosen protection, made, refused or sent for approval, is the webhook `retention.change_requested`; a new approval is `governance.approval_requested`.

## Bulk deletes

`POST /api/v1/assets/bulk-delete` selects up to 10,000 assets by `asset_ids`, or by `dataset`, `path_prefix` and `metadata` together. With `dry_run` it answers the count, the bytes and, for up to 1000 of them, what would hold them back now (`legal_hold`, `retention`, `object_lock`, `quarantine_evidence`). Without it, each is soft-deleted as one deletion would be (held ones wait in PROTECTED), or the whole set waits for approval under the two-person rule.

## Blocked deletions

A deletion held back by a hold, retention or a lock waits in PROTECTED and is checked again every `LIFECYCLE_RECHECK_INTERVAL`. Entering PROTECTED, or a change of its reason, is the webhook `object.deletion_blocked`.

## Lock tamper detection

Each cleanup cycle reads back up to 50 locks still in force, those verified longest ago first, and compares them with what was applied. A legal hold that is off, a retention that ends earlier, or COMPLIANCE that became GOVERNANCE is recorded on the lock (`verify_result`), counted (`object_locks_tampered`), written to the lifecycle trail and sent as the webhook `object.lock_tampered`. The usual cause is someone with `s3:BypassGovernanceRetention` on the bucket. Deny it to every role with a bucket policy, so even GOVERNANCE locks hold:

```json
{
  "Version": "2012-10-17",
  "Statement": [{
    "Sid": "NoGovernanceBypass",
    "Effect": "Deny",
    "Principal": "*",
    "Action": ["s3:BypassGovernanceRetention", "s3:PutBucketObjectLockConfiguration"],
    "Resource": ["arn:aws:s3:::BUCKET", "arn:aws:s3:::BUCKET/*"]
  }]
}
```

Then only the bucket's root account can change the policy back, and doing so is visible in CloudTrail.

## Disposition certificates

When a deletion completes, the platform records a certificate first: the asset, its SHA-256 and size, its connection, key and provider version, who asked and why, when it was purged, and the statement that no hold, retention period or lock held it. `signed_body` is the exact JSON signed; `body_sha256` its SHA-256; `signature` an Ed25519 signature with the key in `CERTIFICATE_SIGNING_KEY` (a base64 32-byte seed; `CERTIFICATE_KEY_ID` names it). Without a key, certificates are stored unsigned with their hash.

- `GET /api/v1/deletions/{id}/certificate`, `GET /api/v1/assets/{id}/certificates` (assets.read)
- `GET /api/v1/certificates/public-key`: the key to verify with
- `osp certificate DELETION --verify` and the console (Operations, Governance) check the hash and the signature without trusting the platform's database.

## SDK and CLI

`osp.governance`: `templates`, `set_policy`, `holds`, `place_hold`, `release_hold`, `hold_proof`, `governance`, `set_governance`, `approvals`, `approve`, `reject`, `bulk_delete`, `certificate`, `public_key`, `verify_certificate`. CLI: `osp templates`, `osp holds`, `osp hold-proof ID --verify`, `osp approvals`, `osp approve ID`, `osp reject ID`, `osp bulk-delete --dataset ... [--yes]`, `osp certificate ID --verify`.

## Not covered

- Archive escrow (a copy to a second provider before disposition) is part of the copy engine.
- The platform cannot remove `s3:BypassGovernanceRetention` from roles in a customer's account; the connection posture checks report it.
