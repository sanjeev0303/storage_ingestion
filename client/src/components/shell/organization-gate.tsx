"use client";

import type { ReactNode } from "react";
import { Button } from "@/components/ui/button";
import { EmptyState } from "@/components/ui/states";
import { PageHeader } from "@/components/ui/page";
import { Panel } from "@/components/ui/panel";
import { useSession } from "@/lib/auth/session";

// Every data screen needs an organization. Users in several must pick one
// first; users in none have nothing to show.
export function OrganizationGate({ children }: { children: ReactNode }) {
  const { organizationId, memberships, switchOrganization, signOut } = useSession();

  if (organizationId) return <>{children}</>;

  if (memberships.length === 0) {
    return (
      <EmptyState
        title="You are not an active member of any organization"
        action={<Button onClick={() => void signOut()}>Sign out</Button>}
      >
        Ask an organization owner to invite you, or ask a platform administrator to reactivate your membership.
      </EmptyState>
    );
  }

  return (
    <>
      <PageHeader title="Choose an organization" description="Everything in the console is scoped to one organization at a time." />
      <Panel>
        <ul className="flex flex-col gap-3">
          {memberships.map((m) => (
            <li key={m.organization_id} className="flex flex-wrap items-center justify-between gap-3">
              <div>
                <p className="font-semibold">{m.organization_name ?? m.organization_id}</p>
                <p className="text-sm text-muted">Role: {m.role_id}</p>
              </div>
              <Button variant="primary" onClick={() => switchOrganization(m.organization_id)}>
                Open
              </Button>
            </li>
          ))}
        </ul>
      </Panel>
    </>
  );
}
