"use client";

import { useQueries } from "@tanstack/react-query";
import { useOrganizationId } from "@/lib/auth/session";
import { orgKey } from "@/lib/query/keys";
import { usePagedQuery } from "@/lib/query/paged";
import { getDatasetName, getLineage, getVersionNumber } from "./api";
import { KIND_LABELS, nodeKey } from "./graph";
import type { Edge, LineageQuery, LineageResponse, Ref } from "./types";
import { shortId } from "@/lib/utils/format";

export function useLineage(query: LineageQuery) {
  const orgId = useOrganizationId();
  const paged = usePagedQuery<LineageResponse>({
    queryKey: orgKey(orgId, "lineage", query.datasetId, query.versionId ?? "dataset", query.direction, query.depth),
    enabled: Boolean(orgId),
    fetchPage: async (cursor, signal) => {
      const page = await getLineage(query, cursor, signal);
      return { items: [page], next: page.next_cursor };
    },
  });

  const pages = paged.items;
  const edges: Edge[] = pages.flatMap((p) => p.edges);
  return {
    ...paged,
    start: pages[0]?.start,
    edges,
    truncated: pages[pages.length - 1]?.truncated ?? false,
  };
}

const MAX_LOOKUPS = 24;

// Friendly names, looked up lazily and only where it is one cheap request:
// a dataset id gives its current name, a version with a known dataset gives its number.
export function useNodeLabels(nodes: Ref[], datasetIds: Record<string, string>, fixed: Record<string, string>) {
  const orgId = useOrganizationId();
  const lookups = nodes
    .filter((n) => (n.kind === "DATASET" || (n.kind === "DATASET_VERSION" && datasetIds[nodeKey(n)])) && !fixed[nodeKey(n)])
    .slice(0, MAX_LOOKUPS);

  const results = useQueries({
    queries: lookups.map((n) => ({
      queryKey: orgKey(orgId, "lineage", "label", n.kind, n.id),
      enabled: Boolean(orgId),
      staleTime: 60_000,
      retry: false,
      queryFn: ({ signal }: { signal: AbortSignal }) =>
        n.kind === "DATASET" ? getDatasetName(n.id, signal) : getVersionNumber(datasetIds[nodeKey(n)], n.id, signal).then((v) => `Version ${v}`),
    })),
  });

  const labels: Record<string, string> = { ...fixed };
  lookups.forEach((n, i) => {
    const value = results[i]?.data;
    if (typeof value === "string") labels[nodeKey(n)] = value;
  });
  return labels;
}

export function nodeLabel(node: Ref, labels: Record<string, string>) {
  return labels[nodeKey(node)] ?? `${KIND_LABELS[node.kind]} ${shortId(node.id, 8)}`;
}
