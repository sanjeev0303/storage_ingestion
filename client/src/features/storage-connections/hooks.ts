import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useOrganizationId } from "@/lib/auth/session";
import { orgKey } from "@/lib/query/keys";
import { getCapabilities, getConnection, listConnections, listProviders } from "./api";

export function useStorageConnections() {
  const organizationId = useOrganizationId();
  return useQuery({
    queryKey: orgKey(organizationId, "storage", "connections"),
    queryFn: listConnections,
    enabled: Boolean(organizationId),
  });
}

export function useStorageConnection(id: string) {
  const organizationId = useOrganizationId();
  return useQuery({
    queryKey: orgKey(organizationId, "storage", "connection", id),
    queryFn: () => getConnection(id),
    enabled: Boolean(organizationId),
  });
}

export function useStorageProviders() {
  const organizationId = useOrganizationId();
  return useQuery({
    queryKey: orgKey(organizationId, "storage", "providers"),
    queryFn: listProviders,
    enabled: Boolean(organizationId),
    staleTime: 5 * 60_000,
  });
}

export function useProviderCapabilities() {
  const organizationId = useOrganizationId();
  return useQuery({
    queryKey: orgKey(organizationId, "storage", "capabilities"),
    queryFn: getCapabilities,
    enabled: Boolean(organizationId),
    staleTime: 5 * 60_000,
  });
}

// Everything under "storage" is stale after any change to a connection.
export function useRefreshStorage() {
  const client = useQueryClient();
  const organizationId = useOrganizationId();
  return () => client.invalidateQueries({ queryKey: orgKey(organizationId, "storage") });
}
