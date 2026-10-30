import { api } from "@/lib/api/client";
import type { BlastRadius, ContentFinding, ContentPolicy, ContentPolicyResponse } from "./types";

const assetPath = (id: string) => `/assets/${encodeURIComponent(id)}`;

export const getFindings = (id: string, signal?: AbortSignal) =>
  api<{ findings: ContentFinding[] }>(`${assetPath(id)}/findings`, { signal });

export const getBlastRadius = (id: string, signal?: AbortSignal) =>
  api<BlastRadius>(`${assetPath(id)}/blast-radius`, { signal });

export const getContentPolicy = (signal?: AbortSignal) => api<ContentPolicyResponse>("/content-policy", { signal });

export const setContentPolicy = (body: ContentPolicy) =>
  api<{ policy: ContentPolicy }>("/content-policy", { method: "PUT", body });
