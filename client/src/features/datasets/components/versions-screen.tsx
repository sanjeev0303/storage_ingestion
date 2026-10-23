"use client";

import Link from "next/link";
import { CopyButton } from "@/components/ui/copy-button";
import { LoadMore } from "@/components/ui/page";
import { Panel } from "@/components/ui/panel";
import { EmptyState, ErrorState, SkeletonRows } from "@/components/ui/states";
import { StateBadge } from "@/components/ui/status";
import { Table, Td, Th } from "@/components/ui/table";
import { Time } from "@/components/ui/time";
import { formatBytes, formatNumber } from "@/lib/utils/format";
import { shortHash, validationLabels, validationTone } from "../helpers";
import { useDataset, useVersions } from "../hooks";
import { ImmutableBadge } from "./draft-notice";
import { SnapshotControl } from "./snapshot-control";

export function VersionsScreen({ id }: { id: string }) {
  const dataset = useDataset(id);
  const versions = useVersions(id);

  return (
    <Panel
      title="Versions"
      description="Each version is an immutable snapshot. Editing the draft never changes one."
      actions={dataset.data && <SnapshotControl dataset={dataset.data} />}
    >
      {versions.isPending ? (
        <SkeletonRows rows={5} />
      ) : versions.isError && versions.items.length === 0 ? (
        <ErrorState error={versions.error} onRetry={() => versions.refetch()} />
      ) : versions.items.length === 0 ? (
        <EmptyState title="No versions yet">
          A version appears when you take a snapshot of the draft. Add members first, then use Create snapshot. Until
          then there is nothing to validate or train on.
        </EmptyState>
      ) : (
        <>
          <Table>
            <thead>
              <tr>
                <Th>Version</Th>
                <Th>Status</Th>
                <Th>Validation</Th>
                <Th className="text-right">Members</Th>
                <Th className="text-right">Total size</Th>
                <Th>Manifest hash</Th>
                <Th>Created</Th>
              </tr>
            </thead>
            <tbody>
              {versions.items.map((version) => (
                <tr key={version.id}>
                  <Td>
                    <Link
                      href={`/app/datasets/${id}/versions/${version.id}`}
                      className="font-semibold underline-offset-2 hover:underline"
                    >
                      Version {version.version}
                    </Link>
                    {dataset.data?.current_version_id === version.id && (
                      <span className="ml-2 text-sm text-muted">current</span>
                    )}
                  </Td>
                  <Td>
                    <span className="inline-flex items-center gap-2">
                      {version.status}
                      <ImmutableBadge />
                    </span>
                  </Td>
                  <Td>
                    <StateBadge tone={validationTone(version.validation_status)}>
                      {validationLabels[version.validation_status]}
                    </StateBadge>
                  </Td>
                  <Td className="text-right tabular-nums">{formatNumber(version.member_count)}</Td>
                  <Td className="text-right tabular-nums">{formatBytes(version.total_bytes)}</Td>
                  <Td>
                    <span className="inline-flex items-center gap-2">
                      <span className="mono-data" title={version.manifest_sha256}>
                        {shortHash(version.manifest_sha256)}
                      </span>
                      <CopyButton value={version.manifest_sha256} label="Copy hash" />
                    </span>
                  </Td>
                  <Td>
                    <Time value={version.created_at} />
                  </Td>
                </tr>
              ))}
            </tbody>
          </Table>
          <LoadMore
            hasNext={Boolean(versions.hasNextPage)}
            loading={versions.isFetchingNextPage}
            onLoad={() => versions.fetchNextPage()}
            shown={versions.items.length}
          />
        </>
      )}
    </Panel>
  );
}
