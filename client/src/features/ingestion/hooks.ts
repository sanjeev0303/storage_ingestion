"use client";

import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useCallback, useEffect, useRef, useState } from "react";
import { describeError } from "@/lib/api/errors";
import { useOrganizationId } from "@/lib/auth/session";
import { orgKey } from "@/lib/query/keys";
import { usePagedQuery } from "@/lib/query/paged";
import { putBlob } from "@/lib/upload/put";
import { getQuota } from "@/features/organization/api";
import { getConnection, listConnections } from "@/features/storage-connections/api";
import {
  claimEntries,
  completeEntry,
  confirmEntryParts,
  entryPartUrls,
  entryUploadUrl,
  getDataset,
  getEntry,
  getJob,
  heartbeatEntry,
  listDatasets,
  listEntries,
  listFailures,
  listJobs,
} from "./api";
import { runDriver, type DriverEnd, type DriverEvent } from "./lib/driver";
import { isTerminal } from "./lib/explain";
import type { IngestionEntry, IngestionJob } from "./types";

export const POLL_MS = 3000;

export function useJob(id: string) {
  const organizationId = useOrganizationId();
  return useQuery({
    queryKey: orgKey(organizationId, "ingestion", "job", id),
    queryFn: ({ signal }) => getJob(id, signal),
    enabled: Boolean(organizationId),
    // react-query does not poll while the tab is hidden.
    refetchInterval: (query) => {
      const job = query.state.data;
      return job && isTerminal(job.state) ? false : POLL_MS;
    },
  });
}

export function useJobs(datasetId: string) {
  const organizationId = useOrganizationId();
  return usePagedQuery({
    queryKey: orgKey(organizationId, "ingestion", "jobs", datasetId),
    enabled: Boolean(organizationId),
    fetchPage: async (after, signal) => {
      const page = await listJobs(after, datasetId, signal);
      return { items: page.jobs ?? [], next: page.next_after };
    },
  });
}

export function useDatasets() {
  const organizationId = useOrganizationId();
  return usePagedQuery({
    queryKey: orgKey(organizationId, "ingestion", "datasets"),
    enabled: Boolean(organizationId),
    fetchPage: (cursor, signal) => listDatasets(cursor, signal),
  });
}

export function useManifestLimits() {
  const organizationId = useOrganizationId();
  return useQuery({
    queryKey: orgKey(organizationId, "ingestion", "quota"),
    queryFn: () => getQuota(),
    enabled: Boolean(organizationId),
    staleTime: 60_000,
    retry: false,
  });
}

export function useEntries(jobId: string, state: string) {
  const organizationId = useOrganizationId();
  return usePagedQuery<IngestionEntry, number>({
    queryKey: orgKey(organizationId, "ingestion", "entries", jobId, state),
    enabled: Boolean(organizationId),
    fetchPage: async (after, signal) => {
      const page = await listEntries(jobId, state, after, signal);
      return { items: page.entries ?? [], next: page.next_after };
    },
  });
}

export function useFailures(jobId: string) {
  const organizationId = useOrganizationId();
  return usePagedQuery<IngestionEntry, number>({
    queryKey: orgKey(organizationId, "ingestion", "failures", jobId),
    enabled: Boolean(organizationId),
    fetchPage: async (after, signal) => {
      const page = await listFailures(jobId, after, signal);
      return { items: page.entries ?? [], next: page.next_after };
    },
  });
}

export function useRefreshJob() {
  const client = useQueryClient();
  const organizationId = useOrganizationId();
  return useCallback(
    () => client.invalidateQueries({ queryKey: orgKey(organizationId, "ingestion") }),
    [client, organizationId],
  );
}

// Best effort: the grants do not name the provider, so ask the dataset's connection.
export function useJobProvider(job: IngestionJob | undefined) {
  const organizationId = useOrganizationId();
  return useQuery({
    queryKey: orgKey(organizationId, "ingestion", "provider", job?.dataset_id),
    enabled: Boolean(organizationId && job),
    retry: false,
    staleTime: 5 * 60_000,
    queryFn: async () => {
      const dataset = await getDataset(job?.dataset_id ?? "");
      if (dataset.storage_connection_id) return (await getConnection(dataset.storage_connection_id)).provider;
      const list = await listConnections();
      return list.connections.find((c) => c.id === list.default_connection_id)?.provider ?? null;
    },
  });
}

export type ActiveUpload = { position: number; path: string; size: number; sent: number; strategy: string };

export type DriverView = {
  running: boolean;
  pausing: boolean;
  active: ActiveUpload[];
  uploaded: number;
  failed: number;
  skipped: number;
  problems: { position: number; message: string }[];
  note: string | null;
  end: DriverEnd | null;
  fatal: string | null;
};

const idle: DriverView = {
  running: false,
  pausing: false,
  active: [],
  uploaded: 0,
  failed: 0,
  skipped: 0,
  problems: [],
  note: null,
  end: null,
  fatal: null,
};

type DriverInput = {
  jobId: string;
  concurrency: number;
  azure: boolean;
  getFile: (entry: IngestionEntry) => Blob | undefined;
};

export function useDriver({ jobId, concurrency, azure, getFile }: DriverInput) {
  const [view, setView] = useState<DriverView>(idle);
  const client = useQueryClient();
  const organizationId = useOrganizationId();
  const store = useRef<DriverView>(idle);
  const activeMap = useRef(new Map<number, ActiveUpload>());
  const pauseRequested = useRef(false);
  const controller = useRef<AbortController | null>(null);
  const getFileRef = useRef(getFile);
  useEffect(() => {
    getFileRef.current = getFile;
  });

  const flush = useCallback(() => {
    setView({ ...store.current, active: [...activeMap.current.values()] });
  }, []);

  function onEvent(event: DriverEvent) {
    const s = store.current;
    if (event.type === "started") {
      activeMap.current.set(event.position, { position: event.position, path: event.path, size: event.size, sent: 0, strategy: event.strategy });
    } else if (event.type === "progress") {
      const item = activeMap.current.get(event.position);
      if (item) item.sent = event.sent;
    } else if (event.type === "done") {
      activeMap.current.delete(event.position);
      s.uploaded++;
    } else if (event.type === "failed") {
      activeMap.current.delete(event.position);
      s.failed++;
      s.problems = [{ position: event.position, message: event.message }, ...s.problems].slice(0, 20);
    } else if (event.type === "skipped") {
      s.skipped++;
      s.problems = [{ position: event.position, message: event.message }, ...s.problems].slice(0, 20);
    } else if (event.type === "waiting") {
      s.note = `Waiting ${event.seconds} s: ${event.reason}`;
    } else if (event.type === "claimed" && event.count > 0) {
      s.note = null;
    }
  }

  const start = useCallback(() => {
    if (store.current.running) return;
    pauseRequested.current = false;
    activeMap.current.clear();
    const abort = new AbortController();
    controller.current = abort;
    store.current = { ...idle, running: true };
    flush();
    const timer = setInterval(flush, 300);

    runDriver({
      api: {
        claim: (max, signal) => claimEntries(jobId, max, signal),
        uploadUrl: (position) => entryUploadUrl(jobId, position),
        partUrls: (position, numbers) => entryPartUrls(jobId, position, numbers),
        confirmParts: (position, parts) => confirmEntryParts(jobId, position, parts),
        heartbeat: (position) => heartbeatEntry(jobId, position),
        complete: (position, parts) => completeEntry(jobId, position, parts),
        getEntry: (position) => getEntry(jobId, position),
      },
      put: putBlob,
      getFile: (entry) => getFileRef.current(entry),
      concurrency,
      azure,
      signal: abort.signal,
      shouldPause: () => pauseRequested.current,
      onEvent,
    })
      .then((end) => {
        store.current.end = end;
      })
      .catch((error) => {
        if (!abort.signal.aborted) store.current.fatal = describeError(error);
      })
      .finally(() => {
        clearInterval(timer);
        activeMap.current.clear();
        store.current = { ...store.current, running: false, pausing: false };
        flush();
        client.invalidateQueries({ queryKey: orgKey(organizationId, "ingestion") });
      });
  }, [jobId, concurrency, azure, flush, client, organizationId]);

  const pause = useCallback(() => {
    pauseRequested.current = true;
    store.current.pausing = true;
    flush();
  }, [flush]);

  useEffect(() => () => controller.current?.abort(), []);

  useEffect(() => {
    if (!view.running) return;
    const warn = (event: BeforeUnloadEvent) => event.preventDefault();
    window.addEventListener("beforeunload", warn);
    return () => window.removeEventListener("beforeunload", warn);
  }, [view.running]);

  return { view, start, pause };
}
