"use client";

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { Button } from "@/components/ui/button";
import { Checkbox, Field, Input, Select, Textarea } from "@/components/ui/field";
import { Panel } from "@/components/ui/panel";
import { EmptyState, ErrorState, PermissionDenied, SkeletonRows } from "@/components/ui/states";
import { StateBadge } from "@/components/ui/status";
import { SubmitButton } from "@/components/ui/submit-button";
import { Table, Td, Th } from "@/components/ui/table";
import { Time } from "@/components/ui/time";
import { notify } from "@/components/ui/toast";
import { useSession } from "@/lib/auth/session";
import { orgKey } from "@/lib/query/keys";
import { formatBytes } from "@/lib/utils/format";
import { useStorageConnections } from "@/features/storage-connections/hooks";
import {
  adoptionEntries,
  cancelAdoption,
  createAdoption,
  listAdoptions,
  resultsFile,
  type AdoptionJob,
  type AdoptionSource,
  type EntryState,
} from "../api";

const SOURCES: Record<AdoptionSource, { name: string; ref: string; hint: string; imports?: boolean; inline?: boolean }> = {
  S3_INVENTORY: { name: "S3 Inventory report", ref: "Key of the report's manifest.json", hint: "CSV reports. Single-part ETags become expected MD5s; the bucket is never listed." },
  MANIFEST: { name: "Manifest (JSON Lines)", ref: "Key of the manifest", hint: 'One line per object: {"key", "path", "md5" or "sha256" or "xxh64"...}.', inline: true },
  MHL: { name: "ASC MHL list", ref: "Key of the .mhl file", hint: "MHL v2 (paths from the folder holding ascmhl/) or legacy v1." },
  BAGIT: { name: "BagIt bag", ref: "The bag's folder", hint: "Every payload manifest present is checked; Payload-Oxum must agree." },
  URL_LIST: { name: "URL list (import)", ref: "Key of the list", hint: "Fetched and uploaded through the usual checks.", imports: true, inline: true },
  STAC: { name: "STAC catalog (import)", ref: "Item, collection or catalog URL", hint: "Every asset of every item, with its properties as metadata.", imports: true },
};

const ENTRY_TONE: Record<EntryState, "done" | "failed" | "neutral" | "active" | "warning"> = {
  ADOPTED: "done",
  IMPORTED: "active",
  PENDING: "active",
  REJECTED: "failed",
  FAILED: "failed",
  SKIPPED: "warning",
};

export function AdoptionScreen() {
  const { can, organizationId } = useSession();
  const [open, setOpen] = useState<string | null>(null);
  const jobs = useQuery({
    queryKey: orgKey(organizationId, "adoptions"),
    queryFn: ({ signal }) => listAdoptions(signal),
    enabled: Boolean(organizationId) && can("ingestion.read"),
    refetchInterval: (q) => (q.state.data?.adoptions.some((j) => ["QUEUED", "READING", "RUNNING"].includes(j.state)) ? 5000 : false),
  });
  if (!can("ingestion.read")) return <PermissionDenied detail="Adoption needs the ingestion.read permission." />;

  return (
    <div className="grid gap-6 lg:grid-cols-3">
      <div className="flex min-w-0 flex-col gap-6 lg:col-span-2">
        <Panel title="Adoption jobs" description="Each job reads one source and checks every object it names before publishing it.">
          {jobs.isPending ? (
            <SkeletonRows rows={3} />
          ) : jobs.isError ? (
            <ErrorState error={jobs.error} onRetry={() => jobs.refetch()} />
          ) : jobs.data.adoptions.length === 0 ? (
            <EmptyState title="Nothing adopted yet">
              Point a job at an S3 Inventory report, a manifest, an MHL list or a bag in your own bucket. Nothing is uploaded
              again and the bucket is not listed.
            </EmptyState>
          ) : (
            <ul className="flex flex-col gap-3">
              {jobs.data.adoptions.map((j) => (
                <JobRow key={j.id} job={j} open={open === j.id} onToggle={() => setOpen(open === j.id ? null : j.id)} />
              ))}
            </ul>
          )}
        </Panel>
      </div>
      <div className="flex min-w-0 flex-col gap-6">{can("ingestion.write") && <NewAdoption />}</div>
    </div>
  );
}

function JobRow({ job, open, onToggle }: { job: AdoptionJob; open: boolean; onToggle: () => void }) {
  const client = useQueryClient();
  const { organizationId } = useSession();
  const live = ["QUEUED", "READING", "RUNNING"].includes(job.state);
  const pending = job.entries - job.adopted - job.rejected - job.failed;

  async function download(format: "csv" | "jsonl") {
    try {
      const blob = await resultsFile(job.id, format);
      const url = URL.createObjectURL(blob);
      const a = document.createElement("a");
      a.href = url;
      a.download = `adoption-${job.id}.${format}`;
      a.click();
      URL.revokeObjectURL(url);
    } catch (error) {
      notify.error(error);
    }
  }

  return (
    <li className="neo-in-sm p-3">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div className="flex min-w-0 flex-wrap items-center gap-2">
          <StateBadge tone={job.state === "COMPLETED" ? "done" : job.state === "FAILED" ? "failed" : live ? "active" : "neutral"}>
            {job.state.toLowerCase()}
          </StateBadge>
          <span className="font-medium">{SOURCES[job.source]?.name ?? job.source}</span>
          <span translate="no" className="mono-data text-sm text-muted [overflow-wrap:anywhere]">
            {job.source_ref || "inline"}
          </span>
        </div>
        <div className="flex flex-wrap gap-2">
          {live && (
            <Button
              onClick={async () => {
                await cancelAdoption(job.id).catch(notify.error);
                await client.invalidateQueries({ queryKey: orgKey(organizationId, "adoptions") });
              }}
            >
              Cancel
            </Button>
          )}
          <Button onClick={onToggle} aria-expanded={open}>
            {open ? "Hide entries" : "Entries"}
          </Button>
        </div>
      </div>
      <p className="mt-2 text-sm tabular-nums">
        {job.entries.toLocaleString()} entries: {job.adopted.toLocaleString()} adopted ({formatBytes(job.bytes)}),{" "}
        {job.rejected.toLocaleString()} refused, {job.failed.toLocaleString()} failed
        {pending > 0 && `, ${pending.toLocaleString()} to go`}
      </p>
      <p className="text-sm text-muted">
        {job.mode === "DETACH" ? "Deleting an asset keeps your object" : "The platform manages the objects"}
        {job.dataset && `, dataset ${job.dataset}`}. Started <Time value={job.created_at} />
      </p>
      {job.error && (
        <p role="alert" className="mt-1 text-sm font-medium [overflow-wrap:anywhere]">
          {job.error}
        </p>
      )}
      {open && (
        <div className="mt-3 flex flex-col gap-2">
          <div className="flex flex-wrap gap-2">
            <Button onClick={() => download("csv")}>Download results (CSV)</Button>
            <Button onClick={() => download("jsonl")}>JSON Lines</Button>
          </div>
          <Entries id={job.id} />
        </div>
      )}
    </li>
  );
}

function Entries({ id }: { id: string }) {
  const { organizationId } = useSession();
  const entries = useQuery({
    queryKey: orgKey(organizationId, "adoptions", id, "entries"),
    queryFn: ({ signal }) => adoptionEntries(id, 0, signal),
    enabled: Boolean(organizationId),
  });
  if (entries.isPending) return <SkeletonRows rows={3} />;
  if (entries.isError) return <ErrorState error={entries.error} onRetry={() => entries.refetch()} />;
  if (entries.data.entries.length === 0) return <p className="text-sm text-muted">The source has not been read yet.</p>;
  return (
    <div className="max-h-96 overflow-auto" data-lenis-prevent>
      <Table>
        <caption className="sr-only">Entries of the adoption job</caption>
        <thead>
          <tr>
            <Th className="tabular-nums">Line</Th>
            <Th>Path</Th>
            <Th>Outcome</Th>
          </tr>
        </thead>
        <tbody>
          {entries.data.entries.map((e) => (
            <tr key={e.line}>
              <Td className="tabular-nums">{e.line}</Td>
              <Td className="max-w-xs">
                <div className="[overflow-wrap:anywhere]">{e.path}</div>
                <div translate="no" className="mono-data text-sm text-muted [overflow-wrap:anywhere]">
                  {e.object_key || e.url}
                </div>
              </Td>
              <Td>
                <StateBadge tone={ENTRY_TONE[e.state]}>{e.state.toLowerCase()}</StateBadge>
                {e.code && <div className="mono-data text-sm">{e.code}</div>}
                {e.detail && <div className="text-sm text-muted [overflow-wrap:anywhere]">{e.detail}</div>}
              </Td>
            </tr>
          ))}
        </tbody>
      </Table>
      {entries.data.next_after > 0 && <p className="mt-2 text-sm text-muted">The first 200 entries; download the results for all.</p>}
    </div>
  );
}

function NewAdoption() {
  const { organizationId } = useSession();
  const client = useQueryClient();
  const connections = useStorageConnections();
  const [source, setSource] = useState<AdoptionSource>("S3_INVENTORY");
  const [connection, setConnection] = useState("");
  const [ref, setRef] = useState("");
  const [inline, setInline] = useState("");
  const [dataset, setDataset] = useState("");
  const [manage, setManage] = useState(false);
  const [requireDigest, setRequireDigest] = useState(true);
  const create = useMutation({ mutationFn: createAdoption });
  const info = SOURCES[source];
  const own = connections.data?.connections.filter((c) => c.provider !== "PLATFORM_MANAGED" && c.status === "ACTIVE") ?? [];

  const problem = !info.imports && !connection ? "Choose the storage connection that holds the objects." : !ref.trim() && !inline.trim() ? "Give the source." : null;

  function submit(e: FormEvent) {
    e.preventDefault();
    if (problem) return;
    create.mutate(
      {
        source,
        ...(connection && { connection_id: connection }),
        ...(ref.trim() && { source_ref: ref.trim() }),
        ...(inline.trim() && { inline }),
        ...(dataset.trim() && { dataset: dataset.trim().toLowerCase() }),
        mode: manage ? "MANAGE" : "DETACH",
        require_digest: requireDigest,
      },
      {
        onSuccess: async () => {
          notify.success("Adoption queued");
          setRef("");
          setInline("");
          await client.invalidateQueries({ queryKey: orgKey(organizationId, "adoptions") });
        },
        onError: notify.error,
      },
    );
  }

  return (
    <Panel title="Adopt or import" description="Objects in your own bucket are checked where they are and published without a copy.">
      <form onSubmit={submit} className="flex flex-col gap-4">
        <Field label="Source" hint={info.hint}>
          {(props) => (
            <Select {...props} name="source" value={source} onChange={(e) => setSource(e.target.value as AdoptionSource)}>
              {(Object.keys(SOURCES) as AdoptionSource[]).map((s) => (
                <option key={s} value={s}>
                  {SOURCES[s].name}
                </option>
              ))}
            </Select>
          )}
        </Field>
        <Field label="Storage connection" hint={info.imports ? "Optional: where imports are stored. Empty uses the default." : "Your own connection that holds the objects."}>
          {(props) => (
            <Select {...props} name="connection" value={connection} onChange={(e) => setConnection(e.target.value)}>
              <option value="">{info.imports ? "Default" : "Choose a connection"}</option>
              {own.map((c) => (
                <option key={c.id} value={c.id}>
                  {c.name} ({c.bucket})
                </option>
              ))}
            </Select>
          )}
        </Field>
        <Field label={info.ref}>
          {(props) => (
            <Input {...props} name="source_ref" value={ref} className="mono-data" autoComplete="off" spellCheck={false} onChange={(e) => setRef(e.target.value)} />
          )}
        </Field>
        {info.inline && (
          <Field label="Or paste it" hint="Up to 16 MiB.">
            {(props) => (
              <Textarea {...props} name="inline" rows={4} value={inline} className="mono-data" spellCheck={false} onChange={(e) => setInline(e.target.value)} />
            )}
          </Field>
        )}
        <Field label="Dataset" hint="Optional namespace for the catalog paths.">
          {(props) => <Input {...props} name="dataset" value={dataset} className="mono-data" autoComplete="off" spellCheck={false} onChange={(e) => setDataset(e.target.value)} />}
        </Field>
        {!info.imports && (
          <>
            <Checkbox checked={requireDigest} onChange={(e) => setRequireDigest(e.target.checked)} label="Refuse objects with no expected digest" />
            <Checkbox checked={manage} onChange={(e) => setManage(e.target.checked)} label="Let the platform delete the objects when their assets are deleted" />
          </>
        )}
        <SubmitButton valid={!problem} loading={create.isPending} hint={problem ?? undefined}>
          Start
        </SubmitButton>
      </form>
    </Panel>
  );
}
