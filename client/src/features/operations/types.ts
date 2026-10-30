// Mirrors docs/api/identity-admin-audit.md sections 6.1, 7 and 8, and storage connections.

export type Classification = "PUBLIC" | "INTERNAL" | "CONFIDENTIAL" | "RESTRICTED";

export type AssetStatus = "QUARANTINED" | "PROCESSING" | "AVAILABLE" | "REJECTED" | "SOFT_DELETED";

export type Asset = {
  id: string;
  file_name: string;
  content_type: string;
  size_bytes: number;
  checksum_sha256?: string;
  dataset?: string;
  status: AssetStatus;
  rejection_reason?: string;
  // Set when a re-scan with newer signatures found the published bytes unsafe.
  blocked_reason?: string;
  // Set by a member, or raised by content findings (docs/access.md).
  classification?: Classification;
  deliverable: boolean;
  etag?: string;
  published_at?: string;
  created_at: string;
  updated_at: string;
};

export type DeletionState =
  | "SCHEDULED"
  | "IN_PROGRESS"
  | "RETRY_WAIT"
  | "STORAGE_DELETED"
  | "FAILED"
  | "COMPLETED"
  | "CANCELLED";

export type Deletion = { id: string; state: DeletionState; purge_after: string; restorable: boolean };

export type DeleteAssetResponse = {
  asset_id: string;
  status: "SOFT_DELETED";
  deletion: Deletion;
  created: boolean;
};

export type RestoreAssetResponse = { asset_id: string; status: AssetStatus };

export type AssetLifecycle = {
  asset_id: string;
  status: AssetStatus;
  dataset?: string;
  retain_until: string;
  expires_at?: string;
  deletion_grace_seconds: number;
  deletion?: Deletion;
  // On a bucket with S3 Object Lock, the provider refuses to delete the
  // stored version before this.
  locked_until?: string;
};

export type QuarantineEntry = {
  id: string;
  category: string;
  reason: string;
  scan_result: string;
  status: "HELD" | "RELEASED" | "REJECTED" | "PURGED";
  created_at: string;
  retain_until: string | null;
};

export type ProcessingRecord = {
  asset_id: string;
  asset_status: AssetStatus;
  job_state: "PENDING" | "SUCCEEDED" | "FAILED" | "EXHAUSTED";
  job_stage: "VERIFY" | "SCAN" | "PROMOTE" | "CLEANUP";
  attempts: number;
  last_error?: string;
  quarantine: QuarantineEntry[];
  // The malware scan's verdict. reused_from names the asset with the same
  // bytes whose clean verdict, from the same engine version, was reused.
  scan?: { scanner: string; result: string; scanner_version?: string; reused_from?: string; scanned_at?: string };
  events: { event: string; detail: string; actor: string; at: string }[];
};

export type ReleaseQuarantineResponse = { asset_id: string; job_state: string; stage: string };

export type RetentionScope = "TENANT" | "DATASET" | "ASSET";
export type HoldScope = RetentionScope | "QUERY";
export type LockMode = "GOVERNANCE" | "COMPLIANCE";

export type RetentionPolicyRequest = {
  scope: RetentionScope;
  scope_key?: string;
  min_retention?: string;
  max_age?: string;
  deletion_grace?: string;
  quarantine_retention?: string;
  reason: string;
  lock_mode?: LockMode;
  retain_from_key?: string;
  template?: string;
  confirm_compliance?: boolean;
  dry_run?: boolean;
};

export type PolicyImpact = {
  assets: number;
  bytes: number;
  lock_mode: LockMode;
  loosens: boolean;
  needs_approval: boolean;
  notes: string[] | null;
};

export type RetentionPolicyResponse = {
  id: string;
  scope: string;
  scope_key: string;
  version: number;
  effective_from: string;
  lock_mode?: string;
  impact?: PolicyImpact;
};

// A change that waits for a second member answers 202 with this.
export type PendingApproval = { status: "PENDING_APPROVAL"; approval_id: string; message: string };

export type RetentionTemplate = {
  id: string;
  name: string;
  citation: string;
  min_retention: string;
  retain_from_key?: string;
  lock_mode: LockMode;
  note: string;
};

export type HoldQuery = {
  dataset?: string;
  path_prefix?: string;
  content_type_prefix?: string;
  metadata?: Record<string, string>;
  created_after?: string;
  created_before?: string;
};

export type LegalHoldRequest = {
  scope: HoldScope;
  scope_key?: string;
  reason: string;
  query?: HoldQuery;
  matter?: string;
  custodians?: string[];
};
export type LegalHold = { id: string; scope: string; scope_key: string; placed_at: string };

export type DeadLetterStatus = "OPEN" | "RETRIED" | "REPLAYED" | "RESOLVED" | "DISCARDED";

export type DeadLetter = {
  id: string;
  event_id: string;
  event_type?: string;
  tenant_id?: string;
  consumer?: string;
  status: DeadLetterStatus;
  category?: string;
  last_error?: string;
  created_at: string;
  replay_event_id?: string;
  stale?: string;
};

export type DeadLetterAction = "retry" | "replay" | "resolve" | "discard";

export type StorageConnection = {
  id: string;
  name: string;
  provider: string;
  status: string;
  is_default: boolean;
  last_tested_at?: string;
  last_test_ok_at?: string;
  last_error_category?: string;
  capabilities_checked_at?: string;
};

export type QuotaReconcileResponse = {
  tenant_id: string;
  settled_reservations: number;
  repaired: boolean;
  reserved_bytes_before: number;
  reserved_bytes_after: number;
  stored_bytes_before: number;
  stored_bytes_after: number;
};

export type PolicyInEffect = {
  id: string;
  lock_mode?: string;
  retain_from_key?: string;
  template?: string;
  scope: RetentionScope;
  scope_key: string;
  version: number;
  min_retention: string | null;
  max_age: string | null;
  deletion_grace: string | null;
  quarantine_retention: string | null;
  effective_from: string;
  created_by: string;
  reason: string;
  created_at: string;
};

export type HeldHold = {
  id: string;
  query?: HoldQuery | null;
  matter?: string;
  custodians?: string[];
  scope: HoldScope;
  scope_key: string;
  reason: string;
  placed_by: string;
  placed_at: string;
  released_by?: string;
  released_at?: string;
};
