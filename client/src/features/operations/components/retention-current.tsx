"use client";

import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { Button } from "@/components/ui/button";
import { ConfirmDialog } from "@/components/ui/dialog";
import { Checkbox } from "@/components/ui/field";
import { Panel } from "@/components/ui/panel";
import { EmptyState, ErrorState, SkeletonRows } from "@/components/ui/states";
import { StateBadge } from "@/components/ui/status";
import { Table, Td, Th } from "@/components/ui/table";
import { Time } from "@/components/ui/time";
import { notify } from "@/components/ui/toast";
import { useSession } from "@/lib/auth/session";
import { orgKey } from "@/lib/query/keys";
import { listHolds, listPolicies, policyHistory, releaseLegalHold } from "../api";
import type { HeldHold, PolicyInEffect } from "../types";

const SCOPE_TEXT: Record<string, string> = { TENANT: "Whole organization", DATASET: "Dataset", ASSET: "Asset" };

// "720h0m0s" as people say it: "30 days".
export function readableDuration(value: string | null | undefined) {
  if (!value) return null;
  const match = /^(?:(\d+)h)?(?:(\d+)m)?(?:([\d.]+)s)?$/.exec(value);
  if (!match) return value;
  const hours = Number(match[1] ?? 0);
  const minutes = Number(match[2] ?? 0);
  if (hours > 0 && hours % 24 === 0 && minutes === 0) {
    const days = hours / 24;
    return days === 1 ? "1 day" : `${days} days`;
  }
  if (hours > 0) return minutes ? `${hours} h ${minutes} min` : `${hours} h`;
  if (minutes > 0) return `${minutes} min`;
  return value;
}

function Setting({ value }: { value: string | null | undefined }) {
  const text = readableDuration(value);
  return text ? <span className="tabular-nums">{text}</span> : <span className="text-muted">Inherited</span>;
}

export function useRetentionKeys() {
  const { organizationId } = useSession();
  const client = useQueryClient();
  return () => client.invalidateQueries({ queryKey: orgKey(organizationId, "retention") });
}

export function PoliciesInEffect() {
  const { organizationId } = useSession();
  const policies = useQuery({
    queryKey: orgKey(organizationId, "retention", "policies"),
    queryFn: ({ signal }) => listPolicies(signal),
    enabled: Boolean(organizationId),
  });

  return (
    <Panel
      title="Policies in effect"
      description="The newest version for each scope. For each setting, the most specific scope that sets it wins; the platform's floors still apply."
    >
      {policies.isPending ? (
        <SkeletonRows rows={3} />
      ) : policies.isError ? (
        <ErrorState error={policies.error} onRetry={() => policies.refetch()} />
      ) : policies.data.policies.length === 0 ? (
        <EmptyState title="Platform defaults only">No policy has been set. Every asset follows the platform&apos;s defaults.</EmptyState>
      ) : (
        <Table>
          <caption className="sr-only">Retention policies in effect</caption>
          <thead>
            <tr>
              <Th>Scope</Th>
              <Th>Minimum retention</Th>
              <Th>Maximum age</Th>
              <Th>Deletion grace</Th>
              <Th>Set</Th>
            </tr>
          </thead>
          <tbody>
            {policies.data.policies.map((p: PolicyInEffect) => (
              <tr key={p.id}>
                <Td>
                  <div>{SCOPE_TEXT[p.scope] ?? p.scope}</div>
                  {p.scope_key && <div className="mono-data text-sm text-muted">{p.scope_key}</div>}
                </Td>
                <Td>
                  <Setting value={p.min_retention} />
                </Td>
                <Td>
                  <Setting value={p.max_age} />
                </Td>
                <Td>
                  <Setting value={p.deletion_grace} />
                </Td>
                <Td>
                  <div className="text-sm">
                    Version {p.version}, <Time value={p.effective_from} />
                  </div>
                  <div className="max-w-xs text-sm text-muted">{p.reason}</div>
                  {p.version > 1 && <PolicyHistory scope={p.scope} scopeKey={p.scope_key} />}
                </Td>
              </tr>
            ))}
          </tbody>
        </Table>
      )}
    </Panel>
  );
}

export function HoldsInEffect() {
  const { organizationId } = useSession();
  const refresh = useRetentionKeys();
  const [showReleased, setShowReleased] = useState(false);
  const [releasing, setReleasing] = useState<HeldHold | null>(null);
  const holds = useQuery({
    queryKey: orgKey(organizationId, "retention", "holds", showReleased),
    queryFn: ({ signal }) => listHolds(showReleased, signal),
    enabled: Boolean(organizationId),
  });

  return (
    <Panel
      title="Legal holds"
      description="Nothing in a held scope is deleted or purged until its hold is released. On buckets with S3 Object Lock, the hold is also set on the stored files."
      actions={<Checkbox checked={showReleased} onChange={(e) => setShowReleased(e.target.checked)} label="Show released" />}
    >
      {holds.isPending ? (
        <SkeletonRows rows={3} />
      ) : holds.isError ? (
        <ErrorState error={holds.error} onRetry={() => holds.refetch()} />
      ) : holds.data.holds.length === 0 ? (
        <EmptyState title={showReleased ? "No holds, ever" : "No active holds"}>
          Place a hold below when a matter needs data kept regardless of retention.
        </EmptyState>
      ) : (
        <Table>
          <caption className="sr-only">Legal holds</caption>
          <thead>
            <tr>
              <Th>Scope</Th>
              <Th>Reason</Th>
              <Th>Placed</Th>
              <Th>
                <span className="sr-only">Actions</span>
              </Th>
            </tr>
          </thead>
          <tbody>
            {holds.data.holds.map((hold) => (
              <tr key={hold.id}>
                <Td>
                  <div>{SCOPE_TEXT[hold.scope] ?? hold.scope}</div>
                  {hold.scope_key && <div className="mono-data text-sm text-muted">{hold.scope_key}</div>}
                </Td>
                <Td className="max-w-sm">{hold.reason}</Td>
                <Td>
                  <Time value={hold.placed_at} />
                  <div className="text-sm text-muted">{hold.placed_by}</div>
                </Td>
                <Td className="text-right">
                  {hold.released_at ? (
                    <StateBadge>
                      Released <Time value={hold.released_at} />
                    </StateBadge>
                  ) : (
                    <Button onClick={() => setReleasing(hold)}>Release</Button>
                  )}
                </Td>
              </tr>
            ))}
          </tbody>
        </Table>
      )}

      <ConfirmDialog
        open={releasing !== null}
        onOpenChange={(open) => !open && setReleasing(null)}
        title="Release this legal hold?"
        description={
          <>
            {releasing && (SCOPE_TEXT[releasing.scope] ?? releasing.scope)}
            {releasing?.scope_key ? ` ${releasing.scope_key}` : ""} stops being protected by it. Anything already due for
            deletion becomes eligible for purge, unless another hold or retention still covers it.
          </>
        }
        confirmLabel="Release hold"
        onConfirm={async () => {
          if (!releasing) return;
          await releaseLegalHold(releasing.id);
          await refresh();
          notify.success("Hold released");
        }}
      />
    </Panel>
  );
}

// Earlier versions are kept, never edited; loaded when someone opens them.
function PolicyHistory({ scope, scopeKey }: { scope: string; scopeKey: string }) {
  const { organizationId } = useSession();
  const [open, setOpen] = useState(false);
  const history = useQuery({
    queryKey: orgKey(organizationId, "retention", "history", scope, scopeKey),
    queryFn: ({ signal }) => policyHistory(scope, scopeKey, signal),
    enabled: Boolean(organizationId) && open,
  });

  return (
    <details className="mt-1" onToggle={(e) => setOpen(e.currentTarget.open)}>
      <summary className="cursor-pointer text-sm">Earlier versions</summary>
      {history.isPending ? (
        <SkeletonRows rows={2} />
      ) : history.isError ? (
        <ErrorState error={history.error} onRetry={() => history.refetch()} />
      ) : (
        <ol className="mt-2 flex flex-col gap-2 text-sm">
          {history.data.policies.slice(1).map((v) => (
            <li key={v.id}>
              Version {v.version}, <Time value={v.effective_from} />: minimum{" "}
              {readableDuration(v.min_retention) ?? "inherited"}, maximum age {readableDuration(v.max_age) ?? "inherited"}
              <span className="block text-muted">{v.reason}</span>
            </li>
          ))}
        </ol>
      )}
    </details>
  );
}
