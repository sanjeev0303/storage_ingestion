"use client";

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { Field, Input, Select } from "@/components/ui/field";
import { Panel } from "@/components/ui/panel";
import { ErrorState, SkeletonRows } from "@/components/ui/states";
import { SubmitButton } from "@/components/ui/submit-button";
import { Time } from "@/components/ui/time";
import { notify } from "@/components/ui/toast";
import { useOrganizationId, useSession } from "@/lib/auth/session";
import { orgKey } from "@/lib/query/keys";
import { getProvenance, putProvenance, type Provenance } from "../api";

const KINDS = [
  ["PUBLIC_DATASET", "Public dataset"],
  ["LICENSED_PRIVATE", "Licensed private data"],
  ["WEB_CRAWLED", "Crawled from the web"],
  ["USER_DATA", "Data from users"],
  ["SYNTHETIC", "Synthetic"],
  ["OTHER", "Other"],
] as const;

// The lawful basis, licence and source, recorded before snapshots where the
// organization requires it. Recording it is an audit chain entry.
export function DatasetProvenance({ datasetId }: { datasetId: string }) {
  const organizationId = useOrganizationId();
  const provenance = useQuery({
    queryKey: orgKey(organizationId, "evidence", "provenance", datasetId),
    queryFn: ({ signal }) => getProvenance(datasetId, signal),
    enabled: Boolean(organizationId),
  });
  return (
    <Panel title="Lawful basis and source" description="Recorded before training. A basis cannot be applied to snapshots taken before it.">
      {provenance.isPending ? (
        <SkeletonRows rows={4} />
      ) : provenance.isError ? (
        <ErrorState error={provenance.error} onRetry={() => provenance.refetch()} />
      ) : (
        <ProvenanceForm key={provenance.data.recorded_at ?? "new"} datasetId={datasetId} initial={provenance.data} />
      )}
    </Panel>
  );
}

function ProvenanceForm({ datasetId, initial }: { datasetId: string; initial: Provenance }) {
  const organizationId = useOrganizationId();
  const { can } = useSession();
  const client = useQueryClient();
  const [draft, setDraft] = useState({
    lawful_basis: initial.lawful_basis,
    license: initial.license,
    source: initial.source,
    source_kind: initial.source_kind || "OTHER",
  });
  const save = useMutation({
    mutationFn: () =>
      putProvenance(datasetId, {
        lawful_basis: draft.lawful_basis.trim(),
        license: draft.license.trim(),
        source: draft.source.trim(),
        source_kind: draft.source_kind,
      }),
    onSuccess: () => {
      notify.success("Recorded in the audit chain");
      client.invalidateQueries({ queryKey: orgKey(organizationId, "evidence", "provenance", datasetId) });
    },
    onError: notify.error,
  });
  const editable = can("datasets.manage");
  const valid = draft.lawful_basis.trim() !== "" && draft.license.trim() !== "" && draft.source.trim() !== "";

  function submit(e: FormEvent) {
    e.preventDefault();
    if (valid) save.mutate();
  }

  return (
    <form onSubmit={submit} className="flex flex-col gap-4">
      <fieldset disabled={!editable} className="flex flex-col gap-4">
        <Field label="Lawful basis" hint="For example legitimate interest, assessment LIA-2026-04" required>
          {(props) => <Input {...props} value={draft.lawful_basis} onChange={(e) => setDraft({ ...draft, lawful_basis: e.target.value })} />}
        </Field>
        <Field label="Licence" hint="For example CC BY 4.0, or the contract reference" required>
          {(props) => <Input {...props} value={draft.license} onChange={(e) => setDraft({ ...draft, license: e.target.value })} />}
        </Field>
        <Field label="Source" hint="Where the data came from" required>
          {(props) => <Input {...props} value={draft.source} onChange={(e) => setDraft({ ...draft, source: e.target.value })} />}
        </Field>
        <Field label="Kind of source">
          {(props) => (
            <Select {...props} value={draft.source_kind} onChange={(e) => setDraft({ ...draft, source_kind: e.target.value })}>
              {KINDS.map(([value, label]) => (
                <option key={value} value={value}>
                  {label}
                </option>
              ))}
            </Select>
          )}
        </Field>
      </fieldset>
      <p className="text-sm text-muted">
        {initial.recorded_at ? (
          <>
            Recorded by <span className="mono-data">{initial.recorded_by}</span> <Time value={initial.recorded_at} />
          </>
        ) : (
          "Not recorded yet."
        )}
      </p>
      {editable && (
        <div>
          <SubmitButton valid={valid} loading={save.isPending}>
            Record basis
          </SubmitButton>
        </div>
      )}
    </form>
  );
}
