"use client";

import Link from "next/link";
import { ButtonLink, Button } from "@/components/ui/button";
import { PageHeader } from "@/components/ui/page";
import { Panel } from "@/components/ui/panel";
import { EmptyState, ErrorState, SkeletonRows } from "@/components/ui/states";
import { Outcome, StateBadge } from "@/components/ui/status";
import { Table, Td, Th } from "@/components/ui/table";
import { Time } from "@/components/ui/time";
import { useSession } from "@/lib/auth/session";
import { useProviderCapabilities, useStorageConnections, useStorageProviders } from "../hooks";
import { PROVIDER_LABELS } from "../schema";
import type { StorageConnection } from "../types";
import { CAPABILITY_ROWS } from "./capability-rows";

const STATUS_LABEL = { PENDING: "Pending", ACTIVE: "Active", DISABLED: "Disabled", DELETED: "Deleted" } as const;

export function ConnectionsPage() {
  const { can } = useSession();
  const connections = useStorageConnections();
  const providers = useStorageProviders();
  const byosEnabled = providers.data?.byos_enabled;

  const add = !can("storage.manage") ? null : byosEnabled === false ? (
    <Button disabled>Add connection</Button>
  ) : (
    <ButtonLink href="/app/storage/new" variant="primary">
      Add connection
    </ButtonLink>
  );

  return (
    <>
      <PageHeader
        title="Storage"
        description="Where this organization's bytes live. New uploads go only to Active connections, and objects already stored never move between connections."
        actions={add}
      />

      {byosEnabled === false && (
        <p role="status" className="neo-in mb-6 p-4 text-base font-medium">
          Customer storage connections are disabled on this deployment. Uploads use the platform-managed storage, and no connection
          can be added, tested or rotated.
        </p>
      )}

      {connections.isPending && <SkeletonRows rows={4} />}
      {connections.isError && <ErrorState error={connections.error} onRetry={() => connections.refetch()} />}
      {connections.data && (
        <div className="flex flex-col gap-6">
          <ConnectionTable connections={connections.data.connections} defaultId={connections.data.default_connection_id} />
          {connections.data.connections.every((c) => c.provider === "PLATFORM_MANAGED") && (
            <EmptyState
              title="Only platform-managed storage so far"
              action={
                add && byosEnabled !== false ? (
                  <ButtonLink href="/app/storage/new">Add your first connection</ButtonLink>
                ) : undefined
              }
            >
              Bring your own bucket from Amazon S3, Cloudflare R2, Google Cloud Storage, Azure Blob or an S3-compatible service. A
              new connection starts Pending, a passing test makes it Active, and you can then make it the default for new uploads.
              Your bucket&apos;s existing contents are never touched.
            </EmptyState>
          )}
          <Capabilities />
        </div>
      )}
    </>
  );
}

function ConnectionTable({ connections, defaultId }: { connections: StorageConnection[]; defaultId: string }) {
  return (
    <Table>
      <thead>
        <tr>
          <Th>Name</Th>
          <Th>Provider</Th>
          <Th>Status</Th>
          <Th>Bucket or container</Th>
          <Th>Default</Th>
          <Th>Last test</Th>
          <Th>Credentials</Th>
        </tr>
      </thead>
      <tbody>
        {connections.map((c) => (
          <tr key={c.id}>
            <Td>
              <Link href={`/app/storage/${c.id}`} className="font-medium underline-offset-2 hover:underline">
                {c.name}
              </Link>
              {c.provider === "PLATFORM_MANAGED" && (
                <div className="mt-1">
                  <StateBadge>Platform-managed</StateBadge>
                </div>
              )}
            </Td>
            <Td>{PROVIDER_LABELS[c.provider]}</Td>
            <Td>
              <StateBadge tone={c.status === "ACTIVE" ? "done" : c.status === "PENDING" ? "attention" : "neutral"}>
                {STATUS_LABEL[c.status]}
              </StateBadge>
            </Td>
            <Td className="mono-data">{c.bucket ?? <span className="text-muted">Not applicable</span>}</Td>
            <Td>{c.is_default || c.id === defaultId ? <StateBadge tone="active">Default</StateBadge> : <span className="text-muted">No</span>}</Td>
            <Td>
              <LastTestCell connection={c} />
            </Td>
            <Td>
              <CredentialCell connection={c} />
            </Td>
          </tr>
        ))}
      </tbody>
    </Table>
  );
}

function LastTestCell({ connection }: { connection: StorageConnection }) {
  if (connection.provider === "PLATFORM_MANAGED") return <span className="text-muted">Not applicable</span>;
  if (!connection.last_tested_at) return <Outcome outcome="not_assessed" label="Not tested" />;
  const failed = Boolean(connection.last_error_category);
  return (
    <div className="flex flex-col gap-0.5">
      <Outcome outcome={failed ? "failed" : "passed"} label={failed ? connection.last_error_category : "Passed"} />
      <span className="text-sm text-muted">
        <Time value={connection.last_tested_at} />
      </span>
    </div>
  );
}

function CredentialCell({ connection }: { connection: StorageConnection }) {
  if (connection.provider === "PLATFORM_MANAGED" || connection.auth_method === "ASSUME_ROLE") {
    return <span className="text-muted">None needed</span>;
  }
  if (!connection.has_credentials) return <span>Not stored</span>;
  return (
    <div className="flex flex-col gap-0.5">
      <span>Stored, version {connection.credential_rotation}</span>
      {connection.credential_rotated_at && (
        <span className="text-sm text-muted">
          Rotated <Time value={connection.credential_rotated_at} />
        </span>
      )}
    </div>
  );
}

function Capabilities() {
  const capabilities = useProviderCapabilities();
  const rows = capabilities.data?.providers ?? [];
  if (capabilities.isPending || capabilities.isError || rows.length === 0) return null;
  return (
    <Panel title="What each provider supports" description="Declared by the platform. A connection test records what your bucket actually allows.">
      <Table>
        <thead>
          <tr>
            <Th>Capability</Th>
            {rows.map((p) => (
              <Th key={p.provider}>{PROVIDER_LABELS[p.provider]}</Th>
            ))}
          </tr>
        </thead>
        <tbody>
          {CAPABILITY_ROWS.map((row) => (
            <tr key={row.label}>
              <Td className="font-medium">{row.label}</Td>
              {rows.map((p) => (
                <Td key={p.provider}>{row.value(p.capabilities)}</Td>
              ))}
            </tr>
          ))}
        </tbody>
      </Table>
    </Panel>
  );
}
