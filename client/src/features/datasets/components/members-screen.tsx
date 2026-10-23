"use client";

import { useState } from "react";
import { Button } from "@/components/ui/button";
import { ConfirmDialog } from "@/components/ui/dialog";
import { LoadMore } from "@/components/ui/page";
import { Panel } from "@/components/ui/panel";
import { EmptyState, ErrorState, SkeletonRows } from "@/components/ui/states";
import { Table, Td, Th } from "@/components/ui/table";
import { useSession } from "@/lib/auth/session";
import { memberSourceLabel } from "../helpers";
import { useDataset, useMembers, useRemoveMember } from "../hooks";
import type { DatasetMember } from "../types";
import { AddMembersDialog } from "./add-members";
import { SnapshotControl } from "./snapshot-control";

export function MembersScreen({ id }: { id: string }) {
  const { can } = useSession();
  const dataset = useDataset(id);
  const members = useMembers(id);
  const remove = useRemoveMember(id);
  const [adding, setAdding] = useState(false);
  const [removing, setRemoving] = useState<DatasetMember | null>(null);

  const archived = dataset.data?.lifecycle_status === "ARCHIVED";
  const canWrite = can("datasets.write") && !archived;
  const unpinned = members.items.filter((m) => !m.version_id).length;

  return (
    <div className="grid gap-6">
      <Panel
        title="Draft members"
        description="This is the mutable draft. Changes here never alter an existing snapshot."
        actions={
          <>
            {canWrite && (
              <Button variant="primary" onClick={() => setAdding(true)}>
                Add members
              </Button>
            )}
            {dataset.data && <SnapshotControl dataset={dataset.data} />}
          </>
        }
      >
        {unpinned > 0 && (
          <p className="neo-in-sm mb-4 p-3 text-sm text-muted">
            {unpinned} of the {members.items.length} loaded members are not pinned. They follow the current version of
            their object until a snapshot pins them.
          </p>
        )}

        {members.isPending ? (
          <SkeletonRows rows={6} />
        ) : members.isError && members.items.length === 0 ? (
          <ErrorState error={members.error} onRetry={() => members.refetch()} />
        ) : members.items.length === 0 ? (
          <EmptyState title="The draft has no members">
            Members are catalog objects, optionally pinned to one version. Add them by path, object id or from the
            catalog, or import a manifest from the Imports tab. You need at least one member before you can take a
            snapshot.
          </EmptyState>
        ) : (
          <>
            <Table>
              <thead>
                <tr>
                  <Th>Object</Th>
                  <Th>Version</Th>
                  <Th>Label</Th>
                  <Th>Split</Th>
                  <Th>Metadata</Th>
                  <Th>Source</Th>
                  {canWrite && (
                    <Th>
                      <span className="sr-only">Actions</span>
                    </Th>
                  )}
                </tr>
              </thead>
              <tbody>
                {members.items.map((member) => (
                  <tr key={member.id}>
                    <Td>
                      <div className="mono-data font-medium">{member.path}</div>
                      <div className="text-sm text-muted">
                        {member.namespace ? `namespace ${member.namespace}` : "root namespace"}
                      </div>
                    </Td>
                    <Td>
                      {member.version_id ? (
                        <span className="mono-data" title="Pinned version (asset id)">
                          Pinned {member.version_id}
                        </span>
                      ) : (
                        <span>Follows current version</span>
                      )}
                    </Td>
                    <Td>{member.label || <span className="text-muted">None</span>}</Td>
                    <Td>{member.split || <span className="text-muted">None</span>}</Td>
                    <Td className="mono-data">
                      {Object.entries(member.metadata ?? {}).map(([k, v]) => (
                        <div key={k}>
                          {k}={v}
                        </div>
                      ))}
                    </Td>
                    <Td>{memberSourceLabel(member.source)}</Td>
                    {canWrite && (
                      <Td>
                        <Button variant="ghost" onClick={() => setRemoving(member)} aria-label={`Remove ${member.path}`}>
                          Remove
                        </Button>
                      </Td>
                    )}
                  </tr>
                ))}
              </tbody>
            </Table>
            <LoadMore
              hasNext={Boolean(members.hasNextPage)}
              loading={members.isFetchingNextPage}
              onLoad={() => members.fetchNextPage()}
              shown={members.items.length}
            />
          </>
        )}
      </Panel>

      {adding && <AddMembersDialog datasetId={id} open onClose={() => setAdding(false)} />}

      <ConfirmDialog
        open={Boolean(removing)}
        onOpenChange={(open) => !open && setRemoving(null)}
        title="Remove from the draft"
        description={
          <>
            Remove <span className="mono-data">{removing?.path}</span> from the draft. The object itself is not
            deleted, and snapshots that already contain it are not changed.
          </>
        }
        confirmLabel="Remove member"
        onConfirm={async () => {
          if (removing) await remove.mutateAsync(removing.id);
        }}
      />
    </div>
  );
}
