"use client";

import { PageHeader } from "@/components/ui/page";
import { Panel } from "@/components/ui/panel";
import { CopyButton } from "@/components/ui/copy-button";
import { DefList } from "@/components/ui/page";
import { ErrorState, SkeletonRows } from "@/components/ui/states";
import { Time } from "@/components/ui/time";
import { useSession } from "@/lib/auth/session";
import { formatBytes, formatNumber, shortId } from "@/lib/utils/format";
import { useDatasets, useJob } from "../hooks";
import { isTerminal } from "../lib/explain";
import { JobBadge } from "./badges";
import { DriverPanel } from "./driver-panel";
import { FinalizePanel, JobActions } from "./job-actions";
import { ManifestPanel } from "./manifest-panel";
import { EntriesTable, FailuresTable, ProgressSummary } from "./results-panel";

export function JobDetail({ id }: { id: string }) {
  const { can } = useSession();
  const job = useJob(id);
  const datasets = useDatasets();

  const crumbs = [
    { label: "Ingestion", href: "/app/ingestion/uploads" },
    { label: "Bulk ingestion", href: "/app/ingestion/jobs" },
    { label: shortId(id) },
  ];

  if (job.isPending) {
    return (
      <>
        <PageHeader title="Ingestion job" crumbs={crumbs} />
        <SkeletonRows />
      </>
    );
  }
  if (job.isError) {
    return (
      <>
        <PageHeader title="Ingestion job" crumbs={crumbs} />
        <ErrorState error={job.error} onRetry={() => job.refetch()} />
      </>
    );
  }

  const data = job.data;
  const datasetName = datasets.items.find((d) => d.id === data.dataset_id)?.name;
  const canWrite = can("ingestion.write");
  const uploading = data.state === "READY" || data.state === "RUNNING";
  const reading = data.state === "VALIDATING" && !data.manifest_complete;
  const hasEntries = data.progress.total > 0;

  return (
    <>
      <PageHeader
        title={
          <span className="flex flex-wrap items-center gap-3">
            Job <span className="mono-data">{shortId(id)}</span>
            <JobBadge state={data.state} />
          </span>
        }
        description={datasetName ? `Loading files into the dataset ${datasetName}.` : "Loading files into a dataset from a manifest."}
        crumbs={crumbs}
        actions={<JobActions job={data} canManage={can("ingestion.manage")} />}
      />

      <div className="flex flex-col gap-6">
        {data.error && (
          <div role="alert" className="neo-in-sm p-4">
            <p className="font-medium">The job reported an error</p>
            <p className="text-sm">{data.error}</p>
          </div>
        )}
        {data.state === "CANCELLING" && (
          <p role="status" className="neo-in-sm p-4 font-medium">
            Cancelling. This page updates until the job is cancelled. Files already stored still finish and publish.
          </p>
        )}

        <Panel title="Job">
          <DefList
            items={[
              { label: "Job id", value: <span className="flex items-center gap-2"><span className="mono-data break-all">{data.id}</span><CopyButton value={data.id} /></span> },
              { label: "Dataset", value: datasetName ?? <span className="mono-data">{data.dataset_id}</span> },
              { label: "Created", value: <Time value={data.created_at} /> },
              { label: "Updated", value: <Time value={data.updated_at} /> },
              { label: "Upload concurrency", value: data.max_concurrency === 0 ? "Server default" : String(data.max_concurrency) },
              { label: "Manifest", value: data.manifest_source ? `${data.manifest_source === "INLINE" ? "Sent inline" : "Staged"}, ${formatBytes(data.manifest_bytes)}, ${formatNumber(data.parsed_lines)} lines read` : "Not submitted" },
            ]}
          />
          {!isTerminal(data.state) && <p className="mt-4 text-sm text-muted">This page checks the job every 3 seconds while it is not finished, and pauses when the tab is hidden.</p>}
        </Panel>

        {data.state === "PENDING_VALIDATION" && <ManifestPanel jobId={id} canWrite={canWrite} />}
        {reading && (
          <Panel title="Reading the manifest">
            <p role="status">The server has read {formatNumber(data.parsed_lines)} lines so far. Large manifests take a while.</p>
          </Panel>
        )}
        {data.state === "VALIDATING" && data.manifest_complete && <FinalizePanel job={data} canWrite={canWrite} />}
        {uploading && <DriverPanel job={data} canWrite={canWrite} />}

        {hasEntries && (
          <Panel title="Progress">
            <ProgressSummary progress={data.progress} />
          </Panel>
        )}
        {hasEntries && <EntriesTable jobId={id} />}
        {hasEntries && <FailuresTable jobId={id} />}
      </div>
    </>
  );
}
