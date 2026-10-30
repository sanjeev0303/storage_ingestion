import { api, apiRaw } from "@/lib/api/client";
import type {
  ClaimResponse,
  Dataset,
  EntriesPage,
  IngestionEntry,
  IngestionJob,
  JobsPage,
  ManifestReport,
  PartRecord,
  PartUrl,
} from "./types";

const jobPath = (id: string) => `/ingestion/jobs/${id}`;
const entryPath = (id: string, position: number) => `${jobPath(id)}/entries/${position}`;

export const PAGE_SIZE = 50;

export const listJobs = (after: string | undefined, datasetId: string, signal?: AbortSignal) =>
  api<JobsPage>("/ingestion/jobs", { query: { dataset_id: datasetId, after, limit: PAGE_SIZE }, signal });

export const getJob = (id: string, signal?: AbortSignal) => api<IngestionJob>(jobPath(id), { signal });

export async function createJob(body: { dataset_id: string; idempotency_key: string; max_concurrency: number }) {
  const res = await apiRaw("/ingestion/jobs", { method: "POST", body });
  // 200 means this idempotency key already made a job; 201 means a new one.
  return { job: (await res.json()) as IngestionJob, existed: res.status === 200 };
}

type DatasetsPage = { datasets: Dataset[] | null; next_cursor?: string };
export async function listDatasets(cursor: string | undefined, signal?: AbortSignal) {
  const page = await api<DatasetsPage>("/datasets", { query: { limit: 100, cursor }, signal });
  return { items: page.datasets ?? [], next: page.next_cursor };
}

export const getDataset = (id: string) => api<Dataset>(`/datasets/${id}`);

export function putManifestInline(id: string, body: Blob, sha256?: string, signal?: AbortSignal) {
  return api<ManifestReport>(`${jobPath(id)}/manifest`, {
    method: "PUT",
    body,
    headers: sha256 ? { "X-Manifest-SHA256": sha256 } : {},
    timeoutMs: 10 * 60_000,
    signal,
  });
}

export const requestStagedUrl = (id: string, sizeBytes: number) =>
  api<{ upload_url: string; expires_at: string }>(`${jobPath(id)}/manifest/staged-url`, {
    method: "POST",
    body: { size_bytes: sizeBytes },
  });

export const submitStaged = (id: string, sha256?: string) =>
  api<IngestionJob>(`${jobPath(id)}/manifest/staged`, { method: "POST", body: sha256 ? { sha256 } : {} });

export const finalizeJob = (id: string) => api<IngestionJob>(`${jobPath(id)}/finalize`, { method: "POST" });

export const claimEntries = (id: string, max: number, signal?: AbortSignal) =>
  api<ClaimResponse>(`${jobPath(id)}/claims`, { method: "POST", body: { max }, signal });

export const getEntry = (id: string, position: number, signal?: AbortSignal) =>
  api<IngestionEntry>(entryPath(id, position), { signal });

export const entryUploadUrl = (id: string, position: number) =>
  api<{ entry: IngestionEntry; upload_url?: string; upload_headers?: Record<string, string> }>(`${entryPath(id, position)}/upload-url`, {
    method: "POST",
  });

export const entryPartUrls = (id: string, position: number, partNumbers: number[]) =>
  api<{ parts: PartUrl[] }>(`${entryPath(id, position)}/parts/urls`, {
    method: "POST",
    body: { part_numbers: partNumbers },
  });

export const confirmEntryParts = (id: string, position: number, parts: PartRecord[]) =>
  api<void>(`${entryPath(id, position)}/parts/confirm`, { method: "POST", body: { parts } });

export const heartbeatEntry = (id: string, position: number) =>
  api<IngestionEntry>(`${entryPath(id, position)}/heartbeat`, { method: "POST" });

export async function completeEntry(id: string, position: number, parts?: { part_number: number; etag: string }[]) {
  const res = await apiRaw(`${entryPath(id, position)}/complete`, {
    method: "POST",
    body: parts ? { parts } : {},
  });
  // 202 means the server is still confirming the upload; read the entry again.
  return { status: res.status, entry: (await res.json()) as IngestionEntry };
}

export const listEntries = (id: string, state: string, after: number | undefined, signal?: AbortSignal, limit = PAGE_SIZE) =>
  api<EntriesPage>(`${jobPath(id)}/entries`, { query: { state, after, limit }, signal });

export const listFailures = (id: string, after: number | undefined, signal?: AbortSignal, limit = PAGE_SIZE) =>
  api<EntriesPage>(`${jobPath(id)}/failures`, { query: { after, limit }, signal });

// The body is required: an empty body is a plain-text 400.
export const retryEntries = (id: string, selection: { positions: number[] } | { all: true }) =>
  api<{ retried: number; requeued: number }>(`${jobPath(id)}/retry`, { method: "POST", body: selection });

export const cancelJob = (id: string) => api<IngestionJob>(`${jobPath(id)}/cancel`, { method: "POST" });
