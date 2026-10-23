"use client";

import { useMutation } from "@tanstack/react-query";
import { useOrganizationId } from "@/lib/auth/session";
import { accountKey, orgKey } from "@/lib/query/keys";
import { usePagedQuery } from "@/lib/query/paged";
import { fetchAuditPage, verifyAuditChain } from "./api";
import type { AuditChain, AuditEvent, AuditFilters } from "./types";

export function useAuditEvents(chain: AuditChain, filters: AuditFilters, enabled: boolean) {
  const organizationId = useOrganizationId();
  // The platform chain is not organization data, so it lives outside the "org" keys.
  const queryKey =
    chain === "org" ? orgKey(organizationId, "audit", filters) : accountKey("audit", "platform", filters);

  return usePagedQuery<AuditEvent, number>({
    queryKey,
    enabled: enabled && (chain === "_platform" || Boolean(organizationId)),
    fetchPage: async (before, signal) => {
      const page = await fetchAuditPage(chain, filters, before, signal);
      // next_before is only sent when the page was full
      return { items: page.events, next: page.next_before || undefined };
    },
  });
}

export function useVerifyAudit(chain: AuditChain) {
  return useMutation({ mutationFn: () => verifyAuditChain(chain) });
}
