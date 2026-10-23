import { api } from "@/lib/api/client";
import type { Direction, LineageResponse } from "./types";

type Params = { datasetId: string; versionId?: string; direction: Direction; depth: number };

export function getLineage({ datasetId, versionId, direction, depth }: Params, cursor: string | undefined, signal?: AbortSignal) {
  const path = versionId ? `/dataset-versions/${versionId}/lineage` : `/datasets/${datasetId}/lineage`;
  return api<LineageResponse>(path, { query: { direction, depth, limit: 100, cursor }, signal });
}

export const getDatasetName = (id: string, signal?: AbortSignal) =>
  api<{ name: string }>(`/datasets/${id}`, { signal }).then((d) => d.name);

export const getVersionNumber = (datasetId: string, versionId: string, signal?: AbortSignal) =>
  api<{ version: number }>(`/datasets/${datasetId}/versions/${versionId}`, { signal }).then((v) => v.version);
