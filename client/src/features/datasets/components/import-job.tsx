"use client";

import Link from "next/link";
import { useEffect, useState } from "react";
import { Button } from "@/components/ui/button";
import { ConfirmDialog } from "@/components/ui/dialog";
import { DefList, LoadMore } from "@/components/ui/page";
import { Panel } from "@/components/ui/panel";
import { ErrorState, SkeletonRows } from "@/components/ui/states";
import { StateBadge } from "@/components/ui/status";
import { Table, Td, Th } from "@/components/ui/table";
import { Time } from "@/components/ui/time";
import { useSession } from "@/lib/auth/session";
import { formatBytes, formatNumber } from "@/lib/utils/format";
import { ApiError } from "@/lib/api/errors";
import { isCode } from "../helpers";
import { useCancelImport, useImport, useImportErrors } from "../hooks";
import { TERMINAL_IMPORT_STATES, type ImportState } from "../types";

const stateLabels: Record<ImportState, string> = {
  AWAITING_UPLOAD: "Waiting for upload",
  QUEUED: "Queued",
  RUNNING: "Running",
  COMPLETED: "Completed",
  FAILED: "Failed",
  CANCELLED: "Cancelled",
};

function stateTone(state: ImportState) {
  if (state === "FAILED") return "attention" as const;
  if (state === "COMPLETED") return "done" as const;
  if (state === "CANCELLED") return "neutral" as const;
  return "active" as const;
}

export function ImportJob({ id, jobId }: { id: string; jobId: string }) {
  const { can } = useSession();
  const job = useImport(id, jobId);
  const cancel = useCancelImport(id, jobId);
  const [confirming, setConfirming] = useState(false);
  const [stopping, setStopping] = useState(false);

  if (job.isPending) return <SkeletonRows rows={5} />;
  if (job.isError && !job.data) return <ErrorState error={job.error} onRetry={() => job.refetch()} />;

  const data = job.data;
  const terminal = TERMINAL_IMPORT_STATES.includes(data.state);
  const p = data.progress;

  return (
    <div className="grid gap-6">
      <Panel
        title="Import"
        description={
          terminal
            ? "This import has finished."
            : "Checking again every 3 seconds. You can leave this page; the import keeps running."
        }
        actions={
          <>
            <Link href={`/app/datasets/${id}/imports`} className="inline-flex h-9 items-center text-base underline underline-offset-2">
              All imports
            </Link>
            {!terminal && can("datasets.write") && (
              <Button variant="accent" onClick={() => setConfirming(true)} disabled={data.cancel_requested || stopping}>
                {data.cancel_requested || stopping ? "Stopping" : "Cancel import"}
              </Button>
            )}
          </>
        }
      >
        <p className="mb-4">
          <StateBadge tone={stateTone(data.state)}>{stateLabels[data.state]}</StateBadge>
        </p>
        {job.isError && (
          <p role="alert" className="mb-3 text-base font-medium">
            The latest check failed, so these numbers may be out of date. It will try again.
          </p>
        )}
        {data.last_error && (
          <p role="alert" className="mb-4 text-base font-medium">
            {data.last_error}
          </p>
        )}
        <dl className="grid grid-cols-2 gap-4 sm:grid-cols-5">
          {[
            ["Lines read", p.lines_read],
            ["Members added", p.members_added],
            ["Members updated", p.members_updated],
            ["Members skipped", p.members_skipped],
            ["Lines rejected", p.lines_rejected],
          ].map(([label, value]) => (
            <div key={label} className="neo-in-sm p-3">
              <dt className="text-sm text-muted">{label}</dt>
              <dd className="mt-1 text-xl font-semibold tabular-nums">{formatNumber(value as number)}</dd>
            </div>
          ))}
        </dl>
        <div className="mt-5">
          <DefList
            items={[
              { label: "Source", value: data.source === "INLINE" ? "Inline request" : "Staged upload" },
              { label: "If a member exists", value: data.duplicate_policy },
              { label: "Manifest size", value: formatBytes(data.manifest_bytes) },
              {
                label: "Manifest SHA-256",
                value: data.manifest_sha256 ? <span className="mono-data">{data.manifest_sha256}</span> : "Not recorded",
              },
              { label: "Attempts", value: data.attempts },
              { label: "Started", value: <Time value={data.started_at ?? data.created_at} /> },
              ...(data.completed_at ? [{ label: "Finished", value: <Time value={data.completed_at} /> }] : []),
              { label: "Id", value: <span className="mono-data">{data.id}</span> },
            ]}
          />
        </div>
        {terminal && p.members_added + p.members_updated > 0 && (
          <p className="mt-4 text-base text-muted">
            These members are in the draft, not in any snapshot yet.{" "}
            <Link href={`/app/datasets/${id}/members`} className="underline underline-offset-2">
              Review the draft members
            </Link>
          </p>
        )}
      </Panel>

      <RejectedLines id={id} jobId={jobId} total={p.lines_rejected} />

      <ConfirmDialog
        open={confirming}
        onOpenChange={setConfirming}
        title="Cancel this import"
        description="Lines not yet read are not imported. Members this import already added stay in the draft."
        confirmLabel="Cancel import"
        onConfirm={async () => {
          try {
            const result = await cancel.mutateAsync();
            setStopping(result.stopping);
          } catch (error) {
            if (!isCode(error, "invalid_state")) throw error;
            job.refetch();
            throw new ApiError({ status: 409, code: "invalid_state", message: "This import has already finished, so there is nothing to cancel." });
          }
        }}
      />
    </div>
  );
}

function RejectedLines({ id, jobId, total }: { id: string; jobId: string; total: number }) {
  const errors = useImportErrors(id, jobId);
  const { refetch } = errors;

  // New rejected lines appear while the import runs.
  useEffect(() => {
    if (total > 0) refetch();
  }, [total, refetch]);

  return (
    <Panel
      title="Rejected lines"
      description={`${formatNumber(total)} rejected in total. The server keeps up to a limit of error rows; the count above stays exact.`}
    >
      {errors.isPending ? (
        <SkeletonRows rows={3} />
      ) : errors.isError && errors.items.length === 0 ? (
        <ErrorState error={errors.error} onRetry={() => errors.refetch()} />
      ) : errors.items.length === 0 ? (
        <p className="text-base text-muted">No rejected lines so far.</p>
      ) : (
        <>
          <Table>
            <thead>
              <tr>
                <Th className="text-right">Line</Th>
                <Th>Code</Th>
                <Th>Detail</Th>
              </tr>
            </thead>
            <tbody>
              {errors.items.map((error) => (
                <tr key={error.line}>
                  <Td className="text-right tabular-nums">{error.line}</Td>
                  <Td className="mono-data">{error.code}</Td>
                  <Td>{error.detail}</Td>
                </tr>
              ))}
            </tbody>
          </Table>
          <LoadMore
            hasNext={Boolean(errors.hasNextPage)}
            loading={errors.isFetchingNextPage}
            onLoad={() => errors.fetchNextPage()}
            shown={errors.items.length}
          />
        </>
      )}
    </Panel>
  );
}
