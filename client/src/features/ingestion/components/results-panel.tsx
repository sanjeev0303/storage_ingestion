"use client";

import { DownloadSimple } from "@phosphor-icons/react";
import { useRef, useState } from "react";
import { Button } from "@/components/ui/button";
import { Field, Select } from "@/components/ui/field";
import { LoadMore } from "@/components/ui/page";
import { Panel } from "@/components/ui/panel";
import { EmptyState, ErrorState, SkeletonRows } from "@/components/ui/states";
import { Table, Td, Th } from "@/components/ui/table";
import { describeError } from "@/lib/api/errors";
import { formatBytes, formatNumber } from "@/lib/utils/format";
import { isAbort } from "@/lib/upload/sha256";
import { listFailures } from "../api";
import { useEntries, useFailures } from "../hooks";
import { ENTRY_STATES, entryLabel, explainError } from "../lib/explain";
import { EXPORT_CAP, exportFailuresCsv } from "../lib/export";
import type { IngestionEntry, Progress } from "../types";
import { EntryBadge } from "./badges";

const SEGMENTS: { key: string; label: string; count: (p: Progress) => number; fill: string }[] = [
  { key: "published", label: "Published", count: (p) => p.published, fill: "bg-ink" },
  { key: "processing", label: "Uploaded or processing", count: (p) => p.uploaded + p.processing, fill: "bg-muted" },
  { key: "uploading", label: "Scheduled or uploading", count: (p) => p.scheduled + p.uploading, fill: "bg-faint outline outline-1 -outline-offset-1 outline-ink" },
  { key: "pending", label: "Waiting", count: (p) => p.pending, fill: "bg-faint" },
  { key: "failed", label: "Failed", count: (p) => p.failed + p.failed_retryable, fill: "bg-accent" },
  { key: "invalid", label: "Invalid", count: (p) => p.invalid, fill: "bg-ground outline outline-2 -outline-offset-2 outline-ink" },
  { key: "cancelled", label: "Cancelled", count: (p) => p.cancelled, fill: "bg-ground outline outline-1 -outline-offset-1 outline-muted" },
];

export function ProgressSummary({ progress }: { progress: Progress }) {
  const total = progress.total;
  return (
    <div className="flex flex-col gap-4">
      <div>
        <p className="text-lg font-semibold tabular-nums">
          {formatNumber(progress.published)} of {formatNumber(progress.valid)} valid entries published ({progress.percent_published.toFixed(1)}%)
        </p>
        <p className="text-sm text-muted tabular-nums">
          {formatBytes(progress.stored_bytes)} stored of {formatBytes(progress.expected_bytes)} expected. {formatNumber(progress.invalid)} lines were invalid.
        </p>
      </div>
      <div
        role="img"
        aria-label={`Entry states: ${SEGMENTS.map((s) => `${s.label} ${s.count(progress)}`).join(", ")}`}
        className="neo-in-sm flex h-4 overflow-hidden p-0.5"
      >
        {total > 0 &&
          SEGMENTS.map((segment) => {
            const count = segment.count(progress);
            if (count === 0) return null;
            return <div key={segment.key} className={segment.fill} style={{ width: `${(count / total) * 100}%` }} />;
          })}
      </div>
      <ul className="grid grid-cols-2 gap-x-6 gap-y-1 text-sm sm:grid-cols-4">
        {SEGMENTS.map((segment) => (
          <li key={segment.key} className="flex items-center gap-2">
            <span aria-hidden className={`inline-block size-3 shrink-0 ${segment.fill}`} />
            <span>{segment.label}</span>
            <span className="ml-auto tabular-nums">{formatNumber(segment.count(progress))}</span>
          </li>
        ))}
      </ul>
    </div>
  );
}

function EntryRows({ entries, withError }: { entries: IngestionEntry[]; withError?: boolean }) {
  return (
    <Table>
      <thead>
        <tr>
          <Th>Entry</Th>
          <Th>Path</Th>
          <Th>State</Th>
          <Th className="text-right">Size</Th>
          <Th className="text-right">Attempts</Th>
          {withError && <Th>Why</Th>}
        </tr>
      </thead>
      <tbody>
        {entries.map((entry) => (
          <tr key={entry.position}>
            <Td className="tabular-nums">{entry.position}</Td>
            <Td className="mono-data max-w-md break-all">{entry.path ?? "No path"}</Td>
            <Td>
              <EntryBadge state={entry.state} />
            </Td>
            <Td className="text-right tabular-nums">{formatBytes(entry.size_bytes)}</Td>
            <Td className="text-right tabular-nums">{entry.attempts}</Td>
            {withError && (
              <Td className="min-w-72 whitespace-normal">
                <span className="mono-data">{entry.error_code}</span>
                <span className="block text-sm text-muted">{explainError(entry.error_code)}</span>
                {entry.error_detail && <span className="block text-sm">{entry.error_detail}</span>}
              </Td>
            )}
          </tr>
        ))}
      </tbody>
    </Table>
  );
}

export function EntriesTable({ jobId }: { jobId: string }) {
  const [state, setState] = useState("");
  const entries = useEntries(jobId, state);

  return (
    <Panel
      title="Entries"
      description="One row per manifest line, in manifest order."
      actions={<Button onClick={() => entries.refetch()}>Refresh</Button>}
    >
      <div className="flex flex-col gap-4">
        <Field label="State" className="max-w-xs">
          {(props) => (
            <Select {...props} value={state} onChange={(e) => setState(e.target.value)}>
              <option value="">All states</option>
              {ENTRY_STATES.map((s) => (
                <option key={s} value={s}>
                  {entryLabel(s)}
                </option>
              ))}
            </Select>
          )}
        </Field>
        {entries.isPending ? (
          <SkeletonRows />
        ) : entries.isError ? (
          <ErrorState error={entries.error} onRetry={() => entries.refetch()} />
        ) : entries.items.length === 0 ? (
          <EmptyState title="No entries here">Entries appear once the manifest is read. Change the state filter to see other entries.</EmptyState>
        ) : (
          <>
            <EntryRows entries={entries.items} withError={state === "FAILED" || state === "INVALID" || state === "FAILED_RETRYABLE"} />
            <LoadMore hasNext={Boolean(entries.hasNextPage)} loading={entries.isFetchingNextPage} onLoad={() => entries.fetchNextPage()} shown={entries.items.length} />
          </>
        )}
      </div>
    </Panel>
  );
}

export function FailuresTable({ jobId }: { jobId: string }) {
  const failures = useFailures(jobId);
  const [exporting, setExporting] = useState(false);
  const [rows, setRows] = useState(0);
  const [exportNote, setExportNote] = useState<string | null>(null);
  const abort = useRef<AbortController | null>(null);

  async function exportCsv() {
    const controller = new AbortController();
    abort.current = controller;
    setExporting(true);
    setRows(0);
    setExportNote(null);
    try {
      const { blob, rows: count, capped } = await exportFailuresCsv((after, limit) => listFailures(jobId, after, controller.signal, limit), {
        signal: controller.signal,
        onProgress: setRows,
      });
      const link = document.createElement("a");
      link.href = URL.createObjectURL(blob);
      link.download = `ingestion-${jobId}-failures.csv`;
      link.click();
      URL.revokeObjectURL(link.href);
      setExportNote(capped ? `Saved the first ${formatNumber(count)} failures. The export stops at ${formatNumber(EXPORT_CAP)}.` : `Saved ${formatNumber(count)} failures.`);
    } catch (e) {
      setExportNote(isAbort(e) ? "Export cancelled." : `Export failed: ${describeError(e)}`);
    } finally {
      setExporting(false);
    }
  }

  return (
    <Panel
      title="Failures"
      description="Invalid lines, failed entries and cancelled entries, with the reason."
      actions={
        exporting ? (
          <Button onClick={() => abort.current?.abort()}>Cancel export ({formatNumber(rows)} rows)</Button>
        ) : (
          <Button onClick={exportCsv}>
            <DownloadSimple aria-hidden />
            Export failures
          </Button>
        )
      }
    >
      <div className="flex flex-col gap-4">
        <p className="text-sm text-muted">
          The API has no error report download. Export pages through this list in your browser and saves a CSV, up to {formatNumber(EXPORT_CAP)} rows.
        </p>
        {exportNote && (
          <p role="status" className="text-sm font-medium">
            {exportNote}
          </p>
        )}
        {failures.isPending ? (
          <SkeletonRows />
        ) : failures.isError ? (
          <ErrorState error={failures.error} onRetry={() => failures.refetch()} />
        ) : failures.items.length === 0 ? (
          <EmptyState title="No failures">Nothing invalid, failed or cancelled so far.</EmptyState>
        ) : (
          <>
            <EntryRows entries={failures.items} withError />
            <LoadMore hasNext={Boolean(failures.hasNextPage)} loading={failures.isFetchingNextPage} onLoad={() => failures.fetchNextPage()} shown={failures.items.length} />
          </>
        )}
      </div>
    </Panel>
  );
}
