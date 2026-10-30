"use client";

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { Button } from "@/components/ui/button";
import { ConfirmDialog } from "@/components/ui/dialog";
import { Checkbox, Field, Input, Select } from "@/components/ui/field";
import { Panel } from "@/components/ui/panel";
import { ErrorState, SkeletonRows } from "@/components/ui/states";
import { StateBadge } from "@/components/ui/status";
import { SubmitButton } from "@/components/ui/submit-button";
import { notify } from "@/components/ui/toast";
import { useOrganizationId, useSession } from "@/lib/auth/session";
import { orgKey } from "@/lib/query/keys";
import { formatNumber } from "@/lib/utils/format";
import { fetchFile, listHolds, packPath, placeHold, releaseHold, saveBlob } from "../api";

// A version's chain-of-custody pack, and FOIA or litigation holds on it.
export function VersionEvidence({ id, versionId, version }: { id: string; versionId: string; version: number }) {
  const { can } = useSession();
  return (
    <>
      {can("audit.read") && <PackPanel id={id} versionId={versionId} version={version} />}
      <HoldsPanel id={id} versionId={versionId} />
    </>
  );
}

function PackPanel({ id, versionId, version }: { id: string; versionId: string; version: number }) {
  const { can } = useSession();
  const [digests, setDigests] = useState(false);
  const pack = useMutation({
    mutationFn: () => fetchFile(packPath(id, versionId), { digests: digests ? "true" : undefined }),
    onSuccess: (blob) => saveBlob(blob, `evidence-v${version}.zip`),
    onError: notify.error,
  });
  return (
    <Panel
      title="Evidence pack"
      description="Custody per file, the signed manifest, the audit chain head and its anchor, Concordance load files, and a script that checks it all with Python alone."
    >
      <div className="flex flex-col gap-4">
        {can("assets.download") && (
          <Checkbox
            label="Add MD5 and SHA-1 for review platforms (reads every file)"
            checked={digests}
            onChange={(e) => setDigests(e.target.checked)}
          />
        )}
        <div>
          <Button onClick={() => pack.mutate()} loading={pack.isPending}>
            Download evidence pack
          </Button>
        </div>
      </div>
    </Panel>
  );
}

function HoldsPanel({ id, versionId }: { id: string; versionId: string }) {
  const organizationId = useOrganizationId();
  const { can } = useSession();
  const client = useQueryClient();
  const key = orgKey(organizationId, "evidence", "holds", versionId);
  const holds = useQuery({
    queryKey: key,
    queryFn: ({ signal }) => listHolds(id, versionId, signal),
    enabled: Boolean(organizationId),
  });
  const [form, setForm] = useState({ kind: "LITIGATION", reference: "", reason: "" });
  const [releasing, setReleasing] = useState<string | null>(null);
  const place = useMutation({
    mutationFn: () => placeHold(id, versionId, { ...form, reference: form.reference.trim(), reason: form.reason.trim() }),
    onSuccess: (h) => {
      notify.success(`${formatNumber(h.hold_ids.length)} files held`);
      setForm({ kind: form.kind, reference: "", reason: "" });
      client.invalidateQueries({ queryKey: key });
    },
    onError: notify.error,
  });
  async function release(matter: string) {
    const r = await releaseHold(id, versionId, matter);
    notify.success(r.released ? `${formatNumber(r.released)} holds released` : "Release asked for; a second member approves it");
    client.invalidateQueries({ queryKey: key });
  }
  const editable = can("retention.manage");
  const valid = form.reference.trim() !== "";

  function submit(e: FormEvent) {
    e.preventDefault();
    if (valid) place.mutate();
  }

  const list = holds.data?.holds ?? [];
  return (
    <Panel title="Holds on this version" description="Every file in the version gets a legal hold, so nothing in it is purged while the matter stands.">
      <div className="flex flex-col gap-5">
        {holds.isPending ? (
          <SkeletonRows rows={2} />
        ) : holds.isError ? (
          <ErrorState error={holds.error} onRetry={() => holds.refetch()} />
        ) : list.length === 0 ? (
          <p className="text-base text-muted">No holds.</p>
        ) : (
          <ul className="flex flex-col gap-3">
            {list.map((h) => (
              <li key={h.matter} className="flex flex-wrap items-center justify-between gap-2">
                <span className="min-w-0">
                  <span className="block break-words">{h.matter}</span>
                  <span className="text-sm text-muted">{formatNumber(h.hold_ids.length)} files</span>
                </span>
                {h.released ? (
                  <StateBadge>Released</StateBadge>
                ) : editable ? (
                  <Button variant="ghost" onClick={() => setReleasing(h.matter)}>
                    Release
                  </Button>
                ) : (
                  <StateBadge tone="active">Active</StateBadge>
                )}
              </li>
            ))}
          </ul>
        )}
        {editable && (
          <form onSubmit={submit} className="flex flex-col gap-4">
            <div className="grid grid-cols-2 gap-4">
              <Field label="Kind">
                {(props) => (
                  <Select {...props} value={form.kind} onChange={(e) => setForm({ ...form, kind: e.target.value })}>
                    <option value="LITIGATION">Litigation</option>
                    <option value="FOIA">FOIA request</option>
                    <option value="AUDIT">Audit</option>
                    <option value="INVESTIGATION">Investigation</option>
                  </Select>
                )}
              </Field>
              <Field label="Reference" hint="Case or request number" required>
                {(props) => (
                  <Input {...props} spellCheck={false} value={form.reference} onChange={(e) => setForm({ ...form, reference: e.target.value })} />
                )}
              </Field>
            </div>
            <Field label="Reason">
              {(props) => <Input {...props} value={form.reason} onChange={(e) => setForm({ ...form, reason: e.target.value })} />}
            </Field>
            <div>
              <SubmitButton valid={valid} loading={place.isPending}>
                Hold this version
              </SubmitButton>
            </div>
          </form>
        )}
      </div>
      <ConfirmDialog
        open={releasing !== null}
        onOpenChange={(open) => !open && setReleasing(null)}
        title="Release this hold?"
        description={`Files held under "${releasing ?? ""}" can be purged again once their retention ends.`}
        confirmLabel="Release hold"
        onConfirm={() => release(releasing ?? "")}
      />
    </Panel>
  );
}
