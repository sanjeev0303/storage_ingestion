"use client";

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { Button } from "@/components/ui/button";
import { Checkbox, Field, Input, Select } from "@/components/ui/field";
import { PageHeader } from "@/components/ui/page";
import { Panel } from "@/components/ui/panel";
import { EmptyState, ErrorState, SkeletonRows } from "@/components/ui/states";
import { StateBadge } from "@/components/ui/status";
import { SubmitButton } from "@/components/ui/submit-button";
import { Table, Td, Th } from "@/components/ui/table";
import { Time } from "@/components/ui/time";
import { notify } from "@/components/ui/toast";
import { useStorageConnections } from "@/features/storage-connections";
import { useOrganizationId, useSession } from "@/lib/auth/session";
import { orgKey } from "@/lib/query/keys";
import { formatNumber } from "@/lib/utils/format";
import { anchorNow, fetchFile, getSettings, listAnchors, putSettings, saveBlob, type AnchorCheck, type EvidenceSettings } from "../api";
import { AuditTabs } from "./audit-tabs";

export function EvidenceScreen() {
  return (
    <>
      <PageHeader
        title="Anchors and exports"
        description="The audit chain's head, signed and written to a locked bucket outside the database, and exports anyone can check without the platform."
      />
      <AuditTabs />
      <div className="grid gap-6 lg:grid-cols-3">
        <div className="flex min-w-0 flex-col gap-6 lg:col-span-2">
          <AnchorsPanel />
          <ExportPanel />
        </div>
        <div className="flex min-w-0 flex-col gap-6">
          <SettingsPanel />
        </div>
      </div>
    </>
  );
}

function anchorState(a: AnchorCheck) {
  if (!a.signature_ok || !a.chain_ok || a.stored_ok === false) return <StateBadge tone="failed">Does not match</StateBadge>;
  return <StateBadge tone="done">Holds</StateBadge>;
}

function AnchorsPanel() {
  const organizationId = useOrganizationId();
  const { can } = useSession();
  const client = useQueryClient();
  const anchors = useQuery({
    queryKey: orgKey(organizationId, "evidence", "anchors"),
    queryFn: ({ signal }) => listAnchors(signal),
    enabled: Boolean(organizationId),
  });
  const now = useMutation({
    mutationFn: anchorNow,
    onSuccess: (a) => {
      notify.success(`Anchored entry ${formatNumber(a.seq)}`);
      client.invalidateQueries({ queryKey: orgKey(organizationId, "evidence", "anchors") });
    },
    onError: notify.error,
  });
  const list = anchors.data?.anchors ?? [];
  return (
    <Panel
      title="Anchors"
      description="Each one is checked three ways: its signature, the chain still holding that hash at that entry, and the bucket copy still being the signed document."
      actions={
        <Button onClick={() => now.mutate()} loading={now.isPending} disabled={!can("audit.read")}>
          Anchor now
        </Button>
      }
    >
      {anchors.isPending ? (
        <SkeletonRows rows={3} />
      ) : anchors.isError ? (
        <ErrorState error={anchors.error} onRetry={() => anchors.refetch()} />
      ) : list.length === 0 ? (
        <EmptyState title="No anchors yet">The worker anchors the chain on the schedule in the settings, or anchor it now.</EmptyState>
      ) : (
        <Table>
          <thead>
            <tr>
              <Th>Entry</Th>
              <Th>Check</Th>
              <Th>Where</Th>
              <Th>Anchored</Th>
            </tr>
          </thead>
          <tbody>
            {list.map((a) => (
              <tr key={a.id}>
                <Td className="tabular-nums">{formatNumber(a.seq)}</Td>
                <Td>
                  {anchorState(a)}
                  {a.problem && <span className="mt-1 block text-sm">{a.problem}</span>}
                </Td>
                <Td className="text-sm">
                  {a.object_key ? (
                    <>
                      <span className="mono-data block break-all">{a.object_key}</span>
                      {a.locked_until && (
                        <span className="text-muted">
                          Locked until <Time value={a.locked_until} />
                        </span>
                      )}
                    </>
                  ) : (
                    <span className="text-muted">Database only</span>
                  )}
                </Td>
                <Td>
                  <Time value={a.created_at} />
                </Td>
              </tr>
            ))}
          </tbody>
        </Table>
      )}
    </Panel>
  );
}

function ExportPanel() {
  const [format, setFormat] = useState<"jsonl" | "cef">("jsonl");
  const [afterSeq, setAfterSeq] = useState("");
  const save = useMutation({
    mutationFn: () => fetchFile("/organization/audit/export", { format, after_seq: afterSeq.trim() || undefined }),
    onSuccess: (blob) => saveBlob(blob, `audit-export.${format === "cef" ? "cef" : "jsonl"}`),
    onError: notify.error,
  });
  const afterError = afterSeq.trim() !== "" && !/^\d+$/.test(afterSeq.trim()) ? "Enter an entry number." : undefined;

  function submit(e: FormEvent) {
    e.preventDefault();
    if (!afterError) save.mutate();
  }

  return (
    <Panel
      title="Export the chain"
      description="Each JSON line carries exactly what its hash covers, so the file checks itself. A page holds 10,000 entries; start the next one after the last entry you have."
    >
      <form onSubmit={submit} className="flex flex-col gap-4">
        <div className="grid gap-4 sm:grid-cols-2">
          <Field label="Format">
            {(props) => (
              <Select {...props} value={format} onChange={(e) => setFormat(e.target.value as "jsonl" | "cef")}>
                <option value="jsonl">JSON lines, to verify</option>
                <option value="cef">CEF, for a SIEM</option>
              </Select>
            )}
          </Field>
          <Field label="After entry" hint="Leave empty to start at the first." error={afterError}>
            {(props) => <Input {...props} inputMode="numeric" value={afterSeq} onChange={(e) => setAfterSeq(e.target.value)} />}
          </Field>
        </div>
        <p className="text-sm text-muted">
          Check a JSON lines export anywhere with <span className="mono-data">osp audit-verify audit-export.jsonl --anchors</span>.
        </p>
        <div>
          <SubmitButton valid={!afterError} loading={save.isPending}>
            Download export
          </SubmitButton>
        </div>
      </form>
    </Panel>
  );
}

function SettingsPanel() {
  const organizationId = useOrganizationId();
  const settings = useQuery({
    queryKey: orgKey(organizationId, "evidence", "settings"),
    queryFn: ({ signal }) => getSettings(signal),
    enabled: Boolean(organizationId),
  });
  return (
    <Panel title="Settings" description="Where anchors go, how often reviews are built, and the lawful-basis gate.">
      {settings.isPending ? (
        <SkeletonRows rows={5} />
      ) : settings.isError ? (
        <ErrorState error={settings.error} onRetry={() => settings.refetch()} />
      ) : (
        <SettingsForm key={settings.data.updated_at} initial={settings.data} />
      )}
    </Panel>
  );
}

function SettingsForm({ initial }: { initial: EvidenceSettings }) {
  const organizationId = useOrganizationId();
  const { can } = useSession();
  const client = useQueryClient();
  const connections = useStorageConnections();
  const [draft, setDraft] = useState(initial);
  const save = useMutation({
    mutationFn: () => putSettings(draft),
    onSuccess: () => {
      notify.success("Evidence settings saved");
      client.invalidateQueries({ queryKey: orgKey(organizationId, "evidence") });
    },
    onError: notify.error,
  });
  const editable = can("org.manage");

  function set<K extends keyof EvidenceSettings>(key: K, value: EvidenceSettings[K]) {
    setDraft((d) => ({ ...d, [key]: value }));
  }
  function number(key: "anchor_every_minutes" | "anchor_lock_days" | "review_every_days" | "work_start_hour" | "work_end_hour" | "bulk_read_threshold") {
    return (e: { target: { value: string } }) => set(key, Number(e.target.value));
  }

  const everyError = draft.anchor_every_minutes < 5 || draft.anchor_every_minutes > 1440 ? "Between 5 and 1440 minutes." : undefined;
  const hoursError =
    draft.work_start_hour < 0 || draft.work_end_hour > 24 || draft.work_start_hour >= draft.work_end_hour
      ? "The day starts before it ends, within 0 to 24."
      : undefined;
  const valid = !everyError && !hoursError && draft.anchor_lock_days >= 0 && draft.review_every_days >= 0 && draft.bulk_read_threshold > 0;

  function submit(e: FormEvent) {
    e.preventDefault();
    if (valid) save.mutate();
  }

  return (
    <form onSubmit={submit} className="flex flex-col gap-4">
      <fieldset disabled={!editable} className="flex flex-col gap-4">
        <Field label="Anchor connection" hint="Best a bucket with Object Lock. None keeps anchors in the database only.">
          {(props) => (
            <Select {...props} value={draft.anchor_connection_id} onChange={(e) => set("anchor_connection_id", e.target.value)}>
              <option value="">None</option>
              {(connections.data?.connections ?? []).map((c) => (
                <option key={c.id} value={c.id}>
                  {c.name}
                </option>
              ))}
            </Select>
          )}
        </Field>
        <div className="grid grid-cols-2 gap-4">
          <Field label="Anchor every" hint="Minutes" error={everyError}>
            {(props) => <Input {...props} type="number" min={5} max={1440} value={draft.anchor_every_minutes} onChange={number("anchor_every_minutes")} />}
          </Field>
          <Field label="Lock anchors for" hint="Days">
            {(props) => <Input {...props} type="number" min={0} value={draft.anchor_lock_days} onChange={number("anchor_lock_days")} />}
          </Field>
        </div>
        <Field label="Build a review every" hint="Days; 0 stops them.">
          {(props) => <Input {...props} type="number" min={0} value={draft.review_every_days} onChange={number("review_every_days")} />}
        </Field>
        <div className="grid grid-cols-2 gap-4">
          <Field label="Working day starts" hint="Hour, 0 to 23" error={hoursError}>
            {(props) => <Input {...props} type="number" min={0} max={23} value={draft.work_start_hour} onChange={number("work_start_hour")} />}
          </Field>
          <Field label="Working day ends" hint="Hour, 1 to 24">
            {(props) => <Input {...props} type="number" min={1} max={24} value={draft.work_end_hour} onChange={number("work_end_hour")} />}
          </Field>
        </div>
        <div className="grid grid-cols-2 gap-4">
          <Field label="Time zone" hint="For example Europe/London">
            {(props) => <Input {...props} value={draft.timezone} onChange={(e) => set("timezone", e.target.value)} spellCheck={false} />}
          </Field>
          <Field label="Bulk read threshold" hint="Reads in an hour, after hours">
            {(props) => <Input {...props} type="number" min={1} value={draft.bulk_read_threshold} onChange={number("bulk_read_threshold")} />}
          </Field>
        </div>
        <Checkbox
          label="Require a lawful basis, licence and source before a dataset is snapshotted or trained on"
          checked={draft.require_lawful_basis}
          onChange={(e) => set("require_lawful_basis", e.target.checked)}
        />
      </fieldset>
      {editable && (
        <div>
          <SubmitButton valid={valid} loading={save.isPending} hint="Fix the fields marked above.">
            Save settings
          </SubmitButton>
        </div>
      )}
    </form>
  );
}
