"use client";

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { Checkbox, Field, Input } from "@/components/ui/field";
import { Panel } from "@/components/ui/panel";
import { ErrorState, SkeletonRows } from "@/components/ui/states";
import { SubmitButton } from "@/components/ui/submit-button";
import { notify } from "@/components/ui/toast";
import { useOrganizationId, useSession } from "@/lib/auth/session";
import { orgKey } from "@/lib/query/keys";
import { getSettings, putSettings, type QualitySettings } from "../api";

// Whether snapshots are profiled, when drift counts, and how a file names
// its subject for leakage checks.
export function QualitySettingsPanel({ datasetId }: { datasetId: string }) {
  const organizationId = useOrganizationId();
  const settings = useQuery({
    queryKey: orgKey(organizationId, "quality", "settings", datasetId),
    queryFn: ({ signal }) => getSettings(datasetId, signal),
    enabled: Boolean(organizationId),
  });
  return (
    <Panel title="Data quality" description="Column profiles of every snapshot, drift thresholds, and the subject for leakage checks.">
      {settings.isPending ? (
        <SkeletonRows rows={4} />
      ) : settings.isError ? (
        <ErrorState error={settings.error} onRetry={() => settings.refetch()} />
      ) : (
        <SettingsForm key={settings.data.updated_at ?? "default"} datasetId={datasetId} initial={settings.data} />
      )}
    </Panel>
  );
}

function SettingsForm({ datasetId, initial }: { datasetId: string; initial: QualitySettings }) {
  const organizationId = useOrganizationId();
  const { can } = useSession();
  const client = useQueryClient();
  const [draft, setDraft] = useState({
    profile_snapshots: initial.profile_snapshots,
    max_psi: String(initial.max_psi),
    max_null_change: String(initial.max_null_change * 100),
    max_row_change: String(initial.max_row_change * 100),
    subject: initial.subject_pattern ? "pattern" : "key",
    subject_value: initial.subject_pattern || initial.subject_key,
  });
  const save = useMutation({
    mutationFn: () =>
      putSettings(datasetId, {
        profile_snapshots: draft.profile_snapshots,
        max_psi: Number(draft.max_psi),
        max_null_change: Number(draft.max_null_change) / 100,
        max_row_change: Number(draft.max_row_change) / 100,
        subject_key: draft.subject === "key" ? draft.subject_value.trim() : "",
        subject_pattern: draft.subject === "pattern" ? draft.subject_value.trim() : "",
      }),
    onSuccess: () => {
      notify.success("Quality settings saved");
      client.invalidateQueries({ queryKey: orgKey(organizationId, "quality", "settings", datasetId) });
    },
    onError: notify.error,
  });
  const psi = Number(draft.max_psi);
  const nulls = Number(draft.max_null_change);
  const rows = Number(draft.max_row_change);
  const psiError = !(psi > 0 && psi <= 10) ? "Above 0, at most 10." : undefined;
  const nullsError = !(nulls > 0 && nulls <= 100) ? "Above 0, at most 100." : undefined;
  const rowsError = !(rows > 0 && rows <= 10000) ? "Above 0, at most 10,000." : undefined;
  const valid = !psiError && !nullsError && !rowsError;
  const editable = can("datasets.manage");

  function submit(e: FormEvent) {
    e.preventDefault();
    if (valid) save.mutate();
  }

  return (
    <form onSubmit={submit} className="flex flex-col gap-4">
      <fieldset disabled={!editable} className="flex flex-col gap-4">
        <Checkbox
          label="Profile every snapshot"
          checked={draft.profile_snapshots}
          onChange={(e) => setDraft({ ...draft, profile_snapshots: e.target.checked })}
        />
        <Field label="Distribution shift (PSI)" hint="0.1 is moving, 0.25 has shifted." error={psiError}>
          {(props) => (
            <Input {...props} inputMode="decimal" value={draft.max_psi} onChange={(e) => setDraft({ ...draft, max_psi: e.target.value })} />
          )}
        </Field>
        <div className="grid grid-cols-2 gap-4">
          <Field label="Null rate change" hint="Points" error={nullsError}>
            {(props) => (
              <Input
                {...props}
                inputMode="decimal"
                value={draft.max_null_change}
                onChange={(e) => setDraft({ ...draft, max_null_change: e.target.value })}
              />
            )}
          </Field>
          <Field label="Row count change" hint="Percent" error={rowsError}>
            {(props) => (
              <Input {...props} inputMode="decimal" value={draft.max_row_change} onChange={(e) => setDraft({ ...draft, max_row_change: e.target.value })} />
            )}
          </Field>
        </div>
        <fieldset className="flex flex-col gap-2">
          <legend className="mb-1 text-sm font-medium">Subject, for leakage checks</legend>
          <label className="flex items-center gap-2 text-base">
            <input
              type="radio"
              name="subject"
              className="accent-(--ink)"
              checked={draft.subject === "key"}
              onChange={() => setDraft({ ...draft, subject: "key" })}
            />
            A metadata key, such as patient_id
          </label>
          <label className="flex items-center gap-2 text-base">
            <input
              type="radio"
              name="subject"
              className="accent-(--ink)"
              checked={draft.subject === "pattern"}
              onChange={() => setDraft({ ...draft, subject: "pattern" })}
            />
            A pattern on the path, its first group the subject
          </label>
        </fieldset>
        <Field
          label={draft.subject === "key" ? "Metadata key" : "Path pattern"}
          hint={draft.subject === "key" ? "Leave empty to skip subject checks." : "For example ^patients/([^/]+)/"}
        >
          {(props) => (
            <Input
              {...props}
              spellCheck={false}
              className="mono-data"
              value={draft.subject_value}
              onChange={(e) => setDraft({ ...draft, subject_value: e.target.value })}
            />
          )}
        </Field>
      </fieldset>
      {editable && (
        <div>
          <SubmitButton valid={valid} loading={save.isPending} hint="Fix the fields marked above.">
            Save quality settings
          </SubmitButton>
        </div>
      )}
    </form>
  );
}
