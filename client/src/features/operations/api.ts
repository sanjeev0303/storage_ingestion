import { api } from "@/lib/api/client";
import type {
  Asset,
  AssetLifecycle,
  DeadLetter,
  DeadLetterAction,
  DeleteAssetResponse,
  HeldHold,
  LegalHold,
  LegalHoldRequest,
  PolicyInEffect,
  ProcessingRecord,
  QuotaReconcileResponse,
  ReleaseQuarantineResponse,
  RestoreAssetResponse,
  PendingApproval,
  PolicyImpact,
  RetentionPolicyRequest,
  RetentionPolicyResponse,
  RetentionTemplate,
  StorageConnection,
} from "./types";

const assetPath = (id: string) => `/assets/${encodeURIComponent(id)}`;

export const getAsset = (id: string, signal?: AbortSignal) => api<Asset>(assetPath(id), { signal });

export const getLifecycle = (id: string, signal?: AbortSignal) =>
  api<AssetLifecycle>(`${assetPath(id)}/lifecycle`, { signal });

export const getProcessing = (id: string, signal?: AbortSignal) =>
  api<ProcessingRecord>(`${assetPath(id)}/quarantine`, { signal });

// Under the GxP vault profile a deletion waits for approval (202, PendingApproval).
export const deleteAsset = (id: string) => api<DeleteAssetResponse | PendingApproval>(assetPath(id), { method: "DELETE" });

export const restoreAsset = (id: string) =>
  api<RestoreAssetResponse>(`${assetPath(id)}/restore`, { method: "POST" });

export const releaseQuarantine = (id: string) =>
  api<ReleaseQuarantineResponse>(`${assetPath(id)}/quarantine/release`, { method: "POST" });

export const setRetentionPolicy = (body: RetentionPolicyRequest) =>
  api<RetentionPolicyResponse | PendingApproval>("/retention/policies", { method: "PUT", body });

export const previewRetentionPolicy = (body: RetentionPolicyRequest) =>
  api<{ dry_run: true; impact: PolicyImpact }>("/retention/policies", { method: "PUT", body: { ...body, dry_run: true } });

export const listTemplates = (signal?: AbortSignal) =>
  api<{ templates: RetentionTemplate[] }>("/retention/templates", { signal });

export const listPolicies = (signal?: AbortSignal) =>
  api<{ policies: PolicyInEffect[] }>("/retention/policies", { signal });

// Every version of one scope's policy, newest first.
export const policyHistory = (scope: string, scopeKey: string, signal?: AbortSignal) =>
  api<{ policies: PolicyInEffect[] }>("/retention/policies", { query: { scope, scope_key: scopeKey }, signal });

export const listHolds = (includeReleased: boolean, signal?: AbortSignal) =>
  api<{ holds: HeldHold[] }>("/legal-holds", { query: { include_released: includeReleased || undefined }, signal });

export const placeLegalHold = (body: LegalHoldRequest) =>
  api<LegalHold>("/legal-holds", { method: "POST", body });

export const releaseLegalHold = (id: string, reason = "") =>
  api<PendingApproval | undefined>(`/legal-holds/${encodeURIComponent(id)}`, {
    method: "DELETE",
    query: { reason: reason || undefined },
  });

export const isPending = (value: unknown): value is PendingApproval =>
  typeof value === "object" && value !== null && (value as PendingApproval).status === "PENDING_APPROVAL";

// Admin routes reject an organization header that is not the caller's, so org is omitted.
export const listDeadLetters = (status: string, signal?: AbortSignal) =>
  api<{ dead_letters: DeadLetter[] }>("/admin/dead-letters", {
    org: false,
    signal,
    query: { status, limit: 100 },
  });

export const getDeadLetter = (id: string, signal?: AbortSignal) =>
  api<DeadLetter>(`/admin/dead-letters/${encodeURIComponent(id)}`, { org: false, signal });

export const actOnDeadLetter = (id: string, action: DeadLetterAction, reason: string) =>
  api<{ replay_event_id?: string } | undefined>(`/admin/dead-letters/${encodeURIComponent(id)}/${action}`, {
    method: "POST",
    org: false,
    body: { reason },
  });

export const listConnections = (signal?: AbortSignal) =>
  api<{ connections: StorageConnection[]; default_connection_id: string }>("/storage-connections", { signal });

export const reconcileQuota = (organizationId: string) =>
  api<QuotaReconcileResponse>(`/admin/organizations/${encodeURIComponent(organizationId)}/quota/reconcile`, {
    method: "POST",
    org: false,
  });
