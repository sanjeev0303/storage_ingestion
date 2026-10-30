"use client";

import { useQuery } from "@tanstack/react-query";
import Link from "next/link";
import { Panel } from "@/components/ui/panel";
import { StateBadge } from "@/components/ui/status";
import { Table, Td, Th } from "@/components/ui/table";
import { useOrganizationId } from "@/lib/auth/session";
import { orgKey } from "@/lib/query/keys";
import { formatBytes, formatNumber } from "@/lib/utils/format";
import { getVersionResidency } from "../api";

// Where this snapshot's bytes are, each location as the provider reported
// it, against what the dataset allows.
export function ResidencyPanel({ id, versionId }: { id: string; versionId: string }) {
  const organizationId = useOrganizationId();
  const residency = useQuery({
    queryKey: orgKey(organizationId, "datasets", "residency", id, versionId),
    queryFn: ({ signal }) => getVersionResidency(id, versionId, signal),
    enabled: Boolean(organizationId),
    retry: false,
  });
  const r = residency.data;
  if (!r || r.locations.length === 0) return null;
  const ruled = r.allowed.length > 0 || r.require_customer_key;

  return (
    <Panel
      title="Where the bytes are"
      description={
        ruled
          ? `Allowed: ${r.allowed.length > 0 ? r.allowed.join(", ") : "anywhere"}${r.require_customer_key ? ", with a customer-managed key" : ""}.`
          : "The dataset allows any location."
      }
      actions={ruled && <StateBadge tone={r.inside ? "done" : "failed"}>{r.inside ? "Inside its rules" : "Outside its rules"}</StateBadge>}
    >
      <Table>
        <thead>
          <tr>
            <Th>Location</Th>
            <Th>Connection</Th>
            <Th className="text-right">Members</Th>
            <Th className="text-right">Size</Th>
          </tr>
        </thead>
        <tbody>
          {r.locations.map((l) => (
            <tr key={`${l.connection_id}-${l.location}`}>
              <Td>
                <span className="mono-data">{l.location || "unknown"}</span>
                <span className="block text-sm text-muted">
                  {l.location_verified ? "Confirmed by the provider" : "Declared, not confirmed"}
                  {l.encryption_key ? `; key ${l.encryption_key}` : ""}
                </span>
                {ruled && !l.allowed && <StateBadge tone="failed">Outside</StateBadge>}
              </Td>
              <Td>
                <Link href={`/app/storage/${l.connection_id}`} className="mono-data underline underline-offset-2">
                  {l.connection_id}
                </Link>
              </Td>
              <Td className="text-right tabular-nums">{formatNumber(l.members)}</Td>
              <Td className="text-right tabular-nums">{formatBytes(l.bytes)}</Td>
            </tr>
          ))}
        </tbody>
      </Table>
    </Panel>
  );
}
