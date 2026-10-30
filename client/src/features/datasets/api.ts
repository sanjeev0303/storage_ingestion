import { api, apiRaw } from "@/lib/api/client";
import { isApiError } from "@/lib/api/errors";
import type {
  AddMembersResponse,
  CatalogObjectPage,
  CreateDatasetRequest,
  CreateDatasetResult,
  Dataset,
  DatasetPage,
  DatasetVersion,
  DuplicatePolicy,
  ImportErrorPage,
  ManifestImport,
  MemberPage,
  MergeRequest,
  MergeResult,
  TrainingRun,
  MemberRequest,
  UpdateDatasetRequest,
  ValidationProfileRef,
  VersionDiff,
  VersionMemberPage,
  VersionPage,
} from "./types";

const SNAPSHOT_TIMEOUT_MS = 10 * 60_000;
const NDJSON = "application/x-ndjson";

async function readJson<T>(res: Response): Promise<T> {
  return (await res.json()) as T;
}

export function listDatasets(params: { status?: string; cursor?: string }, signal?: AbortSignal) {
  return api<DatasetPage>("/datasets", { query: { status: params.status, cursor: params.cursor }, signal });
}

export function getDataset(id: string, signal?: AbortSignal) {
  return api<Dataset>(`/datasets/${id}`, { signal });
}

export async function createDataset(body: CreateDatasetRequest): Promise<CreateDatasetResult> {
  const res = await apiRaw("/datasets", { method: "POST", body });
  return { dataset: await readJson<Dataset>(res), created: res.status === 201 };
}

export function updateDataset(id: string, body: UpdateDatasetRequest) {
  return api<Dataset>(`/datasets/${id}`, { method: "PATCH", body });
}

export function archiveDataset(id: string) {
  return api<Dataset>(`/datasets/${id}/archive`, { method: "POST" });
}

export function listMembers(id: string, cursor: string | undefined, signal?: AbortSignal) {
  return api<MemberPage>(`/datasets/${id}/members`, { query: { cursor }, signal });
}

export function addMembers(id: string, members: MemberRequest[], onDuplicate: DuplicatePolicy) {
  return api<AddMembersResponse>(`/datasets/${id}/members`, {
    method: "POST",
    body: { on_duplicate: onDuplicate, members },
  });
}

// The API answers with the updated dataset, not 204.
export function removeMember(id: string, memberId: string) {
  return api<Dataset>(`/datasets/${id}/members/${memberId}`, { method: "DELETE" });
}

export async function createSnapshot(id: string, idempotencyKey: string) {
  const res = await apiRaw(`/datasets/${id}/snapshots`, {
    method: "POST",
    body: { idempotency_key: idempotencyKey },
    timeoutMs: SNAPSHOT_TIMEOUT_MS,
  });
  return { version: await readJson<DatasetVersion>(res), created: res.status === 201 };
}

export function listVersions(id: string, before: number | undefined, signal?: AbortSignal) {
  return api<VersionPage>(`/datasets/${id}/versions`, { query: { before }, signal });
}

export function getVersion(id: string, versionId: string, signal?: AbortSignal) {
  return api<DatasetVersion>(`/datasets/${id}/versions/${versionId}`, { signal });
}

export type VersionIntegrity = {
  degraded: boolean;
  objects: { path: string; kind: string; detail: string; detected_at: string }[];
};

// Whether a snapshot's members changed outside the platform since it was
// taken (docs/storage-posture.md).
export function getVersionIntegrity(id: string, versionId: string, signal?: AbortSignal) {
  return api<VersionIntegrity>(`/datasets/${id}/versions/${versionId}/integrity`, { signal });
}

export type VersionResidency = {
  allowed: string[];
  require_customer_key: boolean;
  inside: boolean;
  locations: {
    connection_id: string;
    location: string;
    location_verified: boolean;
    encryption_key?: string;
    members: number;
    bytes: number;
    allowed: boolean;
  }[];
};

// Where a snapshot's bytes are, against its dataset's rules (docs/residency.md).
export function getVersionResidency(id: string, versionId: string, signal?: AbortSignal) {
  return api<VersionResidency>(`/datasets/${id}/versions/${versionId}/residency`, { signal });
}

export function listVersionMembers(id: string, versionId: string, after: number | undefined, signal?: AbortSignal) {
  return api<VersionMemberPage>(`/datasets/${id}/versions/${versionId}/members`, { query: { after }, signal });
}

export function diffVersions(id: string, versionId: string, against: string, after: string | undefined, signal?: AbortSignal) {
  return api<VersionDiff>(`/datasets/${id}/versions/${versionId}/diff`, { query: { against, after, limit: 100 }, signal });
}

export function startInlineImport(id: string, file: Blob, key: string, policy: DuplicatePolicy, sha256?: string) {
  return api<ManifestImport>(`/datasets/${id}/manifests`, {
    method: "POST",
    query: { duplicate_policy: policy, sha256 },
    headers: { "Content-Type": NDJSON, "Idempotency-Key": key },
    body: file,
  });
}

export function startStagedImport(id: string, key: string, sizeBytes: number, policy: DuplicatePolicy, sha256?: string) {
  return api<ManifestImport>(`/datasets/${id}/manifests`, {
    method: "POST",
    headers: { "Idempotency-Key": key },
    body: { size_bytes: sizeBytes, sha256, duplicate_policy: policy, idempotency_key: key },
  });
}

export function submitImport(id: string, jobId: string) {
  return api<ManifestImport>(`/datasets/${id}/manifests/${jobId}/submit`, { method: "POST" });
}

export function getImport(id: string, jobId: string, signal?: AbortSignal) {
  return api<ManifestImport>(`/datasets/${id}/manifests/${jobId}`, { signal });
}

export function listImportErrors(id: string, jobId: string, after: number | undefined, signal?: AbortSignal) {
  return api<ImportErrorPage>(`/datasets/${id}/manifests/${jobId}/errors`, { query: { after, limit: 100 }, signal });
}

export async function cancelImport(id: string, jobId: string) {
  const res = await apiRaw(`/datasets/${id}/manifests/${jobId}/cancel`, { method: "POST" });
  return { job: await readJson<ManifestImport>(res), stopping: res.status === 202 };
}

export function searchCatalog(prefix: string, cursor: string | undefined, signal?: AbortSignal) {
  return api<CatalogObjectPage>("/catalog/objects", { query: { prefix, cursor, limit: 25 }, signal });
}

export function listProfiles(signal?: AbortSignal) {
  return api<{ profiles: ValidationProfileRef[] }>("/validation-profiles", { signal });
}

export function listBranches(id: string, signal?: AbortSignal) {
  return api<{ branches: Dataset[] }>(`/datasets/${id}/branches`, { signal });
}

export function createBranch(id: string, name: string, fromVersionId: string) {
  return api<Dataset>(`/datasets/${id}/branches`, { method: "POST", body: { name, from_version_id: fromVersionId } });
}

// A merge stopped by conflicts answers 409 with the conflicts; that is a
// result to show, not an error.
export async function merge(targetId: string, body: MergeRequest): Promise<MergeResult> {
  try {
    return await api<MergeResult>(`/datasets/${targetId}/merges`, { method: "POST", body });
  } catch (error) {
    if (isApiError(error) && error.code === "merge_conflict" && error.body) return error.body as MergeResult;
    throw error;
  }
}

export function listMerges(id: string, signal?: AbortSignal) {
  return api<{ merges: MergeResult[] }>(`/datasets/${id}/merges`, { signal });
}

export function listTrainingRuns(versionId: string, cursor: string | undefined, signal?: AbortSignal) {
  return api<{ runs: TrainingRun[]; next_cursor?: string }>("/training-runs", {
    query: { dataset_version_id: versionId, cursor },
    signal,
  });
}
