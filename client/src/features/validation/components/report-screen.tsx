"use client";

import Link from "next/link";
import { ButtonLink } from "@/components/ui/button";
import { DefList, PageHeader } from "@/components/ui/page";
import { Panel } from "@/components/ui/panel";
import { ErrorState, SkeletonRows } from "@/components/ui/states";
import { Table, Td, Th } from "@/components/ui/table";
import { Time } from "@/components/ui/time";
import { formatBytes, formatNumber } from "@/lib/utils/format";
import { useReport } from "../hooks";
import { checkStatusOutcome, COVERAGE_LABELS, MODE_HELP, MODE_LABELS, reportStatusOutcome } from "../status";
import type { ReportBody, ValidationReport } from "../types";
import { FindingsSection } from "./findings-section";
import { CountTable, IdText, MappedOutcome, SectionHeading } from "./parts";
import { ReadinessSection } from "./readiness-section";

export function ReportScreen({ reportId }: { reportId: string }) {
  const report = useReport(reportId);
  if (report.isPending) return <SkeletonRows rows={8} />;
  if (report.isError) return <ErrorState error={report.error} onRetry={() => report.refetch()} />;
  return <ReportView report={report.data} />;
}

function ReportView({ report }: { report: ValidationReport }) {
  const body = report.report;
  const verdict = reportStatusOutcome(report.status);
  return (
    <div className="flex flex-col gap-8">
      <PageHeader
        crumbs={[
          { label: "Validation", href: "/app/validation" },
          { label: "Datasets", href: "/app/datasets" },
          { label: `Report ${report.id.slice(0, 8)}` },
        ]}
        title={report.readiness ? "Readiness report" : "Validation report"}
        description={report.summary}
        actions={
          <>
            <ButtonLink href={`/app/validation/jobs/${report.job_id}`}>View job</ButtonLink>
            <ButtonLink href={`/app/datasets/${report.dataset_id}/validation`}>Dataset checks</ButtonLink>
          </>
        }
      />

      <div className="grid gap-6 lg:grid-cols-3">
        <Panel title="Verdict" className="lg:col-span-1">
          <p className="text-xl font-semibold">
            <MappedOutcome mapped={verdict} />
          </p>
          <p className="mt-3 text-sm text-muted">
            This verdict comes from the profile&apos;s thresholds: {body ? thresholdText(body) : "see the profile"}. It is not a score, and it says
            nothing about semantic correctness, label quality, legal compliance or whether training will succeed.
          </p>
        </Panel>
        <Panel title="What was checked" className="lg:col-span-2">
          <DefList
            items={[
              {
                label: "Profile",
                value: (
                  <Link
                    href={`/app/validation/profiles/${encodeURIComponent(report.profile_id)}?version=${report.profile_version}`}
                    className="underline underline-offset-2"
                  >
                    <span className="mono-data text-sm">
                      {report.profile_id}@{report.profile_version}
                    </span>
                  </Link>
                ),
              },
              ...(body ? [{ label: "Profile kind", value: body.profile.kind || "Not set" }] : []),
              {
                label: "Mode",
                value: body?.sample_size ? `${MODE_LABELS[report.mode]}, sample of ${formatNumber(body.sample_size)}` : MODE_LABELS[report.mode],
              },
              {
                label: "Dataset version",
                value: (
                  <span>
                    {body ? `Version ${body.dataset_version}, ` : ""}
                    <IdText id={report.version_id} href={`/app/datasets/${report.dataset_id}/versions/${report.version_id}`} />
                  </span>
                ),
              },
              ...(body ? [{ label: "Manifest SHA-256", value: <span className="mono-data text-sm">{body.manifest_sha256}</span> }] : []),
              { label: "Started", value: <Time value={report.started_at} /> },
              { label: "Completed", value: <Time value={report.completed_at} /> },
            ]}
          />
          <p className="mt-4 max-w-prose text-sm text-muted">{MODE_HELP[report.mode]}</p>
        </Panel>
      </div>

      {!body ? (
        <p role="status">The report body was not returned, so only the summary is available.</p>
      ) : (
        <BodySections body={body} readiness={report.readiness} reportId={report.id} />
      )}
      {!body && <FindingsSection reportId={report.id} />}
    </div>
  );
}

function thresholdText(body: ReportBody) {
  const t = body.thresholds;
  const warnings = t.max_warnings === undefined ? "no limit on warnings" : `at most ${t.max_warnings} warnings`;
  return `failed above ${t.max_errors} errors, ${warnings}`;
}

function BodySections({ body, readiness, reportId }: { body: ReportBody; readiness: boolean; reportId: string }) {
  const content = body.content;
  return (
    <>
      <section aria-labelledby="coverage-heading" className="flex flex-col gap-3">
        <SectionHeading id="coverage-heading">Coverage</SectionHeading>
        <div className="grid gap-6 lg:grid-cols-2">
          <Panel title="Members" tone="in">
            <DefList
              items={[
                { label: "Total", value: formatNumber(body.members.total) },
                { label: "Examined", value: formatNumber(body.members.examined) },
                { label: "Passed", value: formatNumber(body.members.passed) },
                { label: "With warnings", value: formatNumber(body.members.warnings) },
                { label: "Failed", value: formatNumber(body.members.failed) },
              ]}
            />
          </Panel>
          <Panel title={`Content: ${COVERAGE_LABELS[content.coverage]}`} tone="in">
            <DefList
              items={[
                { label: "Examined", value: formatNumber(content.examined) },
                { label: "Read from storage", value: formatNumber(content.read) },
                { label: "Not examined", value: formatNumber(content.not_examined) },
                { label: "Bytes represented", value: formatBytes(content.bytes_represented) },
                { label: "Bytes read", value: formatBytes(content.bytes_read) },
              ]}
            />
            <p className="mt-3 max-w-prose text-sm">{content.note}</p>
          </Panel>
        </div>
      </section>

      <section aria-labelledby="checks-heading" className="flex flex-col gap-3">
        <SectionHeading id="checks-heading">Rule results</SectionHeading>
        <p className="text-base">
          {body.checks.passed} passed, {body.checks.failed} failed, {body.checks.partial} partial, {body.checks.not_assessed} not checked.
        </p>
        <Table aria-label="Rule results">
          <thead>
            <tr>
              <Th>Rule</Th>
              <Th>Type</Th>
              <Th>Severity</Th>
              <Th>Scope</Th>
              <Th>Dimension</Th>
              <Th>Status</Th>
              <Th className="text-right">Checked</Th>
              <Th className="text-right">Failed</Th>
              <Th className="text-right">Not checked</Th>
              <Th className="text-right">Read</Th>
              <Th className="text-right">Findings stored</Th>
              <Th>Note</Th>
            </tr>
          </thead>
          <tbody>
            {(body.results ?? []).map((r) => (
              <tr key={`${r.rule_id}-${r.rule_version}`}>
                <Td>
                  <span className="mono-data text-sm">{r.rule_id}</span>
                  <span className="text-xs text-muted"> v{r.rule_version}</span>
                </Td>
                <Td>
                  <span className="mono-data text-sm">{r.type}</span>
                </Td>
                <Td>{r.severity.charAt(0) + r.severity.slice(1).toLowerCase()}</Td>
                <Td>{r.scope.toLowerCase().replace("_", " ")}</Td>
                <Td>{r.dimension.charAt(0) + r.dimension.slice(1).toLowerCase()}</Td>
                <Td>
                  <MappedOutcome mapped={checkStatusOutcome(r.status, content.coverage)} />
                </Td>
                <Td className="text-right tabular-nums">{formatNumber(r.checked)}</Td>
                <Td className="text-right tabular-nums">{formatNumber(r.failed)}</Td>
                <Td className="text-right tabular-nums">{formatNumber(r.not_assessed)}</Td>
                <Td className="text-right tabular-nums">{r.read === undefined ? "n/a" : formatNumber(r.read)}</Td>
                <Td className="text-right tabular-nums">{formatNumber(r.findings_stored)}</Td>
                <Td className="min-w-48 max-w-80 whitespace-normal">{r.note ?? ""}</Td>
              </tr>
            ))}
          </tbody>
        </Table>
      </section>

      {readiness && body.readiness && <ReadinessSection readiness={body.readiness} />}

      <section aria-labelledby="evidence-heading" className="flex flex-col gap-3">
        <SectionHeading id="evidence-heading">Evidence</SectionHeading>
        <p className="max-w-prose text-sm text-muted">
          Counted from the platform&apos;s own processing and lifecycle records. {formatNumber(body.evidence.verified_checksums)} members have a checksum
          that processing verified against the stored bytes.
        </p>
        <div className="grid gap-6 md:grid-cols-3">
          <CountTable caption="Checksum verification" rows={body.evidence.checksum_verification} />
          <CountTable caption="Malware scans" rows={body.evidence.malware_scans} />
          <CountTable caption="Version states" rows={body.evidence.version_states} />
        </div>
      </section>

      <section aria-labelledby="dist-heading" className="flex flex-col gap-3">
        <SectionHeading id="dist-heading">Distributions</SectionHeading>
        <p className="max-w-prose text-sm text-muted">
          Each table lists at most the top 50 values. {formatNumber(body.distributions.distinct_labels)} distinct labels,{" "}
          {formatNumber(body.distributions.unlabeled)} members without a label. Total {formatBytes(body.distributions.sizes.total_bytes)}, smallest{" "}
          {formatBytes(body.distributions.sizes.min_bytes)}, largest {formatBytes(body.distributions.sizes.max_bytes)}.
        </p>
        <div className="grid gap-6 md:grid-cols-2">
          <CountTable caption="Splits" rows={body.distributions.splits} />
          <CountTable caption="Labels" rows={body.distributions.labels} />
          <CountTable caption="Content types" rows={body.distributions.content_types} />
          <CountTable caption="Extensions" rows={body.distributions.extensions} />
        </div>
      </section>

      <FindingsSection reportId={reportId} body={body} />
    </>
  );
}
