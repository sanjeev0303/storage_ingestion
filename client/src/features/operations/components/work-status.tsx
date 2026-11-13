"use client";

import { useQuery } from "@tanstack/react-query";
import { api } from "@/lib/api/client";
import { Panel } from "@/components/ui/panel";
import { ErrorState, SkeletonRows } from "@/components/ui/states";
import { StateBadge } from "@/components/ui/status";
import { Table, Td, Th } from "@/components/ui/table";
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
  // Older APIs do not send these.
  storage?: { shared_assets: number; shared_bytes: number };
  workers?: WorkerHealth[];
};

// One kind of worker. instances is sent to platform administrators only.
type WorkerHealth = {
  kind: string;
  state: "RUNNING" | "STALE" | "NOT_SEEN";
  last_seen_at?: string;
  instances?: { instance: string; started_at: string; last_seen_at: string; stale: boolean }[];
};

const WORKERS: Record<string, { name: string; does: string }> = {
  upload: { name: "Upload", does: "Bulk ingestion: manifests, entry retries and cancellations" },
  processing: { name: "Processing", does: "Verifies and scans uploads, then publishes them" },
  packing: { name: "Packing", does: "Packs small files into larger stored objects" },
  dataset: { name: "Dataset", does: "Manifest imports, validations and readiness assessments" },
  outbox: { name: "Outbox", does: "Publishes events to the queue and reconciles them" },
  cleanup: { name: "Cleanup", does: "Scheduled deletions, retention and leftover files" },
  reconciliation: { name: "Reconciliation", does: "Checks stuck uploads and storage against the database" },
  compaction: { name: "Compaction", does: "Removes operational records past their retention" },
  webhook: { name: "Webhook", does: "Sends webhook deliveries" },
};

const WORKER_STATE = {
  RUNNING: { tone: "done", text: "Running" },
  STALE: { tone: "warning", text: "No recent heartbeat" },
  NOT_SEEN: { tone: "neutral", text: "Not seen" },
} as const;

// Both panels read the same snapshot; react-query shares the request.
function useWorkStatus() {
  const { organizationId } = useSession();
  return useQuery({
    queryKey: orgKey(organizationId, "operations", "status"),
    queryFn: ({ signal }) => api<WorkStatus>("/operations/status", { signal }),
    enabled: Boolean(organizationId),
    refetchInterval: 15_000,
  });
}

function waited(seconds: number) {
  if (seconds < 90) return `${seconds} s`;
  if (seconds < 5400) return `${Math.round(seconds / 60)} min`;
  return `${Math.round(seconds / 3600)} h`;
}

// The organization's background work, counted in one snapshot by the API.
// It answers "is my upload stuck, or just queued?".
export function WorkStatus() {
  const status = useWorkStatus();

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

// Whether each kind of worker is alive, from the heartbeat every worker
// process writes every 30 seconds. Workers serve every organization.
export function WorkerHealthPanel() {
  const status = useWorkStatus();
  const title = "Workers";

  if (status.isPending) {
    return (
      <Panel title={title}>
        <SkeletonRows rows={3} />
      </Panel>
    );
  }
  if (status.isError) {
    return (
      <Panel title={title}>
        <ErrorState error={status.error} onRetry={() => status.refetch()} />
      </Panel>
    );
  }
  const workers = status.data.workers;
  if (!workers) {
    return (
      <Panel title={title}>
        <p className="text-base text-muted">This API does not report worker heartbeats.</p>
      </Panel>
    );
  }
  const detailed = workers.some((w) => w.instances !== undefined);

  return (
    <Panel
      title={title}
      description="Each worker process writes a heartbeat every 30 seconds. A worker silent for 2 minutes shows no recent heartbeat. Workers are shared by every organization."
    >
      <Table>
        <caption className="sr-only">Worker heartbeats</caption>
        <thead>
          <tr>
            <Th>Worker</Th>
            <Th>State</Th>
            <Th>Last heartbeat</Th>
            {detailed && <Th>Processes</Th>}
          </tr>
        </thead>
        <tbody>
          {workers.map((w) => {
            const known = WORKERS[w.kind];
            const state = WORKER_STATE[w.state] ?? WORKER_STATE.NOT_SEEN;
            return (
              <tr key={w.kind}>
                <Td>
                  <span className="font-medium">{known?.name ?? w.kind}</span>
                  {known && <div className="text-sm text-muted">{known.does}</div>}
                </Td>
                <Td>
                  <StateBadge tone={state.tone}>{state.text}</StateBadge>
                </Td>
                <Td>{w.last_seen_at ? <Time value={w.last_seen_at} /> : <span className="text-muted">Never in the last day</span>}</Td>
                {detailed && (
                  <Td>
                    {w.instances && w.instances.length > 0 ? (
                      <ul className="flex flex-col gap-1">
                        {w.instances.map((i) => (
                          <li key={i.instance} className="text-sm">
                            <span className="mono-data">{i.instance}</span>
                            <span className="text-muted">
                              , started <Time value={i.started_at} />
                              {i.stale && (
                                <>
                                  , last heartbeat <Time value={i.last_seen_at} />
                                </>
                              )}
                            </span>
                          </li>
                        ))}
                      </ul>
                    ) : (
                      <span className="text-muted">None</span>
                    )}
                  </Td>
                )}
              </tr>
            );
          })}
        </tbody>
      </Table>
    </Panel>
  );
}
