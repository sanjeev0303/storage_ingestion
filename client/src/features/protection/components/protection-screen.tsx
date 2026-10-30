"use client";

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { Button } from "@/components/ui/button";
import { ConfirmDialog } from "@/components/ui/dialog";
import { Checkbox, Field, Input, Select } from "@/components/ui/field";
import { DefList, Meter } from "@/components/ui/page";
import { Panel } from "@/components/ui/panel";
import { EmptyState, ErrorState, SkeletonRows } from "@/components/ui/states";
import { StateBadge } from "@/components/ui/status";
import { SubmitButton } from "@/components/ui/submit-button";
import { Table, Td, Th } from "@/components/ui/table";
import { Time } from "@/components/ui/time";
import { notify } from "@/components/ui/toast";
import { useOrganizationId, useSession } from "@/lib/auth/session";
import { orgKey } from "@/lib/query/keys";
import { formatBytes, formatNumber } from "@/lib/utils/format";
import { useDataset, useUpdateDataset, useVersions } from "@/features/datasets/hooks";
import type { Dataset } from "@/features/datasets/types";
import { useStorageConnections } from "@/features/storage-connections/hooks";
import { StorageLayoutPanel } from "@/features/streaming/components/storage-layout-panel";
import { DatasetLifecycle } from "@/features/tiering/components/dataset-lifecycle";
import { DatasetProvenance } from "@/features/evidence/components/dataset-provenance";
import { QualitySettingsPanel } from "@/features/quality/components/quality-settings-panel";
import {
  cancelJob,
  cleanDiff,
  getReplication,
  listJobs,
  listScrubs,
  planMigration,
  setCleanPoint,
  startDrill,
  startMigration,
  startRestore,
  type CopyJob,
  type CopyJobState,
  type MigrationPlan,
} from "../api";

const KIND_TEXT: Record<CopyJob["kind"], string> = { MIGRATE: "Migration", RESTORE: "Restore", EXPORT: "Export bundle", DRILL: "Restore drill" };
const STATE_TONE: Record<CopyJobState, "active" | "done" | "failed" | "neutral"> = {
  QUEUED: "neutral",
  RUNNING: "active",
  COMPLETED: "done",
  FAILED: "failed",
  CANCELLED: "neutral",
};

function duration(seconds: number) {
  if (seconds < 120) return `${seconds} seconds`;
  if (seconds < 7200) return `${Math.round(seconds / 60)} minutes`;
  if (seconds < 172800) return `${Math.round(seconds / 3600)} hours`;
  return `${Math.round(seconds / 86400)} days`;
}

function useProtectionKeys(id: string) {
  const organizationId = useOrganizationId();
  const client = useQueryClient();
  return () => client.invalidateQueries({ queryKey: orgKey(organizationId, "protection", id) });
}

export function ProtectionScreen({ id }: { id: string }) {
  const dataset = useDataset(id);
  if (dataset.isPending) return <SkeletonRows rows={6} />;
  if (dataset.isError) return <ErrorState error={dataset.error} onRetry={() => dataset.refetch()} />;
  return (
    <div className="grid gap-6 lg:grid-cols-3">
      <div className="flex min-w-0 flex-col gap-6 lg:col-span-2">
        <ReplicaPanel dataset={dataset.data} />
        <JobsPanel dataset={dataset.data} />
        <CleanPointPanel dataset={dataset.data} />
      </div>
      <div className="flex min-w-0 flex-col gap-6">
        <SettingsPanel key={dataset.data.revision} dataset={dataset.data} />
        <StorageLayoutPanel datasetId={id} />
        <DatasetLifecycle datasetId={id} />
        <DatasetProvenance datasetId={id} />
        <QualitySettingsPanel datasetId={id} />
        <ScrubsPanel id={id} />
      </div>
    </div>
  );
}

function ReplicaPanel({ dataset }: { dataset: Dataset }) {
  const organizationId = useOrganizationId();
  const replication = useQuery({
    queryKey: orgKey(organizationId, "protection", dataset.id, "replication"),
    queryFn: ({ signal }) => getReplication(dataset.id, signal),
    enabled: Boolean(organizationId),
    refetchInterval: 30_000,
  });
  const r = replication.data;
  return (
    <Panel
      title="Replica at a second connection"
      description="Every object the dataset refers to is copied there and verified by SHA-256. Reads fail over to it when the dataset's own storage does not answer."
    >
      {replication.isPending ? (
        <SkeletonRows rows={3} />
      ) : replication.isError ? (
        <ErrorState error={replication.error} onRetry={() => replication.refetch()} />
      ) : !r?.connection_id ? (
        <EmptyState title="No replica">Choose a replica connection in the settings to keep a verified copy elsewhere.</EmptyState>
      ) : (
        <div className="flex flex-col gap-4">
          <Meter label="Objects copied" used={r.replicated} limit={r.objects} format={formatNumber} />
          <DefList
            items={[
              {
                label: "Recovery point",
                value: r.pending === 0 ? "Up to date" : `${duration(r.recovery_point_seconds)} behind (${formatNumber(r.pending)} objects waiting)`,
              },
              { label: "Copied", value: formatBytes(r.replicated_bytes) },
              { label: "Newest copy", value: <Time value={r.newest_replica_at} /> },
              {
                label: "Failing",
                value: r.failing > 0 ? <span>{formatNumber(r.failing)} objects: {r.last_error}</span> : "None",
              },
            ]}
          />
        </div>
      )}
    </Panel>
  );
}

function SettingsPanel({ dataset }: { dataset: Dataset }) {
  const { can } = useSession();
  const connections = useStorageConnections();
  const update = useUpdateDataset(dataset.id);
  const refresh = useProtectionKeys(dataset.id);
  const [replica, setReplica] = useState(dataset.replica_connection_id ?? "");
  const [escrow, setEscrow] = useState(Boolean(dataset.escrow));
  const [vault, setVault] = useState(String(dataset.vault_retention_days ?? 0));
  const [scrub, setScrub] = useState(String(dataset.scrub_days ?? 0));
  const editable = can("datasets.manage");
  const own = dataset.storage_connection_id || connections.data?.default_connection_id;
  const vaultDays = Number(vault);
  const scrubDays = Number(scrub);
  const vaultError =
    !Number.isInteger(vaultDays) || vaultDays < 0 || vaultDays > 36500
      ? "Whole days, 0 to 36500."
      : (dataset.vault_retention_days ?? 0) > 0 && vaultDays < (dataset.vault_retention_days ?? 0)
        ? "A locked vault's retention can only grow."
        : undefined;
  const scrubError = !Number.isInteger(scrubDays) || scrubDays < 0 || scrubDays > 3650 ? "Whole days, 0 to 3650." : undefined;

  function submit(e: FormEvent) {
    e.preventDefault();
    update.mutate(
      {
        revision: dataset.revision,
        ...(replica !== (dataset.replica_connection_id ?? "") && { replica_connection_id: replica }),
        ...(escrow !== Boolean(dataset.escrow) && { escrow }),
        ...(vaultDays !== (dataset.vault_retention_days ?? 0) && { vault_retention_days: vaultDays }),
        ...(scrubDays !== (dataset.scrub_days ?? 0) && { scrub_days: scrubDays }),
      },
      {
        onSuccess: async () => {
          await refresh();
          notify.success("Protection saved");
        },
        onError: notify.error,
      },
    );
  }

  return (
    <Panel title="Protection settings" tone="in">
      <form onSubmit={submit} className="flex flex-col gap-4">
        <fieldset disabled={!editable} className="flex flex-col gap-4">
          <Field label="Replica connection" hint="A connection at another provider or account than the dataset's own.">
            {(props) => (
              <Select {...props} name="replica_connection_id" value={replica} onChange={(e) => setReplica(e.target.value)}>
                <option value="">No replica</option>
                {connections.data?.connections
                  .filter((c) => c.status === "ACTIVE" && c.id !== own)
                  .map((c) => (
                    <option key={c.id} value={c.id}>
                      {c.name}
                    </option>
                  ))}
              </Select>
            )}
          </Field>
          <Checkbox
            checked={escrow}
            onChange={(e) => setEscrow(e.target.checked)}
            label="Escrow: nothing in the dataset is deleted until its copy is verified at the replica"
          />
          <Field
            label="Locked vault, days"
            error={vaultError}
            hint="Copies are locked in COMPLIANCE mode this long. Needs a replica bucket with Object Lock in another account. 0 is off."
          >
            {(props) => <Input {...props} name="vault_retention_days" inputMode="numeric" className="tabular-nums" value={vault} onChange={(e) => setVault(e.target.value)} />}
          </Field>
          <Field label="Scrub every, days" error={scrubError} hint="Each member is re-read and re-hashed once in this many days. 0 is off.">
            {(props) => <Input {...props} name="scrub_days" inputMode="numeric" className="tabular-nums" value={scrub} onChange={(e) => setScrub(e.target.value)} />}
          </Field>
        </fieldset>
        {editable ? (
          <SubmitButton valid={!vaultError && !scrubError} loading={update.isPending} hint="Fix the highlighted fields.">
            Save protection
          </SubmitButton>
        ) : (
          <p className="text-sm text-muted">Changing it needs the datasets.manage permission.</p>
        )}
      </form>
    </Panel>
  );
}

function JobsPanel({ dataset }: { dataset: Dataset }) {
  const organizationId = useOrganizationId();
  const { can } = useSession();
  const refresh = useProtectionKeys(dataset.id);
  const jobs = useQuery({
    queryKey: orgKey(organizationId, "protection", dataset.id, "jobs"),
    queryFn: ({ signal }) => listJobs(dataset.id, signal),
    enabled: Boolean(organizationId),
    refetchInterval: (q) => (q.state.data?.jobs.some((j) => j.state === "QUEUED" || j.state === "RUNNING") ? 5_000 : false),
  });
  const [open, setOpen] = useState<"drill" | "restore" | "migrate" | null>(null);
  const cancel = useMutation({ mutationFn: cancelJob });

  return (
    <Panel
      title="Drills, restores and migrations"
      description="Every copy is read back and checked against its SHA-256 before it counts."
      actions={
        can("datasets.manage") && (
          <div className="flex flex-wrap gap-2">
            <Button onClick={() => setOpen("drill")}>Run a drill</Button>
            <Button onClick={() => setOpen("restore")}>Restore a version</Button>
            {can("storage.manage") && <Button onClick={() => setOpen("migrate")}>Migrate</Button>}
          </div>
        )
      }
    >
      {open === "drill" && <DrillForm id={dataset.id} onDone={() => { setOpen(null); refresh(); }} />}
      {open === "restore" && <RestoreForm id={dataset.id} onDone={() => { setOpen(null); refresh(); }} />}
      {open === "migrate" && <MigrateForm dataset={dataset} onDone={() => { setOpen(null); refresh(); }} />}
      {jobs.isPending ? (
        <SkeletonRows rows={3} />
      ) : jobs.isError ? (
        <ErrorState error={jobs.error} onRetry={() => jobs.refetch()} />
      ) : jobs.data.jobs.length === 0 ? (
        <EmptyState title="No jobs yet">A restore drill shows whether the copies read back intact, and how long a full restore would take.</EmptyState>
      ) : (
        <ul className="mt-2 flex flex-col gap-3">
          {jobs.data.jobs.map((j) => (
            <li key={j.id} className="neo-in-sm p-3">
              <div className="flex flex-wrap items-center justify-between gap-2">
                <div className="flex flex-wrap items-center gap-2">
                  <StateBadge tone={STATE_TONE[j.state]}>{j.state.toLowerCase()}</StateBadge>
                  <span className="font-medium">{KIND_TEXT[j.kind]}</span>
                </div>
                {(j.state === "QUEUED" || j.state === "RUNNING") && (
                  <Button disabled={cancel.isPending} onClick={() => cancel.mutate(j.id, { onSuccess: () => refresh(), onError: notify.error })}>
                    Cancel
                  </Button>
                )}
              </div>
              <p className="mt-1 text-sm text-muted tabular-nums">
                {formatNumber(j.done_items)} of {formatNumber(j.total_items)} objects, {formatBytes(j.done_bytes)}
                {j.failed_items > 0 && `, ${formatNumber(j.failed_items)} failed`}. Started <Time value={j.created_at} />
              </p>
              <JobSummary job={j} />
              {j.error && <p className="mt-1 text-sm font-medium">{j.error}</p>}
            </li>
          ))}
        </ul>
      )}
    </Panel>
  );
}

function JobSummary({ job }: { job: CopyJob }) {
  const r = job.report ?? {};
  if (job.kind === "DRILL" && job.state === "COMPLETED") {
    const seconds = Number(r.estimated_restore_seconds ?? 0);
    return (
      <p className="mt-1 text-sm">
        {String(r.intact ?? 0)} of {String(r.sampled ?? 0)} read back intact
        {seconds > 0 && `; a full restore would take about ${duration(seconds)} at this speed`}.
      </p>
    );
  }
  if (job.kind === "MIGRATE" && job.state === "COMPLETED") {
    return <p className="mt-1 text-sm">{String(r.switched ?? 0)} assets now read from the target. The source objects were left in place.</p>;
  }
  return null;
}

function ConnectionSelect({ name, value, onChange, exclude }: { name: string; value: string; onChange: (v: string) => void; exclude?: string }) {
  const connections = useStorageConnections();
  return (
    <Field label="Target connection">
      {(props) => (
        <Select {...props} name={name} value={value} onChange={(e) => onChange(e.target.value)}>
          <option value="">Choose a connection</option>
          {connections.data?.connections
            .filter((c) => c.status === "ACTIVE" && c.id !== exclude)
            .map((c) => (
              <option key={c.id} value={c.id}>
                {c.name}
              </option>
            ))}
        </Select>
      )}
    </Field>
  );
}

function DrillForm({ id, onDone }: { id: string; onDone: () => void }) {
  const [sample, setSample] = useState("100");
  const [fromReplica, setFromReplica] = useState(true);
  const run = useMutation({ mutationFn: () => startDrill(id, { sample: Number(sample), from_replica: fromReplica }) });
  const valid = Number.isInteger(Number(sample)) && Number(sample) >= 1 && Number(sample) <= 10000;
  return (
    <form
      className="neo-in-sm mb-4 flex flex-col gap-3 p-3"
      onSubmit={(e) => {
        e.preventDefault();
        run.mutate(undefined, { onSuccess: () => { notify.success("Drill queued"); onDone(); }, onError: notify.error });
      }}
    >
      <Field label="Objects to sample" hint="1 to 10,000.">
        {(props) => <Input {...props} name="sample" inputMode="numeric" className="tabular-nums" value={sample} onChange={(e) => setSample(e.target.value)} />}
      </Field>
      <Checkbox checked={fromReplica} onChange={(e) => setFromReplica(e.target.checked)} label="Read from the replica, as a restore would" />
      <SubmitButton valid={valid} loading={run.isPending} hint="Sample 1 to 10,000 objects.">
        Start drill
      </SubmitButton>
    </form>
  );
}

function RestoreForm({ id, onDone }: { id: string; onDone: () => void }) {
  const versions = useVersions(id);
  const [version, setVersion] = useState("");
  const [target, setTarget] = useState("");
  const [prefix, setPrefix] = useState("");
  const run = useMutation({ mutationFn: () => startRestore(id, { version_id: version, target_connection_id: target, prefix: prefix.trim() }) });
  return (
    <form
      className="neo-in-sm mb-4 flex flex-col gap-3 p-3"
      onSubmit={(e) => {
        e.preventDefault();
        run.mutate(undefined, { onSuccess: () => { notify.success("Restore queued"); onDone(); }, onError: notify.error });
      }}
    >
      <Field label="Version">
        {(props) => (
          <Select {...props} name="restore_version" value={version} onChange={(e) => setVersion(e.target.value)}>
            <option value="">Choose a version</option>
            {versions.items.map((v) => (
              <option key={v.id} value={v.id}>
                Version {v.version}
              </option>
            ))}
          </Select>
        )}
      </Field>
      <ConnectionSelect name="restore_target" value={target} onChange={setTarget} />
      <Field label="Folder name" hint="Under the connection's .restores folder; empty uses the job's ID.">
        {(props) => <Input {...props} name="restore_prefix" autoComplete="off" spellCheck={false} className="mono-data" value={prefix} onChange={(e) => setPrefix(e.target.value)} />}
      </Field>
      <SubmitButton valid={version !== "" && target !== ""} loading={run.isPending} hint="Choose a version and a target.">
        Start restore
      </SubmitButton>
    </form>
  );
}

function MigrateForm({ dataset, onDone }: { dataset: Dataset; onDone: () => void }) {
  const [target, setTarget] = useState("");
  const [plan, setPlan] = useState<MigrationPlan | null>(null);
  const [confirming, setConfirming] = useState(false);
  const preview = useMutation({ mutationFn: () => planMigration(dataset.id, target) });
  return (
    <div className="neo-in-sm mb-4 flex flex-col gap-3 p-3">
      <ConnectionSelect name="migration_target" value={target} onChange={(v) => { setTarget(v); setPlan(null); }} exclude={dataset.storage_connection_id} />
      <Button
        className="self-start"
        disabled={!target}
        loading={preview.isPending}
        onClick={() => preview.mutate(undefined, { onSuccess: (r) => setPlan(r.plan), onError: notify.error })}
      >
        Estimate
      </Button>
      {plan && (
        <div role="status" className="flex flex-col gap-2">
          <DefList
            items={[
              { label: "To copy", value: `${formatNumber(plan.objects)} objects, ${formatBytes(plan.bytes)}` },
              { label: "Already there", value: formatNumber(plan.already_there) },
              { label: "Egress estimate", value: `$${plan.egress_estimate_usd.toFixed(2)}` },
            ]}
          />
          <p className="text-sm text-muted">{plan.egress_note}.</p>
          <Button variant="accent" className="self-start" onClick={() => setConfirming(true)}>
            Migrate {formatNumber(plan.objects)} objects
          </Button>
        </div>
      )}
      <ConfirmDialog
        open={confirming}
        onOpenChange={setConfirming}
        title="Migrate this dataset?"
        description="Every object is copied and read back first. Only when all are verified does the dataset switch, in one step; if any fails, nothing switches. Source objects are left in place."
        confirmLabel="Start migration"
        onConfirm={async () => {
          await startMigration(dataset.id, target);
          notify.success("Migration queued");
          onDone();
        }}
      />
    </div>
  );
}

function CleanPointPanel({ dataset }: { dataset: Dataset }) {
  const organizationId = useOrganizationId();
  const { can } = useSession();
  const versions = useVersions(dataset.id);
  const refresh = useProtectionKeys(dataset.id);
  const [version, setVersion] = useState(dataset.clean_version_id ?? "");
  const diff = useQuery({
    queryKey: orgKey(organizationId, "protection", dataset.id, "clean", dataset.clean_version_id ?? ""),
    queryFn: ({ signal }) => cleanDiff(dataset.id, signal),
    enabled: Boolean(organizationId) && Boolean(dataset.clean_version_id),
  });
  const mark = useMutation({ mutationFn: () => setCleanPoint(dataset.id, version) });

  return (
    <Panel
      title="Clean point"
      description="Mark the last version known good before an incident, see everything that changed since and who uploaded it, then restore it elsewhere."
    >
      {can("datasets.manage") && (
        <form
          className="mb-4 flex flex-wrap items-end gap-3"
          onSubmit={(e) => {
            e.preventDefault();
            mark.mutate(undefined, { onSuccess: () => { notify.success("Clean point set"); refresh(); }, onError: notify.error });
          }}
        >
          <Field label="Known-good version" className="w-56">
            {(props) => (
              <Select {...props} name="clean_version" value={version} onChange={(e) => setVersion(e.target.value)}>
                <option value="">None</option>
                {versions.items.map((v) => (
                  <option key={v.id} value={v.id}>
                    Version {v.version}
                  </option>
                ))}
              </Select>
            )}
          </Field>
          <SubmitButton valid={version !== (dataset.clean_version_id ?? "")} loading={mark.isPending} hint="Choose another version.">
            Mark clean
          </SubmitButton>
        </form>
      )}
      {!dataset.clean_version_id ? (
        <EmptyState title="No clean point">After an incident, mark the last version you trust here.</EmptyState>
      ) : diff.isPending ? (
        <SkeletonRows rows={3} />
      ) : diff.isError ? (
        <ErrorState error={diff.error} onRetry={() => diff.refetch()} />
      ) : diff.data.changes.length === 0 ? (
        <p className="text-base">Nothing changed since the clean point.</p>
      ) : (
        <Table>
          <thead>
            <tr>
              <Th>Path</Th>
              <Th>Change</Th>
              <Th>Uploaded by</Th>
            </tr>
          </thead>
          <tbody>
            {diff.data.changes.map((c) => (
              <tr key={`${c.path}-${c.change}`}>
                <Td className="mono-data [overflow-wrap:anywhere]">{c.path}</Td>
                <Td>{c.change.toLowerCase()}</Td>
                <Td className="text-sm">
                  {c.uploaded_by ? (
                    <>
                      <span className="mono-data">{c.uploaded_by}</span>, <Time value={c.uploaded_at} />
                    </>
                  ) : (
                    "-"
                  )}
                </Td>
              </tr>
            ))}
          </tbody>
        </Table>
      )}
    </Panel>
  );
}

function ScrubsPanel({ id }: { id: string }) {
  const organizationId = useOrganizationId();
  const scrubs = useQuery({
    queryKey: orgKey(organizationId, "protection", id, "scrubs"),
    queryFn: ({ signal }) => listScrubs(id, signal),
    enabled: Boolean(organizationId),
  });
  return (
    <Panel title="Scrub findings" description="Members whose last re-read found them damaged or missing." tone="in">
      {scrubs.isPending ? (
        <SkeletonRows rows={2} />
      ) : scrubs.isError ? (
        <ErrorState error={scrubs.error} onRetry={() => scrubs.refetch()} />
      ) : scrubs.data.scrubs.length === 0 ? (
        <p className="text-base text-muted">No damage found.</p>
      ) : (
        <ul className="flex flex-col gap-2">
          {scrubs.data.scrubs.map((s) => (
            <li key={s.asset_id} className="text-sm">
              <StateBadge tone={s.repaired ? "warning" : "failed"}>{s.repaired ? "repaired" : "damaged"}</StateBadge>{" "}
              <span className="mono-data [overflow-wrap:anywhere]">{s.path || s.asset_id}</span>
              <span className="block text-muted">
                {s.detail}. <Time value={s.checked_at} />
              </span>
            </li>
          ))}
        </ul>
      )}
    </Panel>
  );
}
