"use client";

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useSession } from "@/lib/auth/session";
import { orgKey } from "@/lib/query/keys";
import * as api from "./api";

export function useFindings(id: string, enabled: boolean) {
  const { organizationId } = useSession();
  return useQuery({
    queryKey: orgKey(organizationId, "content-checks", "findings", id),
    queryFn: ({ signal }) => api.getFindings(id, signal),
    enabled: Boolean(organizationId) && id !== "" && enabled,
    retry: false,
  });
}

// Loaded only when asked for: it reads the audit log.
export function useBlastRadius(id: string, enabled: boolean) {
  const { organizationId } = useSession();
  return useQuery({
    queryKey: orgKey(organizationId, "content-checks", "blast-radius", id),
    queryFn: ({ signal }) => api.getBlastRadius(id, signal),
    enabled: Boolean(organizationId) && id !== "" && enabled,
    retry: false,
  });
}

export function useContentPolicy() {
  const { organizationId } = useSession();
  return useQuery({
    queryKey: orgKey(organizationId, "content-checks", "policy"),
    queryFn: ({ signal }) => api.getContentPolicy(signal),
    enabled: Boolean(organizationId),
  });
}

export function useSaveContentPolicy() {
  const client = useQueryClient();
  const { organizationId } = useSession();
  return useMutation({
    mutationFn: api.setContentPolicy,
    onSuccess: () => client.invalidateQueries({ queryKey: orgKey(organizationId, "content-checks", "policy") }),
  });
}
