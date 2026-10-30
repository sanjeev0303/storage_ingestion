"use client";

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Button } from "@/components/ui/button";
import { DefList } from "@/components/ui/page";
import { Panel } from "@/components/ui/panel";
import { ErrorState, SkeletonRows } from "@/components/ui/states";
import { notify } from "@/components/ui/toast";
import { useOrganizationId, useSession } from "@/lib/auth/session";
import { orgKey } from "@/lib/query/keys";
import { formatBytes, formatNumber } from "@/lib/utils/format";
import { getVersionEstimate, restoreVersion, type Estimate } from "../api";
import { money } from "./costs-screen";

function line(e: Estimate) {
  const parts = [
    e.request_usd ? `requests ${money(e.request_usd)}` : "",
    e.egress_usd ? `egress ${money(e.egress_usd)}` : "",
    e.retrieval_usd ? `retrieval ${money(e.retrieval_usd)}` : "",
  ].filter(Boolean);
  return `${money(e.total_usd)}${parts.length ? ` (${parts.join(", ")})` : ""}`;
}

// What reading this version costs, and restoring what is archived first.
export function VersionCosts({ id, versionId }: { id: string; versionId: string }) {
  const organizationId = useOrganizationId();
  const { can } = useSession();
  const client = useQueryClient();
  const estimate = useQuery({
    queryKey: orgKey(organizationId, "costs", versionId, "estimate"),
    queryFn: ({ signal }) => getVersionEstimate(id, versionId, signal),
    enabled: Boolean(organizationId),
  });
  const restore = useMutation({
    mutationFn: () => restoreVersion(id, versionId, { days: 7, speed: "STANDARD" }),
    onSuccess: (r) => {
      notify.success(`${r.requested} restores asked for; files read as each is ready`);
      client.invalidateQueries({ queryKey: orgKey(organizationId, "costs", versionId) });
    },
    onError: notify.error,
  });
  const e = estimate.data;
  return (
    <Panel title="Cost to read" description="At the providers' list prices, before anything is read.">
      {estimate.isPending ? (
        <SkeletonRows rows={2} />
      ) : estimate.isError || !e ? (
        <ErrorState error={estimate.error} onRetry={() => estimate.refetch()} />
      ) : (
        <div className="flex flex-col gap-4">
          <DefList
            items={[
              { label: "Download it all", value: line(e.download) },
              { label: "One training epoch", value: `${line(e.epoch)}, ${formatNumber(e.epoch.requests)} requests` },
              {
                label: "Archived",
                value: e.archived_files
                  ? `${formatNumber(e.archived_files)} files, ${formatBytes(e.archived_bytes)}: restoring them costs ${line(e.restore)} and takes about ${e.restore.hours} hours`
                  : "Nothing: every file reads now",
              },
            ]}
          />
          {e.archived_files > 0 && can("assets.download") && (
            <div>
              <Button onClick={() => restore.mutate()} disabled={restore.isPending}>
                Restore archived files for 7 days
              </Button>
            </div>
          )}
        </div>
      )}
    </Panel>
  );
}
