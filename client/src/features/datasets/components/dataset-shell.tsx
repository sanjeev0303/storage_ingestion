"use client";

import type { ReactNode } from "react";
import { PageHeader, TabNav } from "@/components/ui/page";
import { ErrorState, Skeleton } from "@/components/ui/states";
import { StateBadge } from "@/components/ui/status";
import { lifecycleLabels, lifecycleTone } from "../helpers";
import { useDataset } from "../hooks";
import { DraftBadge } from "./draft-notice";

export function DatasetShell({ id, children }: { id: string; children: ReactNode }) {
  const dataset = useDataset(id);
  const base = `/app/datasets/${id}`;
  const tabs = [
    { href: base, label: "Overview" },
    { href: `${base}/members`, label: "Members" },
    { href: `${base}/versions`, label: "Versions" },
    { href: `${base}/imports`, label: "Imports" },
    { href: `${base}/validation`, label: "Validation" },
    { href: `${base}/lineage`, label: "Lineage" },
  ];

  if (dataset.isPending) {
    return (
      <div role="status" aria-label="Loading dataset" className="flex flex-col gap-3">
        <Skeleton className="h-8 w-64" />
        <Skeleton className="h-9 w-full max-w-xl" />
      </div>
    );
  }

  if (dataset.isError) {
    return <ErrorState error={dataset.error} title="The dataset did not load" onRetry={() => dataset.refetch()} />;
  }

  const archived = dataset.data.lifecycle_status === "ARCHIVED";

  return (
    <>
      <PageHeader
        title={
          <span className="flex flex-wrap items-center gap-3">
            {dataset.data.name}
            <StateBadge tone={lifecycleTone(dataset.data.lifecycle_status)}>
              {lifecycleLabels[dataset.data.lifecycle_status]}
            </StateBadge>
            {!archived && <DraftBadge />}
          </span>
        }
        crumbs={[{ label: "Datasets", href: "/app/datasets" }, { label: dataset.data.name }]}
      />
      {archived && (
        <div role="status" className="neo-in mb-6 p-5">
          <h2 className="text-md font-semibold">This dataset is archived</h2>
          <p className="mt-1 max-w-prose text-base text-muted">
            Archived is final. You can still read members, versions, reports and lineage, but every change is refused
            with a dataset_archived error.
          </p>
        </div>
      )}
      <TabNav tabs={tabs} label="Dataset sections" />
      {children}
    </>
  );
}
