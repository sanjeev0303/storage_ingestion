"use client";

import { Panel } from "@/components/ui/panel";
import { ErrorState, SkeletonRows } from "@/components/ui/states";
import { Table, Td, Th } from "@/components/ui/table";
import { formatBytes, formatNumber } from "@/lib/utils/format";
import { useQuotaQuery } from "../hooks";
import type { Quota } from "../types";
import { QuotaMeters } from "./quota-view";

type Row = { label: string; limit: number; used?: number; bytes?: boolean };

function rows(quota: Quota): Row[] {
  const { limits: l, usage: u } = quota;
  return [
    { label: "Storage", limit: l.max_storage_bytes, used: u.stored_bytes, bytes: true },
    { label: "Objects", limit: l.max_assets, used: u.asset_count },
    { label: "Largest single object", limit: l.max_object_bytes, bytes: true },
    { label: "Active uploads", limit: l.max_active_uploads, used: u.active_uploads },
    { label: "Active multipart uploads", limit: l.max_active_multipart, used: u.active_multipart },
    { label: "Pending processing", limit: l.max_pending_processing, used: u.pending_processing },
    { label: "Active ingestion jobs", limit: l.max_active_jobs },
    { label: "Pending manifest entries", limit: l.max_pending_entries },
    { label: "Entries per manifest", limit: l.max_manifest_entries },
    { label: "Manifest size", limit: l.max_manifest_bytes, bytes: true },
    { label: "Requests per minute", limit: l.max_requests_per_minute },
  ];
}

export function QuotaTab() {
  const quota = useQuotaQuery();
  if (quota.isPending) return <SkeletonRows rows={6} />;
  if (quota.isError) return <ErrorState error={quota.error} onRetry={() => quota.refetch()} />;

  const { usage } = quota.data;
  const fmt = (n: number, bytes?: boolean) => (bytes ? formatBytes(n) : formatNumber(n));

  return (
    <div className="grid gap-6 lg:grid-cols-3">
      <div className="flex min-w-0 flex-col gap-4 lg:col-span-2">
        <Table>
          <caption className="sr-only">Quota limits and current usage</caption>
          <thead>
            <tr>
              <Th>Limit</Th>
              <Th>In use</Th>
              <Th>Maximum</Th>
            </tr>
          </thead>
          <tbody>
            {rows(quota.data).map((row) => (
              <tr key={row.label}>
                <Td className="font-medium">{row.label}</Td>
                <Td className="tabular-nums">
                  {row.used === undefined ? (
                    <span className="text-muted">Not available from the API</span>
                  ) : (
                    fmt(row.used, row.bytes)
                  )}
                </Td>
                <Td className="tabular-nums">{fmt(row.limit, row.bytes)}</Td>
              </tr>
            ))}
          </tbody>
        </Table>
        <p className="max-w-prose text-sm text-muted">
          Limits are changed by platform admins, not from this screen. Stored objects and bytes come from the quota
          ledger.
        </p>
      </div>
      <div className="flex min-w-0 flex-col gap-6">
        <Panel title="Usage">
          <QuotaMeters quota={quota.data} />
        </Panel>
        <Panel title="Also counted against storage">
          <dl className="flex flex-col gap-2 text-base">
            <Stat label="Reserved by uploads" value={formatBytes(usage.reserved_bytes)} />
            <Stat label="Waiting for deletion" value={formatBytes(usage.pending_deletion_bytes)} />
            <Stat label="Quarantined" value={formatBytes(usage.quarantined_bytes)} />
            <Stat label="Incomplete multipart" value={formatBytes(usage.incomplete_multipart_bytes)} />
            <Stat label="Remaining" value={formatBytes(usage.remaining_storage_bytes)} />
          </dl>
        </Panel>
      </div>
    </div>
  );
}

function Stat({ label, value }: { label: string; value: string }) {
  return (
    <div className="flex justify-between gap-3">
      <dt className="text-muted">{label}</dt>
      <dd className="font-medium tabular-nums">{value}</dd>
    </div>
  );
}
