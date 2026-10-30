"use client";

import { useQuery } from "@tanstack/react-query";
import { StateBadge } from "@/components/ui/status";
import { useOrganizationId } from "@/lib/auth/session";
import { orgKey } from "@/lib/query/keys";
import { formatNumber } from "@/lib/utils/format";
import { runLeakage } from "../api";

// Whether a run's evaluation inputs were found in its training inputs.
// Runs record roles (TRAIN, VALIDATION, TEST) for this to apply.
export function RunLeakage({ runId, roles }: { runId: string; roles?: Record<string, string> }) {
  const organizationId = useOrganizationId();
  const values = Object.values(roles ?? {});
  const hasPair = values.includes("TRAIN") && (values.includes("VALIDATION") || values.includes("TEST"));
  const checks = useQuery({
    queryKey: orgKey(organizationId, "quality", "run-leakage", runId),
    queryFn: ({ signal }) => runLeakage(runId, signal),
    enabled: Boolean(organizationId) && hasPair,
  });
  if (!hasPair) return null;
  if (checks.isPending || checks.isError) return null;
  const list = checks.data.checks;
  if (list.length === 0) return <span className="mt-1 block text-sm text-muted">Leakage check waiting</span>;
  const leaked = list.filter((c) => c.state === "LEAKED");
  if (leaked.length === 0) return <span className="mt-1 block text-sm text-muted">No leakage into evaluation</span>;
  const files = leaked.reduce((n, c) => Math.max(n, c.overlap), 0);
  return (
    <span className="mt-1 flex flex-wrap items-center gap-2 text-sm">
      <StateBadge tone="failed">Leakage</StateBadge>
      {formatNumber(files)} evaluation files in training
    </span>
  );
}
