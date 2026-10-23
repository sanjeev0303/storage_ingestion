import { useQuery } from "@tanstack/react-query";
import { api } from "@/lib/api/client";
import { useOrganizationId } from "@/lib/auth/session";
import { orgKey } from "@/lib/query/keys";
import type { QuotaResponse } from "./types";

export const getQuota = () => api<QuotaResponse>("/organization/quota");

export function useQuota() {
  const organizationId = useOrganizationId();
  return useQuery({
    queryKey: orgKey(organizationId, "uploads", "quota"),
    queryFn: getQuota,
    enabled: Boolean(organizationId),
    staleTime: 30_000,
  });
}
