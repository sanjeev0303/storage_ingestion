import { api } from "@/lib/api/client";

// Mirrors the copy engine's routes (docs/copy-engine.md).

export type CopyJobKind = "MIGRATE" | "RESTORE" | "EXPORT" | "DRILL";
export type CopyJobState = "QUEUED" | "RUNNING" | "COMPLETED" | "FAILED" | "CANCELLED";

export type CopyJob = {
  id: string;
  kind: CopyJobKind;
  dataset_id?: string;
  version_id?: string;
  target_connection_id?: string;
  target_prefix?: string;
  state: CopyJobState;
  total_items: number;
  done_items: number;
  failed_items: number;
  total_bytes: number;
  done_bytes: number;
  report: Record<string, unknown>;
  error?: string;
  created_by: string;
  created_at: string;
  finished_at?: string;
};

export type Replication = {
  connection_id: string;
  vault_retention_days: number;
  escrow: boolean;
  scrub_days: number;
  objects: number;
  replicated: number;
  pending: number;
  failing: number;
  replicated_bytes: number;
  newest_replica_at?: string;
  oldest_pending_since?: string;
  recovery_point_seconds: number;
  last_error?: string;
};

export type MigrationPlan = {
  objects: number;
  bytes: number;
  already_there: number;
  egress_estimate_usd: number;
  egress_note: string;
};

export type ScrubResult = { asset_id: string; path?: string; checked_at: string; ok: boolean; detail?: string; repaired: boolean };

export type CleanChange = {
  path: string;
  change: "ADDED" | "REMOVED" | "CHANGED";
  asset_id?: string;
  uploaded_by?: string;
  uploaded_at?: string;
};

export type ExportSchedule = { connection_id?: string; interval_hours: number; last_run_at?: string; last_job_id?: string };

const ds = (id: string) => `/datasets/${encodeURIComponent(id)}`;

export const getReplication = (id: string, signal?: AbortSignal) => api<Replication>(`${ds(id)}/replication`, { signal });
export const listScrubs = (id: string, signal?: AbortSignal) => api<{ scrubs: ScrubResult[] }>(`${ds(id)}/scrubs`, { signal });
export const listJobs = (datasetId: string, signal?: AbortSignal) =>
  api<{ jobs: CopyJob[] }>("/copy-jobs", { query: { dataset_id: datasetId || undefined }, signal });
export const cancelJob = (id: string) => api<void>(`/copy-jobs/${encodeURIComponent(id)}/cancel`, { method: "POST" });

export const planMigration = (id: string, target: string) =>
  api<{ plan: MigrationPlan }>(`${ds(id)}/migrations`, { method: "POST", body: { target_connection_id: target, dry_run: true } });
export const startMigration = (id: string, target: string) =>
  api<{ plan: MigrationPlan; job: CopyJob }>(`${ds(id)}/migrations`, { method: "POST", body: { target_connection_id: target } });
export const startRestore = (id: string, body: { version_id: string; target_connection_id: string; prefix: string }) =>
  api<CopyJob>(`${ds(id)}/restores`, { method: "POST", body });
export const startDrill = (id: string, body: { sample: number; from_replica: boolean }) =>
  api<CopyJob>(`${ds(id)}/drills`, { method: "POST", body });

export const setCleanPoint = (id: string, versionId: string) =>
  api<void>(`${ds(id)}/clean-point`, { method: "PUT", body: { version_id: versionId } });
export const cleanDiff = (id: string, signal?: AbortSignal) =>
  api<{ clean_version_id: string; changes: CleanChange[] }>(`${ds(id)}/clean-point/diff`, { signal });

export const startExport = (target: string) => api<CopyJob>("/exports", { method: "POST", body: { target_connection_id: target } });
export const getSchedule = (signal?: AbortSignal) => api<ExportSchedule>("/export-schedule", { signal });
export const putSchedule = (body: { connection_id: string; interval_hours: number }) =>
  api<ExportSchedule>("/export-schedule", { method: "PUT", body });
