"use client";

import Link from "next/link";
import { LoadMore } from "@/components/ui/page";
import { Panel } from "@/components/ui/panel";
import { EmptyState, ErrorState, SkeletonRows } from "@/components/ui/states";
import { StateBadge } from "@/components/ui/status";
import { Time } from "@/components/ui/time";
import { api } from "@/lib/api/client";
import { useOrganizationId } from "@/lib/auth/session";
import { orgKey } from "@/lib/query/keys";
import { usePagedQuery } from "@/lib/query/paged";
import { formatNumber } from "@/lib/utils/format";
import type { ManifestImport } from "../types";

const TONE = { COMPLETED: "done", FAILED: "failed", CANCELLED: "neutral", RUNNING: "active", QUEUED: "active", AWAITING_UPLOAD: "attention" } as const;

// Every manifest import of the dataset, newest first, whoever started it.
export function ImportHistory({ id }: { id: string }) {
  const organizationId = useOrganizationId();
  const imports = usePagedQuery<ManifestImport, string>({
    queryKey: orgKey(organizationId, "datasets", "imports", id),
    enabled: Boolean(organizationId),
    fetchPage: async (cursor, signal) => {
      const page = await api<{ imports: ManifestImport[]; next_cursor?: string }>(`/datasets/${id}/manifests`, {
        query: { cursor, limit: 25 },
        signal,
      });
      return { items: page.imports, next: page.next_cursor };
    },
  });

  return (
    <Panel title="Imports" description="Every import of this dataset, newest first.">
      {imports.isPending ? (
        <SkeletonRows rows={3} />
      ) : imports.isError ? (
        <ErrorState error={imports.error} onRetry={() => imports.refetch()} />
      ) : imports.items.length === 0 ? (
        <EmptyState title="No imports yet">A manifest import adds or updates many members at once. Start one beside this.</EmptyState>
      ) : (
        <>
          <ul className="flex flex-col gap-3">
            {imports.items.map((item) => (
              <li key={item.id} className="flex flex-wrap items-start justify-between gap-2">
                <div className="min-w-0">
                  <Link href={`/app/datasets/${id}/imports/${item.id}`} className="mono-data underline underline-offset-2">
                    {item.id.slice(0, 12)}
                  </Link>
                  <p className="text-sm text-muted">
                    {formatNumber(item.progress.members_added)} added, {formatNumber(item.progress.members_updated)} updated
                    {item.progress.lines_rejected > 0 && `, ${formatNumber(item.progress.lines_rejected)} rejected`}
                  </p>
                </div>
                <div className="text-right">
                  <StateBadge tone={TONE[item.state]}>{item.state.replace("_", " ").toLowerCase()}</StateBadge>
                  <div className="text-sm text-muted">
                    <Time value={item.created_at} />
                  </div>
                </div>
              </li>
            ))}
          </ul>
          <LoadMore
            hasNext={Boolean(imports.hasNextPage)}
            loading={imports.isFetchingNextPage}
            onLoad={() => imports.fetchNextPage()}
            shown={imports.items.length}
          />
        </>
      )}
    </Panel>
  );
}
