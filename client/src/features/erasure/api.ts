import { api } from "@/lib/api/client";

// Mirrors the erasure routes (docs/erasure.md).

export type ErasureKind = "SUBJECT_WITHDRAWAL" | "COPYRIGHT" | "PII" | "CONSENT_EXPIRED" | "OTHER";
export type ErasureState = "PLANNED" | "APPROVED" | "DELETING" | "WAITING_ON_HOLD" | "COMPLETED" | "CANCELLED" | "FAILED";
export type ItemState = "PENDING" | "DELETING" | "HELD" | "DESTROYED" | "FAILED";

export type Selector = { subject_ids: string[]; sha256s: string[]; object_ids: string[]; subject_key?: string };

export type PlanObject = {
  asset_id: string;
  object_id: string;
  version: number;
  path: string;
  sha256: string;
  size_bytes: number;
  connection_id: string;
  status: string;
  held_by: string[];
};
export type PlanVersion = {
  dataset_id: string;
  dataset_name: string;
  version_id: string;
  version: number;
  members: number;
  current: boolean;
  superseded_by?: string;
};
export type Plan = {
  objects: PlanObject[];
  bytes: number;
  connections: string[];
  versions: PlanVersion[];
  drafts: { dataset_id: string; dataset_name: string; members: number }[];
  models: { id: string; name: string; version: string; run_id: string; run_name: string }[];
  not_searched: string[];
};

export type ErasureItem = {
  asset_id: string;
  path: string;
  sha256: string;
  size_bytes: number;
  connection_id: string;
  state: ItemState;
  reason: string;
  destroyed_at?: string;
};

export type Successor = { dataset_id: string; version_id: string; version: number; successor_id: string; successor_version: number; removed: number };

export type Erasure = {
  id: string;
  kind: ErasureKind;
  state: ErasureState;
  reason: string;
  reference: string;
  institution: string;
  selector: Selector;
  subject_hashes: string[];
  plan: Plan;
  successors: Successor[];
  items?: ErasureItem[];
  progress: { total: number; destroyed: number; held: number; failed: number; pending: number };
  requested_by: string;
  approved_by?: string;
  executed_at?: string;
  completed_at?: string;
  certified: boolean;
  created_at: string;
};

export type Certificate = { body: string; body_sha256: string; signature: string; key_id: string; public_key: string };

export const search = (body: Selector) => api<Plan>("/erasures/search", { method: "POST", body });
export const createErasure = (body: Selector & { kind: ErasureKind; reason: string; reference: string; institution: string }) =>
  api<Erasure>("/erasures", { method: "POST", body });
export const listErasures = (signal?: AbortSignal) => api<{ erasures: Erasure[] }>("/erasures", { signal });
export const getErasure = (id: string, signal?: AbortSignal) => api<Erasure>(`/erasures/${encodeURIComponent(id)}`, { signal });
export const approveErasure = (id: string, note: string) => api<Erasure>(`/erasures/${encodeURIComponent(id)}/approve`, { method: "POST", body: { note } });
export const executeErasure = (id: string) => api<Erasure>(`/erasures/${encodeURIComponent(id)}/execute`, { method: "POST" });
export const cancelErasure = (id: string) => api<Erasure>(`/erasures/${encodeURIComponent(id)}/cancel`, { method: "POST" });
export const getCertificate = (id: string) => api<Certificate>(`/erasures/${encodeURIComponent(id)}/certificate`);
