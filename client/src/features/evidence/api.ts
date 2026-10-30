import { api, apiRaw } from "@/lib/api/client";

// Mirrors the evidence routes (docs/evidence.md).

export type EvidenceSettings = {
  anchor_connection_id: string;
  anchor_every_minutes: number;
  anchor_lock_days: number;
  review_every_days: number;
  work_start_hour: number;
  work_end_hour: number;
  timezone: string;
  bulk_read_threshold: number;
  require_lawful_basis: boolean;
  updated_at?: string;
};

export type AnchorCheck = {
  id: string;
  seq: number;
  hash: string;
  object_key?: string;
  locked_until?: string;
  created_at: string;
  signature_ok: boolean;
  chain_ok: boolean;
  stored_ok?: boolean;
  problem?: string;
};

export type FindingKind = "DELETION" | "RETENTION_CHANGE" | "PERMISSION_CHANGE" | "FAILED_HASH" | "AFTER_HOURS_READS";
export type Finding = { kind: FindingKind; at: string; actor?: string; subject?: string; detail: string; audit_seq?: number };

export type Review = {
  id: string;
  period_start: string;
  period_end: string;
  findings: Finding[];
  counts: Record<FindingKind, number>;
  report_sha256: string;
  state: "OPEN" | "SIGNED";
  signed_by?: string;
  signed_at?: string;
  note?: string;
  created_by: string;
  created_at: string;
};

export type Provenance = {
  lawful_basis: string;
  license: string;
  source: string;
  source_kind: string;
  recorded_at?: string;
  recorded_by?: string;
};

export type SnapshotHold = { matter: string; kind: string; hold_ids: string[]; released: boolean };

export type Control = { id: string; title: string; platform: string; status: "MET" | "PARTIAL" | "NOT_MET" | "ORGANIZATION"; evidence?: string[] };
export type Framework = { id: string; name: string; controls: Control[]; note: string };

export const getSettings = (signal?: AbortSignal) => api<EvidenceSettings>("/evidence-settings", { signal });
export const putSettings = (body: EvidenceSettings) => api<EvidenceSettings>("/evidence-settings", { method: "PUT", body });

export const listAnchors = (signal?: AbortSignal) => api<{ anchors: AnchorCheck[] }>("/organization/audit/anchors", { signal });
export const anchorNow = () => api<AnchorCheck>("/organization/audit/anchors", { method: "POST" });

export const listReviews = (signal?: AbortSignal) => api<{ reviews: Review[] }>("/audit-reviews", { signal });
export const createReview = (from: string) => api<Review>("/audit-reviews", { method: "POST", body: { from } });
export const signReview = (id: string, note: string) =>
  api<Review>(`/audit-reviews/${encodeURIComponent(id)}/sign`, { method: "POST", body: { note } });

const ds = (id: string) => `/datasets/${encodeURIComponent(id)}`;
export const getProvenance = (id: string, signal?: AbortSignal) => api<Provenance>(`${ds(id)}/provenance`, { signal });
export const putProvenance = (id: string, body: Omit<Provenance, "recorded_at" | "recorded_by">) =>
  api<Provenance>(`${ds(id)}/provenance`, { method: "PUT", body });

const version = (id: string, versionId: string) => `${ds(id)}/versions/${encodeURIComponent(versionId)}`;
export const listHolds = (id: string, versionId: string, signal?: AbortSignal) =>
  api<{ holds: SnapshotHold[] }>(`${version(id, versionId)}/holds`, { signal });
export const placeHold = (id: string, versionId: string, body: { kind: string; reference: string; reason: string }) =>
  api<SnapshotHold>(`${version(id, versionId)}/holds`, { method: "POST", body });
export const releaseHold = (id: string, versionId: string, matter: string) =>
  api<{ released: number }>(`${version(id, versionId)}/holds/release`, { method: "POST", body: { matter, reason: "" } });

export const listFrameworks = (signal?: AbortSignal) => api<{ frameworks: { id: string; name: string }[] }>("/compliance/frameworks", { signal });
export const getFramework = (id: string, signal?: AbortSignal) => api<Framework>(`/compliance/${encodeURIComponent(id)}`, { signal });
export const regulatorAccess = (body: { name: string; days: number }) =>
  api<{ id: string; name: string; expires_at: string; token: string }>("/compliance/regulator-access", { method: "POST", body });

// Files to save: zips, exports and letters.
export async function fetchFile(path: string, query?: Record<string, string | undefined>) {
  const res = await apiRaw(path, { query, timeoutMs: 600_000 });
  return res.blob();
}

export function saveBlob(blob: Blob, name: string) {
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download = name;
  a.click();
  URL.revokeObjectURL(url);
}

export const packPath = (id: string, versionId: string) => `${version(id, versionId)}/evidence-pack`;

export type OrderStep = { step: string; subject: string; at?: string; audit_seq?: number; detail?: string };
export const runOrder = (runId: string, signal?: AbortSignal) =>
  api<{ run_id: string; in_order: boolean; steps: OrderStep[]; problems: string[] }>(
    `/training-runs/${encodeURIComponent(runId)}/provenance-order`,
    { signal },
  );
export const publishSummary = (modelId: string) =>
  api<unknown>(`/models/${encodeURIComponent(modelId)}/training-summary/publish`, { method: "POST" });
