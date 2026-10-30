"use client";

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { Button } from "@/components/ui/button";
import { Field, Input } from "@/components/ui/field";
import { Panel } from "@/components/ui/panel";
import { ErrorState, SkeletonRows } from "@/components/ui/states";
import { StateBadge } from "@/components/ui/status";
import { SubmitButton } from "@/components/ui/submit-button";
import { Table, Td, Th } from "@/components/ui/table";
import { Time } from "@/components/ui/time";
import { notify } from "@/components/ui/toast";
import { useOrganizationId, useSession } from "@/lib/auth/session";
import { orgKey } from "@/lib/query/keys";
import { formatBytes, formatNumber } from "@/lib/utils/format";
import { cancelRepack, getPacking, listRepacks, putPacking, startRepack, type Repack } from "../api";

const MIB = 1024 * 1024;
const STATE: Record<Repack["state"], { text: string; tone: "active" | "done" | "neutral" }> = {
  PENDING: { text: "Planning", tone: "active" },
  DONE: { text: "Done", tone: "done" },
  CANCELLED: { text: "Cancelled", tone: "neutral" },
};

// How the dataset's small files are packed: the organization's target
// pack size, and repacks that gather packed files in path order.
export function StorageLayoutPanel({ datasetId }: { datasetId: string }) {
  const organizationId = useOrganizationId();
  const { can } = useSession();
  const client = useQueryClient();
  const repacks = useQuery({
    queryKey: orgKey(organizationId, "streaming", datasetId, "repacks"),
    queryFn: ({ signal }) => listRepacks(datasetId, signal),
    enabled: Boolean(organizationId),
    refetchInterval: (q) => (q.state.data?.repacks.some((r) => r.state === "PENDING") ? 10_000 : false),
  });
  const refresh = () => client.invalidateQueries({ queryKey: orgKey(organizationId, "streaming", datasetId, "repacks") });
  const start = useMutation({
    mutationFn: () => startRepack(datasetId),
    onSuccess: () => {
      notify.success("Repack queued: the packing worker plans it over the next minutes");
      refresh();
    },
    onError: (err) => notify.error(err),
  });
  const cancel = useMutation({
    mutationFn: (id: string) => cancelRepack(datasetId, id),
    onSuccess: refresh,
    onError: (err) => notify.error(err),
  });
  const list = repacks.data?.repacks ?? [];
  const pending = list.some((r) => r.state === "PENDING");

  return (
    <Panel
      title="Storage layout"
      description="Small files are packed in path order, so a sample's files sit together and a data loader reads a pack at a time. A repack gathers files packed earlier into new packs in that order."
    >
      <div className="flex flex-col gap-5">
        <PackSize />
        {can("datasets.manage") && (
          <div>
            <Button onClick={() => start.mutate()} disabled={pending || start.isPending}>
              {pending ? "A repack is under way" : "Repack in path order"}
            </Button>
          </div>
        )}
        {repacks.isPending ? (
          <SkeletonRows rows={2} />
        ) : repacks.isError ? (
          <ErrorState error={repacks.error} onRetry={() => repacks.refetch()} />
        ) : list.length > 0 ? (
          <Table>
            <thead>
              <tr>
                <Th>Repack</Th>
                <Th>Planned</Th>
                <Th>Asked</Th>
              </tr>
            </thead>
            <tbody>
              {list.map((r) => (
                <tr key={r.id}>
                  <Td>
                    <StateBadge tone={STATE[r.state].tone}>{STATE[r.state].text}</StateBadge>
                    {r.state === "PENDING" && can("datasets.manage") && (
                      <button
                        type="button"
                        className="ml-2 text-sm underline underline-offset-2"
                        onClick={() => cancel.mutate(r.id)}
                        disabled={cancel.isPending}
                      >
                        Cancel
                      </button>
                    )}
                  </Td>
                  <Td className="tabular-nums">
                    {formatNumber(r.packs_planned)} {r.packs_planned === 1 ? "pack" : "packs"}, {formatNumber(r.entries_planned)} files
                  </Td>
                  <Td>
                    <Time value={r.created_at} />
                  </Td>
                </tr>
              ))}
            </tbody>
          </Table>
        ) : null}
      </div>
    </Panel>
  );
}

function PackSize() {
  const organizationId = useOrganizationId();
  const { can } = useSession();
  const client = useQueryClient();
  const packing = useQuery({
    queryKey: orgKey(organizationId, "streaming", "packing"),
    queryFn: ({ signal }) => getPacking(signal),
    enabled: Boolean(organizationId),
  });
  if (packing.isPending) return <SkeletonRows rows={1} />;
  if (packing.isError) return <ErrorState error={packing.error} onRetry={() => packing.refetch()} />;
  const bytes = packing.data.target_pack_bytes;
  if (!can("org.manage")) {
    return (
      <p className="text-base">
        Target pack size: {bytes ? formatBytes(bytes) : "the platform's default"}
        <span className="block text-sm text-muted">Set for the whole organization by its owners.</span>
      </p>
    );
  }
  return (
    <PackSizeForm
      key={bytes}
      bytes={bytes}
      onSaved={() => client.invalidateQueries({ queryKey: orgKey(organizationId, "streaming", "packing") })}
    />
  );
}

function PackSizeForm({ bytes, onSaved }: { bytes: number; onSaved: () => void }) {
  const [mib, setMib] = useState(bytes ? String(bytes / MIB) : "");
  const value = mib.trim() === "" ? 0 : Number(mib);
  const error =
    mib.trim() !== "" && (!Number.isInteger(value) || value < 1 || value > 10240)
      ? "Whole MiB, 1 to 10240, or empty for the platform's default."
      : undefined;
  const save = useMutation({
    mutationFn: () => putPacking(value * MIB),
    onSuccess: () => {
      notify.success("Target pack size saved for the organization");
      onSaved();
    },
    onError: (err) => notify.error(err),
  });
  const submit = (e: FormEvent) => {
    e.preventDefault();
    if (!error) save.mutate();
  };
  return (
    <form onSubmit={submit} className="flex flex-wrap items-end gap-3">
      <Field
        label="Target pack size, MiB"
        hint="For the whole organization. Empty uses the platform's default."
        error={error}
      >
        {(props) => (
          <Input
            {...props}
            name="target_pack_mib"
            inputMode="numeric"
            autoComplete="off"
            className="w-32 tabular-nums"
            value={mib}
            onChange={(e) => setMib(e.target.value)}
            placeholder="Default…"
          />
        )}
      </Field>
      <SubmitButton valid={!error} loading={save.isPending}>
        Save size
      </SubmitButton>
    </form>
  );
}
