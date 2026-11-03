"use client";

import { useMutation, useQuery } from "@tanstack/react-query";
import Link from "next/link";
import { useState } from "react";
import { Button } from "@/components/ui/button";
import { Field, Input, Select } from "@/components/ui/field";
import { notify } from "@/components/ui/toast";
import { useOrganizationId } from "@/lib/auth/session";
import { orgKey } from "@/lib/query/keys";
import { listIntegrations, shareToChat } from "../api";

// Shares a file to a chat channel as an expiring, sign-in-gated link.
export function ShareToChat({ assetId }: { assetId: string }) {
  const organizationId = useOrganizationId();
  const [open, setOpen] = useState(false);
  const [integration, setIntegration] = useState("");
  const [life, setLife] = useState("24h");
  const [note, setNote] = useState("");
  const channels = useQuery({
    queryKey: orgKey(organizationId, "chat", "integrations"),
    queryFn: ({ signal }) => listIntegrations(signal),
    enabled: Boolean(organizationId) && open,
  });
  const share = useMutation({
    mutationFn: () => shareToChat({ asset_id: assetId, integration_id: integration, note: note.trim(), expires_in: life }),
    onSuccess: () => {
      notify.success("Posted to the channel");
      setOpen(false);
    },
    onError: notify.error,
  });
  if (!open) return <Button onClick={() => setOpen(true)}>Share to chat</Button>;
  const list = channels.data?.integrations ?? [];
  return (
    <div className="neo-in-sm flex w-full flex-col gap-3 p-4">
      {channels.isSuccess && list.length === 0 ? (
        <p className="text-base">
          No chat channels yet.{" "}
          <Link href="/app/settings/chat" className="underline underline-offset-2">
            Add one in settings
          </Link>
          .
        </p>
      ) : (
        <div className="grid gap-3 sm:grid-cols-3">
          <Field label="Channel">
            {(props) => (
              <Select {...props} value={integration} onChange={(e) => setIntegration(e.target.value)}>
                <option value="">Choose a channel</option>
                {list.map((c) => (
                  <option key={c.id} value={c.id}>
                    {c.name}
                  </option>
                ))}
              </Select>
            )}
          </Field>
          <Field label="Link lasts">
            {(props) => (
              <Select {...props} value={life} onChange={(e) => setLife(e.target.value)}>
                <option value="1h">1 hour</option>
                <option value="24h">1 day</option>
                <option value="168h">7 days</option>
                <option value="720h">30 days</option>
              </Select>
            )}
          </Field>
          <Field label="Note">
            {(props) => <Input {...props} maxLength={500} value={note} onChange={(e) => setNote(e.target.value)} />}
          </Field>
        </div>
      )}
      <div className="flex gap-2">
        <Button variant="primary" disabled={!integration} loading={share.isPending} onClick={() => share.mutate()}>
          Post link
        </Button>
        <Button variant="ghost" onClick={() => setOpen(false)}>
          Cancel
        </Button>
      </div>
    </div>
  );
}
