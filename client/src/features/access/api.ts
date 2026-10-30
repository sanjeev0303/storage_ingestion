import { api } from "@/lib/api/client";
import type { Classification } from "@/features/operations/types";

// Mirrors /api-tokens, /access-policy, /links, /egress,
// /assets/{id}/classification and /security/alerts (docs/access.md).

export type ApiToken = {
  id: string;
  name: string;
  user_id: string;
  hint: string;
  permissions: string[];
  datasets: string[];
  networks: string[];
  allow_links: boolean;
  expires_at: string;
  created_at: string;
  last_used_at?: string;
  last_used_ip?: string;
  revoked_at?: string;
  revoked_by?: string;
};

export type NewToken = {
  name: string;
  permissions: string[];
  datasets: string[];
  networks: string[];
  expires_in: string;
  allow_links: boolean;
};

export type AccessPolicy = {
  links_enabled: boolean;
  max_link_ttl: string;
  egress_monthly_bytes: number;
  egress_daily_member_bytes: number;
  egress_action: "WARN" | "BLOCK";
  mass_link_threshold: number;
  anomaly_action: "ALERT" | "REVOKE_LINKS";
  updated_by?: string;
  updated_at?: string;
};

export type ShareLink = {
  id: string;
  asset_id: string;
  file_name: string;
  principal_id: string;
  created_at: string;
  expires_at: string;
  uses: number;
  last_used_at?: string;
  revoked_at?: string;
  revoked_by?: string;
};

export type AlertKind =
  | "READ_SPIKE"
  | "MASS_LINKS"
  | "NEW_NETWORK"
  | "EGRESS_BUDGET"
  | "STORAGE_POSTURE"
  | "LIFECYCLE_CHANGED"
  | "OUT_OF_BAND_WRITE";

export type SecurityAlert = {
  id: string;
  principal_id: string;
  kind: AlertKind;
  detail: string;
  metrics: Record<string, unknown>;
  action_taken?: string;
  state: "OPEN" | "ACKNOWLEDGED";
  created_at: string;
  acknowledged_by?: string;
  acknowledged_at?: string;
};

export type Egress = {
  month_bytes: number;
  month_budget: number;
  daily_member_budget: number;
  action: "WARN" | "BLOCK";
  members: { principal_id: string; bytes: number; reads: number }[];
};

export const listTokens = (all: boolean, signal?: AbortSignal) =>
  api<{ tokens: ApiToken[] }>("/api-tokens", { query: { all: all || undefined }, signal });

export const createToken = (body: NewToken) => api<ApiToken & { token: string }>("/api-tokens", { method: "POST", body });

export const revokeToken = (id: string) => api<void>(`/api-tokens/${encodeURIComponent(id)}`, { method: "DELETE" });

export const getAccessPolicy = (signal?: AbortSignal) => api<AccessPolicy>("/access-policy", { signal });

export const putAccessPolicy = (body: Omit<AccessPolicy, "updated_by" | "updated_at">) =>
  api<AccessPolicy>("/access-policy", { method: "PUT", body });

export const listLinks = (all: boolean, active: boolean, signal?: AbortSignal) =>
  api<{ links: ShareLink[] }>("/links", { query: { all: all || undefined, active: active ? undefined : "false" }, signal });

export const revokeLinks = (body: { link_ids?: string[]; asset_ids?: string[]; principal_id?: string; all?: boolean }) =>
  api<{ revoked: number }>("/links/revoke", { method: "POST", body });

export const getEgress = (signal?: AbortSignal) => api<Egress>("/egress", { signal });

export const classifyAsset = ({ id, classification }: { id: string; classification: Classification }) =>
  api<{ asset_id: string; classification: Classification }>(`/assets/${encodeURIComponent(id)}/classification`, {
    method: "PUT",
    body: { classification },
  });

export const listAlerts = (state: "OPEN" | "", signal?: AbortSignal) =>
  api<{ alerts: SecurityAlert[] }>("/security/alerts", { query: { state: state || undefined }, signal });

export const acknowledgeAlert = (id: string) =>
  api<SecurityAlert>(`/security/alerts/${encodeURIComponent(id)}/acknowledge`, { method: "POST" });
