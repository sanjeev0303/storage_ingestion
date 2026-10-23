import Link from "next/link";
import type { ReactNode } from "react";
import { Outcome } from "@/components/ui/status";
import { StateBadge } from "@/components/ui/status";
import { Td, Th } from "@/components/ui/table";
import { formatBytes, formatNumber, shortId } from "@/lib/utils/format";
import { JOB_STATE_LABELS, type Mapped } from "../status";
import type { Count, JobState, ValidationJob } from "../types";

export function MappedOutcome({ mapped }: { mapped: Mapped }) {
  return <Outcome outcome={mapped.outcome} label={mapped.label} />;
}

const stateTone: Record<JobState, "neutral" | "attention" | "active" | "done"> = {
  QUEUED: "neutral",
  RUNNING: "active",
  COMPLETED: "done",
  COMPLETED_WITH_WARNINGS: "attention",
  FAILED: "attention",
  CANCELLED: "neutral",
};

export function JobStateBadge({ job }: { job: Pick<ValidationJob, "state" | "report_id"> }) {
  const label =
    job.state === "FAILED"
      ? job.report_id
        ? "Failed validation"
        : "Could not run"
      : JOB_STATE_LABELS[job.state];
  return <StateBadge tone={stateTone[job.state]}>{label}</StateBadge>;
}

export function ProgressBar({ label, value, max }: { label: string; value: number; max: number }) {
  const ratio = max > 0 ? Math.min(value / max, 1) : 0;
  return (
    <div
      role="progressbar"
      aria-label={label}
      aria-valuemin={0}
      aria-valuemax={max}
      aria-valuenow={Math.min(value, max)}
      className="neo-in-sm h-3 overflow-hidden p-0.5"
    >
      <div className="h-full origin-left bg-ink transition-transform duration-200 ease-out" style={{ transform: `scaleX(${ratio})` }} />
    </div>
  );
}

export function IdText({ id, href }: { id: string; href?: string }) {
  const text = <span className="mono-data text-sm">{shortId(id, 10)}</span>;
  return href ? (
    <Link href={href} title={id} className="underline underline-offset-2">
      {text}
    </Link>
  ) : (
    <span title={id}>{text}</span>
  );
}

export function SectionHeading({ children, id }: { children: ReactNode; id?: string }) {
  return (
    <h2 id={id} className="text-lg font-semibold">
      {children}
    </h2>
  );
}

// A table of counts with a simple bar, so the numbers stay readable without it.
export function CountTable({ caption, rows: rawRows, emptyText }: { caption: string; rows: Count[] | null; emptyText?: string }) {
  const rows = rawRows ?? [];
  const max = Math.max(1, ...rows.map((r) => r.count));
  return (
    <div>
      <h3 className="mb-1.5 text-sm font-semibold">{caption}</h3>
      {rows.length === 0 ? (
        <p className="text-sm text-muted">{emptyText ?? "None recorded."}</p>
      ) : (
        <table className="w-full border-collapse text-left text-sm">
          <caption className="sr-only">{caption}</caption>
          <thead>
            <tr>
              <Th className="px-2 py-1.5">Value</Th>
              <Th className="px-2 py-1.5 text-right">Count</Th>
              <Th className="w-1/3 px-2 py-1.5">
                <span className="sr-only">Share</span>
              </Th>
            </tr>
          </thead>
          <tbody>
            {rows.map((row) => (
              <tr key={row.value}>
                <Td className="px-2 py-1.5 [overflow-wrap:anywhere]">{row.value || "(empty)"}</Td>
                <Td className="px-2 py-1.5 text-right tabular-nums">{formatNumber(row.count)}</Td>
                <Td className="px-2 py-1.5" aria-hidden>
                  <div className="h-2 bg-faint">
                    <div className="h-full bg-ink" style={{ width: `${(row.count / max) * 100}%` }} />
                  </div>
                </Td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  );
}

export function jobProgressSummary(job: ValidationJob) {
  const p = job.progress;
  if (job.state === "QUEUED") return "Waiting for a worker";
  return `${formatNumber(p.processed)} of ${formatNumber(p.total)} members` + (p.bytes_read ? `, ${formatBytes(p.bytes_read)} read` : "");
}
