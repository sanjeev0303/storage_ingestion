"use client";

import { useQuery } from "@tanstack/react-query";
import { api } from "@/lib/api/client";
import { Panel } from "@/components/ui/panel";
import { ErrorState, SkeletonRows } from "@/components/ui/states";
import { Time } from "@/components/ui/time";
import { useSession } from "@/lib/auth/session";
import { orgKey } from "@/lib/query/keys";
import { formatBytes, formatNumber } from "@/lib/utils/format";

type WorkStatus = {
  at: string;
  processing: { waiting: number; held: number; oldest_wait_seconds: number };
  ingestion: { active_jobs: number; entries_waiting: number; entries_in_flight: number };
  imports: { active: number };
  validations: { queued: number; running: number };
  deletions: { pending: number };
  // Older APIs do not send it.
  storage?: { shared_assets: number; shared_bytes: number };
};

function waited(seconds: number) {
  if (seconds < 90) return `${seconds} s`;
  if (seconds < 5400) return `${Math.round(seconds / 60)} min`;
  return `${Math.round(seconds / 3600)} h`;
}

// The organization's background work, counted in one snapshot by the API.
// It answers "is my upload stuck, or just queued?".
export function WorkStatus() {
  const { organizationId } = useSession();
  const status = useQuery({
    queryKey: orgKey(organizationId, "operations", "status"),
    queryFn: ({ signal }) => api<WorkStatus>("/operations/status", { signal }),
    enabled: Boolean(organizationId),
    refetchInterval: 15_000,
  });

  if (status.isPending) {
    return (
      <Panel title="Background work">
        <SkeletonRows rows={3} />
      </Panel>
    );
  }
  if (status.isError) {
    return (
      <Panel title="Background work">
        <ErrorState error={status.error} onRetry={() => status.refetch()} />
      </Panel>
    );
  }

  const s = status.data;
  const rows = [
    {
      what: "Uploads waiting for verification and scanning",
      count: s.processing.waiting,
      note: s.processing.waiting > 0 ? `oldest has waited ${waited(s.processing.oldest_wait_seconds)}` : "",
    },
    {
      what: "Uploads held after their retries ran out",
      count: s.processing.held,
      note: s.processing.held > 0 ? "an operator can requeue them from the Assets tab" : "",
    },
    { what: "Bulk ingestion jobs running", count: s.ingestion.active_jobs, note: "" },
    { what: "Files waiting to be uploaded", count: s.ingestion.entries_waiting, note: "" },
    { what: "Files uploading or processing", count: s.ingestion.entries_in_flight, note: "" },
    { what: "Manifest imports in progress", count: s.imports.active, note: "" },
    { what: "Validations queued", count: s.validations.queued, note: "" },
    { what: "Validations running", count: s.validations.running, note: "" },
    { what: "Deletions not finished", count: s.deletions.pending, note: "" },
  ];
  if (s.storage) {
    rows.push({
      what: "Files stored once for several uploads",
      count: s.storage.shared_assets,
      note: s.storage.shared_assets > 0 ? `${formatBytes(s.storage.shared_bytes)} not stored twice` : "",
    });
  }

  return (
    <Panel
      title="Background work"
      description={
        <>
          This organization only, counted at <Time value={s.at} />. Refreshes every 15 seconds.
        </>
      }
    >
      <dl className="grid gap-x-8 gap-y-3 sm:grid-cols-2">
        {rows.map((row) => (
          <div key={row.what} className="flex items-baseline justify-between gap-4">
            <dt className="text-base">
              {row.what}
              {row.note && <span className="block text-sm text-muted">{row.note}</span>}
            </dt>
            <dd className={row.count === 0 ? "text-lg tabular-nums text-muted" : "text-lg font-semibold tabular-nums"}>
              {formatNumber(row.count)}
            </dd>
          </div>
        ))}
      </dl>
    </Panel>
  );
}
