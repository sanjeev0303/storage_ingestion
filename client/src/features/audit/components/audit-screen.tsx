"use client";

import { CaretDown, CaretRight } from "@phosphor-icons/react";
import { Fragment, useState, type FormEvent } from "react";
import { Button } from "@/components/ui/button";
import { CopyButton } from "@/components/ui/copy-button";
import { Field, Input, Select } from "@/components/ui/field";
import { DefList, LoadMore, PageHeader } from "@/components/ui/page";
import { Panel } from "@/components/ui/panel";
import { EmptyState, ErrorState, PermissionDenied, SkeletonRows } from "@/components/ui/states";
import { StateBadge } from "@/components/ui/status";
import { SubmitButton } from "@/components/ui/submit-button";
import { Table, Td, Th } from "@/components/ui/table";
import { Time } from "@/components/ui/time";
import { describeError } from "@/lib/api/errors";
import { useSession } from "@/lib/auth/session";
import { splitList } from "@/lib/utils/forgive";
import { formatNumber } from "@/lib/utils/format";
import { useAuditEvents, useVerifyAudit } from "../hooks";
import type { AuditChain, AuditEvent, AuditFilters, AuditOutcome } from "../types";
import { validateAction } from "../validate";

const noFilters: AuditFilters = { action: "", outcome: "", actor: "" };

// Pasted values may carry spaces or quotes. Both filters take a single word.
function oneWord(value: string) {
  return splitList(value).join(" ");
}

export function AuditScreen() {
  const { can, isPlatformAdmin, isPlatformAuditor, organizationId } = useSession();
  const isPlatform = isPlatformAdmin || isPlatformAuditor;
  const canReadOrg = Boolean(organizationId) && can("audit.read");

  const [chain, setChain] = useState<AuditChain>(canReadOrg || !isPlatform ? "org" : "_platform");
  const [draft, setDraft] = useState<AuditFilters>(noFilters);
  const [applied, setApplied] = useState<AuditFilters>(noFilters);

  const allowed = chain === "_platform" ? isPlatform : can("audit.read");
  const events = useAuditEvents(chain, applied, allowed);
  const verify = useVerifyAudit(chain);

  const action = oneWord(draft.action).toLowerCase();
  const actionError = action.includes(" ") ? "Enter one action name." : validateAction(action) ?? undefined;
  const actorError = oneWord(draft.actor).includes(" ") ? "Enter one actor id." : undefined;
  const valid = !actionError && !actorError;

  function apply(e: FormEvent) {
    e.preventDefault();
    if (!valid) return;
    setApplied({ ...draft, action, actor: oneWord(draft.actor) });
  }

  function reset() {
    setDraft(noFilters);
    setApplied(noFilters);
  }

  function changeChain(next: AuditChain) {
    setChain(next);
    verify.reset();
  }

  const filtered = applied.action !== "" || applied.outcome !== "" || applied.actor !== "";

  return (
    <>
      <PageHeader
        title="Audit log"
        description="Every security-relevant action in the organization, newest first. Entries are hash-chained, so tampering can be detected."
        actions={
          <Button onClick={() => verify.mutate()} loading={verify.isPending} disabled={!allowed}>
            Verify audit chain
          </Button>
        }
      />

      {isPlatform && (
        <div className="mb-4 max-w-sm">
          <Field
            label="Chain"
            hint="The platform chain records platform-level actions such as organization and user administration."
          >
            {(props) => (
              <Select {...props} value={chain} onChange={(e) => changeChain(e.target.value as AuditChain)}>
                <option value="org" disabled={!canReadOrg}>
                  This organization
                </option>
                <option value="_platform">Platform (_platform)</option>
              </Select>
            )}
          </Field>
        </div>
      )}

      {verify.isError && (
        <div className="mb-4">
          <ErrorState error={verify.error} title="The chain could not be verified" onRetry={() => verify.mutate()} />
        </div>
      )}
      {verify.data && (
        <Panel
          className="mb-4"
          title={verify.data.intact ? "Chain is intact" : "Chain is broken"}
          description="Result of the server re-computing every hash in this chain."
        >
          <DefList
            items={[
              { label: "Chain", value: <span className="mono-data">{verify.data.chain}</span> },
              { label: "Events checked", value: formatNumber(verify.data.checked) },
              {
                label: "Sequence range",
                value: verify.data.checked > 0 ? `${verify.data.first_seq} to ${verify.data.last_seq}` : "No events yet",
              },
              {
                label: "Intact",
                value: (
                  <StateBadge tone={verify.data.intact ? "done" : "attention"}>
                    {verify.data.intact ? "Yes" : "No"}
                  </StateBadge>
                ),
              },
              ...(verify.data.intact
                ? []
                : [
                    { label: "Broken at sequence", value: verify.data.broken_at ?? "Not reported" },
                    { label: "Problem", value: verify.data.problem ?? "Not reported" },
                  ]),
            ]}
          />
        </Panel>
      )}

      <form onSubmit={apply} noValidate className="neo mb-4 grid gap-4 p-5 sm:grid-cols-2 lg:grid-cols-4 lg:items-start">
        <Field
          label="Action"
          error={actionError}
          hint={actionError ? undefined : "Exact name or a prefix ending in a dot, such as member."}
        >
          {(props) => (
            <Input
              {...props}
              value={draft.action}
              onChange={(e) => setDraft({ ...draft, action: e.target.value })}
              placeholder="member."
              autoComplete="off"
              spellCheck={false}
            />
          )}
        </Field>
        <Field label="Outcome">
          {(props) => (
            <Select
              {...props}
              value={draft.outcome}
              onChange={(e) => setDraft({ ...draft, outcome: e.target.value as "" | AuditOutcome })}
            >
              <option value="">Any outcome</option>
              <option value="SUCCESS">Success</option>
              <option value="DENIED">Denied</option>
              <option value="FAILED">Failed</option>
            </Select>
          )}
        </Field>
        <Field label="Actor id" error={actorError} hint="Exact match: a user id, anonymous or system.">
          {(props) => (
            <Input
              {...props}
              value={draft.actor}
              onChange={(e) => setDraft({ ...draft, actor: e.target.value })}
              autoComplete="off"
              spellCheck={false}
            />
          )}
        </Field>
        <div className="flex gap-2 sm:col-span-2 lg:col-span-1 lg:pt-6">
          <SubmitButton valid={valid} hint="Fix the filter marked above to apply it.">
            Apply filters
          </SubmitButton>
          <Button onClick={reset}>Clear</Button>
        </div>
      </form>

      <p className="mb-4 max-w-prose text-sm text-muted">
        The API filters by action, outcome and actor only. There is no date range, resource or free-text search, so this
        page does not offer them. Load more pages to look further back.
      </p>

      {!allowed ? (
        <PermissionDenied detail="Reading the audit log needs the audit.read permission." />
      ) : events.isPending ? (
        <SkeletonRows rows={6} />
      ) : events.isError && events.items.length === 0 ? (
        <ErrorState error={events.error} onRetry={() => events.refetch()} />
      ) : events.items.length === 0 ? (
        <EmptyState title={filtered ? "No events match these filters" : "No audit events yet"}>
          {filtered
            ? "Clear a filter or check the spelling of the action and actor. Each filter must match exactly."
            : "Sign-ins, membership changes, deletions and other sensitive actions are recorded here as they happen."}
        </EmptyState>
      ) : (
        <>
          <EventTable events={events.items} />
          <LoadMore
            hasNext={Boolean(events.hasNextPage)}
            loading={events.isFetchingNextPage}
            onLoad={() => events.fetchNextPage()}
            shown={events.items.length}
          />
          {events.isFetchNextPageError && (
            <p role="alert" className="mt-2 text-sm font-medium">
              {describeError(events.error)}
            </p>
          )}
        </>
      )}
    </>
  );
}

function EventTable({ events }: { events: AuditEvent[] }) {
  const [open, setOpen] = useState<string | null>(null);

  return (
    <Table>
      <thead>
        <tr>
          <Th>Time</Th>
          <Th>Actor</Th>
          <Th>Action</Th>
          <Th>Resource</Th>
          <Th>Outcome</Th>
          <Th>Reason</Th>
          <Th>
            <span className="sr-only">Details</span>
          </Th>
        </tr>
      </thead>
      <tbody>
        {events.map((event) => {
          const expanded = open === event.id;
          return (
            <Fragment key={event.id}>
              <tr>
                <Td>
                  <Time value={event.created_at} />
                </Td>
                <Td className="min-w-40">
                  <div className="text-sm text-muted">{event.actor_type}</div>
                  <div className="mono-data">{event.actor_id}</div>
                </Td>
                <Td className="mono-data">{event.action}</Td>
                <Td className="min-w-40">
                  <div className="text-sm text-muted">{event.resource_type}</div>
                  {event.resource_id && <div className="mono-data">{event.resource_id}</div>}
                </Td>
                <Td>
                  <StateBadge tone={event.outcome === "SUCCESS" ? "neutral" : "attention"}>{event.outcome}</StateBadge>
                </Td>
                <Td className="min-w-40 max-w-64 [overflow-wrap:anywhere]">{event.reason ?? ""}</Td>
                <Td>
                  <Button
                    variant="ghost"
                    aria-expanded={expanded}
                    aria-controls={`audit-${event.id}`}
                    onClick={() => setOpen(expanded ? null : event.id)}
                  >
                    {expanded ? <CaretDown aria-hidden /> : <CaretRight aria-hidden />}
                    {expanded ? "Hide" : "Details"}
                    <span className="sr-only"> for event {event.seq}</span>
                  </Button>
                </Td>
              </tr>
              {expanded && (
                <tr id={`audit-${event.id}`}>
                  <Td colSpan={7} className="bg-faint">
                    <EventDetails event={event} />
                  </Td>
                </tr>
              )}
            </Fragment>
          );
        })}
      </tbody>
    </Table>
  );
}

function EventDetails({ event }: { event: AuditEvent }) {
  const details = Object.entries(event.details ?? {});
  return (
    <div className="flex flex-col gap-4 py-2">
      <DefList
        items={[
          { label: "Sequence", value: event.seq },
          {
            label: "Correlation id",
            value: event.correlation_id ? <span className="mono-data">{event.correlation_id}</span> : "Not recorded",
          },
          {
            label: "Source IP",
            value: event.source_ip ? <span className="mono-data">{event.source_ip}</span> : "Not recorded",
          },
          {
            label: "Hash",
            value: (
              <span className="flex flex-wrap items-center gap-2">
                <span className="mono-data">{event.hash}</span>
                <CopyButton value={event.hash} />
              </span>
            ),
          },
        ]}
      />
      <div>
        <h4 className="text-sm font-semibold">Details</h4>
        <p className="text-sm text-muted">Secrets, tokens and URLs are removed by the server before they are stored.</p>
        {details.length === 0 ? (
          <p className="mt-1 text-base text-muted">No details recorded.</p>
        ) : (
          <dl className="mt-2 grid grid-cols-[max-content_1fr] gap-x-6 gap-y-1">
            {details.map(([key, value]) => (
              <Fragment key={key}>
                <dt className="mono-data text-muted">{key}</dt>
                <dd className="mono-data [overflow-wrap:anywhere]">{detailText(value)}</dd>
              </Fragment>
            ))}
          </dl>
        )}
      </div>
    </div>
  );
}

// Plain text only. Nested values are shown as JSON text.
function detailText(value: unknown) {
  if (typeof value === "string") return value;
  return JSON.stringify(value);
}
