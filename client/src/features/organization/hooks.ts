"use client";

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useSession } from "@/lib/auth/session";
import { orgKey } from "@/lib/query/keys";
import { usePagedQuery } from "@/lib/query/paged";
import * as api from "./api";

export function useOrganizationQuery() {
  const { organizationId } = useSession();
  return useQuery({
    queryKey: orgKey(organizationId, "organization"),
    queryFn: ({ signal }) => api.getOrganization(signal),
    enabled: Boolean(organizationId),
  });
}

// Same key and shape as the session's own roles query, so they share one cache entry.
export function useRolesQuery() {
  const { organizationId } = useSession();
  return useQuery({
    queryKey: orgKey(organizationId, "roles"),
    queryFn: ({ signal }) => api.getRoles(signal),
    enabled: Boolean(organizationId),
    staleTime: 5 * 60_000,
  });
}

export function useQuotaQuery(options: { enabled?: boolean; keySuffix?: string } = {}) {
  const { organizationId } = useSession();
  return useQuery({
    queryKey: orgKey(organizationId, ...(options.keySuffix ? [options.keySuffix, "quota"] : ["quota"])),
    queryFn: ({ signal }) => api.getQuota(signal),
    enabled: Boolean(organizationId) && (options.enabled ?? true),
  });
}

export function useMembersQuery() {
  const { organizationId } = useSession();
  return usePagedQuery({
    queryKey: orgKey(organizationId, "members"),
    fetchPage: (cursor, signal) => api.getMembersPage(cursor, signal),
    enabled: Boolean(organizationId),
  });
}

export function useInvitationsQuery(enabled = true) {
  const { organizationId } = useSession();
  return usePagedQuery({
    queryKey: orgKey(organizationId, "invitations"),
    fetchPage: (cursor, signal) => api.getInvitationsPage(cursor, signal),
    enabled: Boolean(organizationId) && enabled,
  });
}

// Runs a change, then refreshes everything cached under the given areas.
export function useOrgMutation<Vars, Result>(run: (vars: Vars) => Promise<Result>, areas: string[]) {
  const client = useQueryClient();
  const { organizationId } = useSession();
  return useMutation({
    mutationFn: run,
    onSuccess: async () => {
      await Promise.all(areas.map((area) => client.invalidateQueries({ queryKey: orgKey(organizationId, area) })));
    },
  });
}

export function useAdminOrganizationsQuery() {
  return usePagedQuery({
    queryKey: ["account", "admin", "organizations"],
    fetchPage: (cursor, signal) => api.getAdminOrganizationsPage(cursor, signal),
  });
}

export function useAdminUsersQuery() {
  return usePagedQuery({
    queryKey: ["account", "admin", "users"],
    fetchPage: (cursor, signal) => api.getAdminUsersPage(cursor, signal),
  });
}

export function useAdminMutation<Vars, Result>(run: (vars: Vars) => Promise<Result>, area: "organizations" | "users") {
  const client = useQueryClient();
  return useMutation({
    mutationFn: run,
    onSuccess: () => client.invalidateQueries({ queryKey: ["account", "admin", area] }),
  });
}
