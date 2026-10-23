export type Mode = "METADATA" | "FULL" | "SAMPLED";
export type Severity = "ERROR" | "WARNING" | "INFO";
export type Scope = "DATASET" | "OBJECT_METADATA" | "OBJECT_CONTENT" | "SPLIT";
export type ReportStatus = "READY" | "READY_WITH_WARNINGS" | "FAILED";
export type CheckStatus = "PASSED" | "FAILED" | "PARTIAL" | "NOT_ASSESSED";
export type DimensionStatus = "PASS" | "WARN" | "FAIL" | "NOT_ASSESSED";
export type Coverage = "NONE" | "SAMPLED" | "FULL";
export type DimensionCoverage = "FULL" | "SAMPLED" | "PARTIAL" | "NONE";
export type JobState = "QUEUED" | "RUNNING" | "COMPLETED" | "COMPLETED_WITH_WARNINGS" | "FAILED" | "CANCELLED";

export const DIMENSIONS = [
  "COMPLETENESS",
  "INTEGRITY",
  "SECURITY",
  "CONSISTENCY",
  "DUPLICATION",
  "DISTRIBUTION",
  "REPRODUCIBILITY",
  "GOVERNANCE",
  "ACCESSIBILITY",
] as const;
export type Dimension = (typeof DIMENSIONS)[number];

export type Rule = {
  id: string;
  version: number;
  type: string;
  severity: Severity;
  enabled: boolean;
  scope: Scope;
  config?: Record<string, unknown>;
};

export type Thresholds = { max_errors: number; max_warnings?: number };

export type ProfileDefinition = { kind: string; rules: Rule[]; thresholds: Thresholds };

export type ValidationProfile = {
  id: string;
  version: number;
  name: string;
  description: string;
  built_in: boolean;
  definition: ProfileDefinition;
  created_at?: string;
};

export type ProfileList = { profiles: ValidationProfile[]; rule_types: string[] };

export type CreateProfileRequest = {
  name: string;
  description?: string;
  rules: {
    id: string;
    version: number;
    type: string;
    severity: Severity;
    enabled: boolean;
    scope?: Scope;
    config?: Record<string, unknown>;
  }[];
  thresholds: Thresholds;
};

export type ValidationRequest = {
  version_id?: string;
  profile_id?: string;
  profile_version?: number;
  mode: Mode;
  sample_size?: number;
  idempotency_key: string;
};

export type JobProgress = {
  total: number;
  processed: number;
  passed: number;
  warnings: number;
  failed: number;
  content_examined: number;
  content_not_examined: number;
  bytes_total: number;
  bytes_read: number;
};

export type ValidationJob = {
  id: string;
  dataset_id: string;
  version_id: string;
  profile_id: string;
  profile_version: number;
  mode: Mode;
  sample_size?: number;
  readiness: boolean;
  state: JobState;
  progress: JobProgress;
  attempts: number;
  max_attempts: number;
  cancel_requested: boolean;
  last_error?: string;
  report_id?: string;
  created_by: string;
  created_at: string;
  started_at?: string;
  completed_at?: string;
};

export type Count = { value: string; count: number };

export type CheckResult = {
  rule_id: string;
  rule_version: number;
  type: string;
  severity: Severity;
  scope: Scope;
  dimension: Dimension;
  status: CheckStatus;
  checked: number;
  failed: number;
  not_assessed: number;
  read?: number;
  findings_stored: number;
  note?: string;
};

export type DimensionResult = {
  dimension: Dimension;
  status: DimensionStatus;
  checks: string[] | null;
  coverage: DimensionCoverage;
  notes?: string[];
};

export type ReadinessReport = { profile: string; dimensions: DimensionResult[]; disclaimer: string };

export type ReportBody = {
  schema_version: number;
  report_id: string;
  job_id: string;
  dataset_id: string;
  dataset_version_id: string;
  dataset_version: number;
  manifest_sha256: string;
  profile: { id: string; version: number; kind: string };
  thresholds: Thresholds;
  mode: Mode;
  sample_size?: number;
  status: ReportStatus;
  started_at: string;
  completed_at: string;
  members: { total: number; examined: number; passed: number; warnings: number; failed: number };
  content: {
    coverage: Coverage;
    examined: number;
    read: number;
    not_examined: number;
    bytes_represented: number;
    bytes_read: number;
    note: string;
  };
  checks: { passed: number; failed: number; partial: number; not_assessed: number };
  results: CheckResult[];
  findings: { by_severity: Record<string, number>; by_rule: Record<string, number>; stored: number };
  evidence: {
    verified_checksums: number;
    checksum_verification: Count[];
    malware_scans: Count[];
    version_states: Count[];
  };
  distributions: {
    splits: Count[];
    labels: Count[];
    distinct_labels: number;
    unlabeled: number;
    content_types: Count[];
    extensions: Count[];
    sizes: { total_bytes: number; min_bytes: number; max_bytes: number };
  };
  readiness?: ReadinessReport;
  summary: string;
};

export type ValidationReport = {
  id: string;
  job_id: string;
  dataset_id: string;
  version_id: string;
  profile_id: string;
  profile_version: number;
  mode: Mode;
  status: ReportStatus;
  readiness: boolean;
  schema_version: number;
  summary: string;
  started_at: string;
  completed_at: string;
  report?: ReportBody;
};

export type Finding = {
  id: string;
  rule_id: string;
  rule_version: number;
  severity: Severity;
  code: string;
  object_id?: string;
  version_id?: string;
  message: string;
  expected?: string;
  observed?: string;
  evidence: string;
  remediation?: string;
  created_at: string;
};

export type FindingFilters = { severity: Severity | ""; rule_id: string };

// Only the dataset fields these screens read.
export type DatasetSummary = {
  id: string;
  name: string;
  lifecycle_status: string;
  current_version_id?: string;
  profile_id?: string;
  profile_version?: number;
  latest_report_id?: string;
};

export type DatasetVersionSummary = {
  id: string;
  version: number;
  validation_status: string;
  member_count: number;
  created_at: string;
};
