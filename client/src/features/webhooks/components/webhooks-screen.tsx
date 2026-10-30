"use client";

import { useState, type FormEvent } from "react";
import { Button } from "@/components/ui/button";
import { CopyButton } from "@/components/ui/copy-button";
import { ConfirmDialog, Modal } from "@/components/ui/dialog";
import { Checkbox, Field, Input, Select } from "@/components/ui/field";
import { LoadMore } from "@/components/ui/page";
import { Panel } from "@/components/ui/panel";
import { EmptyState, ErrorState, SkeletonRows } from "@/components/ui/states";
import { StateBadge } from "@/components/ui/status";
import { SubmitButton } from "@/components/ui/submit-button";
import { Table, Td, Th } from "@/components/ui/table";
import { Time } from "@/components/ui/time";
import { notify } from "@/components/ui/toast";
import { describeError } from "@/lib/api/errors";
import { useSession } from "@/lib/auth/session";
import { createWebhook, deleteWebhook, redeliver, rotateSecret, sendTest, updateWebhook } from "../api";
import { useDeliveries, useWebhookMutation, useWebhooks } from "../hooks";
import type { Delivery, DeliveryStatus, Webhook } from "../types";

// What each event means, in the words a receiver's author needs.
const EVENT_TEXT: Record<string, string> = {
  "object.available": "An upload passed verification and scanning and was published",
  "object.rejected": "Processing rejected an upload",
  "object.erased": "A deletion finished and the bytes are gone from storage",
  "object.flagged": "A content check flagged or rejected a file, or a re-scan blocked a published one",
  "object.deletion_blocked": "A deletion is held back by a legal hold, a retention period or a lock",
  "object.lock_tampered": "A lock read back from the provider no longer matches what was applied",
  "retention.change_requested": "Someone tried to shorten retention or release a hold",
  "governance.approval_requested": "A change waits for a second member's approval",
  "ingestion.finished": "A bulk ingestion job ended",
  "dataset.snapshot_created": "A snapshot froze a new dataset version",
  "dataset.validation_finished": "A validation job ended",
  "object.scrub_failed": "A scrub found a stored object damaged or missing, and whether it was rewritten from a replica",
  "copy.job_finished": "A migration, restore, drill or export bundle ended",
  "security.alert": "Unusual reads, mass share links, a new network, a used-up download budget, a failing bucket or a change outside the platform",
  "webhook.test": "A test event you send from this page",
};

export function WebhooksScreen() {
  const { can } = useSession();
  const canManage = can("org.manage");
  const hooks = useWebhooks();
  const [editing, setEditing] = useState<Webhook | "new" | null>(null);
  const [selected, setSelected] = useState<string | null>(null);
  const [secret, setSecret] = useState<{ url: string; value: string } | null>(null);

  if (hooks.isPending) return <SkeletonRows rows={4} />;
  if (hooks.isError) return <ErrorState error={hooks.error} onRetry={() => hooks.refetch()} />;

  const { webhooks, event_types: eventTypes } = hooks.data;
  const current = webhooks.find((w) => w.id === selected) ?? webhooks[0];

  return (
    <div className="grid gap-6 lg:grid-cols-3">
      <div className="flex min-w-0 flex-col gap-6 lg:col-span-2">
        <Panel
          title="Endpoints"
          description="Each endpoint receives the events it subscribes to, signed, as soon as the change commits."
          actions={
            canManage && (
              <Button variant="primary" onClick={() => setEditing("new")}>
                Add endpoint
              </Button>
            )
          }
        >
          {webhooks.length === 0 ? (
            <EmptyState title="No endpoints yet">
              Add the URL of a service that should hear about new objects, finished ingestion jobs, snapshots or
              validations. You get a signing secret to check each request with.
            </EmptyState>
          ) : (
            <Table>
              <caption className="sr-only">Webhook endpoints</caption>
              <thead>
                <tr>
                  <Th>URL</Th>
                  <Th>Events</Th>
                  <Th>State</Th>
                  <Th>
                    <span className="sr-only">Deliveries</span>
                  </Th>
                </tr>
              </thead>
              <tbody>
                {webhooks.map((hook) => (
                  <tr key={hook.id} aria-current={hook.id === current?.id ? "true" : undefined}>
                    <Td className="max-w-md">
                      <div className="mono-data truncate" title={hook.url}>
                        {hook.url}
                      </div>
                      {hook.description && <div className="mt-0.5 text-sm text-muted">{hook.description}</div>}
                    </Td>
                    <Td className="tabular-nums">{hook.event_types.length}</Td>
                    <Td>
                      <StateBadge tone={hook.enabled ? "done" : "neutral"}>{hook.enabled ? "Enabled" : "Disabled"}</StateBadge>
                    </Td>
                    <Td className="text-right">
                      <Button variant={hook.id === current?.id ? "primary" : "secondary"} onClick={() => setSelected(hook.id)}>
                        Deliveries
                      </Button>
                    </Td>
                  </tr>
                ))}
              </tbody>
            </Table>
          )}
        </Panel>

        {current && (
          <EndpointDetail
            key={current.id}
            hook={current}
            canManage={canManage}
            onEdit={() => setEditing(current)}
            onSecret={(value) => setSecret({ url: current.url, value })}
          />
        )}
      </div>

      <SigningHelp />

      {editing && (
        <EndpointForm
          hook={editing === "new" ? null : editing}
          eventTypes={eventTypes}
          onClose={() => setEditing(null)}
          onCreated={(created) => {
            setSelected(created.id);
            if (created.secret) setSecret({ url: created.url, value: created.secret });
          }}
        />
      )}

      <Modal
        open={secret !== null}
        onOpenChange={(open) => !open && setSecret(null)}
        title="Signing secret"
        description="Shown only now. Store it in the receiving service; it checks every request's signature with it."
        footer={<Button onClick={() => setSecret(null)}>I have stored it</Button>}
      >
        {secret && (
          <div className="flex flex-col gap-3">
            <p className="mono-data text-sm text-muted">{secret.url}</p>
            <div className="neo-in-sm flex items-center justify-between gap-3 px-3 py-2">
              <code className="mono-data break-all">{secret.value}</code>
              <CopyButton value={secret.value} />
            </div>
          </div>
        )}
      </Modal>
    </div>
  );
}

function EndpointDetail({
  hook,
  canManage,
  onEdit,
  onSecret,
}: {
  hook: Webhook;
  canManage: boolean;
  onEdit: () => void;
  onSecret: (secret: string) => void;
}) {
  const [status, setStatus] = useState<DeliveryStatus | "">("");
  const deliveries = useDeliveries(hook.id, status);
  const test = useWebhookMutation(sendTest);
  const toggle = useWebhookMutation(updateWebhook);
  const rotate = useWebhookMutation(rotateSecret);
  const remove = useWebhookMutation(deleteWebhook);
  const retry = useWebhookMutation(redeliver);
  const [confirm, setConfirm] = useState<"rotate" | "delete" | null>(null);

  async function run<T>(action: () => Promise<T>, done: string) {
    try {
      await action();
      notify.success(done);
    } catch (error) {
      notify.error(error);
    }
  }

  return (
    <Panel
      title="Deliveries"
      description={<span className="mono-data">{hook.url}</span>}
      actions={
        canManage && (
          <>
            <Button onClick={() => run(() => test.mutateAsync(hook.id), "Test event queued")} disabled={test.isPending}>
              Send test event
            </Button>
            <Button onClick={onEdit}>Edit</Button>
            <Button
              onClick={() =>
                run(() => toggle.mutateAsync({ id: hook.id, enabled: !hook.enabled }), hook.enabled ? "Endpoint disabled" : "Endpoint enabled")
              }
              disabled={toggle.isPending}
            >
              {hook.enabled ? "Disable" : "Enable"}
            </Button>
            <Button onClick={() => setConfirm("rotate")}>Rotate secret</Button>
            <Button onClick={() => setConfirm("delete")}>Delete</Button>
          </>
        )
      }
    >
      <div className="mb-4 flex flex-wrap items-end gap-3">
        <Field label="Show" className="w-48">
          {(props) => (
            <Select {...props} value={status} onChange={(e) => setStatus(e.target.value as DeliveryStatus | "")}>
              <option value="">Every delivery</option>
              <option value="PENDING">Waiting to retry</option>
              <option value="FAILED">Gave up</option>
              <option value="DELIVERED">Delivered</option>
            </Select>
          )}
        </Field>
        <p className="pb-2 text-sm text-muted">Newest first. A failed attempt is retried for about a day and a half.</p>
      </div>

      {deliveries.isPending ? (
        <SkeletonRows rows={4} />
      ) : deliveries.isError ? (
        <ErrorState error={deliveries.error} onRetry={() => deliveries.refetch()} />
      ) : deliveries.items.length === 0 ? (
        <EmptyState title={status ? "Nothing in this state" : "No deliveries yet"}>
          Events are sent once something they cover happens. Send a test event to check the endpoint now.
        </EmptyState>
      ) : (
        <>
          <Table>
            <caption className="sr-only">Deliveries to this endpoint</caption>
            <thead>
              <tr>
                <Th>Event</Th>
                <Th>Outcome</Th>
                <Th className="text-right">Attempts</Th>
                <Th>Created</Th>
                <Th>
                  <span className="sr-only">Actions</span>
                </Th>
              </tr>
            </thead>
            <tbody>
              {deliveries.items.map((d) => (
                <tr key={d.id}>
                  <Td>
                    <div className="mono-data">{d.event_type}</div>
                    <div className="mono-data text-xs text-muted">{d.event_id}</div>
                    <details className="mt-1">
                      <summary className="cursor-pointer text-sm">Event data</summary>
                      <pre className="mono-data neo-in-sm mt-1 max-w-md overflow-x-auto p-2 text-xs" data-lenis-prevent-horizontal>
                        {JSON.stringify(d.payload, null, 2)}
                      </pre>
                    </details>
                  </Td>
                  <Td>
                    <DeliveryOutcome delivery={d} />
                  </Td>
                  <Td className="text-right tabular-nums">{d.attempts}</Td>
                  <Td>
                    <Time value={d.created_at} />
                  </Td>
                  <Td className="text-right">
                    {canManage && d.status !== "DELIVERED" && (
                      <Button
                        onClick={() => run(() => retry.mutateAsync({ id: hook.id, deliveryId: d.id }), "Sent again")}
                        disabled={retry.isPending}
                      >
                        Send again
                      </Button>
                    )}
                  </Td>
                </tr>
              ))}
            </tbody>
          </Table>
          <LoadMore
            hasNext={Boolean(deliveries.hasNextPage)}
            loading={deliveries.isFetchingNextPage}
            onLoad={() => deliveries.fetchNextPage()}
            shown={deliveries.items.length}
          />
        </>
      )}

      <ConfirmDialog
        open={confirm === "rotate"}
        onOpenChange={(open) => !open && setConfirm(null)}
        title="Rotate the signing secret?"
        description="The current secret stops signing at once. Requests fail their signature check until the receiving service has the new one."
        confirmLabel="Rotate secret"
        onConfirm={async () => {
          const { secret } = await rotate.mutateAsync(hook.id);
          onSecret(secret);
        }}
      />
      <ConfirmDialog
        open={confirm === "delete"}
        onOpenChange={(open) => !open && setConfirm(null)}
        title="Delete this endpoint?"
        description="No more events are sent to it, and its delivery history is removed."
        confirmLabel="Delete endpoint"
        onConfirm={async () => {
          await remove.mutateAsync(hook.id);
          notify.success("Endpoint deleted");
        }}
      />
    </Panel>
  );
}

function DeliveryOutcome({ delivery: d }: { delivery: Delivery }) {
  if (d.status === "DELIVERED") {
    return (
      <div>
        <StateBadge tone="done">Delivered</StateBadge>
        <div className="mt-0.5 text-sm text-muted">
          HTTP {d.last_status_code} <Time value={d.delivered_at} />
        </div>
      </div>
    );
  }
  const answered = d.last_status_code ? `HTTP ${d.last_status_code}` : d.last_error;
  return (
    <div>
      <StateBadge tone={d.status === "FAILED" ? "failed" : "warning"}>{d.status === "FAILED" ? "Gave up" : "Retrying"}</StateBadge>
      {d.attempts === 0 ? (
        <div className="mt-0.5 text-sm text-muted">Not sent yet</div>
      ) : (
        <div className="mt-0.5 max-w-xs text-sm text-muted">
          {answered}
          {d.next_attempt_at && (
            <>
              ; next try <Time value={d.next_attempt_at} />
            </>
          )}
        </div>
      )}
    </div>
  );
}

function EndpointForm({
  hook,
  eventTypes,
  onClose,
  onCreated,
}: {
  hook: Webhook | null;
  eventTypes: string[];
  onClose: () => void;
  onCreated: (created: Webhook) => void;
}) {
  const [url, setUrl] = useState(hook?.url ?? "");
  const [description, setDescription] = useState(hook?.description ?? "");
  const [chosen, setChosen] = useState(new Set(hook?.event_types ?? eventTypes.filter((t) => t !== "webhook.test")));
  const [error, setError] = useState<string | null>(null);
  const create = useWebhookMutation(createWebhook);
  const update = useWebhookMutation(updateWebhook);

  const cleanUrl = url.trim();
  const urlProblem = !/^https?:\/\/\S+$/.test(cleanUrl) ? "Enter the full URL, starting with https://." : "";

  async function submit(e: FormEvent) {
    e.preventDefault();
    if (urlProblem || chosen.size === 0) return;
    setError(null);
    const body = { url: cleanUrl, description: description.trim(), event_types: [...chosen] };
    try {
      if (hook) {
        await update.mutateAsync({ id: hook.id, ...body });
        notify.success("Endpoint saved");
      } else {
        onCreated(await create.mutateAsync(body));
      }
      onClose();
    } catch (err) {
      setError(describeError(err));
    }
  }

  function toggle(type: string, on: boolean) {
    const next = new Set(chosen);
    if (on) next.add(type);
    else next.delete(type);
    setChosen(next);
  }

  return (
    <Modal open onOpenChange={(open) => !open && onClose()} title={hook ? "Edit endpoint" : "Add endpoint"}>
      <form onSubmit={submit} className="flex flex-col gap-4" noValidate>
        <Field
          label="URL"
          required
          error={urlProblem}
          hint="HTTPS only. Private, loopback and cloud metadata addresses are refused, and redirects are not followed."
        >
          {(props) => (
            <Input
              {...props}
              type="url"
              inputMode="url"
              spellCheck={false}
              value={url}
              onChange={(e) => setUrl(e.target.value)}
              placeholder="https://hooks.example.com/storage"
              autoComplete="off"
            />
          )}
        </Field>
        <Field label="Description">
          {(props) => <Input {...props} value={description} maxLength={500} onChange={(e) => setDescription(e.target.value)} />}
        </Field>
        <fieldset className="flex flex-col gap-2">
          <legend className="mb-1 text-sm font-medium">Events</legend>
          {eventTypes.map((type) => (
            <Checkbox
              key={type}
              checked={chosen.has(type)}
              onChange={(e) => toggle(type, e.target.checked)}
              label={
                <>
                  <span className="mono-data">{type}</span>
                  <span className="block text-sm text-muted">{EVENT_TEXT[type] ?? ""}</span>
                </>
              }
            />
          ))}
          {chosen.size === 0 && <p className="text-sm font-medium">Choose at least one event.</p>}
        </fieldset>
        {error && (
          <p role="alert" className="text-sm font-medium">
            {error}
          </p>
        )}
        <div className="flex justify-end gap-2">
          <Button type="button" onClick={onClose}>
            Cancel
          </Button>
          <SubmitButton
            valid={!urlProblem && chosen.size > 0}
            loading={create.isPending || update.isPending}
            hint="Enter an https URL and choose at least one event."
          >
            {hook ? "Save endpoint" : "Add endpoint"}
          </SubmitButton>
        </div>
      </form>
    </Modal>
  );
}

function SigningHelp() {
  return (
    <Panel title="Checking a delivery" description="Each request carries three headers. Standard Webhooks libraries verify them as they are.">
      <dl className="flex flex-col gap-3 text-sm">
        <div>
          <dt className="mono-data">webhook-id</dt>
          <dd className="text-muted">The event. The same on every retry: use it to ignore repeats.</dd>
        </div>
        <div>
          <dt className="mono-data">webhook-timestamp</dt>
          <dd className="text-muted">Unix seconds. Refuse one more than five minutes old.</dd>
        </div>
        <div>
          <dt className="mono-data">webhook-signature</dt>
          <dd className="text-muted">
            <span className="mono-data">v1,</span> then the base64 HMAC-SHA256 of <span className="mono-data">id.timestamp.body</span>,
            keyed with the bytes of the secret after <span className="mono-data">whsec_</span>.
          </dd>
        </div>
      </dl>
      <p className="mt-4 text-sm">
        Answer with any 2xx within 10 seconds. Anything else is retried after 10 s, 1 min, 5 min, 30 min, 2 h, 6 h, 12 h and 24 h. The
        body names identifiers and states only, never file content or storage keys.
      </p>
    </Panel>
  );
}
