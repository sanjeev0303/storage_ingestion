"use client";

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useOrganizationId } from "@/lib/auth/session";
import { orgKey } from "@/lib/query/keys";
import { usePagedQuery } from "@/lib/query/paged";
import {
  cancelJob,
  createProfile,
  getDataset,
  getJob,
  getProfile,
  getReport,
  listDatasets,
  listDatasetVersions,
  listFindings,
  listJobs,
  listProfiles,
  listReadinessReports,
  startReadiness,
  startValidation,
} from "./api";
import { isTerminal } from "./status";
import type { DatasetSummary, DatasetVersionSummary, Finding, FindingFilters, ValidationJob, ValidationReport, ValidationRequest } from "./types";

export const POLL_MS = 2500;

export function useValidationDataset(datasetId: string) {
  const orgId = useOrganizationId();
  return useQuery({
    queryKey: orgKey(orgId, "validation", "dataset", datasetId),
    enabled: Boolean(orgId),
    queryFn: ({ signal }) => getDataset(datasetId, signal),
  });
}

export function useDatasetPicker() {
  const orgId = useOrganizationId();
  return usePagedQuery<DatasetSummary>({
    queryKey: orgKey(orgId, "validation", "dataset-picker"),
    enabled: Boolean(orgId),
    fetchPage: async (cursor, signal) => {
      const page = await listDatasets(cursor, signal);
      return { items: page.datasets, next: page.next_cursor };
    },
  });
}

export function useVersions(datasetId: string) {
  const orgId = useOrganizationId();
  return usePagedQuery<DatasetVersionSummary, number>({
    queryKey: orgKey(orgId, "validation", "versions", datasetId),
    enabled: Boolean(orgId),
    fetchPage: async (before, signal) => {
      const page = await listDatasetVersions(datasetId, before, signal);
      return { items: page.versions, next: page.next_before || undefined };
    },
  });
}

export function useProfiles() {
  const orgId = useOrganizationId();
  return useQuery({
    queryKey: orgKey(orgId, "validation", "profiles"),
    enabled: Boolean(orgId),
    queryFn: ({ signal }) => listProfiles(signal),
  });
}

export function useProfile(id: string, version: number | undefined) {
  const orgId = useOrganizationId();
  return useQuery({
    queryKey: orgKey(orgId, "validation", "profile", id, version ?? "latest"),
    enabled: Boolean(orgId),
    queryFn: ({ signal }) => getProfile(id, version, signal),
  });
}

export function useCreateProfile() {
  const orgId = useOrganizationId();
  const client = useQueryClient();
  return useMutation({
    mutationFn: createProfile,
    onSuccess: () => client.invalidateQueries({ queryKey: orgKey(orgId, "validation", "profiles") }),
  });
}

export function useJobs(datasetId: string) {
  const orgId = useOrganizationId();
  return usePagedQuery<ValidationJob>({
    queryKey: orgKey(orgId, "validation", "jobs", datasetId),
    enabled: Boolean(orgId),
    fetchPage: async (cursor, signal) => {
      const page = await listJobs(datasetId, cursor, signal);
      return { items: page.jobs, next: page.next_cursor };
    },
  });
}

export function useReadinessReports(datasetId: string, all: boolean) {
  const orgId = useOrganizationId();
  return usePagedQuery<ValidationReport>({
    queryKey: orgKey(orgId, "validation", "reports", datasetId, all),
    enabled: Boolean(orgId),
    fetchPage: async (cursor, signal) => {
      const page = await listReadinessReports(datasetId, all, cursor, signal);
      return { items: page.reports, next: page.next_cursor };
    },
  });
}

export function useStartJob(datasetId: string, kind: "validation" | "readiness") {
  const orgId = useOrganizationId();
  const client = useQueryClient();
  return useMutation({
    mutationFn: (body: ValidationRequest) =>
      kind === "readiness" ? startReadiness(datasetId, body) : startValidation(datasetId, body),
    onSuccess: () => client.invalidateQueries({ queryKey: orgKey(orgId, "validation") }),
  });
}

// Polls until the job reaches a terminal state, then stops.
export function useJob(jobId: string) {
  const orgId = useOrganizationId();
  return useQuery({
    queryKey: orgKey(orgId, "validation", "job", jobId),
    enabled: Boolean(orgId),
    queryFn: ({ signal }) => getJob(jobId, signal),
    refetchInterval: (query) => {
      const job = query.state.data;
      return job && isTerminal(job.state) ? false : POLL_MS;
    },
  });
}

export function useCancelJob(jobId: string) {
  const orgId = useOrganizationId();
  const client = useQueryClient();
  return useMutation({
    mutationFn: () => cancelJob(jobId),
    onSuccess: ({ job }) => {
      client.setQueryData(orgKey(orgId, "validation", "job", jobId), job);
      return client.invalidateQueries({ queryKey: orgKey(orgId, "validation", "jobs") });
    },
  });
}

export function useReport(reportId: string) {
  const orgId = useOrganizationId();
  return useQuery({
    queryKey: orgKey(orgId, "validation", "report", reportId),
    enabled: Boolean(orgId),
    queryFn: ({ signal }) => getReport(reportId, signal),
  });
}

export function useFindings(reportId: string, filters: FindingFilters) {
  const orgId = useOrganizationId();
  return usePagedQuery<Finding>({
    queryKey: orgKey(orgId, "validation", "findings", reportId, filters.severity, filters.rule_id),
    enabled: Boolean(orgId),
    fetchPage: async (cursor, signal) => {
      const page = await listFindings(reportId, filters, cursor, signal);
      return { items: page.findings, next: page.next_cursor };
    },
  });
}
