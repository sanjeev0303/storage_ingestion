"use client";

import { useQuery } from "@tanstack/react-query";
import { StateBadge } from "@/components/ui/status";
import { useOrganizationId } from "@/lib/auth/session";
import { orgKey } from "@/lib/query/keys";
import { formatNumber } from "@/lib/utils/format";
import { listCheckpoints, listVerifications } from "../api";

// What a run's data loader re-hashed, and the checkpoint to roll back to.
export function RunChecks({ runId }: { runId: string }) {
  const organizationId = useOrganizationId();
  const verifications = useQuery({
    queryKey: orgKey(organizationId, "streaming", runId, "verifications"),
    queryFn: ({ signal }) => listVerifications(runId, signal),
    enabled: Boolean(organizationId),
  });
  const checkpoints = useQuery({
    queryKey: orgKey(organizationId, "streaming", runId, "checkpoints"),
    queryFn: ({ signal }) => listCheckpoints(runId, signal),
    enabled: Boolean(organizationId),
  });
  if (verifications.isPending || checkpoints.isPending) return <span className="text-sm text-muted">Loading…</span>;

  const latest = verifications.data?.verifications[0];
  const last = checkpoints.data?.last_verified;
  const count = checkpoints.data?.checkpoints.length ?? 0;
  return (
    <div className="text-sm">
      {latest ? (
        <span className="flex flex-wrap items-center gap-2">
          {formatNumber(latest.checked)} re-hashed on {latest.host}
          {latest.failed > 0 ? (
            <StateBadge tone="failed">{formatNumber(latest.failed)} failed</StateBadge>
          ) : (
            <span className="text-muted">all intact</span>
          )}
        </span>
      ) : (
        <span className="block text-muted">No reads re-hashed yet</span>
      )}
      {count > 0 && (
        <span className="block text-muted">
          {last ? `Roll back to step ${formatNumber(last.step)}, verified on ${last.verified_by}` : `${count} checkpoints, none verified yet`}
        </span>
      )}
    </div>
  );
}
