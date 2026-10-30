"use client";

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { Button } from "@/components/ui/button";
import { ConfirmDialog } from "@/components/ui/dialog";
import { Field, Input } from "@/components/ui/field";
import { DefList } from "@/components/ui/page";
import { Panel } from "@/components/ui/panel";
import { ErrorState, SkeletonRows } from "@/components/ui/states";
import { SubmitButton } from "@/components/ui/submit-button";
import { notify } from "@/components/ui/toast";
import { useOrganizationId, useSession } from "@/lib/auth/session";
import { orgKey } from "@/lib/query/keys";
import { formatBytes, formatNumber } from "@/lib/utils/format";
import { expireVersions, getDatasetCosts, type Expiry } from "../api";
import { money, TIER_TEXT } from "./costs-screen";

// What keeping the dataset costs, and expiring its old snapshots.
export function DatasetLifecycle({ datasetId }: { datasetId: string }) {
  const organizationId = useOrganizationId();
  const costs = useQuery({
    queryKey: orgKey(organizationId, "costs", datasetId, "dataset"),
    queryFn: ({ signal }) => getDatasetCosts(datasetId, 10, signal),
    enabled: Boolean(organizationId),
  });
  const c = costs.data;
  return (
    <Panel title="Cost and retention" description="Its storage now, and keeping it ten years as it is or archived, at list prices.">
      {costs.isPending ? (
        <SkeletonRows rows={3} />
      ) : costs.isError || !c ? (
        <ErrorState error={costs.error} onRetry={() => costs.refetch()} />
      ) : (
        <div className="flex flex-col gap-5">
          <DefList
            items={[
              { label: "A month now", value: money(c.monthly_usd) },
              { label: `${c.years} years as it is`, value: money(c.retain_usd) },
              { label: `${c.years} years archived`, value: money(c.archived_usd) },
              {
                label: "Where it is",
                value: c.stored.length
                  ? c.stored.map((l) => `${formatBytes(l.bytes)} ${TIER_TEXT[l.tier].toLowerCase()}`).join(", ")
                  : "Nothing stored yet",
              },
            ]}
          />
          <ExpiryForm datasetId={datasetId} />
        </div>
      )}
    </Panel>
  );
}

function ExpiryForm({ datasetId }: { datasetId: string }) {
  const { can } = useSession();
  const organizationId = useOrganizationId();
  const client = useQueryClient();
  const [keep, setKeep] = useState("5");
  const [older, setOlder] = useState("90");
  const [preview, setPreview] = useState<Expiry | null>(null);
  const [confirming, setConfirming] = useState(false);
  const keepLast = Number(keep);
  const olderThan = Number(older || 0);
  const error =
    !Number.isInteger(keepLast) || keepLast < 1 ? "Keep at least the newest version." : !Number.isInteger(olderThan) || olderThan < 0 ? "Whole days." : undefined;
  const look = useMutation({
    mutationFn: () => expireVersions(datasetId, { keep_last: keepLast, older_than_days: olderThan, dry_run: true }),
    onSuccess: setPreview,
    onError: notify.error,
  });
  if (!can("datasets.manage")) return null;
  const submit = (e: FormEvent) => {
    e.preventDefault();
    if (!error) look.mutate();
  };
  return (
    <div className="flex flex-col gap-4 border-t border-edge pt-5">
      <h3 className="text-base font-semibold">Expire old snapshots</h3>
      <form onSubmit={submit} className="flex flex-wrap items-end gap-3">
        <Field label="Keep the newest" error={error}>
          {(props) => <Input {...props} name="keep_last" inputMode="numeric" autoComplete="off" className="w-24 tabular-nums" value={keep} onChange={(e) => setKeep(e.target.value)} />}
        </Field>
        <Field label="Older than, days">
          {(props) => <Input {...props} name="older_than" inputMode="numeric" autoComplete="off" className="w-24 tabular-nums" value={older} onChange={(e) => setOlder(e.target.value)} />}
        </Field>
        <SubmitButton valid={!error} loading={look.isPending}>
          Preview
        </SubmitButton>
      </form>
      {preview && (
        <div className="flex flex-col gap-3">
          <p className="text-base">
            {preview.expire.length === 0
              ? "No snapshot would expire."
              : `${preview.expire.length} ${preview.expire.length === 1 ? "snapshot expires" : "snapshots expire"} (versions ${preview.expire
                  .map((v) => v.version)
                  .join(", ")}), freeing ${formatNumber(preview.freed_objects)} ${preview.freed_objects === 1 ? "object" : "objects"} and ${formatBytes(preview.freed_bytes)}, about ${money(
                  preview.freed_usd_month,
                )} a month.`}
          </p>
          {preview.keep.length > 0 && (
            <ul className="grid gap-1 text-sm text-muted">
              {preview.keep.map((v) => (
                <li key={v.id}>
                  Version {v.version} stays: {v.kept}
                </li>
              ))}
            </ul>
          )}
          {preview.expire.length > 0 && (
            <div>
              <Button onClick={() => setConfirming(true)}>Expire {preview.expire.length === 1 ? "it" : "them"}</Button>
            </div>
          )}
        </div>
      )}
      <ConfirmDialog
        open={confirming}
        onOpenChange={setConfirming}
        title="Expire these snapshots?"
        description="Their records and manifest hashes stay, their member lists go, and objects only they held are deleted through the lifecycle, where holds, retention and escrow still apply."
        confirmLabel="Expire snapshots"
        confirmText="expire"
        onConfirm={async () => {
          const done = await expireVersions(datasetId, { keep_last: keepLast, older_than_days: olderThan, dry_run: false });
          notify.success(`${done.expire.length} expired; ${done.deletions_requested} deletions asked for`);
          setPreview(null);
          await client.invalidateQueries({ queryKey: orgKey(organizationId, "costs", datasetId) });
        }}
      />
    </div>
  );
}
