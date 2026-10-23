import type { DatasetSummary, ReportBody, ValidationJob, ValidationProfile, ValidationReport } from "./types";

// Shapes used by the tests; they follow docs/api/catalog-datasets-validation-lineage.md.
export const dataset: DatasetSummary = {
  id: "ds-1",
  name: "pets",
  lifecycle_status: "DRAFT",
  current_version_id: "ver-2",
};

export const generalProfile: ValidationProfile = {
  id: "builtin:general",
  version: 1,
  name: "General",
  description: "",
  built_in: true,
  definition: { kind: "general", rules: [], thresholds: { max_errors: 0 } },
};

export const profileList = {
  profiles: [generalProfile],
  rule_types: ["required_metadata", "member_count"],
};

export const versions = {
  versions: [
    { id: "ver-2", version: 2, validation_status: "NOT_VALIDATED", member_count: 6, created_at: "2026-10-01T10:00:00Z" },
    { id: "ver-1", version: 1, validation_status: "READY", member_count: 4, created_at: "2026-09-01T10:00:00Z" },
  ],
};

export function job(extra: Partial<ValidationJob> = {}): ValidationJob {
  return {
    id: "job-1",
    dataset_id: "ds-1",
    version_id: "ver-2",
    profile_id: "builtin:general",
    profile_version: 1,
    mode: "METADATA",
    readiness: false,
    state: "RUNNING",
    progress: {
      total: 6,
      processed: 3,
      passed: 3,
      warnings: 0,
      failed: 0,
      content_examined: 0,
      content_not_examined: 0,
      bytes_total: 1000,
      bytes_read: 0,
    },
    attempts: 1,
    max_attempts: 3,
    cancel_requested: false,
    created_by: "user-1",
    created_at: "2026-10-22T10:00:00Z",
    ...extra,
  };
}

export function reportBody(extra: Partial<ReportBody> = {}): ReportBody {
  return {
    schema_version: 1,
    report_id: "rep-1",
    job_id: "job-1",
    dataset_id: "ds-1",
    dataset_version_id: "ver-2",
    dataset_version: 2,
    manifest_sha256: "abc123",
    profile: { id: "builtin:general", version: 1, kind: "general" },
    thresholds: { max_errors: 0 },
    mode: "SAMPLED",
    sample_size: 3,
    status: "READY_WITH_WARNINGS",
    started_at: "2026-10-22T10:00:00Z",
    completed_at: "2026-10-22T10:01:00Z",
    members: { total: 6, examined: 6, passed: 4, warnings: 2, failed: 0 },
    content: {
      coverage: "SAMPLED",
      examined: 3,
      read: 1,
      not_examined: 3,
      bytes_represented: 500,
      bytes_read: 100,
      note: "this is not a full integrity scan",
    },
    checks: { passed: 1, failed: 1, partial: 1, not_assessed: 1 },
    results: [
      { rule_id: "meta", rule_version: 1, type: "required_metadata", severity: "WARNING", scope: "OBJECT_METADATA", dimension: "COMPLETENESS", status: "FAILED", checked: 6, failed: 2, not_assessed: 0, findings_stored: 1 },
      { rule_id: "content", rule_version: 1, type: "content_checksum", severity: "ERROR", scope: "OBJECT_CONTENT", dimension: "INTEGRITY", status: "PARTIAL", checked: 3, failed: 0, not_assessed: 3, read: 1, findings_stored: 0 },
      { rule_id: "scan", rule_version: 1, type: "malware_scan", severity: "ERROR", scope: "OBJECT_METADATA", dimension: "SECURITY", status: "NOT_ASSESSED", checked: 0, failed: 0, not_assessed: 6, findings_stored: 0 },
      { rule_id: "count", rule_version: 1, type: "member_count", severity: "ERROR", scope: "DATASET", dimension: "COMPLETENESS", status: "PASSED", checked: 6, failed: 0, not_assessed: 0, findings_stored: 0 },
    ],
    findings: { by_severity: { WARNING: 2 }, by_rule: { meta: 2 }, stored: 1 },
    evidence: { verified_checksums: 4, checksum_verification: [{ value: "PASSED", count: 4 }], malware_scans: [], version_states: [{ value: "AVAILABLE", count: 6 }] },
    distributions: {
      splits: [{ value: "train", count: 4 }],
      labels: [{ value: "cat", count: 3 }],
      distinct_labels: 1,
      unlabeled: 3,
      content_types: [{ value: "text/plain", count: 6 }],
      extensions: [{ value: ".txt", count: 6 }],
      sizes: { total_bytes: 1000, min_bytes: 100, max_bytes: 300 },
    },
    summary: "summary",
    ...extra,
  };
}

export function report(body: ReportBody = reportBody(), extra: Partial<ValidationReport> = {}): ValidationReport {
  return {
    id: "rep-1",
    job_id: "job-1",
    dataset_id: "ds-1",
    version_id: "ver-2",
    profile_id: "builtin:general",
    profile_version: 1,
    mode: body.mode,
    status: body.status,
    readiness: false,
    schema_version: 1,
    summary: "One validation finished.",
    started_at: body.started_at,
    completed_at: body.completed_at,
    report: body,
    ...extra,
  };
}

export function finding(n: number, extra: object = {}) {
  return {
    id: `f-${n}`,
    rule_id: "meta",
    rule_version: 1,
    severity: "WARNING",
    code: "MISSING_METADATA",
    object_id: `obj-${n}`,
    message: `Member ${n} lacks license`,
    evidence: "catalog",
    remediation: "Add a license key",
    created_at: "2026-10-22T10:00:30Z",
    ...extra,
  };
}
