"use client";

import { useQuery } from "@tanstack/react-query";
import { Panel } from "@/components/ui/panel";
import { StateBadge } from "@/components/ui/status";
import { Time } from "@/components/ui/time";
import { useOrganizationId } from "@/lib/auth/session";
import { orgKey } from "@/lib/query/keys";
import { getVersionIntegrity } from "../api";

const KIND_TEXT: Record<string, string> = {
  OVERWRITTEN: "Replaced",
  DELETED: "Deleted",
  NEW_VERSION: "New version on top",
  UNREADABLE: "Unreadable",
};

// Shown only when a member's stored object changed outside the platform
// after the snapshot pinned it; an intact snapshot shows nothing.
export function IntegrityNotice({ id, versionId }: { id: string; versionId: string }) {
  const organizationId = useOrganizationId();
  const integrity = useQuery({
    queryKey: orgKey(organizationId, "datasets", "integrity", id, versionId),
    queryFn: ({ signal }) => getVersionIntegrity(id, versionId, signal),
    enabled: Boolean(organizationId),
    retry: false,
  });
  if (!integrity.data?.degraded) return null;

  const objects = integrity.data.objects;
  return (
    <Panel
      title="Degraded: members changed outside the platform"
      description="The storage scan found these members' stored objects changed after this snapshot pinned them. Check them against their SHA-256 and restore the pinned versions."
      actions={<StateBadge tone="failed">{`${objects.length} ${objects.length === 1 ? "member" : "members"}`}</StateBadge>}
    >
      <ul className="flex flex-col gap-2">
        {objects.map((o) => (
          <li key={`${o.path}-${o.kind}`} className="grid gap-1 sm:grid-cols-[11rem_1fr] sm:gap-3">
            <span className="font-medium">{KIND_TEXT[o.kind] ?? o.kind}</span>
            <span className="min-w-0">
              <span className="mono-data block [overflow-wrap:anywhere]">{o.path}</span>
              <span className="block text-sm text-muted">
                {o.detail}. Found <Time value={o.detected_at} />
              </span>
            </span>
          </li>
        ))}
      </ul>
    </Panel>
  );
}
