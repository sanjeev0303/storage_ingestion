"use client";

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { Button } from "@/components/ui/button";
import { ConfirmDialog } from "@/components/ui/dialog";
import { Field, Input, Select, Textarea } from "@/components/ui/field";
import { DefList, Meter } from "@/components/ui/page";
import { Panel } from "@/components/ui/panel";
import { EmptyState, ErrorState, SkeletonRows } from "@/components/ui/states";
import { StateBadge } from "@/components/ui/status";
import { SubmitButton } from "@/components/ui/submit-button";
import { Table, Td, Th } from "@/components/ui/table";
import { Time } from "@/components/ui/time";
import { notify } from "@/components/ui/toast";
import { saveBlob } from "@/features/evidence/api";
import { useOrganizationId, useSession } from "@/lib/auth/session";
import { orgKey } from "@/lib/query/keys";
import { formatBytes, formatNumber } from "@/lib/utils/format";
import { splitList } from "@/lib/utils/forgive";
import {
  approveErasure,
  cancelErasure,
  createErasure,
  executeErasure,
  getCertificate,
  getErasure,
  listErasures,
  search,
  type ErasureKind,
  type ErasureState,
  type Plan,
  type Selector,
} from "../api";

const KIND_TEXT: Record<ErasureKind, string> = {
  SUBJECT_WITHDRAWAL: "Subject withdrawal",
  COPYRIGHT: "Copyright takedown",
  PII: "Personal data found",
  CONSENT_EXPIRED: "Consent expired",
  OTHER: "Other",
};

const STATE: Record<ErasureState, { text: string; tone: "neutral" | "active" | "done" | "failed" | "warning" }> = {
  PLANNED: { text: "Planned", tone: "neutral" },
  APPROVED: { text: "Approved", tone: "neutral" },
  DELETING: { text: "Deleting", tone: "active" },
  WAITING_ON_HOLD: { text: "Waiting on a hold", tone: "warning" },
  COMPLETED: { text: "Completed", tone: "done" },
  CANCELLED: { text: "Cancelled", tone: "neutral" },
  FAILED: { text: "Failed", tone: "failed" },
};

function useErasureKeys() {
  const organizationId = useOrganizationId();
  const client = useQueryClient();
  return { organizationId, refresh: () => client.invalidateQueries({ queryKey: orgKey(organizationId, "erasure") }) };
}

// Find content by subject, hash or object, see everything it reaches,
// and erase it with a signed certificate (docs/erasure.md).
export function ErasureScreen() {
  const [openId, setOpenId] = useState<string | null>(null);
  return (
    <div className="grid gap-6 lg:grid-cols-3">
      <div className="flex min-w-0 flex-col gap-6 lg:col-span-2">
        <FindPanel onCreated={setOpenId} />
        {openId && <ErasureDetail key={openId} id={openId} />}
      </div>
      <div className="flex min-w-0 flex-col gap-6">
        <RequestsPanel openId={openId} onOpen={setOpenId} />
      </div>
    </div>
  );
}

function FindPanel({ onCreated }: { onCreated: (id: string) => void }) {
  const { refresh } = useErasureKeys();
  const [subjects, setSubjects] = useState("");
  const [hashes, setHashes] = useState("");
  const [objects, setObjects] = useState("");
  const [subjectKey, setSubjectKey] = useState("subject_id");
  const [request, setRequest] = useState({ kind: "SUBJECT_WITHDRAWAL" as ErasureKind, reason: "", reference: "", institution: "" });
  const selector = (): Selector => ({
    subject_ids: splitList(subjects),
    sha256s: splitList(hashes).map((h) => h.toLowerCase()),
    object_ids: splitList(objects),
    subject_key: subjectKey.trim() || "subject_id",
  });
  const sel = selector();
  const badHash = sel.sha256s.find((h) => !/^[0-9a-f]{64}$/.test(h));
  const any = sel.subject_ids.length + sel.sha256s.length + sel.object_ids.length > 0;
  const find = useMutation({ mutationFn: () => search(selector()), onError: notify.error });
  const create = useMutation({
    mutationFn: () => createErasure({ ...selector(), ...request, reason: request.reason.trim() }),
    onSuccess: (e) => {
      notify.success("Erasure request recorded");
      refresh();
      find.reset();
      onCreated(e.id);
    },
    onError: notify.error,
  });

  function submit(e: FormEvent) {
    e.preventDefault();
    if (any && !badHash) find.mutate();
  }

  return (
    <Panel
      title="Find what to erase"
      description="By subject, by content hash or by object. Every exact copy on every storage connection is found, and every snapshot, draft and model it reached."
    >
      <form onSubmit={submit} className="flex flex-col gap-4">
        <div className="grid gap-4 sm:grid-cols-3">
          <Field label="Subjects" hint="One per line">
            {(props) => <Textarea {...props} rows={4} spellCheck={false} value={subjects} onChange={(e) => setSubjects(e.target.value)} />}
          </Field>
          <Field label="SHA-256 hashes" hint="One per line" error={badHash ? `${badHash.slice(0, 16)}… is not a SHA-256` : undefined}>
            {(props) => (
              <Textarea {...props} rows={4} spellCheck={false} className="mono-data" value={hashes} onChange={(e) => setHashes(e.target.value)} />
            )}
          </Field>
          <Field label="Object IDs" hint="Every version of each">
            {(props) => <Textarea {...props} rows={4} spellCheck={false} value={objects} onChange={(e) => setObjects(e.target.value)} />}
          </Field>
        </div>
        <Field label="Subject metadata key" hint="Where files name their subject" className="max-w-xs">
          {(props) => <Input {...props} spellCheck={false} value={subjectKey} onChange={(e) => setSubjectKey(e.target.value)} />}
        </Field>
        <div>
          <SubmitButton valid={any && !badHash} loading={find.isPending} hint="Name a subject, a hash or an object.">
            Find
          </SubmitButton>
        </div>
      </form>
      {find.data && (
        <div className="mt-6 flex flex-col gap-5 border-t border-faint pt-6" aria-live="polite">
          <PlanView plan={find.data} />
          {find.data.objects.length + find.data.versions.length + find.data.drafts.length > 0 && (
            <form
              onSubmit={(e) => {
                e.preventDefault();
                if (request.reason.trim()) create.mutate();
              }}
              className="flex flex-col gap-4"
            >
              <h3 className="text-base font-semibold">Record the request</h3>
              <div className="grid gap-4 sm:grid-cols-2">
                <Field label="Kind">
                  {(props) => (
                    <Select {...props} value={request.kind} onChange={(e) => setRequest({ ...request, kind: e.target.value as ErasureKind })}>
                      {Object.entries(KIND_TEXT).map(([value, label]) => (
                        <option key={value} value={value}>
                          {label}
                        </option>
                      ))}
                    </Select>
                  )}
                </Field>
                <Field label="Institution" hint="Who the certificate is for">
                  {(props) => <Input {...props} value={request.institution} onChange={(e) => setRequest({ ...request, institution: e.target.value })} />}
                </Field>
                <Field label="Reason" required>
                  {(props) => <Input {...props} value={request.reason} onChange={(e) => setRequest({ ...request, reason: e.target.value })} />}
                </Field>
                <Field label="Reference" hint="A case, ticket or order">
                  {(props) => <Input {...props} value={request.reference} onChange={(e) => setRequest({ ...request, reference: e.target.value })} />}
                </Field>
              </div>
              <div>
                <SubmitButton valid={request.reason.trim() !== ""} loading={create.isPending} hint="Give the reason.">
                  Record erasure request
                </SubmitButton>
              </div>
            </form>
          )}
        </div>
      )}
    </Panel>
  );
}

function PlanView({ plan }: { plan: Plan }) {
  const held = plan.objects.filter((o) => o.held_by.length > 0).length;
  return (
    <div className="flex flex-col gap-4">
      <DefList
        items={[
          { label: "Files", value: `${formatNumber(plan.objects.length)}, ${formatBytes(plan.bytes)} on ${formatNumber(plan.connections.length)} connections` },
          { label: "Held back now", value: held ? `${formatNumber(held)} files` : "None" },
          { label: "Snapshots that pin them", value: formatNumber(plan.versions.length) },
          { label: "Drafts that hold them", value: formatNumber(plan.drafts.length) },
          { label: "Models trained on them", value: plan.models.length ? plan.models.map((m) => `${m.name} ${m.version}`).join(", ") : "None" },
        ]}
      />
      {plan.objects.length > 0 && (
        <Table>
          <thead>
            <tr>
              <Th>File</Th>
              <Th>Size</Th>
              <Th>Held by</Th>
            </tr>
          </thead>
          <tbody>
            {plan.objects.slice(0, 50).map((o) => (
              <tr key={o.asset_id}>
                <Td className="text-sm">
                  <span className="mono-data block break-all">{o.path}</span>
                  <span className="text-muted">
                    {o.connection_id}, SHA-256 {o.sha256.slice(0, 12)}…
                  </span>
                </Td>
                <Td className="text-sm tabular-nums">{formatBytes(o.size_bytes)}</Td>
                <Td className="text-sm">{o.held_by.length ? o.held_by.join("; ") : <span className="text-muted">Nothing</span>}</Td>
              </tr>
            ))}
          </tbody>
        </Table>
      )}
      {plan.objects.length > 50 && <p className="text-sm text-muted">And {formatNumber(plan.objects.length - 50)} more files.</p>}
      {plan.versions.length > 0 && (
        <ul className="flex flex-col gap-1 text-sm">
          {plan.versions.map((v) => (
            <li key={v.version_id}>
              {v.dataset_name} version {v.version}: {formatNumber(v.members)} members leave it
              {v.current ? ", the current version" : ""}
              {v.superseded_by ? ", already superseded" : ""}
            </li>
          ))}
        </ul>
      )}
      <ul className="flex flex-col gap-1 text-sm text-muted">
        {plan.not_searched.map((n) => (
          <li key={n}>Not searched: {n}</li>
        ))}
      </ul>
    </div>
  );
}

function RequestsPanel({ openId, onOpen }: { openId: string | null; onOpen: (id: string) => void }) {
  const { organizationId } = useErasureKeys();
  const list = useQuery({
    queryKey: orgKey(organizationId, "erasure", "list"),
    queryFn: ({ signal }) => listErasures(signal),
    enabled: Boolean(organizationId),
  });
  const items = list.data?.erasures ?? [];
  return (
    <Panel title="Requests" description="Newest first.">
      {list.isPending ? (
        <SkeletonRows rows={3} />
      ) : list.isError ? (
        <ErrorState error={list.error} onRetry={() => list.refetch()} />
      ) : items.length === 0 ? (
        <EmptyState title="No erasure requests">Find a subject, a hash or an object to start one.</EmptyState>
      ) : (
        <ul className="flex flex-col gap-2">
          {items.map((e) => (
            <li key={e.id}>
              <button
                type="button"
                aria-current={e.id === openId ? "true" : undefined}
                onClick={() => onOpen(e.id)}
                className={`w-full rounded-sm p-3 text-left transition-colors hover:bg-faint ${e.id === openId ? "bg-faint" : ""}`}
              >
                <span className="flex flex-wrap items-center gap-2">
                  <span className="font-medium">{KIND_TEXT[e.kind]}</span>
                  <StateBadge tone={STATE[e.state].tone}>{STATE[e.state].text}</StateBadge>
                </span>
                <span className="block text-sm text-muted">
                  {e.institution || e.reason.slice(0, 60)}, <Time value={e.created_at} />
                </span>
              </button>
            </li>
          ))}
        </ul>
      )}
    </Panel>
  );
}

function ErasureDetail({ id }: { id: string }) {
  const { organizationId, refresh } = useErasureKeys();
  const { user } = useSession();
  const [confirm, setConfirm] = useState(false);
  const erasure = useQuery({
    queryKey: orgKey(organizationId, "erasure", "detail", id),
    queryFn: ({ signal }) => getErasure(id, signal),
    enabled: Boolean(organizationId),
    refetchInterval: (q) => (q.state.data?.state === "DELETING" ? 10_000 : false),
  });
  const done = (message: string) => () => {
    notify.success(message);
    refresh();
  };
  const approve = useMutation({ mutationFn: () => approveErasure(id, ""), onSuccess: done("Approved"), onError: notify.error });
  const cancel = useMutation({ mutationFn: () => cancelErasure(id), onSuccess: done("Cancelled"), onError: notify.error });
  const certificate = useMutation({
    mutationFn: () => getCertificate(id),
    onSuccess: (c) => saveBlob(new Blob([JSON.stringify(c, null, 2)], { type: "application/json" }), `erasure-${id}-certificate.json`),
    onError: notify.error,
  });

  if (erasure.isPending) return <SkeletonRows rows={4} />;
  if (erasure.isError) return <ErrorState error={erasure.error} onRetry={() => erasure.refetch()} />;
  const e = erasure.data;
  const open = e.state === "PLANNED" || e.state === "APPROVED";
  const unfinished = (e.items ?? []).filter((i) => i.state !== "DESTROYED");

  return (
    <Panel
      title={KIND_TEXT[e.kind]}
      description={e.reason}
      actions={<StateBadge tone={STATE[e.state].tone}>{STATE[e.state].text}</StateBadge>}
    >
      <div className="flex flex-col gap-5">
        <DefList
          items={[
            { label: "Institution", value: e.institution || "Not named" },
            { label: "Reference", value: e.reference || "None" },
            { label: "Asked by", value: <span className="mono-data">{e.requested_by}</span> },
            { label: "Approved by", value: e.approved_by ? <span className="mono-data">{e.approved_by}</span> : "Not yet" },
          ]}
        />
        {open ? (
          <PlanView plan={e.plan} />
        ) : (
          <>
            <Meter label="Files destroyed" used={e.progress.destroyed} limit={e.progress.total} format={formatNumber} />
            {unfinished.length > 0 && (
              <ul className="flex flex-col gap-1 text-sm">
                {unfinished.slice(0, 50).map((i) => (
                  <li key={i.asset_id} className="break-words">
                    <span className="font-medium">{i.state === "HELD" ? "Held" : i.state === "FAILED" ? "Failed" : "Deleting"}</span>{" "}
                    <span className="mono-data">{i.path}</span>
                    {i.reason && <span className="text-muted">: {i.reason}</span>}
                  </li>
                ))}
                {unfinished.length > 50 && <li className="text-muted">And {formatNumber(unfinished.length - 50)} more.</li>}
              </ul>
            )}
            {e.successors.length > 0 && (
              <ul className="flex flex-col gap-1 text-sm">
                {e.successors.map((s) => (
                  <li key={s.version_id}>
                    Version {s.version} is superseded by version {s.successor_version}, without {formatNumber(s.removed)} members
                  </li>
                ))}
              </ul>
            )}
            {e.plan.models.length > 0 && (
              <p className="text-sm">
                Retrain or retire: {e.plan.models.map((m) => `${m.name} ${m.version}`).join(", ")}
              </p>
            )}
          </>
        )}
        <div className="flex flex-wrap gap-2">
          {e.state === "PLANNED" && user.id !== e.requested_by && (
            <Button onClick={() => approve.mutate()} loading={approve.isPending}>
              Approve
            </Button>
          )}
          {open && (
            <Button variant="accent" onClick={() => setConfirm(true)}>
              Erase
            </Button>
          )}
          {open && (
            <Button variant="ghost" onClick={() => cancel.mutate()} loading={cancel.isPending}>
              Cancel request
            </Button>
          )}
          {e.certified && (
            <Button onClick={() => certificate.mutate()} loading={certificate.isPending}>
              Download certificate
            </Button>
          )}
        </div>
      </div>
      <ConfirmDialog
        open={confirm}
        onOpenChange={setConfirm}
        title="Erase for good?"
        description={`${formatNumber(e.plan.objects.length)} files are deleted everywhere a legal hold allows, ${formatNumber(
          e.plan.versions.length,
        )} snapshots get successors without them, and training stops reading the old ones. This cannot be undone.`}
        confirmLabel="Erase"
        confirmText="ERASE"
        onConfirm={async () => {
          await executeErasure(id);
          done("Erasing: files are deleted as each hold allows")();
        }}
      />
    </Panel>
  );
}
