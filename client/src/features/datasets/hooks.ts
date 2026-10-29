import { useInfiniteQuery, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useOrganizationId } from "@/lib/auth/session";
import { orgKey } from "@/lib/query/keys";
import { usePagedQuery } from "@/lib/query/paged";
import * as api from "./api";
import { TERMINAL_IMPORT_STATES, type MemberRequest, type DuplicatePolicy } from "./types";

const IMPORT_POLL_MS = 3000;

export function useDatasets(status: string) {
  const organizationId = useOrganizationId();
  return usePagedQuery({
    queryKey: orgKey(organizationId, "datasets", "list", status),
    enabled: Boolean(organizationId),
    fetchPage: async (cursor, signal) => {
      const page = await api.listDatasets({ status, cursor }, signal);
      return { items: page.datasets, next: page.next_cursor };
    },
  });
}

export function useDataset(id: string, enabled = true) {
  const organizationId = useOrganizationId();
  return useQuery({
    queryKey: orgKey(organizationId, "datasets", "detail", id),
    queryFn: ({ signal }) => api.getDataset(id, signal),
    enabled: Boolean(organizationId) && enabled,
  });
}

// Anything that changes a dataset also changes its lists, members and versions.
export function useRefreshDatasets() {
  const client = useQueryClient();
  const organizationId = useOrganizationId();
  return () => client.invalidateQueries({ queryKey: orgKey(organizationId, "datasets") });
}

export function useCreateDataset() {
  const refresh = useRefreshDatasets();
  return useMutation({ mutationFn: api.createDataset, onSuccess: refresh });
}

export function useUpdateDataset(id: string) {
  const refresh = useRefreshDatasets();
  return useMutation({
    mutationFn: (body: Parameters<typeof api.updateDataset>[1]) => api.updateDataset(id, body),
    onSuccess: refresh,
    // A stale revision means someone else changed the dataset; show their version.
    onError: (error) => {
      if ((error as { code?: string }).code === "revision_conflict") refresh();
    },
  });
}

export function useArchiveDataset(id: string) {
  const refresh = useRefreshDatasets();
  return useMutation({ mutationFn: () => api.archiveDataset(id), onSuccess: refresh });
}

export function useMembers(id: string) {
  const organizationId = useOrganizationId();
  return usePagedQuery({
    queryKey: orgKey(organizationId, "datasets", "members", id),
    enabled: Boolean(organizationId),
    fetchPage: async (cursor, signal) => {
      const page = await api.listMembers(id, cursor, signal);
      return { items: page.members, next: page.next_cursor };
    },
  });
}

export function useAddMembers(id: string) {
  const refresh = useRefreshDatasets();
  return useMutation({
    mutationFn: (input: { members: MemberRequest[]; onDuplicate: DuplicatePolicy }) =>
      api.addMembers(id, input.members, input.onDuplicate),
    onSuccess: refresh,
  });
}

export function useRemoveMember(id: string) {
  const refresh = useRefreshDatasets();
  return useMutation({ mutationFn: (memberId: string) => api.removeMember(id, memberId), onSuccess: refresh });
}

export function useCreateSnapshot(id: string) {
  const refresh = useRefreshDatasets();
  return useMutation({
    mutationFn: (key: string) => api.createSnapshot(id, key),
    onSuccess: refresh,
  });
}

export function useVersions(id: string) {
  const organizationId = useOrganizationId();
  return usePagedQuery<Awaited<ReturnType<typeof api.listVersions>>["versions"][number], number>({
    queryKey: orgKey(organizationId, "datasets", "versions", id),
    enabled: Boolean(organizationId),
    fetchPage: async (before, signal) => {
      const page = await api.listVersions(id, before, signal);
      return { items: page.versions, next: page.next_before };
    },
  });
}

export function useVersion(id: string, versionId: string, enabled = true) {
  const organizationId = useOrganizationId();
  return useQuery({
    queryKey: orgKey(organizationId, "datasets", "version", id, versionId),
    queryFn: ({ signal }) => api.getVersion(id, versionId, signal),
    enabled: Boolean(organizationId) && enabled,
  });
}

export function useVersionMembers(id: string, versionId: string) {
  const organizationId = useOrganizationId();
  return usePagedQuery<Awaited<ReturnType<typeof api.listVersionMembers>>["members"][number], number>({
    queryKey: orgKey(organizationId, "datasets", "version-members", id, versionId),
    enabled: Boolean(organizationId),
    fetchPage: async (after, signal) => {
      const page = await api.listVersionMembers(id, versionId, after, signal);
      return { items: page.members, next: page.next_after };
    },
  });
}

export function useImport(id: string, jobId: string) {
  const organizationId = useOrganizationId();
  return useQuery({
    queryKey: orgKey(organizationId, "datasets", "import", id, jobId),
    queryFn: ({ signal }) => api.getImport(id, jobId, signal),
    enabled: Boolean(organizationId),
    refetchInterval: (query) => {
      const state = query.state.data?.state;
      return state && TERMINAL_IMPORT_STATES.includes(state) ? false : IMPORT_POLL_MS;
    },
  });
}

export function useImportErrors(id: string, jobId: string) {
  const organizationId = useOrganizationId();
  return usePagedQuery<Awaited<ReturnType<typeof api.listImportErrors>>["errors"][number], number>({
    queryKey: orgKey(organizationId, "datasets", "import-errors", id, jobId),
    enabled: Boolean(organizationId),
    fetchPage: async (after, signal) => {
      const page = await api.listImportErrors(id, jobId, after, signal);
      return { items: page.errors, next: page.next_after };
    },
  });
}

export function useCancelImport(id: string, jobId: string) {
  const refresh = useRefreshDatasets();
  return useMutation({ mutationFn: () => api.cancelImport(id, jobId), onSuccess: refresh });
}

export function useCatalogSearch(prefix: string) {
  const organizationId = useOrganizationId();
  return usePagedQuery({
    queryKey: orgKey(organizationId, "datasets", "catalog-search", prefix),
    enabled: Boolean(organizationId),
    fetchPage: async (cursor, signal) => {
      const page = await api.searchCatalog(prefix, cursor, signal);
      return { items: page.objects, next: page.next_cursor };
    },
  });
}

export function useValidationProfiles() {
  const organizationId = useOrganizationId();
  return useQuery({
    queryKey: orgKey(organizationId, "datasets", "profiles"),
    queryFn: ({ signal }) => api.listProfiles(signal),
    enabled: Boolean(organizationId),
    staleTime: 5 * 60_000,
  });
}

// The summary comes with every page; the differences page by object.
export function useVersionDiff(id: string, versionId: string, against: string) {
  const organizationId = useOrganizationId();
  const query = useInfiniteQuery({
    queryKey: orgKey(organizationId, "datasets", "diff", id, versionId, against),
    enabled: Boolean(organizationId) && Boolean(against),
    initialPageParam: undefined as string | undefined,
    queryFn: ({ pageParam, signal }) => api.diffVersions(id, versionId, against, pageParam, signal),
    getNextPageParam: (last) => last.next_after || undefined,
  });
  return {
    ...query,
    summary: query.data?.pages[0]?.summary,
    items: query.data?.pages.flatMap((page) => page.differences) ?? [],
  };
}

export function useBranches(id: string, enabled = true) {
  const organizationId = useOrganizationId();
  return useQuery({
    queryKey: orgKey(organizationId, "datasets", "branches", id),
    queryFn: ({ signal }) => api.listBranches(id, signal).then((page) => page.branches),
    enabled: Boolean(organizationId) && enabled,
  });
}

export function useMerges(id: string) {
  const organizationId = useOrganizationId();
  return useQuery({
    queryKey: orgKey(organizationId, "datasets", "merges", id),
    queryFn: ({ signal }) => api.listMerges(id, signal).then((page) => page.merges),
    enabled: Boolean(organizationId),
  });
}

export function useCreateBranch(id: string) {
  const refresh = useRefreshDatasets();
  return useMutation({
    mutationFn: (input: { name: string; fromVersionId: string }) => api.createBranch(id, input.name, input.fromVersionId),
    onSuccess: refresh,
  });
}

export function useMerge(targetId: string) {
  const refresh = useRefreshDatasets();
  return useMutation({
    mutationFn: (body: Parameters<typeof api.merge>[1]) => api.merge(targetId, body),
    onSuccess: (result) => {
      if (result.applied) return refresh();
    },
  });
}

export function useTrainingRuns(versionId: string) {
  const organizationId = useOrganizationId();
  return usePagedQuery({
    queryKey: orgKey(organizationId, "datasets", "training-runs", versionId),
    enabled: Boolean(organizationId),
    fetchPage: async (cursor, signal) => {
      const page = await api.listTrainingRuns(versionId, cursor, signal);
      return { items: page.runs, next: page.next_cursor || undefined };
    },
  });
}
