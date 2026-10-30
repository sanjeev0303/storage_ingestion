import { api, apiRaw } from "@/lib/api/client";

// Mirrors /adoptions (docs/adoption.md).

export type AdoptionSource = "MANIFEST" | "S3_INVENTORY" | "MHL" | "BAGIT" | "URL_LIST" | "STAC";
export type AdoptionState = "QUEUED" | "READING" | "RUNNING" | "COMPLETED" | "FAILED" | "CANCELLED";
export type EntryState = "PENDING" | "ADOPTED" | "IMPORTED" | "REJECTED" | "FAILED" | "SKIPPED";

export type AdoptionJob = {
  id: string;
  connection_id: string;
  dataset: string;
  source: AdoptionSource;
  source_ref: string;
  mode: "DETACH" | "MANAGE";
  require_digest: boolean;
  state: AdoptionState;
  entries: number;
  adopted: number;
  rejected: number;
  failed: number;
  bytes: number;
  error?: string;
  created_by: string;
  created_at: string;
  finished_at?: string;
};

export type AdoptionEntry = {
  line: number;
  object_key?: string;
  url?: string;
  path: string;
  size_bytes: number;
  state: EntryState;
  code?: string;
  detail?: string;
  asset_id?: string;
  sha256?: string;
};

export type NewAdoption = {
  connection_id?: string;
  dataset?: string;
  source: AdoptionSource;
  source_ref?: string;
  inline?: string;
  mode: "DETACH" | "MANAGE";
  require_digest: boolean;
};

const path = (id: string) => `/adoptions/${encodeURIComponent(id)}`;

export const listAdoptions = (signal?: AbortSignal) => api<{ adoptions: AdoptionJob[] }>("/adoptions", { signal });

export const createAdoption = (body: NewAdoption) => api<AdoptionJob>("/adoptions", { method: "POST", body });

export const cancelAdoption = (id: string) => api<void>(`${path(id)}/cancel`, { method: "POST" });

export const adoptionEntries = (id: string, after: number, signal?: AbortSignal) =>
  api<{ entries: AdoptionEntry[]; next_after: number }>(`${path(id)}/results`, { query: { after: after || undefined, limit: 200 }, signal });

// The whole per-entry file, as a Blob to save.
export async function resultsFile(id: string, format: "csv" | "jsonl") {
  const res = await apiRaw(`${path(id)}/results`, { query: { format }, timeoutMs: 300_000 });
  return res.blob();
}
