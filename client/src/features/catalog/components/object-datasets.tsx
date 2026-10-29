"use client";

import { useQuery } from "@tanstack/react-query";
import Link from "next/link";
import { Panel } from "@/components/ui/panel";
import { ErrorState, SkeletonRows } from "@/components/ui/states";
import { api } from "@/lib/api/client";
import { useOrganizationId } from "@/lib/auth/session";
import { orgKey } from "@/lib/query/keys";

type Usage = {
  datasets: { dataset_id: string; dataset_name: string; label?: string; split?: string; added_at: string }[];
  snapshots: { dataset_id: string; dataset_name: string; version_id: string; version: number; object_version: number }[];
};

// Which drafts hold this object and which snapshots pinned one of its
// versions: what deleting or erasing it would touch.
export function ObjectDatasets({ objectId }: { objectId: string }) {
  const organizationId = useOrganizationId();
  const usage = useQuery({
    queryKey: orgKey(organizationId, "catalog", "object-datasets", objectId),
    queryFn: ({ signal }) => api<Usage>(`/catalog/objects/${encodeURIComponent(objectId)}/datasets`, { signal }),
    enabled: Boolean(organizationId),
  });

  return (
    <Panel title="Used by" description="Deleting this object leaves these records in place; snapshots then show its bytes as erased.">
      {usage.isPending ? (
        <SkeletonRows rows={2} />
      ) : usage.isError ? (
        <ErrorState error={usage.error} onRetry={() => usage.refetch()} />
      ) : usage.data.datasets.length === 0 && usage.data.snapshots.length === 0 ? (
        <p className="text-base text-muted">No dataset or snapshot uses this object.</p>
      ) : (
        <div className="flex flex-col gap-4 text-base">
          {usage.data.datasets.length > 0 && (
            <div>
              <h3 className="text-sm font-semibold">Dataset drafts</h3>
              <ul className="mt-1 flex flex-col gap-1">
                {usage.data.datasets.map((d) => (
                  <li key={d.dataset_id}>
                    <Link href={`/app/datasets/${d.dataset_id}/members`} className="underline underline-offset-2">
                      {d.dataset_name}
                    </Link>
                    {(d.label || d.split) && <span className="text-sm text-muted"> {[d.label, d.split].filter(Boolean).join(", ")}</span>}
                  </li>
                ))}
              </ul>
            </div>
          )}
          {usage.data.snapshots.length > 0 && (
            <div>
              <h3 className="text-sm font-semibold">Snapshots</h3>
              <ul className="mt-1 flex flex-col gap-1">
                {usage.data.snapshots.map((s) => (
                  <li key={s.version_id}>
                    <Link href={`/app/datasets/${s.dataset_id}/versions/${s.version_id}`} className="underline underline-offset-2">
                      {s.dataset_name} version {s.version}
                    </Link>
                    <span className="text-sm text-muted"> pins object version {s.object_version}</span>
                  </li>
                ))}
              </ul>
            </div>
          )}
        </div>
      )}
    </Panel>
  );
}
