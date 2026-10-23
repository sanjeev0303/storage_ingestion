"use client";

import { useState } from "react";
import { Button } from "@/components/ui/button";
import { CopyButton } from "@/components/ui/copy-button";
import { ConfirmDialog } from "@/components/ui/dialog";
import { Field, Select, Textarea } from "@/components/ui/field";
import { DefList } from "@/components/ui/page";
import { Panel } from "@/components/ui/panel";
import { EmptyState, ErrorState, PermissionDenied, SkeletonRows } from "@/components/ui/states";
import { StateBadge } from "@/components/ui/status";
import { Table, Td, Th } from "@/components/ui/table";
import { Time } from "@/components/ui/time";
import { ApiError } from "@/lib/api/errors";
import { useSession } from "@/lib/auth/session";
import { isNotFound, useDeadLetter, useDeadLetterAction, useDeadLetters } from "../hooks";
import type { DeadLetter, DeadLetterAction } from "../types";
import { MAX_REASON, validateReason } from "../validate";

const statuses = ["", "OPEN", "RETRIED", "REPLAYED", "RESOLVED", "DISCARDED"] as const;

const actionCopy: Record<DeadLetterAction, { label: string; title: string; effect: string }> = {
  retry: {
    label: "Retry",
    title: "Retry this dead letter?",
    effect: "The original event is put back in the queue for its consumer to try again.",
  },
  replay: {
    label: "Replay",
    title: "Replay this dead letter?",
    effect: "A new copy of the event is published for every consumer. The original entry is closed.",
  },
  resolve: {
    label: "Resolve",
    title: "Mark this dead letter resolved?",
    effect: "The entry is closed as handled by hand. The event is not processed again.",
  },
  discard: {
    label: "Discard",
    title: "Discard this dead letter?",
    effect: "The entry is closed and the event is dropped. It is not processed again.",
  },
};

export function DeadLettersScreen() {
  const { isPlatformAdmin } = useSession();
  if (!isPlatformAdmin) {
    return (
      <PermissionDenied detail="Dead-letter administration is limited to platform administrators. Organization roles, including owner, do not include it." />
    );
  }
  return <DeadLetterList />;
}

function DeadLetterList() {
  const [status, setStatus] = useState<(typeof statuses)[number]>("OPEN");
  const [selected, setSelected] = useState<string | null>(null);
  const list = useDeadLetters(status, true);

  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-wrap items-end justify-between gap-4">
        <Field label="Status" className="w-56">
          {(props) => (
            <Select
              {...props}
              value={status}
              onChange={(e) => {
                setStatus(e.target.value as (typeof statuses)[number]);
                setSelected(null);
              }}
            >
              {statuses.map((value) => (
                <option key={value} value={value}>
                  {value === "" ? "All statuses" : value.charAt(0) + value.slice(1).toLowerCase()}
                </option>
              ))}
            </Select>
          )}
        </Field>
        <p className="max-w-prose text-sm text-muted">
          Newest first. The API returns at most 100 entries and has no way to page further, so older entries are
          reachable only by closing newer ones.
        </p>
      </div>

      {list.isPending ? (
        <SkeletonRows rows={5} />
      ) : list.isError && isNotFound(list.error) ? (
        <EmptyState title="Dead-letter administration is not enabled on this deployment">
          The API did not register these routes, which happens when the dead-letter service is not wired. Nothing is
          wrong with your account.
        </EmptyState>
      ) : list.isError ? (
        <ErrorState error={list.error} onRetry={() => list.refetch()} />
      ) : list.data.dead_letters.length === 0 ? (
        <EmptyState title={status ? `No ${status.toLowerCase()} dead letters` : "No dead letters"}>
          An event lands here when a consumer gives up on it after repeated failures. Entries can then be retried,
          replayed, resolved or discarded, each with a reason that is written to the audit log.
        </EmptyState>
      ) : (
        <div className="grid gap-6 lg:grid-cols-3">
          <div className="min-w-0 lg:col-span-2">
            <Table>
              <thead>
                <tr>
                  <Th>Event type</Th>
                  <Th>Consumer</Th>
                  <Th>Status</Th>
                  <Th>Category</Th>
                  <Th>Dead-lettered</Th>
                  <Th>
                    <span className="sr-only">Inspect</span>
                  </Th>
                </tr>
              </thead>
              <tbody>
                {list.data.dead_letters.map((d) => (
                  <tr key={d.id} aria-selected={selected === d.id}>
                    <Td className="mono-data">{d.event_type ?? "Unknown"}</Td>
                    <Td className="mono-data">{d.consumer ?? "None"}</Td>
                    <Td>
                      <StateBadge tone={d.status === "OPEN" ? "attention" : "neutral"}>{d.status}</StateBadge>
                    </Td>
                    <Td>{d.category ?? "None"}</Td>
                    <Td>
                      <Time value={d.created_at} />
                    </Td>
                    <Td>
                      <Button variant="ghost" onClick={() => setSelected(d.id)}>
                        Inspect<span className="sr-only"> {d.event_type ?? d.id}</span>
                      </Button>
                    </Td>
                  </tr>
                ))}
              </tbody>
            </Table>
          </div>
          <div className="min-w-0">
            {selected ? (
              <Detail id={selected} />
            ) : (
              <EmptyState title="Pick an entry">Inspect one to see its error and act on it.</EmptyState>
            )}
          </div>
        </div>
      )}
    </div>
  );
}

function Detail({ id }: { id: string }) {
  const detail = useDeadLetter(id);
  const act = useDeadLetterAction();
  const [action, setAction] = useState<DeadLetterAction | null>(null);
  const [reason, setReason] = useState("");
  const [replayId, setReplayId] = useState<string | null>(null);

  if (detail.isPending) return <SkeletonRows rows={4} />;
  if (detail.isError) return <ErrorState error={detail.error} onRetry={() => detail.refetch()} />;

  const d: DeadLetter = detail.data;
  const open = d.status === "OPEN";

  function start(next: DeadLetterAction) {
    setReason("");
    act.reset();
    setAction(next);
  }

  async function confirm() {
    if (!action) return;
    const problem = validateReason(reason);
    if (problem) throw new ApiError({ status: 400, message: problem });
    const response = await act.mutateAsync({ id, action, reason: reason.trim() });
    setReplayId(action === "replay" ? (response?.replay_event_id ?? null) : null);
  }

  return (
    <Panel title="Dead letter" actions={<StateBadge tone={open ? "attention" : "neutral"}>{d.status}</StateBadge>}>
      <DefList
        items={[
          { label: "Id", value: <span className="mono-data">{d.id}</span> },
          { label: "Event id", value: <span className="mono-data">{d.event_id}</span> },
          { label: "Event type", value: d.event_type ?? "Unknown" },
          { label: "Consumer", value: d.consumer ?? "None" },
          { label: "Organization", value: d.tenant_id ? <span className="mono-data">{d.tenant_id}</span> : "None" },
          { label: "Category", value: d.category ?? "None" },
          { label: "Last error", value: d.last_error ?? "None recorded" },
          { label: "Dead-lettered", value: <Time value={d.created_at} /> },
        ]}
      />
      {d.stale && (
        <p className="neo-in-sm mt-4 p-3 text-base">
          <span className="font-semibold">Stale. </span>
          {d.stale}
        </p>
      )}

      {replayId && (
        <div role="status" className="neo-in-sm mt-4 flex flex-wrap items-center justify-between gap-2 p-3">
          <span>
            Replayed as event <span className="mono-data">{replayId}</span>
          </span>
          <CopyButton value={replayId} label="Copy id" />
        </div>
      )}

      <div className="mt-5 flex flex-wrap gap-2">
        {(Object.keys(actionCopy) as DeadLetterAction[]).map((key) => (
          <Button key={key} onClick={() => start(key)} disabled={!open}>
            {actionCopy[key].label}
          </Button>
        ))}
      </div>
      {!open && <p className="mt-2 text-sm text-muted">Only open entries can be acted on.</p>}

      {action && (
        <ConfirmDialog
          open
          onOpenChange={(next) => !next && setAction(null)}
          title={actionCopy[action].title}
          description={
            <div className="flex flex-col gap-4">
              <p>{actionCopy[action].effect}</p>
              <Field label="Reason" required hint={`Written to the audit log. Up to ${MAX_REASON} characters.`}>
                {(props) => <Textarea {...props} value={reason} onChange={(e) => setReason(e.target.value)} />}
              </Field>
            </div>
          }
          confirmLabel={actionCopy[action].label}
          onConfirm={confirm}
        />
      )}
    </Panel>
  );
}
