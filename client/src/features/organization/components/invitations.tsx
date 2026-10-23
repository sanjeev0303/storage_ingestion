"use client";

import { useState, type FormEvent } from "react";
import { Button } from "@/components/ui/button";
import { ConfirmDialog } from "@/components/ui/dialog";
import { Field, Input, Select } from "@/components/ui/field";
import { LoadMore } from "@/components/ui/page";
import { Panel } from "@/components/ui/panel";
import { SubmitButton } from "@/components/ui/submit-button";
import { EmptyState, ErrorState, PermissionDenied, SkeletonRows } from "@/components/ui/states";
import { StateBadge } from "@/components/ui/status";
import { Table, Td, Th } from "@/components/ui/table";
import { Time } from "@/components/ui/time";
import { notify } from "@/components/ui/toast";
import { describeError } from "@/lib/api/errors";
import { useSession } from "@/lib/auth/session";
import { cleanEmail, emailError } from "@/lib/utils/forgive";
import { createInvitation, revokeInvitation } from "../api";
import { friendlyError } from "../errors";
import { holdsAll } from "../escalation";
import { useInvitationsQuery, useOrgMutation, useRolesQuery } from "../hooks";
import type { Invitation } from "../types";
import { TokenModal } from "./token-modal";

export function Invitations() {
  const { can, permissions } = useSession();
  const canManage = can("members.manage");
  const invitations = useInvitationsQuery(canManage);
  const roles = useRolesQuery();
  const [created, setCreated] = useState<Invitation | null>(null);
  const [revoking, setRevoking] = useState<Invitation | null>(null);
  const revoke = useOrgMutation(revokeInvitation, ["invitations"]);

  if (!canManage) return <PermissionDenied detail="Listing and creating invitations needs the members.manage permission." />;

  const roleList = roles.data?.roles ?? [];
  const grantable = roleList.filter((r) => holdsAll(permissions, r));
  const roleName = (id: string) => roleList.find((r) => r.id === id)?.name ?? id;

  return (
    <div className="grid gap-6 lg:grid-cols-3">
      <div className="min-w-0 lg:col-span-2">
        {invitations.isPending && <SkeletonRows rows={5} />}
        {invitations.isError && <ErrorState error={invitations.error} onRetry={() => invitations.refetch()} />}
        {invitations.isSuccess && invitations.items.length === 0 && (
          <EmptyState title="No invitations yet">
            Invite a teammate with their email address and a role. The server creates a token you hand over yourself,
            because it sends no email.
          </EmptyState>
        )}
        {invitations.isSuccess && invitations.items.length > 0 && (
          <>
            <Table>
              <caption className="sr-only">Invitations, newest first</caption>
              <thead>
                <tr>
                  <Th>Email</Th>
                  <Th>Role</Th>
                  <Th>Status</Th>
                  <Th>Expires</Th>
                  <Th>Actions</Th>
                </tr>
              </thead>
              <tbody>
                {invitations.items.map((inv) => (
                  <tr key={inv.id}>
                    <Td className="font-medium">{inv.email}</Td>
                    <Td>{roleName(inv.role_id)}</Td>
                    <Td>
                      <StateBadge tone={inv.status === "PENDING" ? "active" : "neutral"}>{inv.status}</StateBadge>
                    </Td>
                    <Td>
                      <Time value={inv.expires_at} />
                    </Td>
                    <Td>
                      {inv.status === "PENDING" && <Button onClick={() => setRevoking(inv)}>Revoke</Button>}
                    </Td>
                  </tr>
                ))}
              </tbody>
            </Table>
            <LoadMore
              shown={invitations.items.length}
              hasNext={Boolean(invitations.hasNextPage)}
              loading={invitations.isFetchingNextPage}
              onLoad={() => invitations.fetchNextPage()}
            />
          </>
        )}
      </div>

      <InviteForm roles={grantable} onCreated={setCreated} />

      <TokenModal title="Invitation created" token={created?.token ?? null} expiresAt={created?.expires_at} onClose={() => setCreated(null)}>
        <p className="text-base">
          No email is sent. Give this token to <span className="font-medium">{created?.email}</span> yourself. They
          enter it on the accept-invitation page.
        </p>
      </TokenModal>

      <ConfirmDialog
        open={revoking !== null}
        onOpenChange={(open) => !open && setRevoking(null)}
        title="Revoke this invitation?"
        description={`The token sent to ${revoking?.email ?? ""} stops working. You can invite them again afterwards.`}
        confirmLabel="Revoke invitation"
        onConfirm={async () => {
          if (!revoking) return;
          try {
            await revoke.mutateAsync(revoking.id);
          } catch (e) {
            throw friendlyError(e);
          }
          notify.success("Invitation revoked");
        }}
      />
    </div>
  );
}

function InviteForm({ roles, onCreated }: { roles: { id: string; name: string }[]; onCreated: (inv: Invitation) => void }) {
  const [email, setEmail] = useState("");
  const [roleId, setRoleId] = useState("");
  const [error, setError] = useState<string | null>(null);
  const create = useOrgMutation(createInvitation, ["invitations"]);
  const chosenRole = roleId || roles.find((r) => r.id === "developer")?.id || roles[0]?.id || "";

  const emailProblem = emailError(email);
  const valid = !emailProblem && chosenRole !== "";

  async function submit(event: FormEvent) {
    event.preventDefault();
    if (!valid) return;
    setError(null);
    try {
      const invitation = await create.mutateAsync({ email: cleanEmail(email), role_id: chosenRole });
      setEmail("");
      onCreated(invitation);
    } catch (e) {
      setError(describeError(e));
    }
  }

  return (
    <Panel title="Invite someone" description="Creates a one-time token. No email is sent.">
      <form onSubmit={submit} noValidate className="flex flex-col gap-4">
        <Field label="Email address" required error={emailProblem || error || undefined}>
          {(p) => (
            <Input {...p} type="email" autoComplete="off" spellCheck={false} value={email} onChange={(e) => setEmail(e.target.value)} required />
          )}
        </Field>
        <Field label="Role" hint="You can only offer roles whose permissions you hold.">
          {(p) => (
            <Select {...p} value={chosenRole} onChange={(e) => setRoleId(e.target.value)}>
              {roles.map((r) => (
                <option key={r.id} value={r.id}>
                  {r.name}
                </option>
              ))}
            </Select>
          )}
        </Field>
        <SubmitButton
          valid={valid}
          loading={create.isPending}
          hint={emailProblem ? "Enter a valid email address to continue." : "Choose a role to continue."}
        >
          Create invitation
        </SubmitButton>
      </form>
    </Panel>
  );
}
