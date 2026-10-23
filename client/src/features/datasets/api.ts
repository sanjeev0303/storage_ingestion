import { api, apiRaw } from "@/lib/api/client";
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
  MemberRequest,
  UpdateDatasetRequest,
  ValidationProfileRef,
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

export function listVersionMembers(id: string, versionId: string, after: number | undefined, signal?: AbortSignal) {
  return api<VersionMemberPage>(`/datasets/${id}/versions/${versionId}/members`, { query: { after }, signal });
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
