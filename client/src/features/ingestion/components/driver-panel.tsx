"use client";

import { Pause, Play } from "@phosphor-icons/react";
import { useRef, useState, type DragEvent } from "react";
import { Button } from "@/components/ui/button";
import { Panel } from "@/components/ui/panel";
import { Table, Td, Th } from "@/components/ui/table";
import { describeError } from "@/lib/api/errors";
import { formatBytes, formatNumber } from "@/lib/utils/format";
import { listEntries } from "../api";
import { useDriver, useJobProvider } from "../hooks";
import { BROWSER_CONCURRENCY_CAP } from "../lib/driver";
import { findFile, indexDroppedItems, indexPickedFolder, type FileIndex } from "../lib/files";
import type { IngestionEntry, IngestionJob } from "../types";

const CHECK_CAP = 5000;
const END_TEXT = {
  finished: "Nothing is waiting to be uploaded right now. Files that are processing or waiting to retry move on by themselves. Press Start again if failed entries return to pending.",
  paused: "Paused. Started uploads finished first.",
  job_not_running: "The job is not accepting uploads any more.",
  unmatched: "Ten claimed entries in a row had no matching local file, so the loop stopped instead of claiming the whole job. Attach the right folder and press Start again. The unmatched entries return to the queue when their 15 minute lease runs out.",
} as const;

type MatchCheck = { checked: number; matched: number; unmatched: IngestionEntry[]; unmatchedCount: number; wrongSize: number; capped: boolean };

async function checkMatches(jobId: string, index: FileIndex, signal: AbortSignal): Promise<MatchCheck> {
  const result: MatchCheck = { checked: 0, matched: 0, unmatched: [], unmatchedCount: 0, wrongSize: 0, capped: false };
  let after: number | undefined;
  while (result.checked < CHECK_CAP) {
    const page = await listEntries(jobId, "PENDING,FAILED_RETRYABLE", after, signal, 1000);
    for (const entry of page.entries ?? []) {
      result.checked++;
      const file = findFile(index, entry);
      if (!file) {
        result.unmatchedCount++;
        if (result.unmatched.length < 20) result.unmatched.push(entry);
      } else if (file.size !== entry.size_bytes) {
        result.wrongSize++;
      } else {
        result.matched++;
      }
    }
    if (page.next_after === undefined) return result;
    after = page.next_after;
  }
  result.capped = true;
  return result;
}

export function DriverPanel({ job, canWrite }: { job: IngestionJob; canWrite: boolean }) {
  const index = useRef<FileIndex>(new Map());
  const [fileCount, setFileCount] = useState(0);
  const [dragging, setDragging] = useState(false);
  const [check, setCheck] = useState<MatchCheck | null>(null);
  const [checking, setChecking] = useState(false);
  const [checkError, setCheckError] = useState<string | null>(null);
  const provider = useJobProvider(job);

  const concurrency = Math.min(BROWSER_CONCURRENCY_CAP, job.max_concurrency || 4);
  const { view, start, pause } = useDriver({
    jobId: job.id,
    concurrency,
    azure: provider.data === "AZURE_BLOB",
    getFile: (entry) => findFile(index.current, entry),
  });

  function attach(next: FileIndex) {
    index.current = next;
    setFileCount(next.size);
    setCheck(null);
  }

  async function onDrop(event: DragEvent) {
    event.preventDefault();
    setDragging(false);
    attach(await indexDroppedItems(event.dataTransfer.items));
  }

  async function runCheck() {
    setChecking(true);
    setCheckError(null);
    try {
      setCheck(await checkMatches(job.id, index.current, new AbortController().signal));
    } catch (e) {
      setCheckError(describeError(e));
    } finally {
      setChecking(false);
    }
  }

  const canStart = canWrite && fileCount > 0 && !view.running;

  return (
    <Panel
      title="Upload files"
      description="This browser tab uploads the files straight to storage. The server keeps all progress."
    >
      <div className="flex flex-col gap-5">
        <p className="max-w-prose text-sm text-muted">
          If you refresh or close the tab, uploads stop and nothing is lost on the server. Open this page again, attach the same folder and press Start. Entries that were mid-upload return to the queue when their 15 minute lease runs out.
        </p>

        <div
          onDragOver={(e) => {
            e.preventDefault();
            setDragging(true);
          }}
          onDragLeave={() => setDragging(false)}
          onDrop={onDrop}
          className={`flex flex-col gap-3 p-4 ${dragging ? "neo-in" : "neo-in-sm"}`}
        >
          <label htmlFor="attach-folder" className="text-sm font-medium">
            Folder with the files
          </label>
          <input
            id="attach-folder"
            type="file"
            multiple
            // Not in React's types, but every current browser supports it.
            {...{ webkitdirectory: "" }}
            disabled={view.running}
            onChange={(e) => attach(indexPickedFolder(Array.from(e.target.files ?? [])))}
            className="w-full text-base"
          />
          <p className="text-sm text-muted">
            Choose the folder the manifest paths are relative to, or drop it here. The folder&apos;s own name is ignored, so a manifest path images/cat.jpg matches
            the file at your-folder/images/cat.jpg.
          </p>
          <p role="status" className="text-sm font-medium">
            {fileCount === 0 ? "No files attached." : `${formatNumber(fileCount)} files attached.`}
          </p>
        </div>

        <div className="flex flex-wrap items-center gap-2">
          <Button onClick={runCheck} loading={checking} disabled={fileCount === 0 || view.running}>
            Check matches
          </Button>
          {view.running ? (
            <Button variant="primary" onClick={pause} disabled={view.pausing}>
              <Pause aria-hidden />
              {view.pausing ? "Pausing" : "Pause"}
            </Button>
          ) : (
            <Button variant="primary" onClick={start} disabled={!canStart}>
              <Play aria-hidden />
              {view.end === "paused" ? "Resume" : "Start uploading"}
            </Button>
          )}
          <span className="text-sm text-muted">Up to {concurrency} files at a time.</span>
        </div>
        {!canWrite && <p className="text-sm text-muted">Your role cannot upload. It needs ingestion.write.</p>}
        {provider.data === "AZURE_BLOB" && (
          <p className="text-sm text-muted">
            This dataset stores on Azure Blob, which needs an extra header (x-ms-blob-type) on small uploads. The ingestion API does not hand that header out, so this page adds it itself.
          </p>
        )}

        {checkError && (
          <p role="alert" className="font-medium">
            {checkError}
          </p>
        )}
        {check && (
          <div role="status" className="flex flex-col gap-2 text-sm">
            <p>
              Of {formatNumber(check.checked)} waiting entries{check.capped ? " (first 5,000 checked)" : ""}: {formatNumber(check.matched)} match a local file
              {check.wrongSize > 0 ? `, ${formatNumber(check.wrongSize)} match by name but have a different size` : ""}
              {check.unmatchedCount > 0 ? `, ${formatNumber(check.unmatchedCount)} have no local file` : ""}.
            </p>
            {check.unmatched.length > 0 && (
              <ul className="mono-data list-disc pl-5">
                {check.unmatched.map((entry) => (
                  <li key={entry.position} className="break-all">
                    {entry.path}
                  </li>
                ))}
              </ul>
            )}
          </div>
        )}

        {view.fatal && (
          <p role="alert" className="font-medium">
            Uploading stopped: {view.fatal}
          </p>
        )}
        {view.note && (
          <p role="status" className="text-sm font-medium">
            {view.note}
          </p>
        )}
        {view.end && !view.running && <p className="text-sm">{END_TEXT[view.end]}</p>}

        {(view.running || view.uploaded > 0 || view.failed > 0 || view.skipped > 0) && (
          <p className="text-sm tabular-nums" aria-live="polite">
            This session: {formatNumber(view.uploaded)} sent, {formatNumber(view.failed)} failed, {formatNumber(view.skipped)} skipped.
          </p>
        )}

        {view.active.length > 0 && (
          <Table>
            <caption className="sr-only">Files being uploaded now</caption>
            <thead>
              <tr>
                <Th>Entry</Th>
                <Th>Path</Th>
                <Th>Kind</Th>
                <Th className="text-right">Sent</Th>
              </tr>
            </thead>
            <tbody>
              {view.active.map((item) => (
                <tr key={item.position}>
                  <Td className="tabular-nums">{item.position}</Td>
                  <Td className="mono-data min-w-40 max-w-md break-all">{item.path}</Td>
                  <Td>{item.strategy === "MULTIPART" ? "Large, in parts" : "Single request"}</Td>
                  <Td className="text-right tabular-nums">
                    {formatBytes(item.sent)} of {formatBytes(item.size)}
                  </Td>
                </tr>
              ))}
            </tbody>
          </Table>
        )}

        {view.problems.length > 0 && (
          <div>
            <h3 className="mb-2 text-md font-semibold">Problems this session</h3>
            <ul className="flex flex-col gap-1 text-sm">
              {view.problems.map((problem, i) => (
                <li key={`${problem.position}-${i}`}>
                  <span className="tabular-nums">Entry {problem.position}:</span> {problem.message}
                </li>
              ))}
            </ul>
            <p className="mt-2 text-sm text-muted">One failed file never stops the others. Failed entries are listed under Failures below.</p>
          </div>
        )}
      </div>
    </Panel>
  );
}
