import { api } from "@/lib/api/client";

// Mirrors the edge routes (docs/edge.md).

export type EdgeDevice = {
  id: string;
  name: string;
  dataset_id: string;
  namespace: string;
  path_prefix: string;
  last_seen?: string;
  pending_files: number;
  pending_bytes: number;
  failed_files: number;
  oldest_pending?: string;
  agent_version: string;
  note: string;
  created_at: string;
  revoked_at?: string;
};

export type EdgeRecording = {
  id: string;
  device_id: string;
  name: string;
  kind: "RUN" | "RECORDING";
  state: "OPEN" | "COMPLETE" | "INCOMPLETE";
  missing: string[];
  version_id?: string;
  chunks: number;
  bytes: number;
  created_at: string;
  closed_at?: string;
};

export type RecordingDetail = EdgeRecording & { gaps: { from: string; to: string }[] };

export const listDevices = (signal?: AbortSignal) => api<{ devices: EdgeDevice[] }>("/edge-devices", { signal });
export const registerDevice = (body: { name: string; dataset_id: string; path_prefix: string }) =>
  api<{ device: EdgeDevice; token: string }>("/edge-devices", { method: "POST", body });
export const revokeDevice = (id: string) => api<void>(`/edge-devices/${encodeURIComponent(id)}`, { method: "DELETE" });
export const listRecordings = (signal?: AbortSignal) => api<{ recordings: EdgeRecording[] }>("/edge-recordings", { signal });
export const getRecording = (id: string, signal?: AbortSignal) => api<RecordingDetail>(`/edge-recordings/${encodeURIComponent(id)}`, { signal });
