"use client";

import { Panel } from "@/components/ui/panel";
import { useSession } from "@/lib/auth/session";
import { useRolesQuery } from "../hooks";

export function YourRole() {
  const { membership, permissions } = useSession();
  const roles = useRolesQuery();
  const roleName = roles.data?.roles.find((r) => r.id === membership?.role_id)?.name ?? membership?.role_id;

  return (
    <Panel title="Your role" description="What you can do in this organization. The server decides on every request.">
      <p className="text-base">
        Role: <span className="font-semibold">{roleName ?? "Unknown"}</span>
      </p>
      {permissions === null ? (
        <p className="mt-3 text-base text-muted">
          Your permissions could not be read, so every action stays visible. The server refuses anything your role does
          not allow.
        </p>
      ) : (
        <ul className="mt-3 flex flex-wrap gap-1.5" aria-label="Your permissions">
          {[...permissions].map((p) => (
            <li key={p} className="mono-data bg-faint rounded-sm px-2 py-0.5 text-xs">
              {p}
            </li>
          ))}
        </ul>
      )}
    </Panel>
  );
}
