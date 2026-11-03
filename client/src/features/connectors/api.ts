import { api } from "@/lib/api/client";

// Mirrors the connector routes (docs/connectors.md).

export type ChatKind = "SLACK" | "TEAMS";
export type ChatIntegration = { id: string; kind: ChatKind; name: string; url_host: string; created_at: string };
export type ChatShare = {
  id: string;
  asset_id: string;
  integration_id: string;
  note: string;
  expires_at: string;
  revoked_at?: string;
  opens: number;
  created_by: string;
  created_at: string;
  path: string;
  size_bytes: number;
};
export type Collection = {
  id: string;
  format: "COMMS_JSONL" | "RSMF" | "UFDR" | "MANIFEST";
  source: string;
  export_sha256: string;
  items: number;
  verified: number;
  mismatches: string[];
  retention: string;
  created_at: string;
};

export const listIntegrations = (signal?: AbortSignal) => api<{ integrations: ChatIntegration[] }>("/chat-integrations", { signal });
export const addIntegration = (body: { kind: ChatKind; name: string; url: string }) =>
  api<ChatIntegration>("/chat-integrations", { method: "POST", body });
export const deleteIntegration = (id: string) => api<void>(`/chat-integrations/${encodeURIComponent(id)}`, { method: "DELETE" });

export const listShares = (signal?: AbortSignal) => api<{ shares: ChatShare[] }>("/chat-shares", { signal });
export const shareToChat = (body: { asset_id: string; integration_id: string; note: string; expires_in: string }) =>
  api<ChatShare>("/chat-shares", { method: "POST", body });
export const getShare = (id: string, signal?: AbortSignal) => api<ChatShare>(`/chat-shares/${encodeURIComponent(id)}`, { signal });
export const openShare = (id: string) => api<{ url: string }>(`/chat-shares/${encodeURIComponent(id)}/open`, { method: "POST" });
export const revokeShare = (id: string) => api<void>(`/chat-shares/${encodeURIComponent(id)}`, { method: "DELETE" });

export const listCollections = (datasetId: string, signal?: AbortSignal) =>
  api<{ collections: Collection[] }>(`/datasets/${encodeURIComponent(datasetId)}/collections`, { signal });
