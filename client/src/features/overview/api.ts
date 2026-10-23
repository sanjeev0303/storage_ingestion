import { api } from "@/lib/api/client";
import type {
  AuditEventSummary,
  ConnectionSummary,
  DatasetSummary,
  DeadLetterSummary,
  JobSummary,
} from "./types";

export const JOB_WINDOW = 50;
export const DATASET_LIMIT = 5;

export const getLatestJobs = (signal?: AbortSignal) =>
  api<{ jobs: JobSummary[] }>("/ingestion/jobs", { query: { limit: JOB_WINDOW }, signal });

export const getDatasetsByStatus = (status: string, signal?: AbortSignal) =>
  api<{ datasets: DatasetSummary[] }>("/datasets", { query: { status, limit: DATASET_LIMIT }, signal });

export const getConnections = (signal?: AbortSignal) =>
  api<{ connections: ConnectionSummary[]; default_connection_id: string }>("/storage-connections", { signal });

export const getRecentAudit = (signal?: AbortSignal) =>
  api<{ events: AuditEventSummary[] }>("/organization/audit", { query: { limit: 8 }, signal });

export const getOpenDeadLetters = (signal?: AbortSignal) =>
  api<{ dead_letters: DeadLetterSummary[] }>("/admin/dead-letters", {
    query: { status: "OPEN", limit: 5 },
    org: false,
    signal,
  });
