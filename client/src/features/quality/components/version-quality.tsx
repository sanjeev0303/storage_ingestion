"use client";

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { Button } from "@/components/ui/button";
import { Field, Input, Select, Textarea } from "@/components/ui/field";
import { DefList } from "@/components/ui/page";
import { Panel } from "@/components/ui/panel";
import { EmptyState, ErrorState, SkeletonRows } from "@/components/ui/states";
import { StateBadge } from "@/components/ui/status";
import { SubmitButton } from "@/components/ui/submit-button";
import { Table, Td, Th } from "@/components/ui/table";
import { notify } from "@/components/ui/toast";
import { useDatasets, useVersions } from "@/features/datasets/hooks";
import { useOrganizationId, useSession } from "@/lib/auth/session";
import { orgKey } from "@/lib/query/keys";
import { formatBytes, formatNumber } from "@/lib/utils/format";
import {
  checkLeakage,
  getProfile,
  listReleases,
  release,
  releaseDiff,
  reprofile,
  type ColumnProfile,
  type DriftReport,
  type LeakageReport,
  type ProfileState,
  type TableProfile,
} from "../api";

const percent = new Intl.NumberFormat(undefined, { style: "percent", maximumFractionDigits: 1 });
const decimal = new Intl.NumberFormat(undefined, { maximumFractionDigits: 3 });
const day = new Intl.DateTimeFormat(undefined, { dateStyle: "medium", timeZone: "UTC" });
const SHOWN_COLUMNS = 50;

const STATE: Record<ProfileState, { text: string; tone: "active" | "done" | "failed" | "neutral" }> = {
  PENDING: { text: "Waiting", tone: "active" },
  RUNNING: { text: "Profiling", tone: "active" },
  DONE: { text: "Profiled", tone: "done" },
  FAILED: { text: "Failed", tone: "failed" },
  NONE: { text: "Not profiled", tone: "neutral" },
};

// What a version's tables hold, how they moved, whether its evaluation
// files are in training, and its release label.
export function VersionQuality({ id, versionId }: { id: string; versionId: string }) {
  return (
    <>
      <ProfilePanel id={id} versionId={versionId} />
      <LeakagePanel id={id} versionId={versionId} />
      <ReleasePanel id={id} versionId={versionId} />
    </>
  );
}

function ProfilePanel({ id, versionId }: { id: string; versionId: string }) {
  const organizationId = useOrganizationId();
  const { can } = useSession();
  const client = useQueryClient();
  const key = orgKey(organizationId, "quality", "profile", versionId);
  const profile = useQuery({
    queryKey: key,
    queryFn: ({ signal }) => getProfile(id, versionId, signal),
    enabled: Boolean(organizationId),
    refetchInterval: (q) => (q.state.data?.state === "PENDING" || q.state.data?.state === "RUNNING" ? 5000 : false),
  });
  const again = useMutation({
    mutationFn: () => reprofile(id, versionId),
    onSuccess: () => {
      notify.success("Queued; the worker profiles it next");
      client.invalidateQueries({ queryKey: key });
    },
    onError: notify.error,
  });
  const p = profile.data;
  return (
    <Panel
      title="Column profile"
      description="Each table's rows and columns: kinds, nulls, distinct values, ranges and the most common values. CSV, TSV, JSON lines and Parquet."
      actions={
        can("datasets.manage") && p && p.state !== "PENDING" && p.state !== "RUNNING" ? (
          <Button onClick={() => again.mutate()} loading={again.isPending}>
            Profile again
          </Button>
        ) : undefined
      }
    >
      {profile.isPending ? (
        <SkeletonRows rows={4} />
      ) : profile.isError || !p ? (
        <ErrorState error={profile.error} onRetry={() => profile.refetch()} />
      ) : (
        <div className="flex flex-col gap-5">
          <div className="flex flex-wrap items-center gap-3">
            <StateBadge tone={STATE[p.state].tone}>{STATE[p.state].text}</StateBadge>
            {p.state === "DONE" && (
              <span className="text-sm text-muted">
                {formatNumber(p.files_profiled ?? 0)} table files, {formatBytes(p.bytes_read ?? 0)} read, {formatNumber(p.other_files ?? 0)} other files
              </span>
            )}
            {p.error && <span className="text-sm">{p.error}</span>}
          </div>
          {p.state === "DONE" && p.tables.length === 0 && (
            <EmptyState title="No tables">This version has no CSV, TSV, JSON lines or Parquet files to profile.</EmptyState>
          )}
          {p.tables.map((t) => (
            <TableColumns key={t.table} table={t} />
          ))}
          {p.skipped.length > 0 && (
            <div>
              <h3 className="mb-2 text-base font-semibold">Not profiled</h3>
              <ul className="flex flex-col gap-1 text-sm">
                {p.skipped.slice(0, 50).map((s) => (
                  <li key={s.path} className="break-words">
                    <span className="mono-data">{s.path}</span>: {s.reason}
                  </li>
                ))}
              </ul>
              {p.skipped.length > 50 && <p className="mt-1 text-sm text-muted">And {formatNumber(p.skipped.length - 50)} more.</p>}
            </div>
          )}
          {p.drift && <DriftView report={p.drift} />}
          {p.state === "DONE" && !p.drift && (
            <p className="text-sm text-muted">
              {p.baseline_version_id ? "The version before it has no finished profile to compare with." : "The first version: nothing before it to compare with."}
            </p>
          )}
        </div>
      )}
    </Panel>
  );
}

function rangeOf(c: ColumnProfile) {
  if (c.mean !== undefined && c.min !== undefined && c.max !== undefined) {
    if (c.kind === "time") return `${day.format(c.min * 1000)} to ${day.format(c.max * 1000)}`;
    return `${decimal.format(c.min)} to ${decimal.format(c.max)}, mean ${decimal.format(c.mean)}`;
  }
  if (c.top?.length) return c.top.slice(0, 3).map((v) => `${v.value} (${formatNumber(v.count)})`).join(", ");
  return "";
}

function TableColumns({ table }: { table: TableProfile }) {
  const [all, setAll] = useState(false);
  const columns = all ? table.columns : table.columns.slice(0, SHOWN_COLUMNS);
  return (
    <div className="min-w-0">
      <h3 className="mb-2 flex flex-wrap items-baseline gap-x-3 text-base font-semibold">
        <span className="mono-data break-all">{table.table}</span>
        <span className="text-sm font-normal text-muted">
          {table.format}, {formatNumber(table.files)} files, {formatNumber(table.rows)} rows{table.partial ? ", stopped at the row limit" : ""}
        </span>
      </h3>
      <Table>
        <thead>
          <tr>
            <Th>Column</Th>
            <Th>Kind</Th>
            <Th>Nulls</Th>
            <Th>Distinct</Th>
            <Th>Values</Th>
          </tr>
        </thead>
        <tbody>
          {columns.map((c) => (
            <tr key={c.name}>
              <Td className="mono-data text-sm break-all">{c.name}</Td>
              <Td className="text-sm">{c.kind}</Td>
              <Td className="text-sm tabular-nums">{percent.format(c.nulls / Math.max(c.nulls + c.count, 1))}</Td>
              <Td className="text-sm tabular-nums">
                {formatNumber(c.distinct)}
                {c.distinct_capped ? "+" : ""}
              </Td>
              <Td className="text-sm">{rangeOf(c)}</Td>
            </tr>
          ))}
        </tbody>
      </Table>
      {table.columns.length > SHOWN_COLUMNS && (
        <Button variant="ghost" className="mt-2" onClick={() => setAll(!all)}>
          {all ? `Show the first ${SHOWN_COLUMNS} columns` : `Show all ${formatNumber(table.columns.length)} columns`}
        </Button>
      )}
    </div>
  );
}

export function DriftView({ report }: { report: DriftReport }) {
  const t = report.thresholds;
  return (
    <div className="flex flex-col gap-3">
      <h3 className="flex flex-wrap items-center gap-3 text-base font-semibold">
        Drift against the version before
        {report.breaches > 0 ? (
          <StateBadge tone="warning">{`${formatNumber(report.breaches)} past the thresholds`}</StateBadge>
        ) : (
          <StateBadge tone="done">Within thresholds</StateBadge>
        )}
      </h3>
      <p className="text-sm text-muted">
        Thresholds: PSI {t.max_psi}, null rate {percent.format(t.max_null_change)} points, rows {percent.format(t.max_row_change)}.
      </p>
      <ul className="flex flex-col gap-2 text-sm">
        {report.tables.map((tb) => {
          const moved = tb.columns.filter((c) => c.breached.length > 0);
          return (
            <li key={tb.table} className="min-w-0">
              <span className="mono-data break-all">{tb.table}</span>: {formatNumber(tb.rows_before)} to {formatNumber(tb.rows_after)} rows (
              {tb.row_change >= 0 ? "+" : ""}
              {percent.format(tb.row_change)}){tb.breached.length > 0 && <strong> past the threshold: {tb.breached.join(", ")}</strong>}
              {tb.columns_removed.length > 0 && <span className="block">Removed: {tb.columns_removed.join(", ")}</span>}
              {tb.columns_added.length > 0 && <span className="block text-muted">Added: {tb.columns_added.join(", ")}</span>}
              {moved.map((c) => (
                <span key={c.name} className="block">
                  <span className="mono-data">{c.name}</span>: {c.breached.join(", ")}
                  {c.psi !== undefined && `, PSI ${decimal.format(c.psi)}`}
                  {c.breached.includes("nulls") && `, nulls ${percent.format(c.null_rate_before)} to ${percent.format(c.null_rate_after)}`}
                  {c.breached.includes("kind") && `, ${c.kind_before} to ${c.kind_after}`}
                </span>
              ))}
              {moved.length === 0 && <span className="block text-muted">{formatNumber(tb.columns.length)} columns within thresholds</span>}
            </li>
          );
        })}
        {report.not_compared.map((n) => (
          <li key={n} className="text-muted">
            Not compared: {n}
          </li>
        ))}
      </ul>
    </div>
  );
}

function LeakagePanel({ id, versionId }: { id: string; versionId: string }) {
  const datasets = useDatasets("ACTIVE");
  const [trainDataset, setTrainDataset] = useState("");
  const [trainVersion, setTrainVersion] = useState("");
  const versions = useVersions(trainDataset);
  const check = useMutation({
    mutationFn: () => checkLeakage(id, versionId, trainVersion),
    onError: notify.error,
  });

  function submit(e: FormEvent) {
    e.preventDefault();
    if (trainVersion) check.mutate();
  }

  return (
    <Panel
      title="Leakage into training"
      description="Treat this version as an evaluation set: are any of its files, or its subjects, also in a training version?"
    >
      <div className="flex flex-col gap-4">
        <form onSubmit={submit} className="grid gap-4 sm:grid-cols-2">
          <Field label="Training dataset">
            {(props) => (
              <Select
                {...props}
                value={trainDataset}
                onChange={(e) => {
                  setTrainDataset(e.target.value);
                  setTrainVersion("");
                  check.reset();
                }}
              >
                <option value="">Choose a dataset</option>
                {datasets.items.map((d) => (
                  <option key={d.id} value={d.id}>
                    {d.name}
                  </option>
                ))}
              </Select>
            )}
          </Field>
          <Field label="Training version">
            {(props) => (
              <Select {...props} value={trainVersion} disabled={!trainDataset} onChange={(e) => setTrainVersion(e.target.value)}>
                <option value="">Choose a version</option>
                {(trainDataset ? versions.items : [])
                  .filter((v) => v.id !== versionId)
                  .map((v) => (
                    <option key={v.id} value={v.id}>
                      Version {v.version}
                    </option>
                  ))}
              </Select>
            )}
          </Field>
          <div className="sm:col-span-2">
            <SubmitButton valid={Boolean(trainVersion)} loading={check.isPending} hint="Choose the training version to compare with.">
              Check for leakage
            </SubmitButton>
          </div>
        </form>
        <div aria-live="polite">{check.data && <LeakageResult report={check.data} />}</div>
      </div>
    </Panel>
  );
}

const METHOD_TEXT = { EXACT_HASH: "Same bytes", SUBJECT: "Same subject", NGRAM: "Shared text" } as const;

export function LeakageResult({ report }: { report: LeakageReport }) {
  return (
    <div className="flex flex-col gap-3">
      {report.checks.map((c) => (
        <div key={c.id} className="min-w-0">
          <p className="flex flex-wrap items-center gap-2 text-base">
            <span className="font-semibold">{METHOD_TEXT[c.method]}</span>
            <StateBadge tone={c.state === "LEAKED" ? "failed" : c.state === "CLEAN" ? "done" : "neutral"}>
              {c.state === "LEAKED" ? "Leaked" : c.state === "CLEAN" ? "Clean" : "Not checked"}
            </StateBadge>
          </p>
          <p className="text-sm text-muted">{c.detail}</p>
          {c.examples.length > 0 && (
            <ul className="mt-1 flex flex-col gap-0.5 text-sm">
              {c.examples.slice(0, 5).map((x) => (
                <li key={x.eval_path} className="break-all">
                  <span className="mono-data">{x.eval_path}</span>
                  {x.train_path && (
                    <>
                      {" "}
                      is also <span className="mono-data">{x.train_path}</span>
                    </>
                  )}
                </li>
              ))}
            </ul>
          )}
        </div>
      ))}
      {report.not_checked.map((n) => (
        <p key={n} className="text-sm">
          Not checked: {n}
        </p>
      ))}
    </div>
  );
}

function ReleasePanel({ id, versionId }: { id: string; versionId: string }) {
  const organizationId = useOrganizationId();
  const { can } = useSession();
  const client = useQueryClient();
  const key = orgKey(organizationId, "quality", "releases", id);
  const releases = useQuery({
    queryKey: key,
    queryFn: ({ signal }) => listReleases(id, signal),
    enabled: Boolean(organizationId),
  });
  const list = releases.data?.releases ?? [];
  const mine = list.find((r) => r.version_id === versionId);
  const earlier = mine ? list.filter((r) => r.version < mine.version).length > 0 : false;
  const diff = useQuery({
    queryKey: orgKey(organizationId, "quality", "release-diff", id, mine?.label ?? ""),
    queryFn: ({ signal }) => releaseDiff(id, mine!.label, signal),
    enabled: Boolean(organizationId && mine && earlier),
  });
  const [label, setLabel] = useState("");
  const [notes, setNotes] = useState("");
  const name = useMutation({
    mutationFn: () => release(id, versionId, label.trim(), notes.trim()),
    onSuccess: (r) => {
      notify.success(`Released as ${r.label}`);
      client.invalidateQueries({ queryKey: key });
    },
    onError: notify.error,
  });
  const labelError = label.trim() !== "" && !/^[A-Za-z0-9][A-Za-z0-9._-]{0,39}$/.test(label.trim()) ? "Letters, digits, '.', '_' and '-', up to 40." : undefined;

  function submit(e: FormEvent) {
    e.preventDefault();
    if (label.trim() && !labelError) name.mutate();
  }

  return (
    <Panel title="Release" description="Name a version a release, such as DR1 or 2026.10, once. Each release is compared with the one before it.">
      {releases.isPending ? (
        <SkeletonRows rows={2} />
      ) : releases.isError ? (
        <ErrorState error={releases.error} onRetry={() => releases.refetch()} />
      ) : mine ? (
        <div className="flex flex-col gap-4">
          <DefList
            items={[
              { label: "Release", value: <span className="font-semibold">{mine.label}</span> },
              { label: "Files", value: `${formatNumber(mine.members)}, ${formatBytes(mine.bytes)}` },
              ...(mine.notes ? [{ label: "Notes", value: mine.notes }] : []),
            ]}
          />
          {!earlier ? (
            <p className="text-sm text-muted">The first release: nothing before it to compare with.</p>
          ) : diff.isPending ? (
            <SkeletonRows rows={2} />
          ) : diff.isError ? (
            <ErrorState error={diff.error} onRetry={() => diff.refetch()} />
          ) : (
            <div className="flex flex-col gap-3">
              <p className="text-base">
                Against {diff.data.previous.label}: {formatNumber(diff.data.members.added)} added, {formatNumber(diff.data.members.removed)} removed,{" "}
                {formatNumber(diff.data.members.changed)} changed, {formatNumber(diff.data.members.relabeled)} relabelled.
              </p>
              {diff.data.drift ? <DriftView report={diff.data.drift} /> : <p className="text-sm text-muted">{diff.data.drift_note}</p>}
            </div>
          )}
        </div>
      ) : can("datasets.manage") ? (
        <form onSubmit={submit} className="flex flex-col gap-4">
          <Field label="Label" hint="For example DR2" error={labelError} required>
            {(props) => <Input {...props} spellCheck={false} maxLength={40} value={label} onChange={(e) => setLabel(e.target.value)} />}
          </Field>
          <Field label="Notes" hint="What changed, for the people who use it.">
            {(props) => <Textarea {...props} rows={3} maxLength={4000} value={notes} onChange={(e) => setNotes(e.target.value)} />}
          </Field>
          <div>
            <SubmitButton valid={label.trim() !== "" && !labelError} loading={name.isPending}>
              Release this version
            </SubmitButton>
          </div>
        </form>
      ) : (
        <p className="text-base text-muted">Not released.</p>
      )}
    </Panel>
  );
}
