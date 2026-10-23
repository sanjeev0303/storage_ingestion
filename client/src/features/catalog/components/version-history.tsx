"use client";

import Link from "next/link";
import { EmptyState, ErrorState, SkeletonRows } from "@/components/ui/states";
import { LoadMore } from "@/components/ui/page";
import { Panel } from "@/components/ui/panel";
import { StateBadge } from "@/components/ui/status";
import { Table, Td, Th } from "@/components/ui/table";
import { Time } from "@/components/ui/time";
import { formatBytes } from "@/lib/utils/format";
import { useVersions } from "../hooks";
import { isTombstone } from "../preview";
import { statusLabel } from "./version-detail";

export function VersionHistory({ objectId, selected }: { objectId: string; selected?: number }) {
  const history = useVersions(objectId);
  const base = `/app/catalog/${encodeURIComponent(objectId)}`;

  return (
    <Panel title="Version history" description="Newest first. Deleted versions stay listed as a record and cannot be downloaded.">
      {history.isPending && <SkeletonRows rows={4} />}
      {history.isError && <ErrorState error={history.error} onRetry={() => history.refetch()} />}
      {history.isSuccess && history.items.length === 0 && (
        <EmptyState title="No published versions">
          Versions appear here after an upload of this path has been verified and published.
        </EmptyState>
      )}
      {history.isSuccess && history.items.length > 0 && (
        <>
          <Table aria-label="Versions">
            <thead>
              <tr>
                <Th>Version</Th>
                <Th>State</Th>
                <Th>Size</Th>
                <Th>Content type</Th>
                <Th>Published</Th>
              </tr>
            </thead>
            <tbody>
              {history.items.map((version) => (
                <tr key={version.asset_id} className={version.version === selected ? "bg-faint" : undefined}>
                  <Td>
                    <Link
                      href={`${base}?version=${version.version}`}
                      scroll={false}
                      className="font-medium underline underline-offset-2"
                      aria-label={`Version ${version.version}`}
                    >
                      {version.version}
                    </Link>
                  </Td>
                  <Td>
                    <span className="flex flex-wrap gap-1.5">
                      {version.current ? <StateBadge tone="done">Current</StateBadge> : <StateBadge>Historical</StateBadge>}
                      <StateBadge tone={isTombstone(version.status) ? "attention" : "neutral"}>{statusLabel(version)}</StateBadge>
                    </span>
                  </Td>
                  <Td className="whitespace-nowrap tabular-nums">{formatBytes(version.size_bytes)}</Td>
                  <Td>{version.content_type}</Td>
                  <Td className="whitespace-nowrap">
                    <Time value={version.published_at ?? version.created_at} />
                  </Td>
                </tr>
              ))}
            </tbody>
          </Table>
          <LoadMore
            shown={history.items.length}
            hasNext={Boolean(history.hasNextPage)}
            loading={history.isFetchingNextPage}
            onLoad={() => history.fetchNextPage()}
          />
        </>
      )}
    </Panel>
  );
}
