"use client";

import Link from "next/link";
import { useState } from "react";
import { ButtonLink } from "@/components/ui/button";
import { Field, Select } from "@/components/ui/field";
import { LoadMore, PageHeader } from "@/components/ui/page";
import { EmptyState, ErrorState, SkeletonRows } from "@/components/ui/states";
import { StateBadge } from "@/components/ui/status";
import { Table, Td, Th } from "@/components/ui/table";
import { Time } from "@/components/ui/time";
import { useSession } from "@/lib/auth/session";
import { formatNumber, shortId } from "@/lib/utils/format";
import { lifecycleLabels, lifecycleTone } from "../helpers";
import { useDatasets } from "../hooks";
import type { LifecycleStatus } from "../types";

const statuses = Object.keys(lifecycleLabels) as LifecycleStatus[];

export function DatasetsList() {
  const { can } = useSession();
  const [status, setStatus] = useState("");
  const datasets = useDatasets(status);
  const canWrite = can("datasets.write");

  return (
    <>
      <PageHeader
        title="Datasets"
        description="A dataset is a draft collection of catalog objects. Take a snapshot to freeze it into a version you can validate and train on."
        actions={
          canWrite && (
            <ButtonLink href="/app/datasets/new" variant="primary">
              New dataset
            </ButtonLink>
          )
        }
      />

      <div className="mb-4 max-w-xs">
        <Field label="State">
          {(props) => (
            <Select {...props} value={status} onChange={(e) => setStatus(e.target.value)}>
              <option value="">All states</option>
              {statuses.map((value) => (
                <option key={value} value={value}>
                  {lifecycleLabels[value]}
                </option>
              ))}
            </Select>
          )}
        </Field>
      </div>

      {datasets.isPending ? (
        <SkeletonRows rows={6} />
      ) : datasets.isError && datasets.items.length === 0 ? (
        <ErrorState error={datasets.error} onRetry={() => datasets.refetch()} />
      ) : datasets.items.length === 0 ? (
        <EmptyState
          title={status ? "No datasets in this state" : "No datasets yet"}
        >
          {status
            ? "Choose another state to see the rest."
            : "Create a dataset, add catalog objects to its draft, then take a snapshot. The snapshot is the immutable version that validation, readiness and lineage refer to."}
        </EmptyState>
      ) : (
        <>
          <Table>
            <thead>
              <tr>
                <Th>Name</Th>
                <Th>State</Th>
                <Th className="text-right">Members in draft</Th>
                <Th>Current version</Th>
                <Th>Profile</Th>
                <Th>Updated</Th>
              </tr>
            </thead>
            <tbody>
              {datasets.items.map((dataset) => (
                <tr key={dataset.id}>
                  <Td>
                    <Link href={`/app/datasets/${dataset.id}`} className="font-semibold underline-offset-2 hover:underline">
                      {dataset.name}
                    </Link>
                  </Td>
                  <Td>
                    <StateBadge tone={lifecycleTone(dataset.lifecycle_status)}>
                      {lifecycleLabels[dataset.lifecycle_status]}
                    </StateBadge>
                  </Td>
                  <Td className="text-right tabular-nums">{formatNumber(dataset.member_count)}</Td>
                  <Td>
                    {dataset.current_version_id ? (
                      <Link
                        href={`/app/datasets/${dataset.id}/versions/${dataset.current_version_id}`}
                        className="mono-data underline-offset-2 hover:underline"
                      >
                        {shortId(dataset.current_version_id)}
                      </Link>
                    ) : (
                      <span className="text-muted">No snapshot yet</span>
                    )}
                  </Td>
                  <Td>
                    {dataset.profile_id ? (
                      <span className="mono-data">
                        {dataset.profile_id}
                        {dataset.profile_version ? `@${dataset.profile_version}` : ""}
                      </span>
                    ) : (
                      <span className="text-muted">None</span>
                    )}
                  </Td>
                  <Td>
                    <Time value={dataset.updated_at} />
                  </Td>
                </tr>
              ))}
            </tbody>
          </Table>
          <LoadMore
            hasNext={Boolean(datasets.hasNextPage)}
            loading={datasets.isFetchingNextPage}
            onLoad={() => datasets.fetchNextPage()}
            shown={datasets.items.length}
          />
        </>
      )}
    </>
  );
}
