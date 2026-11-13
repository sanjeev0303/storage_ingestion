"use client";

import Link from "next/link";
import { LoadMore } from "@/components/ui/page";
import { Panel } from "@/components/ui/panel";
import { ErrorState, SkeletonRows } from "@/components/ui/states";
import { api } from "@/lib/api/client";
import { useOrganizationId } from "@/lib/auth/session";
import { orgKey } from "@/lib/query/keys";
import { usePagedQuery } from "@/lib/query/paged";

type Membership = { dataset_id: string; dataset_name: string; label?: string; split?: string; added_at: string };
type Snapshot = { dataset_id: string; dataset_name: string; version_id: string; version: number; object_version: number };
type UsagePage = {
  datasets: Membership[];
  snapshots: Snapshot[];
  next_datasets_cursor?: string;
  next_snapshots_cursor?: string;
};

const PAGE_SIZE = 20;

function usagePath(objectId: string) {
  return `/catalog/objects/${encodeURIComponent(objectId)}/datasets`;
}

// Which drafts hold this object and which snapshots pinned one of its
// versions: what deleting or erasing it would touch. Each list pages on its own.
export function ObjectDatasets({ objectId }: { objectId: string }) {
  const organizationId = useOrganizationId();
  const drafts = usePagedQuery<Membership, string>({
    queryKey: orgKey(organizationId, "catalog", "object-datasets", objectId, "datasets"),
    enabled: Boolean(organizationId),
    fetchPage: async (cursor, signal) => {
      const page = await api<UsagePage>(usagePath(objectId), {
        query: { part: "datasets", cursor, limit: PAGE_SIZE },
        signal,
      });
      return { items: page.datasets, next: page.next_datasets_cursor };
    },
  });
  const snapshots = usePagedQuery<Snapshot, string>({
    queryKey: orgKey(organizationId, "catalog", "object-datasets", objectId, "snapshots"),
    enabled: Boolean(organizationId),
    fetchPage: async (cursor, signal) => {
      const page = await api<UsagePage>(usagePath(objectId), {
        query: { part: "snapshots", cursor, limit: PAGE_SIZE },
        signal,
      });
      return { items: page.snapshots, next: page.next_snapshots_cursor };
    },
  });

  let body;
  if (drafts.isPending || snapshots.isPending) {
    body = <SkeletonRows rows={2} />;
  } else if (drafts.isError) {
    body = <ErrorState error={drafts.error} onRetry={() => drafts.refetch()} />;
  } else if (snapshots.isError) {
    body = <ErrorState error={snapshots.error} onRetry={() => snapshots.refetch()} />;
  } else if (drafts.items.length === 0 && snapshots.items.length === 0) {
    body = <p className="text-base text-muted">No dataset or snapshot uses this object.</p>;
  } else {
    body = (
      <div className="flex flex-col gap-4 text-base">
        {drafts.items.length > 0 && (
          <div>
            <h3 className="text-sm font-semibold">Dataset drafts</h3>
            <ul className="mt-1 flex flex-col gap-1">
              {drafts.items.map((d) => (
                <li key={d.dataset_id} className="[overflow-wrap:anywhere]">
                  <Link href={`/app/datasets/${d.dataset_id}/members`} className="underline underline-offset-2">
                    {d.dataset_name}
                  </Link>
                  {(d.label || d.split) && <span className="text-sm text-muted"> {[d.label, d.split].filter(Boolean).join(", ")}</span>}
                </li>
              ))}
            </ul>
            {(drafts.hasNextPage || drafts.data.pages.length > 1) && (
              <LoadMore
                hasNext={Boolean(drafts.hasNextPage)}
                loading={drafts.isFetchingNextPage}
                onLoad={() => drafts.fetchNextPage()}
                shown={drafts.items.length}
              />
            )}
          </div>
        )}
        {snapshots.items.length > 0 && (
          <div>
            <h3 className="text-sm font-semibold">Snapshots</h3>
            <ul className="mt-1 flex flex-col gap-1">
              {snapshots.items.map((s) => (
                <li key={s.version_id} className="[overflow-wrap:anywhere]">
                  <Link href={`/app/datasets/${s.dataset_id}/versions/${s.version_id}`} className="underline underline-offset-2">
                    {s.dataset_name} version {s.version}
                  </Link>
                  <span className="text-sm text-muted"> pins object version {s.object_version}</span>
                </li>
              ))}
            </ul>
            {(snapshots.hasNextPage || snapshots.data.pages.length > 1) && (
              <LoadMore
                hasNext={Boolean(snapshots.hasNextPage)}
                loading={snapshots.isFetchingNextPage}
                onLoad={() => snapshots.fetchNextPage()}
                shown={snapshots.items.length}
              />
            )}
          </div>
        )}
      </div>
    );
  }

  return (
    <Panel title="Used by" description="Deleting this object leaves these records in place; snapshots then show its bytes as erased.">
      {body}
    </Panel>
  );
}
