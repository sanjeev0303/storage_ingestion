export type LifecycleStatus = "DRAFT" | "VALIDATING" | "READY" | "READY_WITH_WARNINGS" | "FAILED" | "ARCHIVED";
export type CompletionPolicy = "ALL" | "REQUIRED" | "PARTIAL";
export type DuplicatePolicy = "REJECT" | "SKIP" | "REPLACE";
export type Split = "train" | "validation" | "test";
export type ValidationStatus = "NOT_VALIDATED" | "READY" | "READY_WITH_WARNINGS" | "FAILED";

export type Transformation = { name: string; version: string; config_sha256?: string };

export type Derivation = {
  version_ids: string[];
  splits?: string[];
  labels?: string[];
  transformation?: Transformation;
};

export type Dataset = {
  id: string;
  name: string;
  completion_policy: CompletionPolicy;
  // Legacy bulk-ingestion status. The managed state is lifecycle_status.
  status: string;
  lifecycle_status: LifecycleStatus;
  max_bytes: number;
  reserved_bytes: number;
  stored_bytes: number;
  created_at: string;
  updated_at: string;
  ready_at?: string;
  description: string;
  tags: string[];
  metadata: Record<string, string>;
  revision: number;
  member_count: number;
  current_version_id?: string;
  profile_id?: string;
  profile_version?: number;
  latest_report_id?: string;
  derived_from?: Derivation;
  created_by: string;
  updated_by?: string;
  archived_at?: string;
  storage_connection_id?: string;
  branch?: BranchInfo;
};

export type BranchInfo = {
  parent_id: string;
  name: string;
  fork_version_id: string;
  // The last version merged between the branch and its parent, in either direction.
  merge_base_version_id: string;
};

export type DatasetPage = { datasets: Dataset[]; next_cursor?: string };

export type CreateDatasetRequest = {
  name: string;
  description?: string;
  tags?: string[];
  metadata?: Record<string, string>;
  completion_policy?: CompletionPolicy;
  max_bytes?: number;
  profile_id?: string;
  profile_version?: number;
  derived_from?: {
    version_ids: string[];
    splits?: Split[];
    labels?: string[];
    transformation?: { name: string; version?: string };
  };
};

export type CreateDatasetResult = { dataset: Dataset; created: boolean };

export type UpdateDatasetRequest = {
  revision: number;
  description?: string;
  tags?: string[];
  metadata?: Record<string, string | null>;
  profile_id?: string;
  profile_version?: number;
  storage_connection_id?: string;
};

export type DatasetMember = {
  id: string;
  object_id: string;
  namespace: string;
  path: string;
  version_id?: string;
  label?: string;
  split?: Split;
  metadata: Record<string, string>;
  source: string;
  added_by: string;
  created_at: string;
  updated_at: string;
};

export type MemberPage = { members: DatasetMember[]; next_cursor?: string };

export type MemberRequest = {
  object_id?: string;
  namespace?: string;
  path?: string;
  version_id?: string;
  version?: number;
  label?: string;
  split?: Split;
  metadata?: Record<string, string>;
};

export type MemberOutcome = "ADDED" | "UPDATED" | "SKIPPED" | "REJECTED";

export type MemberResult = {
  index: number;
  outcome: MemberOutcome;
  object_id?: string;
  version_id?: string;
  code?: string;
  error?: string;
};

export type AddMembersResponse = {
  added: number;
  updated: number;
  skipped: number;
  rejected: number;
  results: MemberResult[];
  dataset: Dataset;
};

export type DatasetVersion = {
  id: string;
  dataset_id: string;
  version: number;
  status: "FINALIZED";
  validation_status: ValidationStatus;
  member_count: number;
  total_bytes: number;
  manifest_sha256: string;
  manifest_format: string;
  profile_id?: string;
  profile_version?: number;
  config: Record<string, unknown>;
  parent_version_id?: string;
  derived_from?: Derivation;
  latest_report_id?: string;
  created_by: string;
  created_at: string;
};

export type VersionPage = { versions: DatasetVersion[]; next_before?: number };

export type VersionMember = {
  ordinal: number;
  object_id: string;
  version_id: string;
  object_version: number;
  path: string;
  label?: string;
  split?: Split;
  size_bytes: number;
  content_type: string;
  checksum_sha256?: string;
  metadata: Record<string, string>;
  // Whether the pinned bytes still exist. A member's record outlives an
  // erasure; its bytes do not.
  availability: "AVAILABLE" | "DELETION_PENDING" | "ERASED";
  erased_at?: string;
};

export type DiffKind = "ADDED" | "REMOVED" | "CHANGED" | "RELABELED";

export type DiffSide = { path: string; checksum_sha256?: string; label?: string; split?: string };

export type VersionDifference = { object_id: string; kind: DiffKind; base?: DiffSide; target?: DiffSide };

export type VersionDiff = {
  summary: { added: number; removed: number; changed: number; relabeled: number; unchanged: number };
  differences: VersionDifference[];
  next_after?: string;
};

export type VersionMemberPage = { members: VersionMember[]; next_after?: number };

export type ImportState = "AWAITING_UPLOAD" | "QUEUED" | "RUNNING" | "COMPLETED" | "FAILED" | "CANCELLED";

export type ManifestImport = {
  id: string;
  dataset_id: string;
  state: ImportState;
  source: "INLINE" | "STAGED";
  duplicate_policy: DuplicatePolicy;
  manifest_bytes: number;
  manifest_sha256?: string;
  progress: {
    lines_read: number;
    members_added: number;
    members_updated: number;
    members_skipped: number;
    lines_rejected: number;
  };
  attempts: number;
  cancel_requested: boolean;
  last_error?: string;
  upload_url?: string;
  upload_expires_at?: string;
  created_at: string;
  started_at?: string;
  completed_at?: string;
};

export type ImportError = { line: number; code: string; detail: string };
export type ImportErrorPage = { errors: ImportError[]; next_after?: number };

export type CatalogObjectRef = {
  id: string;
  dataset: string;
  path: string;
  current_version?: { version: number; asset_id: string; size_bytes: number; content_type: string };
  versions: number;
};

export type CatalogObjectPage = { objects: CatalogObjectRef[]; next_cursor?: string };

export type ValidationProfileRef = { id: string; version: number; name: string; description: string; built_in: boolean };

export const TERMINAL_IMPORT_STATES: ImportState[] = ["COMPLETED", "FAILED", "CANCELLED"];

export type MergeStrategy = "FAIL" | "SOURCE" | "TARGET";

// One side of a conflicting object: its membership in the base, the source or the target.
export type MergeSide = {
  present: boolean;
  version_id?: string;
  label?: string;
  split?: string;
  metadata?: Record<string, string>;
};

export type MergeConflict = {
  object_id: string;
  namespace: string;
  path: string;
  base: MergeSide;
  source: MergeSide;
  target: MergeSide;
};

export type MergeResult = {
  id?: string;
  target_dataset_id: string;
  source_dataset_id: string;
  source_version_id: string;
  base_version_id: string;
  strategy: MergeStrategy;
  applied: boolean;
  added: number;
  updated: number;
  removed: number;
  already_same: number;
  conflicts: number;
  conflict_list: MergeConflict[];
  created_by?: string;
  created_at?: string;
};

export type MergeRequest = {
  source_dataset_id: string;
  source_version_id?: string;
  strategy: MergeStrategy;
  dry_run: boolean;
  idempotency_key?: string;
};
