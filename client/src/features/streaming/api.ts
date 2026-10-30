import { api } from "@/lib/api/client";

// Mirrors the streaming routes (docs/streaming.md).

export type Shard = { ordinals: number[]; files: number; bytes: number; packs: number; first_path: string; last_path: string };
export type ShardPage = { shards: Shard[]; next_cursor?: string };

export type SignedManifest = { body: string; algorithm?: string; key_id?: string; signature?: string };

export type Repack = {
  id: string;
  dataset_id: string;
  state: "PENDING" | "DONE" | "CANCELLED";
  packs_planned: number;
  entries_planned: number;
  requested_by: string;
  created_at: string;
  finished_at?: string;
};

export type VerifyFailure = { path: string; reason: string };

export type RunVerification = {
  id: string;
  version_id?: string;
  host: string;
  sample_rate: number;
  checked: number;
  failed: number;
  failures: VerifyFailure[];
  created_at: string;
};

export type Checkpoint = {
  id: string;
  step: number;
  version_id: string;
  written_by: string;
  verified_by?: string;
  verified_at?: string;
  verify_error?: string;
  created_at: string;
};

const version = (id: string, versionId: string) =>
  `/datasets/${encodeURIComponent(id)}/versions/${encodeURIComponent(versionId)}`;
const run = (runId: string) => `/training-runs/${encodeURIComponent(runId)}`;

export const getShards = (id: string, versionId: string, signal?: AbortSignal) =>
  api<ShardPage>(`${version(id, versionId)}/shards`, { signal });
export const getSignedManifest = (id: string, versionId: string, signal?: AbortSignal) =>
  api<SignedManifest>(`${version(id, versionId)}/signed-manifest`, { signal });

export const getPacking = (signal?: AbortSignal) => api<{ target_pack_bytes: number }>("/packing-settings", { signal });
export const putPacking = (bytes: number) =>
  api<{ target_pack_bytes: number }>("/packing-settings", { method: "PUT", body: { target_pack_bytes: bytes } });

export const listRepacks = (id: string, signal?: AbortSignal) =>
  api<{ repacks: Repack[] }>(`/datasets/${encodeURIComponent(id)}/repacks`, { signal });
export const startRepack = (id: string) => api<Repack>(`/datasets/${encodeURIComponent(id)}/repacks`, { method: "POST" });
export const cancelRepack = (id: string, repackId: string) =>
  api<void>(`/datasets/${encodeURIComponent(id)}/repacks/${encodeURIComponent(repackId)}/cancel`, { method: "POST" });

export const listVerifications = (runId: string, signal?: AbortSignal) =>
  api<{ verifications: RunVerification[] }>(`${run(runId)}/verifications`, { signal });
export const listCheckpoints = (runId: string, signal?: AbortSignal) =>
  api<{ checkpoints: Checkpoint[]; last_verified: Checkpoint | null }>(`${run(runId)}/checkpoints`, { signal });
