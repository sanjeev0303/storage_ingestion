"use client";

import { useMutation, useQuery } from "@tanstack/react-query";
import { Button } from "@/components/ui/button";
import { StateBadge } from "@/components/ui/status";
import { notify } from "@/components/ui/toast";
import { useOrganizationId } from "@/lib/auth/session";
import { orgKey } from "@/lib/query/keys";
import { fetchFile, runOrder, saveBlob } from "../api";

// Whether each dataset's basis was recorded before its snapshot, and the
// snapshot taken before the run.
export function RunProvenance({ runId }: { runId: string }) {
  const organizationId = useOrganizationId();
  const order = useQuery({
    queryKey: orgKey(organizationId, "evidence", "run-order", runId),
    queryFn: ({ signal }) => runOrder(runId, signal),
    enabled: Boolean(organizationId),
  });
  if (order.isPending || order.isError) return null;
  const o = order.data;
  return (
    <span className="mt-1 block text-sm">
      {o.in_order ? (
        <span className="text-muted">Basis recorded before every snapshot</span>
      ) : (
        <>
          <StateBadge tone="warning">Basis out of order</StateBadge>
          {o.problems[0] && <span className="mt-1 block">{o.problems[0]}</span>}
        </>
      )}
    </span>
  );
}

// The model's training-content summary on the EU template, as Markdown.
export function ModelSummaryButton({ modelId, name }: { modelId: string; name: string }) {
  const download = useMutation({
    mutationFn: () => fetchFile(`/models/${encodeURIComponent(modelId)}/training-summary`, { format: "md" }),
    onSuccess: (blob) => saveBlob(blob, `${name}-training-summary.md`),
    onError: notify.error,
  });
  return (
    <Button variant="ghost" className="h-7 px-2 text-sm" onClick={() => download.mutate()} loading={download.isPending}>
      Training summary
    </Button>
  );
}
