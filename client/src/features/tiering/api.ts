import { api } from "@/lib/api/client";

// Mirrors the tiering and cost routes (docs/tiering.md).

export type Tier = "STANDARD" | "INFREQUENT" | "ARCHIVE" | "DEEP_ARCHIVE";
export type Speed = "FAST" | "STANDARD" | "BULK";

export type TierRule = {
  id: string;
  dataset_id?: string;
  name: string;
  tier: Tier;
  min_age_days: number;
  unread_days: number;
  enabled: boolean;
  created_at: string;
};

export type TierMove = {
  id: string;
  asset_id: string;
  from_tier: Tier;
  to_tier: Tier;
  size_bytes: number;
  state: "MOVED" | "FAILED";
  reason?: string;
  created_at: string;
};

export type Estimate = {
  operation: string;
  requests: number;
  bytes: number;
  storage_usd: number;
  request_usd: number;
  egress_usd: number;
  retrieval_usd: number;
  total_usd: number;
  hours?: number;
  note: string;
};

export type StoredLine = { connection_id: string; provider: string; tier: Tier; objects: number; bytes: number; monthly_usd: number };

export type Budget = { monthly_usd: number; monthly_requests: number; action: "ALERT" | "BLOCK"; updated_at?: string };

export type CostReport = {
  month: string;
  stored: StoredLine[];
  storage_usd: number;
  reads: number;
  egress_bytes: number;
  writes: number;
  restored_bytes: number;
  request_usd: number;
  egress_usd: number;
  restore_usd: number;
  total_usd: number;
  budget?: Budget;
  budget_used?: number;
  savings: {
    packed_files: number;
    packs: number;
    reads_saved_per_pass: number;
    shared_copies: number;
    shared_bytes: number;
    dedup_usd_month: number;
    tiering_usd_month: number;
  };
  note: string;
};

export type VersionEstimate = {
  files: number;
  bytes: number;
  packed_files: number;
  packs: number;
  download: Estimate;
  epoch: Estimate;
  archived_files: number;
  archived_bytes: number;
  restore: Estimate;
};

export type DatasetCosts = { stored: StoredLine[]; monthly_usd: number; years: number; retain_usd: number; archived_usd: number; note: string };

export type ExpiringVersion = { id: string; version: number; members: number; bytes: number; created_at: string; kept?: string };
export type Expiry = {
  expire: ExpiringVersion[];
  keep: ExpiringVersion[];
  freed_objects: number;
  freed_bytes: number;
  freed_usd_month: number;
  dry_run: boolean;
  deletions_requested: number;
  refused?: Record<string, string>;
};

const ds = (id: string) => `/datasets/${encodeURIComponent(id)}`;

export const listRules = (signal?: AbortSignal) => api<{ rules: TierRule[] }>("/tier-rules", { signal });
export const createRule = (body: { name: string; tier: Tier; min_age_days: number; unread_days: number; dataset_id?: string }) =>
  api<TierRule>("/tier-rules", { method: "POST", body });
export const deleteRule = (id: string) => api<void>(`/tier-rules/${encodeURIComponent(id)}`, { method: "DELETE" });
export const listMoves = (signal?: AbortSignal) => api<{ moves: TierMove[] }>("/tier-moves", { signal });

export const getReport = (signal?: AbortSignal) => api<CostReport>("/cost-report", { signal });
export const getBudget = (signal?: AbortSignal) => api<Budget>("/cost-budget", { signal });
export const putBudget = (body: Budget) => api<Budget>("/cost-budget", { method: "PUT", body });

export const getVersionEstimate = (id: string, versionId: string, signal?: AbortSignal) =>
  api<VersionEstimate>(`${ds(id)}/versions/${encodeURIComponent(versionId)}/cost-estimate`, { signal });
export const restoreVersion = (id: string, versionId: string, body: { days: number; speed: Speed }) =>
  api<{ archived: number; requested: number; already_open: number; readable: number; estimate: Estimate }>(
    `${ds(id)}/versions/${encodeURIComponent(versionId)}/archive-restores`,
    { method: "POST", body },
  );
export const getDatasetCosts = (id: string, years: number, signal?: AbortSignal) =>
  api<DatasetCosts>(`${ds(id)}/costs`, { query: { years }, signal });
export const expireVersions = (id: string, body: { keep_last: number; older_than_days: number; dry_run: boolean }) =>
  api<Expiry>(`${ds(id)}/version-expiry`, { method: "POST", body });
