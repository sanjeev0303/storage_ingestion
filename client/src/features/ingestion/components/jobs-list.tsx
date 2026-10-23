"use client";

import { Plus } from "@phosphor-icons/react";
import Link from "next/link";
import { useState } from "react";
import { ButtonLink } from "@/components/ui/button";
import { Field, Select } from "@/components/ui/field";
import { LoadMore } from "@/components/ui/page";
import { EmptyState, ErrorState, SkeletonRows } from "@/components/ui/states";
import { Table, Td, Th } from "@/components/ui/table";
import { Time } from "@/components/ui/time";
import { useSession } from "@/lib/auth/session";
import { formatNumber, shortId } from "@/lib/utils/format";
import { useDatasets, useJobs, useManifestLimits } from "../hooks";
import { isTerminal } from "../lib/explain";
import { JobBadge } from "./badges";

export function NewJobButton() {
  const { can } = useSession();
  if (!can("ingestion.write")) return null;
  return (
    <ButtonLink href="/app/ingestion/jobs/new" variant="primary">
      <Plus aria-hidden />
      New ingestion job
    </ButtonLink>
  );
}

export function JobsList() {
  const [datasetId, setDatasetId] = useState("");
  const jobs = useJobs(datasetId);
  const datasets = useDatasets();
  const quota = useManifestLimits();

  const names = new Map(datasets.items.map((d) => [d.id, d.name]));
  const activeJobs = jobs.items.filter((job) => !isTerminal(job.state)).length;
  const maxActive = quota.data?.limits.max_active_jobs;

  return (
    <div className="flex flex-col gap-5">
      <div className="flex flex-wrap items-end justify-between gap-4">
        <Field label="Dataset" className="w-full max-w-xs">
          {(props) => (
            <Select {...props} value={datasetId} onChange={(e) => setDatasetId(e.target.value)}>
              <option value="">All datasets</option>
              {datasets.items.map((d) => (
                <option key={d.id} value={d.id}>
                  {d.name}
                </option>
              ))}
              {datasetId && !names.has(datasetId) && <option value={datasetId}>{shortId(datasetId)}</option>}
            </Select>
          )}
        </Field>
        <div className="flex flex-col items-end gap-1">
          {datasets.hasNextPage && (
            <button
              type="button"
              className="text-sm underline underline-offset-2"
              onClick={() => datasets.fetchNextPage()}
              disabled={datasets.isFetchingNextPage}
            >
              Load more datasets
            </button>
          )}
          {maxActive ? (
            <p className="text-sm text-muted" aria-live="polite">
              {activeJobs} unfinished {activeJobs === 1 ? "job" : "jobs"} in the list. Your organization allows {formatNumber(maxActive)} active at once.
            </p>
          ) : null}
        </div>
      </div>

      {jobs.isPending ? (
        <SkeletonRows />
      ) : jobs.isError ? (
        <ErrorState error={jobs.error} onRetry={() => jobs.refetch()} />
      ) : jobs.items.length === 0 ? (
        <EmptyState title={datasetId ? "No jobs for this dataset" : "No ingestion jobs yet"} action={<NewJobButton />}>
          A job loads many files into one dataset from a manifest: a JSON Lines file with one line per file, giving its path, size, content type and SHA-256 checksum. You create the job, submit the manifest, finalize it, then attach the files from your computer and this page uploads them straight to storage.
        </EmptyState>
      ) : (
        <>
          <Table>
            <thead>
              <tr>
                <Th>Job</Th>
                <Th>State</Th>
                <Th>Dataset</Th>
                <Th className="text-right">Published / total</Th>
                <Th className="text-right">Failed</Th>
                <Th className="text-right">Invalid</Th>
                <Th className="text-right">Done</Th>
                <Th>Created</Th>
              </tr>
            </thead>
            <tbody>
              {jobs.items.map((job) => (
                <tr key={job.id}>
                  <Td>
                    <Link href={`/app/ingestion/jobs/${job.id}`} className="mono-data underline underline-offset-2">
                      {shortId(job.id)}
                    </Link>
                  </Td>
                  <Td>
                    <JobBadge state={job.state} />
                  </Td>
                  <Td>{names.get(job.dataset_id) ?? <span className="mono-data">{shortId(job.dataset_id)}</span>}</Td>
                  <Td className="text-right tabular-nums">
                    {formatNumber(job.progress.published)} / {formatNumber(job.progress.total)}
                  </Td>
                  <Td className="text-right tabular-nums">{formatNumber(job.progress.failed + job.progress.failed_retryable)}</Td>
                  <Td className="text-right tabular-nums">{formatNumber(job.progress.invalid)}</Td>
                  <Td className="text-right tabular-nums">{job.progress.percent_published.toFixed(1)}%</Td>
                  <Td>
                    <Time value={job.created_at} />
                  </Td>
                </tr>
              ))}
            </tbody>
          </Table>
          <LoadMore
            hasNext={Boolean(jobs.hasNextPage)}
            loading={jobs.isFetchingNextPage}
            onLoad={() => jobs.fetchNextPage()}
            shown={jobs.items.length}
          />
        </>
      )}
    </div>
  );
}
