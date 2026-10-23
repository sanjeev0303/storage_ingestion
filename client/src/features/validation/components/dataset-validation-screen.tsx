"use client";

import Link from "next/link";
import { useState } from "react";
import { Checkbox } from "@/components/ui/field";
import { Panel } from "@/components/ui/panel";
import { LoadMore } from "@/components/ui/page";
import { EmptyState, ErrorState, SkeletonRows } from "@/components/ui/states";
import { Table, Td, Th } from "@/components/ui/table";
import { Time } from "@/components/ui/time";
import { useJobs, useReadinessReports, useValidationDataset } from "../hooks";
import { MODE_LABELS, reportStatusOutcome } from "../status";
import type { ValidationJob, ValidationReport } from "../types";
import { IdText, JobStateBadge, MappedOutcome, ProgressBar, SectionHeading } from "./parts";
import { StartForm } from "./start-form";
import { formatNumber } from "@/lib/utils/format";

export function DatasetValidationScreen({ datasetId, versionId }: { datasetId: string; versionId?: string }) {
  const dataset = useValidationDataset(datasetId);

  if (dataset.isPending) return <SkeletonRows rows={6} />;
  if (dataset.isError) return <ErrorState error={dataset.error} onRetry={() => dataset.refetch()} />;

  return (
    <div className="flex flex-col gap-8">
      <section aria-labelledby="start-heading" className="flex flex-col gap-4">
        <SectionHeading id="start-heading">Start a check</SectionHeading>
        <p className="max-w-prose text-base text-muted">
          A check runs against one immutable snapshot, never against the draft. A passing report says the profile&apos;s rules held for what was
          examined. It does not say the data is semantically correct, legally cleared, or good for training a model.
        </p>
        <div className="grid gap-6 xl:grid-cols-2">
          <StartForm dataset={dataset.data} kind="validation" initialVersionId={versionId} />
          <StartForm dataset={dataset.data} kind="readiness" initialVersionId={versionId} />
        </div>
      </section>
      <JobsSection datasetId={datasetId} />
      <ReportsSection datasetId={datasetId} />
    </div>
  );
}

function JobsSection({ datasetId }: { datasetId: string }) {
  const jobs = useJobs(datasetId);
  return (
    <section aria-labelledby="jobs-heading" className="flex flex-col gap-3">
      <SectionHeading id="jobs-heading">Validation jobs</SectionHeading>
      <p className="text-sm text-muted">Newest first. Readiness assessments are listed here too and marked.</p>
      {jobs.isPending ? (
        <SkeletonRows rows={4} />
      ) : jobs.isError ? (
        <ErrorState error={jobs.error} onRetry={() => jobs.refetch()} />
      ) : jobs.items.length === 0 ? (
        <EmptyState title="No checks have run on this dataset">
          Start a validation above. It needs a snapshot first, because checks run against a fixed version, not the draft.
        </EmptyState>
      ) : (
        <>
          <Table aria-label="Validation jobs">
            <thead>
              <tr>
                <Th>Job</Th>
                <Th>Kind</Th>
                <Th>State</Th>
                <Th>Mode</Th>
                <Th>Version</Th>
                <Th>Progress</Th>
                <Th>Created</Th>
                <Th>Report</Th>
              </tr>
            </thead>
            <tbody>
              {jobs.items.map((job) => (
                <JobRow key={job.id} job={job} />
              ))}
            </tbody>
          </Table>
          <LoadMore hasNext={Boolean(jobs.hasNextPage)} loading={jobs.isFetchingNextPage} onLoad={() => jobs.fetchNextPage()} shown={jobs.items.length} />
        </>
      )}
    </section>
  );
}

function JobRow({ job }: { job: ValidationJob }) {
  const p = job.progress;
  return (
    <tr>
      <Td>
        <Link href={`/app/validation/jobs/${job.id}`} className="underline underline-offset-2">
          <IdText id={job.id} />
        </Link>
      </Td>
      <Td>{job.readiness ? "Readiness assessment" : "Validation"}</Td>
      <Td>
        <JobStateBadge job={job} />
      </Td>
      <Td>
        {MODE_LABELS[job.mode]}
        {job.sample_size ? ` (${formatNumber(job.sample_size)})` : ""}
      </Td>
      <Td>
        <IdText id={job.version_id} href={`/app/datasets/${job.dataset_id}/versions/${job.version_id}`} />
      </Td>
      <Td className="min-w-40">
        <ProgressBar label={`Job ${job.id} progress`} value={p.processed} max={p.total} />
        <span className="mt-1 block text-xs text-muted tabular-nums">
          {formatNumber(p.processed)} of {formatNumber(p.total)}
        </span>
      </Td>
      <Td>
        <Time value={job.created_at} />
      </Td>
      <Td>
        {job.report_id ? (
          <Link href={`/app/validation/reports/${job.report_id}`} className="font-medium underline underline-offset-2">
            Open report
          </Link>
        ) : (
          <span className="text-muted">None</span>
        )}
      </Td>
    </tr>
  );
}

function ReportsSection({ datasetId }: { datasetId: string }) {
  const [all, setAll] = useState(false);
  const reports = useReadinessReports(datasetId, all);
  return (
    <section aria-labelledby="reports-heading" className="flex flex-col gap-3">
      <SectionHeading id="reports-heading">{all ? "All reports" : "Readiness reports"}</SectionHeading>
      <Checkbox label="Include plain validation reports" checked={all} onChange={(e) => setAll(e.target.checked)} />
      {reports.isPending ? (
        <SkeletonRows rows={3} />
      ) : reports.isError ? (
        <ErrorState error={reports.error} onRetry={() => reports.refetch()} />
      ) : reports.items.length === 0 ? (
        <EmptyState title={all ? "No reports yet" : "No readiness reports yet"}>
          A report appears here when a job finishes with a verdict. Readiness assessments judge nine dimensions against a profile; plain validations
          give the overall verdict only.
        </EmptyState>
      ) : (
        <Panel padded={false} className="p-0">
          <Table aria-label="Reports">
            <thead>
              <tr>
                <Th>Report</Th>
                <Th>Kind</Th>
                <Th>Verdict</Th>
                <Th>Profile</Th>
                <Th>Mode</Th>
                <Th>Completed</Th>
              </tr>
            </thead>
            <tbody>
              {reports.items.map((r) => (
                <ReportRow key={r.id} report={r} />
              ))}
            </tbody>
          </Table>
          <div className="px-5 pb-4">
            <LoadMore
              hasNext={Boolean(reports.hasNextPage)}
              loading={reports.isFetchingNextPage}
              onLoad={() => reports.fetchNextPage()}
              shown={reports.items.length}
            />
          </div>
        </Panel>
      )}
    </section>
  );
}

function ReportRow({ report }: { report: ValidationReport }) {
  return (
    <tr>
      <Td>
        <Link href={`/app/validation/reports/${report.id}`} className="underline underline-offset-2">
          <IdText id={report.id} />
        </Link>
      </Td>
      <Td>{report.readiness ? "Readiness" : "Validation"}</Td>
      <Td>
        <MappedOutcome mapped={reportStatusOutcome(report.status)} />
      </Td>
      <Td>
        <Link href={`/app/validation/profiles/${encodeURIComponent(report.profile_id)}?version=${report.profile_version}`} className="underline underline-offset-2">
          <span className="mono-data text-sm">
            {report.profile_id}@{report.profile_version}
          </span>
        </Link>
      </Td>
      <Td>{MODE_LABELS[report.mode]}</Td>
      <Td>
        <Time value={report.completed_at} />
      </Td>
    </tr>
  );
}
