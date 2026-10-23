"use client";

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { Button } from "@/components/ui/button";
import { ConfirmDialog } from "@/components/ui/dialog";
import { Field } from "@/components/ui/field";
import { PasswordInput, PasswordRules } from "@/components/ui/password-field";
import { Panel } from "@/components/ui/panel";
import { SubmitButton } from "@/components/ui/submit-button";
import { EmptyState, ErrorState, SkeletonRows } from "@/components/ui/states";
import { Table, Td, Th } from "@/components/ui/table";
import { Time } from "@/components/ui/time";
import { notify } from "@/components/ui/toast";
import { describeError, isApiError } from "@/lib/api/errors";
import type { Session } from "@/lib/api/types/identity";
import { useSession } from "@/lib/auth/session";
import { accountKey } from "@/lib/query/keys";
import { passwordRules } from "@/lib/validation/password";
import { changePassword, getSessions, revokeSession } from "../api";

export function AccountScreen() {
  const { user, signOut } = useSession();
  return (
    <div className="grid gap-6 lg:grid-cols-3">
      <div className="min-w-0 lg:col-span-2">
        <Sessions />
      </div>
      <div className="flex min-w-0 flex-col gap-6">
        <PasswordForm />
        <Panel title="Sign out" description={`Signed in as ${user.email}.`}>
          <Button onClick={() => void signOut()}>Sign out of this browser</Button>
        </Panel>
      </div>
    </div>
  );
}

function Sessions() {
  const client = useQueryClient();
  const sessions = useQuery({ queryKey: accountKey("sessions"), queryFn: ({ signal }) => getSessions(signal) });
  const [revoking, setRevoking] = useState<Session | null>(null);
  const revoke = useMutation({
    mutationFn: revokeSession,
    onSuccess: () => client.invalidateQueries({ queryKey: accountKey("sessions") }),
  });

  return (
    <Panel
      title="Active sessions"
      description="Every browser or device signed in to your account, newest first. The API does not say which one is this browser."
    >
      {sessions.isPending && <SkeletonRows rows={3} />}
      {sessions.isError && <ErrorState error={sessions.error} onRetry={() => sessions.refetch()} />}
      {sessions.isSuccess && sessions.data.sessions.length === 0 && (
        <EmptyState title="No active sessions">Sessions appear here when you sign in.</EmptyState>
      )}
      {sessions.isSuccess && sessions.data.sessions.length > 0 && (
        <Table>
          <caption className="sr-only">Active sessions</caption>
          <thead>
            <tr>
              <Th>Started</Th>
              <Th>Last used</Th>
              <Th>Device</Th>
              <Th>Expires</Th>
              <Th>Action</Th>
            </tr>
          </thead>
          <tbody>
            {sessions.data.sessions.map((s) => (
              <tr key={s.id}>
                <Td>
                  <Time value={s.created_at} />
                </Td>
                <Td>
                  <Time value={s.last_used_at} />
                </Td>
                <Td className="max-w-xs">
                  <div className="[overflow-wrap:anywhere]">{s.user_agent || "Unknown device"}</div>
                  {s.source_ip && <div className="mono-data text-sm text-muted">{s.source_ip}</div>}
                </Td>
                <Td>
                  <Time value={s.expires_at} />
                </Td>
                <Td>
                  <Button onClick={() => setRevoking(s)}>Revoke</Button>
                </Td>
              </tr>
            ))}
          </tbody>
        </Table>
      )}
      <ConfirmDialog
        open={revoking !== null}
        onOpenChange={(open) => !open && setRevoking(null)}
        title="Revoke this session?"
        description="That device is signed out the next time it contacts the server. If it is this browser, you will be sent to sign in again."
        confirmLabel="Revoke session"
        onConfirm={async () => {
          if (!revoking) return;
          await revoke.mutateAsync(revoking.id);
          notify.success("Session revoked");
        }}
      />
    </Panel>
  );
}

type Errors = { current?: string; next?: string; confirm?: string; form?: string };

// What the server said about a field, shown until the person edits it.
type ServerErrors = Pick<Errors, "current" | "next" | "form">;

function PasswordForm() {
  const client = useQueryClient();
  const { user } = useSession();
  const [current, setCurrent] = useState("");
  const [next, setNext] = useState("");
  const [confirm, setConfirm] = useState("");
  const [serverErrors, setServerErrors] = useState<ServerErrors>({});
  const [done, setDone] = useState(false);
  const change = useMutation({ mutationFn: changePassword });

  const rules = passwordRules(next, { email: user.email, current });
  const errors: Errors = {
    current: serverErrors.current ?? (current ? undefined : "Enter your current password."),
    next: serverErrors.next ?? (rules.every((rule) => rule.met) ? undefined : "The new password does not meet the requirements below."),
    confirm: confirm === next ? undefined : "The two passwords do not match.",
    form: serverErrors.form,
  };
  const valid = !errors.current && !errors.next && !errors.confirm && confirm !== "";

  async function submit(event: FormEvent) {
    event.preventDefault();
    setDone(false);
    setServerErrors({});
    if (!valid) return;

    try {
      await change.mutateAsync({ current_password: current, new_password: next });
      setCurrent("");
      setNext("");
      setConfirm("");
      setDone(true);
      void client.invalidateQueries({ queryKey: accountKey("sessions") });
    } catch (e) {
      if (isApiError(e) && e.code === "invalid_credentials") {
        setServerErrors({ current: "That is not your current password." });
      } else if (isApiError(e) && e.status === 400) {
        setServerErrors({ next: e.message });
      } else {
        setServerErrors({ form: describeError(e) });
      }
    }
  }

  return (
    <Panel title="Change password" description="This signs out every other session. This browser stays signed in.">
      <form onSubmit={submit} className="flex flex-col gap-4" noValidate>
        <Field label="Current password" required error={errors.current}>
          {(p) => (
            <PasswordInput {...p} autoComplete="current-password" value={current} onChange={(e) => { setCurrent(e.target.value); setServerErrors({}); }} />
          )}
        </Field>
        <Field label="New password" required error={errors.next}>
          {(p) => (
            <PasswordInput {...p} autoComplete="new-password" value={next} onChange={(e) => { setNext(e.target.value); setServerErrors({}); }} />
          )}
        </Field>
        <PasswordRules rules={rules} />
        <Field label="Repeat new password" required error={errors.confirm}>
          {(p) => (
            <PasswordInput {...p} autoComplete="new-password" value={confirm} onChange={(e) => setConfirm(e.target.value)} />
          )}
        </Field>
        {serverErrors.form && (
          <p role="alert" className="text-base font-medium">
            {serverErrors.form}
          </p>
        )}
        {done && (
          <p role="status" className="text-base font-medium">
            Password changed. Other sessions were signed out.
          </p>
        )}
        <SubmitButton valid={valid} loading={change.isPending} hint="Enter your current password and a new one that meets the requirements.">
          Change password
        </SubmitButton>
      </form>
    </Panel>
  );
}
