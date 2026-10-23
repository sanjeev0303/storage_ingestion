"use client";

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { isApiError } from "@/lib/api/errors";
import { useOrganizationId, useSession } from "@/lib/auth/session";
import { orgKey } from "@/lib/query/keys";
import { usePagedQuery } from "@/lib/query/paged";
import {
  createDownloadUrl,
  deleteObject,
  deleteVersion,
  getCapabilities,
  getLifecycle,
  getObject,
  getVersion,
  listConnectionNames,
  listObjects,
  listVersions,
  patchMetadata,
  promoteVersion,
  renameObject,
  revokeDownloadLinks,
  setVersioning,
} from "./api";
import { toApiParams } from "./filters";
import type { DownloadMode } from "./types";

export function useCatalogList(query: string) {
  const orgId = useOrganizationId();
  return usePagedQuery({
    // the cursor belongs to one set of filters, so the filters are part of the key
    queryKey: orgKey(orgId, "catalog", "list", query),
    enabled: Boolean(orgId),
    fetchPage: async (cursor: string | undefined, signal) => {
      const page = await listObjects(toApiParams(new URLSearchParams(query)), cursor, signal);
      return { items: page.objects, next: page.next_cursor };
    },
  });
}

export function useCapabilities() {
  const orgId = useOrganizationId();
  return useQuery({
    queryKey: orgKey(orgId, "catalog", "capabilities"),
    queryFn: getCapabilities,
    enabled: Boolean(orgId),
    retry: false,
    staleTime: 5 * 60_000,
  });
}

export function useObject(id: string) {
  const orgId = useOrganizationId();
  return useQuery({
    queryKey: orgKey(orgId, "catalog", "object", id),
    queryFn: () => getObject(id),
    enabled: Boolean(orgId),
  });
}

export function useVersions(id: string) {
  const orgId = useOrganizationId();
  return usePagedQuery({
    queryKey: orgKey(orgId, "catalog", "versions", id),
    enabled: Boolean(orgId),
    fetchPage: async (before: number | undefined, signal) => {
      const page = await listVersions(id, before, signal);
      return { items: page.versions, next: page.next_before };
    },
  });
}

export function useVersion(id: string, version: number | "current") {
  const orgId = useOrganizationId();
  return useQuery({
    queryKey: orgKey(orgId, "catalog", "version", id, version),
    queryFn: () => getVersion(id, version),
    enabled: Boolean(orgId),
  });
}

export function useLifecycle(assetId: string) {
  const orgId = useOrganizationId();
  return useQuery({
    queryKey: orgKey(orgId, "catalog", "lifecycle", assetId),
    queryFn: () => getLifecycle(assetId),
    enabled: Boolean(orgId),
    retry: false,
  });
}

// Connection names are a convenience; without storage.read the id is shown instead.
export function useConnectionNames() {
  const orgId = useOrganizationId();
  const { can } = useSession();
  return useQuery({
    queryKey: orgKey(orgId, "catalog", "connection-names"),
    queryFn: async () => {
      const connections = await listConnectionNames();
      return Object.fromEntries(connections.map((c) => [c.id, c.name]));
    },
    enabled: Boolean(orgId) && can("storage.read"),
    retry: false,
    staleTime: 5 * 60_000,
  });
}

function useCatalogMutation<V, R>(fn: (vars: V) => Promise<R>) {
  const queryClient = useQueryClient();
  const orgId = useOrganizationId();
  return useMutation({
    mutationFn: fn,
    onSuccess: () => queryClient.invalidateQueries({ queryKey: orgKey(orgId, "catalog") }),
    // a stale revision means someone else changed the object: reload it
    onError: (error) => {
      if (isApiError(error) && error.status === 412) {
        queryClient.invalidateQueries({ queryKey: orgKey(orgId, "catalog") });
      }
    },
  });
}

export const useEditMetadata = (id: string) =>
  useCatalogMutation((v: { revision: number; metadata: Record<string, string | null> }) =>
    patchMetadata(id, v.revision, v.metadata),
  );

export const useRename = (id: string) =>
  useCatalogMutation((v: { revision: number; path: string }) => renameObject(id, v.revision, v.path));

export const useSetVersioning = (id: string) =>
  useCatalogMutation((v: { revision: number; versioning: "ENABLED" | "DISABLED" }) =>
    setVersioning(id, v.revision, v.versioning),
  );

export const usePromote = (id: string) =>
  useCatalogMutation((v: { revision: number; version: number }) => promoteVersion(id, v.revision, v.version));

export const useDeleteVersion = (id: string) => useCatalogMutation((version: number) => deleteVersion(id, version));

export const useDeleteObject = (id: string) => useCatalogMutation(() => deleteObject(id));

export const useDownloadUrl = (assetId: string) =>
  useMutation({
    mutationFn: (v: { mode: DownloadMode; filename?: string }) => createDownloadUrl(assetId, v.mode, v.filename),
  });

export const useRevokeLinks = (assetId: string) => useMutation({ mutationFn: () => revokeDownloadLinks(assetId) });
