"use client";

import Link from "next/link";
import { useState } from "react";
import { Button } from "@/components/ui/button";
import { ConfirmDialog } from "@/components/ui/dialog";
import { DefList } from "@/components/ui/page";
import { Panel } from "@/components/ui/panel";
import { EmptyState, ErrorState, SkeletonRows } from "@/components/ui/states";
import { StateBadge } from "@/components/ui/status";
import { Table, Td, Th } from "@/components/ui/table";
import { Time } from "@/components/ui/time";
import { useSession } from "@/lib/auth/session";
import { formatBytes, formatNumber } from "@/lib/utils/format";
import { useConnections, useReconcileQuota } from "../hooks";
import { WorkStatus } from "./work-status";
import type { StorageConnection } from "../types";

const notExposed = [
  { name: "Worker and queue health", where: "Worker logs and the process metrics endpoint of each worker. The counts beside this show the work waiting, not the workers." },
  { name: "Reconciliation runs", where: "Reconciler logs. The one action the API offers is the quota reconcile above." },
  { name: "Compaction and cleanup jobs", where: "Background job logs. Their results appear indirectly as lifecycle changes on assets." },
  { name: "API readiness (/ready)", where: "The readiness probe is served by the Go API directly and is not proxied to this console. Check it from the host or your orchestrator." },
];

export function StatusScreen() {
  const { can, isPlatformAdmin, organizationId } = useSession();

  return (
    <div className="grid gap-6 lg:grid-cols-3">
      <div className="flex min-w-0 flex-col gap-6 lg:col-span-2">
        {can("ingestion.read") && <WorkStatus />}
        {can("storage.read") ? (
          <ConnectionHealth />
        ) : (
          <Panel title="Storage connections">
            <p className="text-base text-muted">Your role cannot read storage connections (storage.read).</p>
          </Panel>
        )}
        {isPlatformAdmin && organizationId && <ReconcilePanel />}
      </div>

      <Panel
        title="Not exposed by the API"
        description="The API has no endpoint for these, so this page does not guess. Where the answer lives:"
      >
        <ul className="flex flex-col gap-4">
          {notExposed.map((item) => (
            <li key={item.name}>
              <p className="font-medium">{item.name}</p>
              <p className="text-sm text-muted">{item.where}</p>
            </li>
          ))}
        </ul>
      </Panel>
    </div>
  );
}

function connectionTone(c: StorageConnection) {
  if (c.last_error_category) return "attention";
  return c.status === "ACTIVE" ? "done" : "neutral";
}

function ConnectionHealth() {
  const connections = useConnections();

  return (
    <Panel title="Storage connection health" description="From each connection's last stored test result. Nothing here runs a new test.">
      {connections.isPending ? (
        <SkeletonRows rows={3} />
      ) : connections.isError ? (
        <ErrorState error={connections.error} onRetry={() => connections.refetch()} />
      ) : connections.data.connections.length === 0 ? (
        <EmptyState title="No storage connections">Connect a bucket on the Storage page. Uploads use the platform default until you do.</EmptyState>
      ) : (
        <Table>
          <thead>
            <tr>
              <Th>Connection</Th>
              <Th>Status</Th>
              <Th>Last tested</Th>
              <Th>Last passed</Th>
              <Th>Last error</Th>
              <Th>Capabilities checked</Th>
            </tr>
          </thead>
          <tbody>
            {connections.data.connections.map((c) => (
              <tr key={c.id}>
                <Td>
                  {c.id === "platform" ? (
                    <span className="font-medium">{c.name}</span>
                  ) : (
                    <Link href={`/app/storage/${c.id}`} className="font-medium underline underline-offset-2">
                      {c.name}
                    </Link>
                  )}
                  <div className="text-sm text-muted">{c.provider}</div>
                </Td>
                <Td>
                  <StateBadge tone={connectionTone(c)}>{c.status}</StateBadge>
                </Td>
                <Td>{c.last_tested_at ? <Time value={c.last_tested_at} /> : <span className="text-muted">Never tested</span>}</Td>
                <Td>{c.last_test_ok_at ? <Time value={c.last_test_ok_at} /> : <span className="text-muted">Never passed</span>}</Td>
                <Td>{c.last_error_category ?? <span className="text-muted">None</span>}</Td>
                <Td>
                  {c.capabilities_checked_at ? <Time value={c.capabilities_checked_at} /> : <span className="text-muted">Not checked</span>}
                </Td>
              </tr>
            ))}
          </tbody>
        </Table>
      )}
    </Panel>
  );
}

function ReconcilePanel() {
  const reconcile = useReconcileQuota();
  const [confirming, setConfirming] = useState(false);
  const result = reconcile.data;

  return (
    <Panel
      title="Quota reconcile"
      description="Platform administrators only. Settles reservations left behind by uploads that never finished, for the active organization."
      actions={<Button onClick={() => setConfirming(true)}>Reconcile quota</Button>}
    >
      {result ? (
        <div role="status">
          <p className="mb-3 font-medium">
            {result.repaired ? "Counters were repaired." : "Counters were already correct."}
          </p>
          <DefList
            items={[
              { label: "Reservations settled", value: formatNumber(result.settled_reservations) },
              { label: "Reserved bytes", value: `${formatBytes(result.reserved_bytes_before)} before, ${formatBytes(result.reserved_bytes_after)} after` },
              { label: "Stored bytes", value: `${formatBytes(result.stored_bytes_before)} before, ${formatBytes(result.stored_bytes_after)} after` },
            ]}
          />
        </div>
      ) : (
        <p className="text-base text-muted">No result yet. Running it changes quota counters, so it asks first.</p>
      )}
      <ConfirmDialog
        open={confirming}
        onOpenChange={setConfirming}
        title="Reconcile quota for this organization?"
        description="Counters for reserved and stored bytes are recomputed from the real uploads and assets and corrected if they differ. Uploads in progress are not touched."
        confirmLabel="Reconcile"
        onConfirm={() => reconcile.mutateAsync()}
      />
    </Panel>
  );
}
