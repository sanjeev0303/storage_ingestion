"use client";

import { useQuery } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { Button } from "@/components/ui/button";
import { ConfirmDialog, Modal } from "@/components/ui/dialog";
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
import { cleanEmail, cleanText, cleanWholeNumber, emailError } from "@/lib/utils/forgive";
import {
  createAdminOrganization,
  createAdminUser,
  deactivateAdminUser,
  getAdminQuota,
  issueAdminPasswordReset,
  reactivateAdminUser,
  setAdminOrganizationStatus,
  setAdminQuota,
  setAdminUserPlatformRole,
} from "../api";
import { friendlyError } from "../errors";
import { useAdminMutation, useAdminOrganizationsQuery, useAdminUsersQuery } from "../hooks";
import { LIMIT_KEYS, type CredentialToken, type Organization, type QuotaLimits, type User } from "../types";
import { TokenModal } from "./token-modal";

export function PlatformAdmin() {
  const { isPlatformAdmin } = useSession();
  if (!isPlatformAdmin) return <PermissionDenied detail="Platform administration is for platform admins only." />;
  return (
    <div className="flex flex-col gap-6">
      <Organizations />
      <Users />
    </div>
  );
}

function Organizations() {
  const orgs = useAdminOrganizationsQuery();
  const [creating, setCreating] = useState(false);
  const [statusFor, setStatusFor] = useState<Organization | null>(null);
  const [quotaFor, setQuotaFor] = useState<Organization | null>(null);

  return (
    <Panel
      title="Organizations"
      description="Every organization on the platform, ordered by id."
      actions={<Button onClick={() => setCreating(true)}>Create organization</Button>}
    >
      {orgs.isPending && <SkeletonRows rows={4} />}
      {orgs.isError && <ErrorState error={orgs.error} onRetry={() => orgs.refetch()} />}
      {orgs.isSuccess && orgs.items.length === 0 && (
        <EmptyState title="No organizations">Create one and name an existing user as its owner.</EmptyState>
      )}
      {orgs.isSuccess && orgs.items.length > 0 && (
        <>
          <Table>
            <caption className="sr-only">Organizations</caption>
            <thead>
              <tr>
                <Th>Name</Th>
                <Th>Slug</Th>
                <Th>Status</Th>
                <Th>Created</Th>
                <Th>Actions</Th>
              </tr>
            </thead>
            <tbody>
              {orgs.items.map((org) => (
                <tr key={org.id}>
                  <Td className="font-medium">{org.name}</Td>
                  <Td className="mono-data">{org.slug}</Td>
                  <Td>
                    <StateBadge tone={org.status === "ACTIVE" ? "neutral" : "attention"}>{org.status}</StateBadge>
                  </Td>
                  <Td>
                    <Time value={org.created_at} />
                  </Td>
                  <Td>
                    <div className="flex gap-2">
                      <Button onClick={() => setStatusFor(org)}>Set status</Button>
                      <Button onClick={() => setQuotaFor(org)}>Set quota</Button>
                    </div>
                  </Td>
                </tr>
              ))}
            </tbody>
          </Table>
          <LoadMore
            shown={orgs.items.length}
            hasNext={Boolean(orgs.hasNextPage)}
            loading={orgs.isFetchingNextPage}
            onLoad={() => orgs.fetchNextPage()}
          />
        </>
      )}
      {creating && <CreateOrganization onClose={() => setCreating(false)} />}
      {statusFor && <SetStatus org={statusFor} onClose={() => setStatusFor(null)} />}
      {quotaFor && <SetQuota org={quotaFor} onClose={() => setQuotaFor(null)} />}
    </Panel>
  );
}

const SLUG = /^[a-z0-9-]{3,64}$/;

function slugError(value: string) {
  if (value === "") return "Enter a slug.";
  return SLUG.test(value) ? "" : "Use 3 to 64 lowercase letters, digits and hyphens.";
}

function CreateOrganization({ onClose }: { onClose: () => void }) {
  const { user: me } = useSession();
  const [name, setName] = useState("");
  const [slug, setSlug] = useState("");
  const [owner, setOwner] = useState(me.id);
  const [error, setError] = useState<string | null>(null);
  const create = useAdminMutation(createAdminOrganization, "organizations");

  const cleanName = cleanText(name);
  const cleanSlug = slug.trim().toLowerCase();
  const cleanOwner = owner.replace(/\s+/g, "");
  const nameProblem = cleanName === "" ? "Enter a name for the organization." : "";
  const slugProblem = slugError(cleanSlug);
  const ownerProblem = cleanOwner === "" ? "Enter the id of the owner." : "";
  const valid = !nameProblem && !slugProblem && !ownerProblem;

  async function submit(event: FormEvent) {
    event.preventDefault();
    if (!valid) return;
    setError(null);
    try {
      await create.mutateAsync({ name: cleanName, slug: cleanSlug, owner_user_id: cleanOwner });
      notify.success("Organization created");
      onClose();
    } catch (e) {
      setError(describeError(e));
    }
  }

  return (
    <Modal open onOpenChange={(open) => !open && onClose()} title="Create organization">
      <form onSubmit={submit} noValidate className="flex flex-col gap-4">
        <Field label="Name" required error={nameProblem}>
          {(p) => <Input {...p} value={name} onChange={(e) => setName(e.target.value)} />}
        </Field>
        <Field label="Slug" required hint="3 to 64 characters: lowercase letters, digits and hyphens." error={slugProblem}>
          {(p) => <Input {...p} maxLength={64} value={slug} onChange={(e) => setSlug(e.target.value)} />}
        </Field>
        <Field
          label="Owner user id"
          required
          hint="The id of an existing user. Starts as your own id. Find others in the users list below."
          error={ownerProblem}
        >
          {(p) => <Input {...p} className="mono-data" value={owner} onChange={(e) => setOwner(e.target.value)} />}
        </Field>
        {error && (
          <p role="alert" className="text-base font-medium">
            {error}
          </p>
        )}
        <div className="flex justify-end gap-2">
          <Button onClick={onClose}>Cancel</Button>
          <SubmitButton valid={valid} loading={create.isPending} hint={nameProblem || slugProblem || ownerProblem}>
            Create organization
          </SubmitButton>
        </div>
      </form>
    </Modal>
  );
}

const STATUSES = ["ACTIVE", "SUSPENDED", "DEACTIVATED"];

function SetStatus({ org, onClose }: { org: Organization; onClose: () => void }) {
  const [status, setStatus] = useState<string>(STATUSES.includes(org.status) ? org.status : "ACTIVE");
  const set = useAdminMutation(() => setAdminOrganizationStatus(org.id, status), "organizations");
  return (
    <ConfirmDialog
      open
      onOpenChange={(open) => !open && onClose()}
      title={`Change status of ${org.name}`}
      description={
        <div className="flex flex-col gap-3">
          <p>Suspended and deactivated organizations refuse every member request until set back to active.</p>
          <Select aria-label="New status" value={status} onChange={(e) => setStatus(e.target.value)}>
            {STATUSES.map((s) => (
              <option key={s}>{s}</option>
            ))}
          </Select>
        </div>
      }
      confirmLabel="Set status"
      onConfirm={async () => {
        try {
          await set.mutateAsync(undefined);
        } catch (e) {
          throw friendlyError(e);
        }
        notify.success(`${org.name} is now ${status.toLowerCase()}`);
      }}
    />
  );
}

function SetQuota({ org, onClose }: { org: Organization; onClose: () => void }) {
  const quota = useQuery({
    queryKey: ["account", "admin", "quota", org.id],
    queryFn: ({ signal }) => getAdminQuota(org.id, signal),
    gcTime: 0,
  });

  return (
    <Modal
      open
      onOpenChange={(open) => !open && onClose()}
      title={`Quota for ${org.name}`}
      description="Saving replaces all overrides with the values below. A value of 0 falls back to the platform default."
    >
      {quota.isPending && <SkeletonRows rows={4} />}
      {quota.isError && <ErrorState error={quota.error} onRetry={() => quota.refetch()} />}
      {quota.data && <QuotaForm org={org} limits={quota.data.limits} onClose={onClose} />}
    </Modal>
  );
}

function QuotaForm({ org, limits, onClose }: { org: Organization; limits: QuotaLimits; onClose: () => void }) {
  const [values, setValues] = useState(() => Object.fromEntries(LIMIT_KEYS.map((k) => [k, String(limits[k])])));
  const [error, setError] = useState<string | null>(null);
  const save = useAdminMutation((body: Record<string, number>) => setAdminQuota(org.id, body), "organizations");

  const problems = Object.fromEntries(
    LIMIT_KEYS.map((key) => [key, cleanWholeNumber(values[key]) === "" ? "Enter a whole number, zero or more." : ""]),
  );
  const changed = LIMIT_KEYS.some((key) => Number(cleanWholeNumber(values[key])) !== limits[key]);
  const valid = LIMIT_KEYS.every((key) => !problems[key]) && changed;

  async function submit(event: FormEvent) {
    event.preventDefault();
    if (!valid) return;
    setError(null);
    const body: Record<string, number> = {};
    for (const key of LIMIT_KEYS) body[key] = Number(cleanWholeNumber(values[key]));
    try {
      await save.mutateAsync(body);
      notify.success("Quota saved");
      onClose();
    } catch (e) {
      setError(describeError(e));
    }
  }

  return (
    <form onSubmit={submit} noValidate className="flex flex-col gap-4">
      <div className="grid gap-3 sm:grid-cols-2">
        {LIMIT_KEYS.map((key) => (
          <Field key={key} label={key} error={problems[key]}>
            {(p) => (
              <Input
                {...p}
                inputMode="numeric"
                className="mono-data"
                value={values[key]}
                onChange={(e) => setValues({ ...values, [key]: e.target.value })}
              />
            )}
          </Field>
        ))}
      </div>
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
          hint={LIMIT_KEYS.some((key) => problems[key]) ? "Every limit needs a whole number." : "Change a limit to save."}
        >
          Save quota
        </SubmitButton>
      </div>
    </form>
  );
}

function Users() {
  const { user: me } = useSession();
  const users = useAdminUsersQuery();
  const [creating, setCreating] = useState(false);
  const [token, setToken] = useState<{ title: string; value: CredentialToken; note: string } | null>(null);
  const [deactivating, setDeactivating] = useState<User | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);
  const reactivate = useAdminMutation(reactivateAdminUser, "users");
  const deactivate = useAdminMutation(deactivateAdminUser, "users");
  const setRole = useAdminMutation((v: { id: string; role: string }) => setAdminUserPlatformRole(v.id, v.role), "users");
  const reset = useAdminMutation(issueAdminPasswordReset, "users");

  async function run(action: () => Promise<unknown>, done?: string) {
    setActionError(null);
    try {
      await action();
      if (done) notify.success(done);
    } catch (e) {
      setActionError(describeError(e));
    }
  }

  return (
    <Panel
      title="Users"
      description="Platform accounts. Creating a user returns an activation token to hand over; no email is sent."
      actions={<Button onClick={() => setCreating(true)}>Create user</Button>}
    >
      {actionError && (
        <p role="alert" className="neo-in-sm mb-4 p-3 text-base font-medium">
          {actionError}
        </p>
      )}
      {users.isPending && <SkeletonRows rows={4} />}
      {users.isError && <ErrorState error={users.error} onRetry={() => users.refetch()} />}
      {users.isSuccess && users.items.length === 0 && (
        <EmptyState title="No users">Create the first user to get an activation token.</EmptyState>
      )}
      {users.isSuccess && users.items.length > 0 && (
        <>
          <Table>
            <caption className="sr-only">Users</caption>
            <thead>
              <tr>
                <Th>User</Th>
                <Th>Status</Th>
                <Th>Platform role</Th>
                <Th>Last sign-in</Th>
                <Th>Actions</Th>
              </tr>
            </thead>
            <tbody>
              {users.items.map((u) => (
                <tr key={u.id}>
                  <Td>
                    <div className="font-medium">{u.display_name || u.email}</div>
                    <div className="text-sm text-muted">{u.display_name ? u.email : null}</div>
                    <div className="mono-data text-xs text-muted">{u.id}</div>
                  </Td>
                  <Td>
                    <StateBadge tone={u.status === "ACTIVE" ? "neutral" : "attention"}>{u.status}</StateBadge>
                  </Td>
                  <Td>
                    <Select
                      aria-label={`Platform role for ${u.email}`}
                      value={u.platform_role ?? ""}
                      disabled={u.id === me.id}
                      onChange={(e) => run(() => setRole.mutateAsync({ id: u.id, role: e.target.value }), "Platform role changed")}
                    >
                      <option value="">None</option>
                      <option value="ADMIN">ADMIN</option>
                      <option value="AUDITOR">AUDITOR</option>
                    </Select>
                  </Td>
                  <Td>
                    <Time value={u.last_login_at} />
                  </Td>
                  <Td>
                    <div className="flex flex-wrap gap-2">
                      {u.status === "DEACTIVATED" ? (
                        <Button onClick={() => run(() => reactivate.mutateAsync(u.id), "User reactivated")}>Reactivate</Button>
                      ) : (
                        <>
                          <Button
                            onClick={() =>
                              run(async () => {
                                const value = await reset.mutateAsync(u.id);
                                setToken({
                                  title: "Password reset token",
                                  value,
                                  note: `Give this to ${u.email}. It works once. A user who never set a password gets an activation token instead.`,
                                });
                              })
                            }
                          >
                            Reset password
                          </Button>
                          {u.id !== me.id && <Button onClick={() => setDeactivating(u)}>Deactivate</Button>}
                        </>
                      )}
                    </div>
                  </Td>
                </tr>
              ))}
            </tbody>
          </Table>
          <LoadMore
            shown={users.items.length}
            hasNext={Boolean(users.hasNextPage)}
            loading={users.isFetchingNextPage}
            onLoad={() => users.fetchNextPage()}
          />
        </>
      )}

      {creating && (
        <CreateUser
          onClose={() => setCreating(false)}
          onCreated={(email, value) => {
            setCreating(false);
            setToken({ title: "User created", value, note: `Give this activation token to ${email}. It works once, for 72 hours.` });
          }}
        />
      )}
      <TokenModal
        title={token?.title ?? ""}
        token={token?.value.token ?? null}
        expiresAt={token?.value.expires_at}
        onClose={() => setToken(null)}
      >
        <p className="text-base">{token?.note}</p>
      </TokenModal>
      <ConfirmDialog
        open={deactivating !== null}
        onOpenChange={(open) => !open && setDeactivating(null)}
        title={`Deactivate ${deactivating?.email}?`}
        description="All their sessions end now and they cannot sign in until reactivated. The last active platform admin cannot be deactivated."
        confirmLabel="Deactivate user"
        onConfirm={async () => {
          if (!deactivating) return;
          try {
            await deactivate.mutateAsync(deactivating.id);
          } catch (e) {
            throw friendlyError(e);
          }
          notify.success("User deactivated");
        }}
      />
    </Panel>
  );
}

function CreateUser({ onClose, onCreated }: { onClose: () => void; onCreated: (email: string, token: CredentialToken) => void }) {
  const [email, setEmail] = useState("");
  const [displayName, setDisplayName] = useState("");
  const [role, setRole] = useState("");
  const [error, setError] = useState<string | null>(null);
  const create = useAdminMutation(createAdminUser, "users");

  const emailProblem = emailError(email);

  async function submit(event: FormEvent) {
    event.preventDefault();
    if (emailProblem) return;
    setError(null);
    try {
      const body: { email: string; display_name?: string; platform_role?: string } = { email: cleanEmail(email) };
      if (cleanText(displayName)) body.display_name = cleanText(displayName);
      if (role) body.platform_role = role;
      const res = await create.mutateAsync(body);
      onCreated(res.user.email, res.activation);
    } catch (e) {
      setError(describeError(e));
    }
  }

  return (
    <Modal open onOpenChange={(open) => !open && onClose()} title="Create user">
      <form onSubmit={submit} noValidate className="flex flex-col gap-4">
        <Field label="Email address" required error={emailProblem}>
          {(p) => <Input {...p} type="email" autoComplete="off" spellCheck={false} value={email} onChange={(e) => setEmail(e.target.value)} />}
        </Field>
        <Field label="Display name">{(p) => <Input {...p} value={displayName} onChange={(e) => setDisplayName(e.target.value)} />}</Field>
        <Field label="Platform role" hint="Most users need none. Organization roles are set per organization.">
          {(p) => (
            <Select {...p} value={role} onChange={(e) => setRole(e.target.value)}>
              <option value="">None</option>
              <option value="ADMIN">ADMIN</option>
              <option value="AUDITOR">AUDITOR</option>
            </Select>
          )}
        </Field>
        {error && (
          <p role="alert" className="text-base font-medium">
            {error}
          </p>
        )}
        <div className="flex justify-end gap-2">
          <Button onClick={onClose}>Cancel</Button>
          <SubmitButton valid={!emailProblem} loading={create.isPending} hint="Enter a valid email address to continue.">
            Create user
          </SubmitButton>
        </div>
      </form>
    </Modal>
  );
}
