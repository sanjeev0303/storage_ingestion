"use client";

import { useState, type FormEvent } from "react";
import { Button } from "@/components/ui/button";
import { ConfirmDialog, Modal } from "@/components/ui/dialog";
import { Checkbox, Field, Input, Textarea } from "@/components/ui/field";
import { SubmitButton } from "@/components/ui/submit-button";
import { EmptyState, ErrorState, SkeletonRows } from "@/components/ui/states";
import { StateBadge } from "@/components/ui/status";
import { Table, Td, Th } from "@/components/ui/table";
import { notify } from "@/components/ui/toast";
import { describeError } from "@/lib/api/errors";
import { useSession } from "@/lib/auth/session";
import { PERMISSIONS } from "@/lib/permissions";
import { cleanText } from "@/lib/utils/forgive";
import { createRole, deleteRole, updateRole, type RoleInput } from "../api";
import { friendlyError } from "../errors";
import { useOrgMutation, useRolesQuery } from "../hooks";
import type { Role } from "../types";

// Custom roles can never carry ownership.
const GRANTABLE = PERMISSIONS.filter((p) => p !== "org.ownership");

export function Roles() {
  const { can } = useSession();
  const canManage = can("roles.manage");
  const roles = useRolesQuery();
  const [editing, setEditing] = useState<Role | "new" | null>(null);
  const [deleting, setDeleting] = useState<Role | null>(null);
  const remove = useOrgMutation(deleteRole, ["roles"]);

  if (roles.isPending) return <SkeletonRows rows={5} />;
  if (roles.isError) return <ErrorState error={roles.error} onRetry={() => roles.refetch()} />;

  const list = roles.data.roles;
  const hasCustom = list.some((r) => !r.built_in);

  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <p className="max-w-prose text-base text-muted">
          Built-in roles are fixed. Create a custom role to give a smaller or different set of permissions.
        </p>
        {canManage && (
          <Button variant="primary" onClick={() => setEditing("new")}>
            Create role
          </Button>
        )}
      </div>

      <Table>
        <caption className="sr-only">Roles and their permissions</caption>
        <thead>
          <tr>
            <Th>Role</Th>
            <Th>Permissions</Th>
            <Th>Actions</Th>
          </tr>
        </thead>
        <tbody>
          {list.map((role) => (
            <tr key={role.id}>
              <Td className="min-w-48">
                <div className="flex items-center gap-2 font-medium">
                  {role.name}
                  <StateBadge>{role.built_in ? "Built in" : "Custom"}</StateBadge>
                </div>
                {role.description && <div className="mt-1 max-w-xs text-sm text-muted">{role.description}</div>}
              </Td>
              <Td>
                <details>
                  <summary className="cursor-pointer text-base">{role.permissions.length} permissions</summary>
                  <ul className="mt-2 flex max-w-xl flex-wrap gap-1.5">
                    {role.permissions.map((p) => (
                      <li key={p} className="mono-data bg-faint rounded-sm px-2 py-0.5 text-xs">
                        {p}
                      </li>
                    ))}
                  </ul>
                </details>
              </Td>
              <Td>
                {!role.built_in && canManage && (
                  <div className="flex gap-2">
                    <Button onClick={() => setEditing(role)}>Edit</Button>
                    <Button onClick={() => setDeleting(role)}>Delete</Button>
                  </div>
                )}
              </Td>
            </tr>
          ))}
        </tbody>
      </Table>
      {!hasCustom && (
        <EmptyState title="No custom roles">
          Built-in roles cover most teams. Add a custom role when someone needs, for example, to read audit logs without
          managing members.
        </EmptyState>
      )}

      {editing && (
        <RoleForm
          key={editing === "new" ? "new" : editing.id}
          role={editing === "new" ? null : editing}
          onClose={() => setEditing(null)}
        />
      )}

      <ConfirmDialog
        open={deleting !== null}
        onOpenChange={(open) => !open && setDeleting(null)}
        title={`Delete the ${deleting?.name} role?`}
        description="Roles that members or pending invitations still use cannot be deleted. Reassign them first."
        confirmLabel="Delete role"
        onConfirm={async () => {
          if (!deleting) return;
          try {
            await remove.mutateAsync(deleting.id);
          } catch (e) {
            throw friendlyError(e);
          }
          notify.success("Role deleted");
        }}
      />
    </div>
  );
}

function RoleForm({ role, onClose }: { role: Role | null; onClose: () => void }) {
  const { permissions: mine } = useSession();
  const [name, setName] = useState(role?.name ?? "");
  const [description, setDescription] = useState(role?.description ?? "");
  const [chosen, setChosen] = useState(new Set(role?.permissions ?? []));
  const [error, setError] = useState<string | null>(null);
  const save = useOrgMutation((input: RoleInput) => (role ? updateRole(role.id, input) : createRole(input)), ["roles"]);

  function toggle(permission: string, on: boolean) {
    const next = new Set(chosen);
    if (on) next.add(permission);
    else next.delete(permission);
    setChosen(next);
  }

  const cleanName = cleanText(name);
  const nameProblem = cleanName === "" ? "Enter a name for the role." : "";
  const noPermissions = chosen.size === 0;
  const changed =
    !role ||
    cleanName !== role.name ||
    description.trim() !== (role.description ?? "") ||
    chosen.size !== role.permissions.length ||
    role.permissions.some((p) => !chosen.has(p));
  const valid = !nameProblem && !noPermissions && changed;

  async function submit(event: FormEvent) {
    event.preventDefault();
    if (!valid) return;
    setError(null);
    const input: RoleInput = { name: cleanName, permissions: GRANTABLE.filter((p) => chosen.has(p)) };
    if (description.trim()) input.description = description.trim();
    try {
      await save.mutateAsync(input);
      notify.success(role ? "Role updated" : "Role created");
      onClose();
    } catch (e) {
      setError(describeError(e));
    }
  }

  return (
    <Modal
      open
      onOpenChange={(open) => !open && onClose()}
      title={role ? `Edit ${role.name}` : "Create role"}
      description="Saving replaces the name, description and permissions of this role."
    >
      <form onSubmit={submit} noValidate className="flex flex-col gap-4">
        <Field label="Name" required hint="1 to 64 characters." error={nameProblem}>
          {(p) => <Input {...p} value={name} maxLength={64} onChange={(e) => setName(e.target.value)} />}
        </Field>
        <Field label="Description" hint="Optional, up to 500 characters.">
          {(p) => <Textarea {...p} value={description} maxLength={500} onChange={(e) => setDescription(e.target.value)} />}
        </Field>
        <fieldset className="flex flex-col gap-2">
          <legend className="text-sm font-medium">Permissions (choose at least one)</legend>
          <p className="text-sm text-muted">
            You can only grant permissions you hold. Ownership cannot be given to a custom role.
          </p>
          <div className="neo-in-sm grid gap-2 p-3 sm:grid-cols-2">
            {GRANTABLE.map((p) => (
              <Checkbox
                key={p}
                label={<span className="mono-data text-sm">{p}</span>}
                checked={chosen.has(p)}
                disabled={Boolean(mine) && !mine?.has(p)}
                onChange={(e) => toggle(p, e.target.checked)}
              />
            ))}
          </div>
        </fieldset>
        {error && (
          <p role="alert" className="text-base font-medium">
            {error}
          </p>
        )}
        <div className="flex justify-end gap-2">
          <Button onClick={onClose}>Cancel</Button>
          <SubmitButton
            valid={valid}
            loading={save.isPending}
            hint={nameProblem || (noPermissions ? "Choose at least one permission." : "Change something to save.")}
          >
            {role ? "Save role" : "Create role"}
          </SubmitButton>
        </div>
      </form>
    </Modal>
  );
}
