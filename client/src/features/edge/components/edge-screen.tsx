"use client";

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import Link from "next/link";
import { useState, type FormEvent } from "react";
import { Button } from "@/components/ui/button";
import { CopyButton } from "@/components/ui/copy-button";
import { ConfirmDialog } from "@/components/ui/dialog";
import { Field, Input, Select } from "@/components/ui/field";
import { Panel } from "@/components/ui/panel";
import { EmptyState, ErrorState, SkeletonRows } from "@/components/ui/states";
import { StateBadge } from "@/components/ui/status";
import { SubmitButton } from "@/components/ui/submit-button";
import { Table, Td, Th } from "@/components/ui/table";
import { Time } from "@/components/ui/time";
import { notify } from "@/components/ui/toast";
import { useDatasets } from "@/features/datasets/hooks";
import { useOrganizationId, useSession } from "@/lib/auth/session";
import { orgKey } from "@/lib/query/keys";
import { formatBytes, formatNumber } from "@/lib/utils/format";
import { listDevices, listRecordings, registerDevice, revokeDevice, type EdgeDevice, type EdgeRecording } from "../api";

// A device beats every minute; three quiet minutes means it needs a look.
const SILENT_AFTER_MS = 3 * 60 * 1000;

function deviceState(d: EdgeDevice) {
  if (d.revoked_at) return <StateBadge>Revoked</StateBadge>;
  if (!d.last_seen) return <StateBadge>Never seen</StateBadge>;
  if (Date.now() - new Date(d.last_seen).getTime() > SILENT_AFTER_MS) return <StateBadge tone="warning">Silent</StateBadge>;
  return <StateBadge tone="done">Online</StateBadge>;
}

const RECORDING_STATE: Record<EdgeRecording["state"], { text: string; tone: "active" | "done" | "warning" }> = {
  OPEN: { text: "Sending", tone: "active" },
  COMPLETE: { text: "Complete", tone: "done" },
  INCOMPLETE: { text: "Incomplete", tone: "warning" },
};

// Instruments and robots that upload through an agent, and what they sent.
export function EdgeScreen() {
  const organizationId = useOrganizationId();
  const { can } = useSession();
  const client = useQueryClient();
  const refresh = () => client.invalidateQueries({ queryKey: orgKey(organizationId, "edge") });
  const devices = useQuery({
    queryKey: orgKey(organizationId, "edge", "devices"),
    queryFn: ({ signal }) => listDevices(signal),
    enabled: Boolean(organizationId),
    refetchInterval: 30_000,
  });
  const recordings = useQuery({
    queryKey: orgKey(organizationId, "edge", "recordings"),
    queryFn: ({ signal }) => listRecordings(signal),
    enabled: Boolean(organizationId),
    refetchInterval: 30_000,
  });
  const [revoking, setRevoking] = useState<EdgeDevice | null>(null);
  const list = devices.data?.devices ?? [];
  const names = new Map(list.map((d) => [d.id, d.name]));
  const runs = recordings.data?.recordings ?? [];

  return (
    <div className="grid gap-6 lg:grid-cols-3">
      <div className="flex min-w-0 flex-col gap-6 lg:col-span-2">
        <Panel title="Devices" description="Each uploads with its own credential, which can only write under its path and can read nothing.">
          {devices.isPending ? (
            <SkeletonRows rows={3} />
          ) : devices.isError ? (
            <ErrorState error={devices.error} onRetry={() => devices.refetch()} />
          ) : list.length === 0 ? (
            <EmptyState title="No devices yet">Register a device, then run osp-agent on it with the token you are given.</EmptyState>
          ) : (
            <Table>
              <thead>
                <tr>
                  <Th>Device</Th>
                  <Th>State</Th>
                  <Th>Waiting</Th>
                  <Th>Last seen</Th>
                  <Th>
                    <span className="sr-only">Actions</span>
                  </Th>
                </tr>
              </thead>
              <tbody>
                {list.map((d) => (
                  <tr key={d.id}>
                    <Td>
                      <span className="block font-medium">{d.name}</span>
                      <span className="mono-data block text-sm break-all text-muted">
                        {d.namespace}/{d.path_prefix}
                      </span>
                    </Td>
                    <Td>{deviceState(d)}</Td>
                    <Td className="text-sm tabular-nums">
                      {formatNumber(d.pending_files)} files, {formatBytes(d.pending_bytes)}
                      {d.failed_files > 0 && <span className="block">{formatNumber(d.failed_files)} refused</span>}
                      {d.note && <span className="block text-muted">{d.note}</span>}
                    </Td>
                    <Td>{d.last_seen ? <Time value={d.last_seen} /> : <span className="text-muted">Never</span>}</Td>
                    <Td>
                      {!d.revoked_at && can("org.manage") && (
                        <Button variant="ghost" onClick={() => setRevoking(d)}>
                          Revoke
                        </Button>
                      )}
                    </Td>
                  </tr>
                ))}
              </tbody>
            </Table>
          )}
        </Panel>
        <Panel title="Runs and recordings" description="A run gets a snapshot only when every file arrived with the hash the device sent. Incomplete ones say what is missing.">
          {recordings.isPending ? (
            <SkeletonRows rows={3} />
          ) : recordings.isError ? (
            <ErrorState error={recordings.error} onRetry={() => recordings.refetch()} />
          ) : runs.length === 0 ? (
            <EmptyState title="Nothing sent yet">Runs and recordings appear here as devices send them.</EmptyState>
          ) : (
            <Table>
              <thead>
                <tr>
                  <Th>Name</Th>
                  <Th>State</Th>
                  <Th>Files</Th>
                  <Th>Started</Th>
                </tr>
              </thead>
              <tbody>
                {runs.map((r) => {
                  const device = list.find((d) => d.id === r.device_id);
                  return (
                    <tr key={r.id}>
                      <Td>
                        <span className="block font-medium break-all">{r.name}</span>
                        <span className="text-sm text-muted">
                          {names.get(r.device_id) ?? "A device"}, {r.kind === "RUN" ? "run" : "recording"}
                        </span>
                      </Td>
                      <Td>
                        <StateBadge tone={RECORDING_STATE[r.state].tone}>{RECORDING_STATE[r.state].text}</StateBadge>
                        {r.missing.slice(0, 3).map((m) => (
                          <span key={m} className="mt-1 block text-sm break-words">
                            {m}
                          </span>
                        ))}
                        {r.version_id && device && (
                          <Link href={`/app/datasets/${device.dataset_id}/versions/${r.version_id}`} className="mt-1 block text-sm underline underline-offset-2">
                            Its snapshot
                          </Link>
                        )}
                      </Td>
                      <Td className="text-sm tabular-nums">
                        {formatNumber(r.chunks)}, {formatBytes(r.bytes)}
                      </Td>
                      <Td>
                        <Time value={r.created_at} />
                      </Td>
                    </tr>
                  );
                })}
              </tbody>
            </Table>
          )}
        </Panel>
      </div>
      <div className="flex min-w-0 flex-col gap-6">{can("org.manage") && <RegisterPanel onDone={refresh} />}</div>
      <ConfirmDialog
        open={revoking !== null}
        onOpenChange={(open) => !open && setRevoking(null)}
        title={`Revoke ${revoking?.name ?? "this device"}?`}
        description="Its credential stops working at once. What it already sent stays."
        confirmLabel="Revoke device"
        onConfirm={async () => {
          await revokeDevice(revoking?.id ?? "");
          notify.success("Device revoked");
          refresh();
        }}
      />
    </div>
  );
}

function RegisterPanel({ onDone }: { onDone: () => void }) {
  const datasets = useDatasets("");
  const [form, setForm] = useState({ name: "", dataset_id: "", path_prefix: "" });
  const register = useMutation({
    mutationFn: () => registerDevice({ name: form.name.trim(), dataset_id: form.dataset_id, path_prefix: form.path_prefix.trim() }),
    onSuccess: () => {
      setForm({ name: "", dataset_id: "", path_prefix: "" });
      onDone();
    },
    onError: notify.error,
  });
  const valid = form.name.trim() !== "" && form.dataset_id !== "";

  function submit(e: FormEvent) {
    e.preventDefault();
    if (valid) register.mutate();
  }

  const made = register.data;
  return (
    <Panel title="Register a device" description="The token is shown once. Put it in the file the agent's token_file names.">
      {made ? (
        <div className="flex flex-col gap-3">
          <p className="text-base">
            {made.device.name} writes to <span className="mono-data break-all">{made.device.path_prefix}</span>.
          </p>
          <p className="mono-data neo-in-sm p-3 text-sm break-all">{made.token}</p>
          <div className="flex gap-2">
            <CopyButton value={made.token} label="Copy token" />
            <Button variant="ghost" onClick={() => register.reset()}>
              Done
            </Button>
          </div>
        </div>
      ) : (
        <form onSubmit={submit} className="flex flex-col gap-4">
          <Field label="Name" hint="For example novaseq-2" required>
            {(props) => <Input {...props} maxLength={100} value={form.name} onChange={(e) => setForm({ ...form, name: e.target.value })} />}
          </Field>
          <Field label="Dataset" required>
            {(props) => (
              <Select {...props} value={form.dataset_id} onChange={(e) => setForm({ ...form, dataset_id: e.target.value })}>
                <option value="">Choose a dataset</option>
                {datasets.items.map((d) => (
                  <option key={d.id} value={d.id}>
                    {d.name}
                  </option>
                ))}
              </Select>
            )}
          </Field>
          <Field label="Path prefix" hint="Leave empty for devices/<name>/">
            {(props) => <Input {...props} spellCheck={false} value={form.path_prefix} onChange={(e) => setForm({ ...form, path_prefix: e.target.value })} />}
          </Field>
          <div>
            <SubmitButton valid={valid} loading={register.isPending} hint="Name the device and choose its dataset.">
              Register device
            </SubmitButton>
          </div>
        </form>
      )}
    </Panel>
  );
}
