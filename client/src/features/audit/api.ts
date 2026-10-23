import { api } from "@/lib/api/client";
import type { AuditChain, AuditFilters, AuditPage, AuditVerification } from "./types";

export const PAGE_SIZE = 50;

function chainRequest(chain: AuditChain) {
  if (chain === "org") return { path: "/organization/audit", extra: {}, org: undefined };
  // Admin routes reject an X-Organization-ID that is not the caller's, so omit it.
  return { path: "/admin/audit", extra: { chain: "_platform" }, org: false as const };
}

export function fetchAuditPage(
  chain: AuditChain,
  filters: AuditFilters,
  before: number | undefined,
  signal?: AbortSignal,
) {
  const target = chainRequest(chain);
  return api<AuditPage>(target.path, {
    org: target.org,
    signal,
    query: {
      ...target.extra,
      limit: PAGE_SIZE,
      before,
      action: filters.action.trim(),
      outcome: filters.outcome,
      actor: filters.actor.trim(),
    },
  });
}

export function verifyAuditChain(chain: AuditChain) {
  if (chain === "org") return api<AuditVerification>("/organization/audit/verify");
  return api<AuditVerification>("/admin/audit/verify", { org: false, query: { chain: "_platform" } });
}
