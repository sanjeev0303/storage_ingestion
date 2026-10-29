import { api } from "@/lib/api/client";

export type SsoProtocol = "OIDC" | "SAML";

export type SsoSettings = {
  configured: boolean;
  protocol?: SsoProtocol;
  issuer?: string;
  client_id?: string;
  has_idp_metadata: boolean;
  allowed_domains: string[];
  default_role: string;
  enabled: boolean;
  updated_by?: string;
  updated_at?: string;
  redirect_uris: string[];
  // What to register at a SAML provider; absent when the server has SAML off.
  saml?: { entity_id: string; acs_url: string };
};

export type SsoInput = {
  protocol: SsoProtocol;
  issuer?: string;
  client_id?: string;
  // Empty keeps the stored secret.
  client_secret?: string;
  // Empty keeps the stored metadata.
  idp_metadata?: string;
  allowed_domains: string[];
  default_role: string;
  enabled: boolean;
};

export type ScimTokenStatus = {
  enabled: boolean;
  created_at?: string;
  last_used_at?: string | null;
};

export const getSso = (signal?: AbortSignal) => api<SsoSettings>("/organization/sso", { signal });

export const putSso = (body: SsoInput) => api<SsoSettings>("/organization/sso", { method: "PUT", body });

export const deleteSso = () => api<void>("/organization/sso", { method: "DELETE" });

export const getScimToken = (signal?: AbortSignal) => api<ScimTokenStatus>("/organization/scim-token", { signal });

export const issueScimToken = () => api<{ token: string }>("/organization/scim-token", { method: "POST" });

export const revokeScimToken = () => api<void>("/organization/scim-token", { method: "DELETE" });
