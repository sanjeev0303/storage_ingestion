import { api } from "@/lib/api/client";

// Mirrors the data quality routes (docs/quality.md).

export type ValueCount = { value: string; count: number };
export type ColumnKind = "number" | "string" | "bool" | "time" | "nested" | "mixed" | "empty";

export type ColumnProfile = {
  name: string;
  kind: ColumnKind;
  count: number;
  nulls: number;
  distinct: number;
  distinct_capped?: boolean;
  min?: number;
  max?: number;
  mean?: number;
  stddev?: number;
  top?: ValueCount[];
};

export type TableProfile = { table: string; format: string; files: number; rows: number; partial?: boolean; columns: ColumnProfile[] };

export type ColumnDrift = {
  name: string;
  kind_before: string;
  kind_after: string;
  psi?: number;
  null_rate_before: number;
  null_rate_after: number;
  not_compared?: string;
  breached: string[];
};

export type TableDrift = {
  table: string;
  rows_before: number;
  rows_after: number;
  row_change: number;
  columns_added: string[];
  columns_removed: string[];
  columns: ColumnDrift[];
  breached: string[];
};

export type DriftReport = {
  baseline_version_id: string;
  version_id: string;
  thresholds: { max_psi: number; max_null_change: number; max_row_change: number };
  tables: TableDrift[];
  breaches: number;
  not_compared: string[];
};

export type ProfileState = "PENDING" | "RUNNING" | "DONE" | "FAILED" | "NONE";

export type VersionProfile = {
  version_id: string;
  state: ProfileState;
  tables: TableProfile[];
  skipped: { path: string; reason: string }[];
  files_profiled?: number;
  bytes_read?: number;
  other_files?: number;
  baseline_version_id?: string;
  drift?: DriftReport;
  drifted: boolean;
  error?: string;
  finished_at?: string;
};

export type QualitySettings = {
  profile_snapshots: boolean;
  max_psi: number;
  max_null_change: number;
  max_row_change: number;
  subject_key: string;
  subject_pattern: string;
  updated_at?: string;
};

export type LeakageExample = { eval_path: string; train_path?: string; key: string };
export type LeakageCheck = {
  id: string;
  run_id?: string;
  train_version_id: string;
  eval_version_id: string;
  method: "EXACT_HASH" | "SUBJECT" | "NGRAM";
  state: "CLEAN" | "LEAKED" | "NOT_CHECKED";
  overlap: number;
  eval_members: number;
  examples: LeakageExample[];
  detail: string;
  created_at: string;
};
export type LeakageReport = { train_version_id: string; eval_version_id: string; checks: LeakageCheck[]; not_checked: string[] };

export type Release = { label: string; version_id: string; version: number; notes: string; members: number; bytes: number; released_at: string };
export type ReleaseDiff = {
  release: Release;
  previous: Release;
  drift?: DriftReport;
  drift_note?: string;
  members: { added: number; removed: number; changed: number; relabeled: number; unchanged: number; first: { kind: string; path: string }[] };
};

const ds = (id: string) => `/datasets/${encodeURIComponent(id)}`;
const ver = (id: string, versionId: string) => `${ds(id)}/versions/${encodeURIComponent(versionId)}`;

export const getSettings = (id: string, signal?: AbortSignal) => api<QualitySettings>(`${ds(id)}/quality-settings`, { signal });
export const putSettings = (id: string, body: QualitySettings) => api<QualitySettings>(`${ds(id)}/quality-settings`, { method: "PUT", body });

export const getProfile = (id: string, versionId: string, signal?: AbortSignal) => api<VersionProfile>(`${ver(id, versionId)}/profile`, { signal });
export const reprofile = (id: string, versionId: string) => api<unknown>(`${ver(id, versionId)}/profile`, { method: "POST" });

export const checkLeakage = (id: string, versionId: string, trainVersionId: string) =>
  api<LeakageReport>(`${ver(id, versionId)}/leakage`, { method: "POST", body: { train_version_id: trainVersionId } });
export const runLeakage = (runId: string, signal?: AbortSignal) =>
  api<{ checks: LeakageCheck[] }>(`/training-runs/${encodeURIComponent(runId)}/leakage`, { signal });

export const listReleases = (id: string, signal?: AbortSignal) => api<{ releases: Release[] }>(`${ds(id)}/releases`, { signal });
export const release = (id: string, versionId: string, label: string, notes: string) =>
  api<Release>(`${ver(id, versionId)}/release`, { method: "PUT", body: { label, notes } });
export const releaseDiff = (id: string, label: string, signal?: AbortSignal) =>
  api<ReleaseDiff>(`${ds(id)}/releases/${encodeURIComponent(label)}/diff`, { signal });
