import { api, apiRaw } from "@/lib/api/client";
import type {
  CreateProfileRequest,
  DatasetSummary,
  DatasetVersionSummary,
  Finding,
  FindingFilters,
  ProfileList,
  ValidationJob,
  ValidationProfile,
  ValidationReport,
  ValidationRequest,
} from "./types";

export type StartResult = { job: ValidationJob; created: boolean };

export const getDataset = (id: string, signal?: AbortSignal) => api<DatasetSummary>(`/datasets/${id}`, { signal });

export const listDatasets = (cursor: string | undefined, signal?: AbortSignal) =>
  api<{ datasets: DatasetSummary[]; next_cursor?: string }>("/datasets", { query: { cursor, limit: 50 }, signal });

export const listDatasetVersions = (id: string, before: number | undefined, signal?: AbortSignal) =>
  api<{ versions: DatasetVersionSummary[]; next_before?: number }>(`/datasets/${id}/versions`, {
    query: { before, limit: 50 },
    signal,
  });

export const listProfiles = (signal?: AbortSignal) => api<ProfileList>("/validation-profiles", { signal });

export const getProfile = (id: string, version: number | undefined, signal?: AbortSignal) =>
  api<ValidationProfile>(`/validation-profiles/${encodeURIComponent(id)}`, { query: { version }, signal });

export const createProfile = (body: CreateProfileRequest) =>
  api<ValidationProfile>("/validation-profiles", { method: "POST", body });

// 202 means a new job, 200 means the same idempotency key was seen before.
async function startJob(path: string, body: ValidationRequest): Promise<StartResult> {
  const res = await apiRaw(path, { method: "POST", body });
  return { job: (await res.json()) as ValidationJob, created: res.status === 202 };
}

export const startValidation = (datasetId: string, body: ValidationRequest) =>
  startJob(`/datasets/${datasetId}/validations`, body);

export const startReadiness = (datasetId: string, body: ValidationRequest) =>
  startJob(`/datasets/${datasetId}/readiness-assessments`, body);

export const listJobs = (datasetId: string, cursor: string | undefined, signal?: AbortSignal) =>
  api<{ jobs: ValidationJob[]; next_cursor?: string }>(`/datasets/${datasetId}/validations`, {
    query: { cursor, limit: 25 },
    signal,
  });

export const listReadinessReports = (datasetId: string, all: boolean, cursor: string | undefined, signal?: AbortSignal) =>
  api<{ reports: ValidationReport[]; next_cursor?: string }>(`/datasets/${datasetId}/readiness-reports`, {
    query: { all: all ? "true" : undefined, cursor, limit: 25 },
    signal,
  });

export const getJob = (jobId: string, signal?: AbortSignal) =>
  api<ValidationJob>(`/validation-jobs/${jobId}`, { signal });

export async function cancelJob(jobId: string) {
  const res = await apiRaw(`/validation-jobs/${jobId}/cancel`, { method: "POST" });
  const job = (await res.json()) as ValidationJob;
  return { job, stopping: res.status === 202 };
}

export const getReport = (reportId: string, signal?: AbortSignal) =>
  api<ValidationReport>(`/validation-reports/${reportId}`, { signal });

export const listFindings = (
  reportId: string,
  filters: FindingFilters,
  cursor: string | undefined,
  signal?: AbortSignal,
) =>
  api<{ findings: Finding[]; next_cursor?: string }>(`/validation-reports/${reportId}/findings`, {
    query: { severity: filters.severity, rule_id: filters.rule_id, cursor, limit: 50 },
    signal,
  });
