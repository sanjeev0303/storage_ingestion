"use client";

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { Button } from "@/components/ui/button";
import { ConfirmDialog } from "@/components/ui/dialog";
import { Field, Input, Select } from "@/components/ui/field";
import { Panel } from "@/components/ui/panel";
import { EmptyState, ErrorState, SkeletonRows } from "@/components/ui/states";
import { StateBadge } from "@/components/ui/status";
import { SubmitButton } from "@/components/ui/submit-button";
import { Table, Td, Th } from "@/components/ui/table";
import { Time } from "@/components/ui/time";
import { notify } from "@/components/ui/toast";
import { useOrganizationId, useSession } from "@/lib/auth/session";
import { orgKey } from "@/lib/query/keys";
import { formatNumber } from "@/lib/utils/format";
import { addIntegration, deleteIntegration, listIntegrations, listShares, revokeShare, type ChatKind, type ChatShare } from "../api";

function shareState(s: ChatShare) {
  if (s.revoked_at) return <StateBadge>Revoked</StateBadge>;
  if (new Date(s.expires_at) <= new Date()) return <StateBadge>Expired</StateBadge>;
  return <StateBadge tone="done">Live</StateBadge>;
}

// Chat channels files can be shared to, and what was shared. A message holds
// a link for signed-in members, so a chat export holds only dead links.
export function ChatSettings() {
  const organizationId = useOrganizationId();
  const { can } = useSession();
  const client = useQueryClient();
  const refresh = () => client.invalidateQueries({ queryKey: orgKey(organizationId, "chat") });
  const integrations = useQuery({
    queryKey: orgKey(organizationId, "chat", "integrations"),
    queryFn: ({ signal }) => listIntegrations(signal),
    enabled: Boolean(organizationId),
  });
  const shares = useQuery({
    queryKey: orgKey(organizationId, "chat", "shares"),
    queryFn: ({ signal }) => listShares(signal),
    enabled: Boolean(organizationId),
  });
  const [form, setForm] = useState({ kind: "SLACK" as ChatKind, name: "", url: "" });
  const [removing, setRemoving] = useState<string | null>(null);
  const add = useMutation({
    mutationFn: () => addIntegration({ kind: form.kind, name: form.name.trim(), url: form.url.trim() }),
    onSuccess: () => {
      notify.success("Channel added");
      setForm({ kind: form.kind, name: "", url: "" });
      refresh();
    },
    onError: notify.error,
  });
  const revoke = useMutation({
    mutationFn: revokeShare,
    onSuccess: () => {
      notify.success("Link revoked");
      refresh();
    },
    onError: notify.error,
  });
  const valid = form.name.trim() !== "" && form.url.trim().startsWith("https://");

  function submit(e: FormEvent) {
    e.preventDefault();
    if (valid) add.mutate();
  }

  const list = integrations.data?.integrations ?? [];
  return (
    <div className="flex flex-col gap-6">
      <Panel title="Chat channels" description="Slack or Teams incoming webhooks. The webhook URL is sealed and never shown again.">
        <div className="flex flex-col gap-5">
          {integrations.isPending ? (
            <SkeletonRows rows={2} />
          ) : integrations.isError ? (
            <ErrorState error={integrations.error} onRetry={() => integrations.refetch()} />
          ) : list.length === 0 ? (
            <EmptyState title="No channels yet">Add a channel, then share files to it from their page in the catalog.</EmptyState>
          ) : (
            <ul className="flex flex-col gap-2">
              {list.map((c) => (
                <li key={c.id} className="flex flex-wrap items-center justify-between gap-2">
                  <span>
                    <span className="font-medium">{c.name}</span>{" "}
                    <span className="text-sm text-muted">
                      {c.kind === "SLACK" ? "Slack" : "Teams"}, {c.url_host}
                    </span>
                  </span>
                  {can("org.manage") && (
                    <Button variant="ghost" onClick={() => setRemoving(c.id)}>
                      Remove
                    </Button>
                  )}
                </li>
              ))}
            </ul>
          )}
          {can("org.manage") && (
            <form onSubmit={submit} className="grid gap-4 sm:grid-cols-3">
              <Field label="Service">
                {(props) => (
                  <Select {...props} value={form.kind} onChange={(e) => setForm({ ...form, kind: e.target.value as ChatKind })}>
                    <option value="SLACK">Slack</option>
                    <option value="TEAMS">Teams</option>
                  </Select>
                )}
              </Field>
              <Field label="Name" hint="For example #data-review" required>
                {(props) => <Input {...props} maxLength={100} value={form.name} onChange={(e) => setForm({ ...form, name: e.target.value })} />}
              </Field>
              <Field label="Webhook URL" required>
                {(props) => (
                  <Input {...props} type="url" autoComplete="off" spellCheck={false} value={form.url} onChange={(e) => setForm({ ...form, url: e.target.value })} />
                )}
              </Field>
              <div className="sm:col-span-3">
                <SubmitButton valid={valid} loading={add.isPending} hint="Name the channel and give its https webhook URL.">
                  Add channel
                </SubmitButton>
              </div>
            </form>
          )}
        </div>
      </Panel>
      <Panel title="Shared files" description="Each link needs a signed-in member with the right to read the file, and stops working at its expiry.">
        {shares.isPending ? (
          <SkeletonRows rows={3} />
        ) : shares.isError ? (
          <ErrorState error={shares.error} onRetry={() => shares.refetch()} />
        ) : (shares.data?.shares.length ?? 0) === 0 ? (
          <EmptyState title="Nothing shared yet">Files shared to a channel appear here.</EmptyState>
        ) : (
          <Table>
            <thead>
              <tr>
                <Th>File</Th>
                <Th>State</Th>
                <Th>Opens</Th>
                <Th>Expires</Th>
                <Th>
                  <span className="sr-only">Actions</span>
                </Th>
              </tr>
            </thead>
            <tbody>
              {shares.data?.shares.map((s) => (
                <tr key={s.id}>
                  <Td className="mono-data text-sm break-all">{s.path}</Td>
                  <Td>{shareState(s)}</Td>
                  <Td className="tabular-nums">{formatNumber(s.opens)}</Td>
                  <Td>
                    <Time value={s.expires_at} />
                  </Td>
                  <Td>
                    {!s.revoked_at && new Date(s.expires_at) > new Date() && (
                      <Button variant="ghost" onClick={() => revoke.mutate(s.id)} loading={revoke.isPending && revoke.variables === s.id}>
                        Revoke
                      </Button>
                    )}
                  </Td>
                </tr>
              ))}
            </tbody>
          </Table>
        )}
      </Panel>
      <ConfirmDialog
        open={removing !== null}
        onOpenChange={(open) => !open && setRemoving(null)}
        title="Remove this channel?"
        description="Links already posted to it stop working."
        confirmLabel="Remove channel"
        onConfirm={async () => {
          await deleteIntegration(removing ?? "");
          notify.success("Channel removed");
          refresh();
        }}
      />
    </div>
  );
}
