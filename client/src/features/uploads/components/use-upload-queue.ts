"use client";

import { useCallback, useEffect, useMemo, useRef, useState, useSyncExternalStore } from "react";
import { runPool } from "@/lib/upload/parts";
import type { StorageConnection } from "@/features/storage-connections/types";
import { draftErrors, draftFromRequest, hasErrors, newDraft, toRequest, type DraftErrors, type FileDraft } from "../draft";
import { FileUpload } from "../engine/file-upload";
import { indexedDbStore } from "../engine/store";
import type { ResumeRecord, UploadSnapshot } from "../types";

export type QueueEntry = { id: string; task: FileUpload; draft: FileDraft };

const FILES_AT_ONCE = 2;
const WORKING: UploadSnapshot["state"][] = ["hashing", "uploading", "finalizing"];

function sameList(a: UploadSnapshot[], b: UploadSnapshot[]) {
  return a.length === b.length && a.every((item, i) => item === b[i]);
}

function useSnapshots(tasks: FileUpload[]) {
  const cache = useRef<UploadSnapshot[]>([]);
  const subscribe = useCallback(
    (onChange: () => void) => {
      const stops = tasks.map((task) => task.subscribe(onChange));
      return () => stops.forEach((stop) => stop());
    },
    [tasks],
  );
  const getSnapshot = () => {
    const next = tasks.map((task) => task.getSnapshot());
    if (sameList(next, cache.current)) return cache.current;
    cache.current = next;
    return next;
  };
  return useSyncExternalStore(subscribe, getSnapshot, getSnapshot);
}

type Context = {
  connections: StorageConnection[];
  defaultConnectionId: string;
  maxObjectBytes?: number;
};

export function useUploadQueue({ connections, defaultConnectionId, maxObjectBytes }: Context) {
  const store = useMemo(() => indexedDbStore(), []);
  const [entries, setEntries] = useState<QueueEntry[]>([]);
  const [chosenDestination, setDestination] = useState<string | null>(null);
  // The default arrives with the connections list, after the first render.
  const destination = chosenDestination ?? defaultConnectionId;

  const tasks = useMemo(() => entries.map((entry) => entry.task), [entries]);
  const snapshots = useSnapshots(tasks);

  useEffect(() => {
    if (!snapshots.some((snap) => WORKING.includes(snap.state))) return;
    const warn = (event: BeforeUnloadEvent) => {
      event.preventDefault();
      event.returnValue = "";
    };
    window.addEventListener("beforeunload", warn);
    return () => window.removeEventListener("beforeunload", warn);
  }, [snapshots]);

  const errors = useMemo(() => {
    const result = new Map<string, DraftErrors>();
    for (const entry of entries) result.set(entry.id, draftErrors(entry.draft, entry.task.file, maxObjectBytes));
    return result;
  }, [entries, maxObjectBytes]);

  const connectionFor = useCallback((id: string) => connections.find((c) => c.id === id), [connections]);

  const addFiles = useCallback(
    (files: File[]) => {
      const added = files.map((file) => {
        const draft = newDraft(destination);
        const task = new FileUpload(file, toRequest(file, draft, defaultConnectionId), { store });
        return { id: task.getSnapshot().id, task, draft };
      });
      setEntries((current) => [...current, ...added]);
    },
    [destination, defaultConnectionId, store],
  );

  const updateDraft = useCallback((id: string, change: Partial<FileDraft>) => {
    setEntries((current) => current.map((entry) => (entry.id === id ? { ...entry, draft: { ...entry.draft, ...change } } : entry)));
  }, []);

  const remove = useCallback((id: string) => {
    setEntries((current) => current.filter((entry) => entry.id !== id));
  }, []);

  const clearFinished = useCallback(() => {
    const done = ["available", "rejected", "failed", "cancelled"];
    setEntries((current) => current.filter((entry) => !done.includes(entry.task.getSnapshot().state)));
  }, []);

  const configure = useCallback(
    (task: FileUpload, connectionId: string) => {
      const capabilities = connectionFor(connectionId)?.capabilities;
      task.configure({
        hasMultipart: capabilities?.multipart ?? true,
        requireEtag: capabilities?.part_validator !== "BLOCK_ID",
      });
    },
    [connectionFor],
  );

  const ready = entries.filter((entry) => entry.task.getSnapshot().state === "queued");
  const startable = ready.filter((entry) => !hasErrors(errors.get(entry.id) ?? {}));

  const start = useCallback(() => {
    for (const entry of startable) {
      entry.task.setRequest(toRequest(entry.task.file, entry.draft, defaultConnectionId));
      configure(entry.task, entry.draft.connectionId || defaultConnectionId);
    }
    void runPool(startable, FILES_AT_ONCE, (entry) => entry.task.run());
  }, [startable, defaultConnectionId, configure]);

  // Continue a multipart upload from its saved record with the file picked again.
  const resume = useCallback(
    (file: File, record: ResumeRecord) => {
      const task = new FileUpload(file, record.request, { store });
      configure(task, record.request.storage_connection_id ?? defaultConnectionId);
      const entry = { id: task.getSnapshot().id, task, draft: draftFromRequest(record.request, defaultConnectionId) };
      setEntries((current) => [...current, entry]);
      void task.run(record);
    },
    [store, configure, defaultConnectionId],
  );

  return {
    store,
    entries,
    snapshots,
    errors,
    destination,
    setDestination,
    addFiles,
    updateDraft,
    remove,
    clearFinished,
    start,
    startableCount: startable.length,
    waitingCount: ready.length,
    resume,
  };
}
