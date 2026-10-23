"use client";

import Link from "next/link";
import { useState } from "react";
import { Button, ButtonLink } from "@/components/ui/button";
import { ConfirmDialog } from "@/components/ui/dialog";
import { CopyButton } from "@/components/ui/copy-button";
import { DefList, PageHeader } from "@/components/ui/page";
import { Panel } from "@/components/ui/panel";
import { ErrorState, SkeletonRows } from "@/components/ui/states";
import { Time } from "@/components/ui/time";
import { describeError } from "@/lib/api/errors";
import { useSession } from "@/lib/auth/session";
import { formatBytes, formatNumber } from "@/lib/utils/format";
import { useCancelJob, useJob } from "../hooks";
import { failedKind, isTerminal, MODE_LABELS } from "../status";
import type { ValidationJob } from "../types";
import { IdText, JobStateBadge, ProgressBar } from "./parts";

export function JobScreen({ jobId }: { jobId: string }) {
  const job = useJob(jobId);

  if (job.isPending) return <SkeletonRows rows={6} />;
  if (job.isError) return <ErrorState error={job.error} onRetry={() => job.refetch()} />;
  return <JobView job={job.data} />;
}

function JobOutcome({ job }: { job: ValidationJob }) {
  const kind = failedKind(job);
  const report = job.report_id ? (
    <Link href={`/app/validation/reports/${job.report_id}`} className="font-semibold underline underline-offset-2">
      Open the report
    </Link>
  ) : null;

  if (kind === "version_failed") {
    return (
      <Panel title="The version failed validation" tone="in">
        <p className="max-w-prose text-base">
          The job ran to the end and the report says this version did not meet the profile&apos;s thresholds. Nothing is wrong with the job itself. {report}
        </p>
      </Panel>
    );
  }
  if (kind === "could_not_run") {
    return (
      <Panel title="The job could not run" tone="in" role="alert">
        <p className="max-w-prose text-base">
          No report was written, so this says nothing about the data. The version was not judged. Start a new check once the cause is fixed.
        </p>
        <p className="mono-data mt-3 text-sm [overflow-wrap:anywhere]">{job.last_error ?? "The backend gave no reason."}</p>
      </Panel>
    );
  }
  if (job.state === "COMPLETED" || job.state === "COMPLETED_WITH_WARNINGS") {
    return (
      <Panel title={job.state === "COMPLETED" ? "Finished: ready" : "Finished: ready with warnings"} tone="in">
        <p className="max-w-prose text-base">{report}</p>
      </Panel>
    );
  }
  if (job.state === "CANCELLED") {
    return (
      <Panel title="Cancelled" tone="in">
        <p className="max-w-prose text-base">The job was stopped before it finished. No verdict was reached for this run.</p>
      </Panel>
    );
  }
  return null;
}

function JobView({ job }: { job: ValidationJob }) {
  const { can } = useSession();
  const cancel = useCancelJob(job.id);
  const [confirming, setConfirming] = useState(false);
  const [stopping, setStopping] = useState(false);
  const terminal = isTerminal(job.state);
  const p = job.progress;
  const polling = !terminal;

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        crumbs={[
          { label: "Validation", href: "/app/validation" },
          { label: "Datasets", href: "/app/datasets" },
          { label: `Job ${job.id.slice(0, 8)}` },
        ]}
        title={job.readiness ? "Readiness assessment job" : "Validation job"}
        description={
          <>
            <JobStateBadge job={job} />
            {polling && <span className="ml-2 text-sm">Checking the backend every few seconds.</span>}
          </>
        }
        actions={
          <>
            <ButtonLink href={`/app/datasets/${job.dataset_id}/validation`}>Back to the dataset</ButtonLink>
            {!terminal && (
              <Button onClick={() => setConfirming(true)} disabled={!can("datasets.write") || job.cancel_requested}>
                {job.cancel_requested ? "Stopping" : "Cancel job"}
              </Button>
            )}
          </>
        }
      />

      <div aria-live="polite">
        {(job.cancel_requested || stopping) && !terminal && (
          <p className="mb-3 text-base font-medium">Cancel requested. The worker stops after its current batch.</p>
        )}
        <JobOutcome job={job} />
      </div>

      <Panel title="Progress" description={terminal ? "Final counts." : "Counts update as members are processed."}>
        <div className="flex flex-col gap-4">
          <ProgressBar label="Members processed" value={p.processed} max={p.total} />
          <p className="text-base tabular-nums">
            {formatNumber(p.processed)} of {formatNumber(p.total)} members processed
          </p>
          <DefList
            items={[
              { label: "Passed", value: formatNumber(p.passed) },
              { label: "Warnings", value: formatNumber(p.warnings) },
              { label: "Failed", value: formatNumber(p.failed) },
              { label: "Content examined", value: formatNumber(p.content_examined) },
              { label: "Content not examined", value: formatNumber(p.content_not_examined) },
              { label: "Bytes in the version", value: formatBytes(p.bytes_total) },
              { label: "Bytes read", value: formatBytes(p.bytes_read) },
            ]}
          />
          {job.mode === "METADATA" && (
            <p className="max-w-prose text-sm text-muted">Metadata-only mode never reads content, so content examined stays at zero by design.</p>
          )}
        </div>
      </Panel>

      <Panel title="Job">
        <DefList
          items={[
            { label: "Mode", value: job.sample_size ? `${MODE_LABELS[job.mode]}, sample of ${formatNumber(job.sample_size)}` : MODE_LABELS[job.mode] },
            {
              label: "Profile",
              value: (
                <Link href={`/app/validation/profiles/${encodeURIComponent(job.profile_id)}?version=${job.profile_version}`} className="underline underline-offset-2">
                  <span className="mono-data text-sm">
                    {job.profile_id}@{job.profile_version}
                  </span>
                </Link>
              ),
            },
            { label: "Dataset", value: <IdText id={job.dataset_id} href={`/app/datasets/${job.dataset_id}`} /> },
            { label: "Version", value: <IdText id={job.version_id} href={`/app/datasets/${job.dataset_id}/versions/${job.version_id}`} /> },
            { label: "Attempts", value: `${job.attempts} of ${job.max_attempts}` },
            { label: "Created", value: <Time value={job.created_at} /> },
            { label: "Started", value: <Time value={job.started_at} /> },
            { label: "Completed", value: <Time value={job.completed_at} /> },
            {
              label: "Job id",
              value: (
                <span className="flex flex-wrap items-center gap-2">
                  <span className="mono-data text-sm">{job.id}</span>
                  <CopyButton value={job.id} />
                </span>
              ),
            },
          ]}
        />
        {job.last_error && failedKind(job) !== "could_not_run" && (
          <p className="mono-data mt-4 text-sm [overflow-wrap:anywhere]">Last error: {job.last_error}</p>
        )}
      </Panel>

      {cancel.isError && <p role="alert">{describeError(cancel.error)}</p>}

      <ConfirmDialog
        open={confirming}
        onOpenChange={setConfirming}
        title="Cancel this job?"
        description="The job stops and no verdict is written for this run. A running job finishes its current batch first. You can start a new check afterwards."
        confirmLabel="Cancel job"
        onConfirm={async () => {
          const result = await cancel.mutateAsync();
          setStopping(result.stopping);
        }}
      />
    </div>
  );
}
