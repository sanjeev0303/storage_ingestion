"use client";

import Link from "next/link";
import { LoadMore } from "@/components/ui/page";
import { Panel } from "@/components/ui/panel";
import { ErrorState, SkeletonRows } from "@/components/ui/states";
import { StateBadge } from "@/components/ui/status";
import { Table, Td, Th } from "@/components/ui/table";
import { Time } from "@/components/ui/time";
import { useTrainingRuns } from "../hooks";
import type { TrainingRun } from "../types";

const SOURCE_NAMES: Record<TrainingRun["source"], string> = {
  SDK: "SDK",
  MLFLOW: "MLflow",
  WANDB: "Weights & Biases",
  OTHER: "Other",
};

const STATUS: Record<TrainingRun["status"], { text: string; tone: "active" | "done" | "failed" }> = {
  RUNNING: { text: "Running", tone: "active" },
  FINISHED: { text: "Finished", tone: "done" },
  FAILED: { text: "Failed", tone: "failed" },
};

const MAX_METRICS = 4;

const EXAMPLE = `from osp import track_run

with track_run(client, "resnet50", versions=[stream]) as run:
    ...  # train on the stream
    run.log_metrics(accuracy=0.91)
    run.log_model("resnet50", "3", uri="s3://models/resnet50/3")`;

function metricText(metrics: TrainingRun["metrics"]) {
  const entries = Object.entries(metrics).sort(([a], [b]) => a.localeCompare(b));
  const shown = entries.slice(0, MAX_METRICS).map(([k, v]) => `${k} ${Number.isInteger(v) ? v : v.toPrecision(4)}`);
  if (entries.length > MAX_METRICS) shown.push(`${entries.length - MAX_METRICS} more`);
  return shown.join(", ");
}

// The training runs recorded as having read this version, and the models they produced.
export function TrainingRuns({ datasetId, versionId }: { datasetId: string; versionId: string }) {
  const runs = useTrainingRuns(versionId);

  return (
    <Panel
      title="Trained on this version"
      description="Training runs recorded with the SDK, MLflow or Weights & Biases, and the models they produced. Lineage follows them too."
    >
      {runs.isPending ? (
        <SkeletonRows rows={3} />
      ) : runs.isError && runs.items.length === 0 ? (
        <ErrorState error={runs.error} onRetry={() => runs.refetch()} />
      ) : runs.items.length === 0 ? (
        <div className="flex flex-col gap-3">
          <p className="max-w-prose text-base text-muted">
            No training run has recorded reading this version yet. Record one from the training script; inside an MLflow or Weights &amp;
            Biases run, that run is linked too.
          </p>
          <pre className="neo-in-sm mono-data overflow-x-auto p-4 text-sm" data-lenis-prevent-horizontal>
            {EXAMPLE}
          </pre>
        </div>
      ) : (
        <>
          <Table>
            <thead>
              <tr>
                <Th>Run</Th>
                <Th>Status</Th>
                <Th>Model</Th>
                <Th>Metrics</Th>
                <Th>Started</Th>
              </tr>
            </thead>
            <tbody>
              {runs.items.map((run) => (
                <tr key={run.id}>
                  <Td>
                    <span className="block font-semibold">{run.name}</span>
                    <span className="block text-sm text-muted">
                      {run.url ? (
                        <a href={run.url} target="_blank" rel="noopener noreferrer" className="underline underline-offset-2">
                          {SOURCE_NAMES[run.source]}
                        </a>
                      ) : (
                        SOURCE_NAMES[run.source]
                      )}
                      {run.dataset_version_ids.length > 1 &&
                        `, with ${run.dataset_version_ids.length - 1} other ${run.dataset_version_ids.length === 2 ? "version" : "versions"}`}
                    </span>
                  </Td>
                  <Td>
                    <StateBadge tone={STATUS[run.status].tone}>{STATUS[run.status].text}</StateBadge>
                  </Td>
                  <Td>
                    {run.models.length === 0 ? (
                      <span className="text-muted">None recorded</span>
                    ) : (
                      run.models.map((m) => (
                        <span key={m.id} className="block mono-data text-sm" title={m.uri}>
                          {m.name} {m.version}
                        </span>
                      ))
                    )}
                  </Td>
                  <Td className="text-sm tabular-nums">{metricText(run.metrics) || <span className="text-muted">None</span>}</Td>
                  <Td>
                    <Time value={run.created_at} />
                  </Td>
                </tr>
              ))}
            </tbody>
          </Table>
          <LoadMore
            hasNext={Boolean(runs.hasNextPage)}
            loading={runs.isFetchingNextPage}
            onLoad={() => runs.fetchNextPage()}
            shown={runs.items.length}
          />
          <p className="mt-3 text-sm">
            <Link
              href={`/app/datasets/${datasetId}/lineage?version=${versionId}`}
              className="underline underline-offset-2"
            >
              See this version&apos;s lineage
            </Link>
            , then choose Downstream to follow runs to their models.
          </p>
        </>
      )}
    </Panel>
  );
}
