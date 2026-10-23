"use client";

import { useQuery } from "@tanstack/react-query";
import { useSession } from "@/lib/auth/session";
import { orgKey } from "@/lib/query/keys";
import * as api from "./api";

// All overview reads sit under one key so Refresh can refetch them together.
// None of them poll: the audit read writes an audit event of its own.
export const overviewKey = (orgId: string | null, ...parts: unknown[]) => orgKey(orgId, "overview", ...parts);

function useOverviewQuery<T>(part: string, enabled: boolean, run: (signal: AbortSignal) => Promise<T>) {
  const { organizationId } = useSession();
  return useQuery({
    queryKey: overviewKey(organizationId, part),
    queryFn: ({ signal }) => run(signal),
    enabled: Boolean(organizationId) && enabled,
    staleTime: 30_000,
  });
}

export function useOverviewData() {
  const { can, isPlatformAdmin } = useSession();
  return {
    jobs: useOverviewQuery("jobs", can("ingestion.read"), api.getLatestJobs),
    failedDatasets: useOverviewQuery("datasets-failed", can("datasets.read"), (s) => api.getDatasetsByStatus("FAILED", s)),
    validatingDatasets: useOverviewQuery("datasets-validating", can("datasets.read"), (s) =>
      api.getDatasetsByStatus("VALIDATING", s),
    ),
    connections: useOverviewQuery("connections", can("storage.read"), api.getConnections),
    audit: useOverviewQuery("audit", can("audit.read"), api.getRecentAudit),
    deadLetters: useOverviewQuery("dead-letters", isPlatformAdmin, api.getOpenDeadLetters),
  };
}
