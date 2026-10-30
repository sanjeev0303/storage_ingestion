"use client";

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { Button } from "@/components/ui/button";
import { Field, Input, Select } from "@/components/ui/field";
import { Panel } from "@/components/ui/panel";
import { SkeletonRows } from "@/components/ui/states";
import { SubmitButton } from "@/components/ui/submit-button";
import { Time } from "@/components/ui/time";
import { notify } from "@/components/ui/toast";
import { useOrganizationId, useSession } from "@/lib/auth/session";
import { orgKey } from "@/lib/query/keys";
import { useStorageConnections } from "@/features/storage-connections/hooks";
import { getSchedule, putSchedule, startExport } from "../api";

// The export-everything bundle: what rebuilding the catalog from a second
// bucket needs, written there now or on a schedule.
export function ExportPanel() {
  const organizationId = useOrganizationId();
  const { can } = useSession();
  const client = useQueryClient();
  const connections = useStorageConnections();
  const schedule = useQuery({
    queryKey: orgKey(organizationId, "protection", "export-schedule"),
    queryFn: ({ signal }) => getSchedule(signal),
    enabled: Boolean(organizationId),
  });
  if (!can("audit.read")) return null;

  return (
    <Panel
      title="Export bundle"
      description="Manifests with SHA-256, snapshot definitions, the catalog and the hash-chained audit log, written to a second bucket so the catalog can be rebuilt without this platform."
    >
      {schedule.isPending ? (
        <SkeletonRows rows={2} />
      ) : (
        <ScheduleForm
          key={schedule.data?.connection_id ?? "none"}
          connection={schedule.data?.connection_id ?? ""}
          hours={schedule.data?.interval_hours ?? 0}
          lastRun={schedule.data?.last_run_at}
          options={(connections.data?.connections ?? []).filter((c) => c.status === "ACTIVE")}
          editable={can("org.manage")}
          onSaved={() => client.invalidateQueries({ queryKey: orgKey(organizationId, "protection", "export-schedule") })}
        />
      )}
    </Panel>
  );
}

function ScheduleForm({
  connection,
  hours,
  lastRun,
  options,
  editable,
  onSaved,
}: {
  connection: string;
  hours: number;
  lastRun?: string;
  options: { id: string; name: string }[];
  editable: boolean;
  onSaved: () => void;
}) {
  const [target, setTarget] = useState(connection);
  const [every, setEvery] = useState(String(hours || 24));
  const save = useMutation({ mutationFn: putSchedule });
  const now = useMutation({ mutationFn: () => startExport(target) });
  const everyValid = Number.isInteger(Number(every)) && Number(every) >= 0 && Number(every) <= 8760;

  function submit(e: FormEvent) {
    e.preventDefault();
    save.mutate(
      { connection_id: target, interval_hours: Number(every) },
      { onSuccess: () => { notify.success(Number(every) === 0 ? "Schedule stopped" : "Schedule saved"); onSaved(); }, onError: notify.error },
    );
  }

  return (
    <form onSubmit={submit} className="flex flex-col gap-4">
      <Field label="Bucket to write to">
        {(props) => (
          <Select {...props} name="export_connection" value={target} onChange={(e) => setTarget(e.target.value)}>
            <option value="">Choose a connection</option>
            {options.map((c) => (
              <option key={c.id} value={c.id}>
                {c.name}
              </option>
            ))}
          </Select>
        )}
      </Field>
      <Field label="Every, hours" hint="0 stops the schedule.">
        {(props) => (
          <Input {...props} name="export_every" inputMode="numeric" className="tabular-nums" disabled={!editable} value={every} onChange={(e) => setEvery(e.target.value)} />
        )}
      </Field>
      {lastRun && (
        <p className="text-sm text-muted">
          Last bundle <Time value={lastRun} />
        </p>
      )}
      <div className="flex flex-wrap gap-2">
        {editable && (
          <SubmitButton valid={target !== "" && everyValid} loading={save.isPending} hint="Choose a bucket and an interval.">
            Save schedule
          </SubmitButton>
        )}
        <Button
          disabled={!target}
          loading={now.isPending}
          onClick={() => now.mutate(undefined, { onSuccess: () => notify.success("Export bundle queued"), onError: notify.error })}
        >
          Export now
        </Button>
      </div>
    </form>
  );
}
