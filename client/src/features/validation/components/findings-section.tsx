"use client";

import { useState } from "react";
import { Field, Input, Select } from "@/components/ui/field";
import { LoadMore } from "@/components/ui/page";
import { EmptyState, ErrorState, SkeletonRows } from "@/components/ui/states";
import { Table, Td, Th } from "@/components/ui/table";
import { formatNumber } from "@/lib/utils/format";
import { useFindings } from "../hooks";
import type { CheckResult, Finding, FindingFilters, ReportBody, Severity } from "../types";
import { IdText, SectionHeading } from "./parts";

const SEVERITIES: Severity[] = ["ERROR", "WARNING", "INFO"];

// Counts in the report stay exact; stored findings are capped per rule.
export function cappedRules(body: ReportBody) {
  const stored = new Map<string, number>((body.results ?? []).map((r: CheckResult) => [r.rule_id, r.findings_stored]));
  return Object.entries(body.findings.by_rule ?? {})
    .filter(([ruleId, count]) => count > (stored.get(ruleId) ?? 0))
    .map(([ruleId, count]) => ({ ruleId, count, stored: stored.get(ruleId) ?? 0 }));
}

export function FindingsSection({ reportId, body }: { reportId: string; body?: ReportBody }) {
  const [filters, setFilters] = useState<FindingFilters>({ severity: "", rule_id: "" });
  const [localText, setLocalText] = useState("");
  const findings = useFindings(reportId, filters);

  const text = localText.trim().toLowerCase();
  const visible = text
    ? findings.items.filter((f) => [f.code, f.object_id, f.version_id, f.message].some((v) => v?.toLowerCase().includes(text)))
    : findings.items;

  const capped = body ? cappedRules(body) : [];
  const ruleIds = body ? Object.keys(body.findings.by_rule ?? {}).sort() : [];

  return (
    <section aria-labelledby="findings-heading" className="flex flex-col gap-3">
      <SectionHeading id="findings-heading">Findings</SectionHeading>
      {body && (
        <p className="text-base">
          {Object.entries(body.findings.by_severity ?? {})
            .map(([severity, count]) => `${formatNumber(count)} ${severity.toLowerCase()}`)
            .join(", ") || "No findings counted"}
          .
        </p>
      )}
      {capped.length > 0 && (
        <div role="note" className="neo-in-sm max-w-prose p-3 text-sm">
          <p className="font-medium">Some findings are not stored.</p>
          <p className="mt-1 text-muted">
            Each rule keeps at most 10,000 findings per job. The counts above stay exact; the list below is shorter for:
          </p>
          <ul className="mt-1 list-disc pl-5">
            {capped.map((c) => (
              <li key={c.ruleId}>
                <span className="mono-data">{c.ruleId}</span>: {formatNumber(c.count)} counted, {formatNumber(c.stored)} stored
              </li>
            ))}
          </ul>
        </div>
      )}

      <div className="flex flex-wrap items-end gap-4">
        <Field label="Severity" className="w-44">
          {(props) => (
            <Select {...props} value={filters.severity} onChange={(e) => setFilters({ ...filters, severity: e.target.value as FindingFilters["severity"] })}>
              <option value="">All severities</option>
              {SEVERITIES.map((s) => (
                <option key={s} value={s}>
                  {s.charAt(0) + s.slice(1).toLowerCase()}
                </option>
              ))}
            </Select>
          )}
        </Field>
        <Field label="Rule" className="w-64">
          {(props) => (
            <Select {...props} value={filters.rule_id} onChange={(e) => setFilters({ ...filters, rule_id: e.target.value })}>
              <option value="">All rules</option>
              {ruleIds.map((id) => (
                <option key={id} value={id}>
                  {id}
                </option>
              ))}
            </Select>
          )}
        </Field>
        <Field
          label="Search loaded findings"
          hint="Filters only the findings already loaded below, by code, object, version or message. The API cannot filter by these."
          className="w-72"
        >
          {(props) => <Input {...props} value={localText} onChange={(e) => setLocalText(e.target.value)} />}
        </Field>
      </div>

      {findings.isPending ? (
        <SkeletonRows rows={4} />
      ) : findings.isError ? (
        <ErrorState error={findings.error} onRetry={() => findings.refetch()} />
      ) : findings.items.length === 0 ? (
        <EmptyState title={filters.severity || filters.rule_id ? "No findings match these filters" : "This report has no stored findings"}>
          {filters.severity || filters.rule_id
            ? "Clear the severity or rule filter to see every stored finding."
            : "A rule records a finding for each member or group that broke it. None were recorded for this report."}
        </EmptyState>
      ) : (
        <>
          <Table aria-label="Findings">
            <thead>
              <tr>
                <Th>Severity</Th>
                <Th>Code</Th>
                <Th>Rule</Th>
                <Th>Object</Th>
                <Th>Message</Th>
                <Th>Expected</Th>
                <Th>Observed</Th>
                <Th>Evidence</Th>
                <Th>Remediation</Th>
              </tr>
            </thead>
            <tbody>
              {visible.map((f) => (
                <FindingRow key={f.id} finding={f} />
              ))}
            </tbody>
          </Table>
          {visible.length === 0 && <p className="text-sm text-muted">Nothing in the loaded findings matches. Load more to search further.</p>}
          <LoadMore hasNext={Boolean(findings.hasNextPage)} loading={findings.isFetchingNextPage} onLoad={() => findings.fetchNextPage()} shown={findings.items.length} />
        </>
      )}
    </section>
  );
}

const wide = "min-w-52 max-w-80 whitespace-normal [overflow-wrap:anywhere]";

function FindingRow({ finding: f }: { finding: Finding }) {
  return (
    <tr>
      <Td className="font-medium">{f.severity.charAt(0) + f.severity.slice(1).toLowerCase()}</Td>
      <Td>
        <span className="mono-data text-sm">{f.code}</span>
      </Td>
      <Td>
        <span className="mono-data text-sm">{f.rule_id}</span>
        <span className="text-xs text-muted"> v{f.rule_version}</span>
      </Td>
      <Td>
        {f.object_id ? <IdText id={f.object_id} href={`/app/catalog/${f.object_id}`} /> : <span className="text-muted">Dataset level</span>}
        {f.version_id && (
          <span className="mt-0.5 block text-xs text-muted">
            asset version <span className="mono-data">{f.version_id.slice(0, 8)}</span>
          </span>
        )}
      </Td>
      <Td className={wide}>{f.message}</Td>
      <Td className={wide}>{f.expected ?? <span className="text-muted">Not given</span>}</Td>
      <Td className={wide}>{f.observed ?? <span className="text-muted">Not given</span>}</Td>
      <Td className={wide}>{f.evidence || <span className="text-muted">None</span>}</Td>
      <Td className={wide}>{f.remediation ?? <span className="text-muted">None offered</span>}</Td>
    </tr>
  );
}

