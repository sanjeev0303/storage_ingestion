"use client";

import { useQueryClient } from "@tanstack/react-query";
import { useRouter } from "next/navigation";
import { useState } from "react";
import { Button } from "@/components/ui/button";
import { ConfirmDialog } from "@/components/ui/dialog";
import { Select } from "@/components/ui/field";
import { LoadMore } from "@/components/ui/page";
import { EmptyState, ErrorState, SkeletonRows } from "@/components/ui/states";
import { StateBadge } from "@/components/ui/status";
import { Table, Td, Th } from "@/components/ui/table";
import { Time } from "@/components/ui/time";
import { notify } from "@/components/ui/toast";
import { useSession } from "@/lib/auth/session";
import { accountKey } from "@/lib/query/keys";
import { changeMemberRole, reactivateMember, removeMember, suspendMember } from "../api";
import { describeMemberError, friendlyError } from "../errors";
import { holdsAll } from "../escalation";
import { useMembersQuery, useOrgMutation, useRolesQuery } from "../hooks";
import type { Membership, Role } from "../types";
import { YourRole } from "./your-role";

export function Members() {
  const { user, can, permissions } = useSession();
  const client = useQueryClient();
  const router = useRouter();
  const members = useMembersQuery();
  const roles = useRolesQuery();
  const [actionError, setActionError] = useState<string | null>(null);
  const [removing, setRemoving] = useState<Membership | null>(null);

  const changeRole = useOrgMutation((v: { userId: string; roleId: string }) => changeMemberRole(v.userId, v.roleId), ["members"]);
  const suspend = useOrgMutation(suspendMember, ["members"]);
  const reactivate = useOrgMutation(reactivateMember, ["members"]);
  const remove = useOrgMutation(removeMember, ["members"]);

  const roleList = roles.data?.roles ?? [];
  const canManage = can("members.manage");

  async function run(action: () => Promise<unknown>, done: string) {
    setActionError(null);
    try {
      await action();
      notify.success(done);
    } catch (e) {
      setActionError(describeMemberError(e));
    }
  }

  if (members.isPending) return <SkeletonRows rows={6} />;
  if (members.isError) return <ErrorState error={members.error} onRetry={() => members.refetch()} />;

  const leaving = removing?.user_id === user.id;

  return (
    <div className="flex flex-col gap-6">
      {actionError && (
        <p role="alert" className="neo-in-sm p-3 text-base font-medium">
          {actionError}
        </p>
      )}
      {members.items.length === 0 ? (
        <EmptyState title="No members yet">Members appear here after they accept an invitation. Invite someone from the Invitations tab.</EmptyState>
      ) : (
        <>
          <Table aria-label="Members">
            <caption className="sr-only">Organization members</caption>
            <thead>
              <tr>
                <Th>Member</Th>
                <Th>Role</Th>
                <Th>Status</Th>
                <Th>Joined</Th>
                <Th>Actions</Th>
              </tr>
            </thead>
            <tbody>
              {members.items.map((m) => {
                const isSelf = m.user_id === user.id;
                const targetRole = roleList.find((r) => r.id === m.role_id);
                const manageable = canManage && !isSelf && holdsAll(permissions, targetRole);
                return (
                  <tr key={m.user_id}>
                    <Td>
                      <div className="font-medium">{m.display_name || m.email}</div>
                      {m.display_name && <div className="text-sm text-muted">{m.email}</div>}
                      {isSelf && <div className="text-sm text-muted">You</div>}
                    </Td>
                    <Td>
                      <RoleSelect
                        member={m}
                        roles={roleList}
                        disabled={!manageable || changeRole.isPending}
                        permissions={permissions}
                        onChange={(roleId) =>
                          run(() => changeRole.mutateAsync({ userId: m.user_id, roleId }), "Role changed")
                        }
                      />
                    </Td>
                    <Td>
                      <StateBadge tone={m.status === "ACTIVE" ? "neutral" : "attention"}>{m.status}</StateBadge>
                    </Td>
                    <Td>
                      <Time value={m.created_at} />
                    </Td>
                    <Td>
                      <div className="flex flex-wrap gap-2">
                        {manageable && m.status === "ACTIVE" && (
                          <Button onClick={() => run(() => suspend.mutateAsync(m.user_id), "Member suspended")}>
                            Suspend
                          </Button>
                        )}
                        {manageable && m.status === "SUSPENDED" && (
                          <Button onClick={() => run(() => reactivate.mutateAsync(m.user_id), "Member reactivated")}>
                            Reactivate
                          </Button>
                        )}
                        {manageable && <Button onClick={() => setRemoving(m)}>Remove</Button>}
                        {isSelf && <Button onClick={() => setRemoving(m)}>Leave organization</Button>}
                      </div>
                    </Td>
                  </tr>
                );
              })}
            </tbody>
          </Table>
          <LoadMore
            shown={members.items.length}
            hasNext={Boolean(members.hasNextPage)}
            loading={members.isFetchingNextPage}
            onLoad={() => members.fetchNextPage()}
          />
        </>
      )}
      <YourRole />
      <ConfirmDialog
        open={removing !== null}
        onOpenChange={(open) => !open && setRemoving(null)}
        title={leaving ? "Leave this organization?" : `Remove ${removing?.display_name || removing?.email}?`}
        description={
          leaving
            ? "You lose access to its data straight away. Only another admin can invite you back. The last owner cannot leave."
            : "They lose access to this organization straight away. Their account is not deleted."
        }
        confirmLabel={leaving ? "Leave organization" : "Remove member"}
        onConfirm={async () => {
          if (!removing) return;
          try {
            await remove.mutateAsync(removing.user_id);
          } catch (e) {
            throw friendlyError(e);
          }
          if (leaving) {
            await client.invalidateQueries({ queryKey: accountKey("me") });
            router.push("/app/overview");
          }
        }}
      />
    </div>
  );
}

function RoleSelect({
  member,
  roles,
  permissions,
  disabled,
  onChange,
}: {
  member: Membership;
  roles: Role[];
  permissions: ReadonlySet<string> | null;
  disabled: boolean;
  onChange: (roleId: string) => void;
}) {
  const options = roles.filter((r) => r.id === member.role_id || holdsAll(permissions, r));
  const label = `Role for ${member.display_name || member.email}`;
  if (options.length === 0) return <span>{member.role_id}</span>;
  return (
    <Select
      aria-label={label}
      value={member.role_id}
      disabled={disabled}
      onChange={(e) => onChange(e.target.value)}
      className="min-w-36"
    >
      {options.map((r) => (
        <option key={r.id} value={r.id}>
          {r.name}
        </option>
      ))}
    </Select>
  );
}
