"use client";

import { useIsFetching, useQueryClient, type UseQueryResult } from "@tanstack/react-query";
import { ArrowClockwise } from "@phosphor-icons/react";
import Link from "next/link";
import { useState, type ReactNode } from "react";
import { Button, ButtonLink } from "@/components/ui/button";
import { PageHeader } from "@/components/ui/page";
import { ErrorState, SkeletonRows } from "@/components/ui/states";
import { StateBadge, StateMark } from "@/components/ui/status";
import { Time } from "@/components/ui/time";
import { isApiError } from "@/lib/api/errors";
import { useSession } from "@/lib/auth/session";
import { cn } from "@/lib/utils/cn";
import { useQuotaQuery } from "@/features/organization/hooks";
import { QuotaMeters } from "@/features/organization/components/quota-view";
import { shortId } from "@/lib/utils/format";
import { useOverviewData, overviewKey } from "../hooks";
import { describeConnectionTest, lastTestFailed, rackJobs } from "../attention";
import type { AuditEventSummary, ConnectionSummary, DatasetSummary, DeadLetterSummary } from "../types";

type OverviewData = ReturnType<typeof useOverviewData>;

// The overview is a strip board: every job, dataset, connection and dead letter is
// a strip racked by its state, and what needs the user sits in the front bay.
export function OverviewScreen() {
  const { can, organizationId, isPlatformAdmin } = useSession();
  const client = useQueryClient();
  const data = useOverviewData();
  const quota = useQuotaQuery({
    enabled: can("quota.read"),
    keySuffix: "overview",
  });
  const fetching = useIsFetching({ queryKey: overviewKey(organizationId) }) > 0;

  const all = [...Object.values(data), quota];
  const updatedAt = Math.max(0, ...all.map((q) => q.dataUpdatedAt));

  return (
    <>
      <PageHeader
        title="Overview"
        description={<StatusLine data={data} />}
        actions={
          <>
            <span className="text-sm text-muted" aria-live="polite">
              {updatedAt > 0 ? (
                <>
                  Updated <Time value={new Date(updatedAt).toISOString()} />
                </>
              ) : (
                "Loading…"
              )}
            </span>
            <Button loading={fetching} onClick={() => client.refetchQueries({ queryKey: overviewKey(organizationId) })}>
              {!fetching && <ArrowClockwise aria-hidden />}
              Refresh
            </Button>
            <QuickActions />
          </>
        }
      />

      <div className="mt-8 grid gap-x-6 gap-y-10 lg:grid-cols-3">
        <div className="flex min-w-0 flex-col gap-10 lg:col-span-2">
          <NeedsYouBay data={data} />
          <MovingRack data={data} />
        </div>
        <div className="flex min-w-0 flex-col gap-10">
          <StorageRack query={data.connections} allowed={can("storage.read")} />
          <QuotaRack query={quota} allowed={can("quota.read")} />
          {isPlatformAdmin && <DeadLetterRack query={data.deadLetters} />}
        </div>
      </div>

      <ActivityLog query={data.audit} allowed={can("audit.read")} />
    </>
  );
}

function sum(...parts: (number | undefined)[]) {
  let total: number | undefined;
  for (const part of parts) {
    if (part !== undefined) total = (total ?? 0) + part;
  }
  return total;
}

// The line a person opens this page for: how many strips sit in each rack.
// Only the needs-you count takes the orange, and only above zero.
function StatusLine({ data }: { data: OverviewData }) {
  const { jobs, failedDatasets, validatingDatasets } = data;
  // Queries the role cannot run stay idle with no data; they say nothing here.
  const settled = [jobs, failedDatasets].filter((q) => q.fetchStatus !== "idle" || q.data);
  if (settled.length === 0) return null;
  if (settled.some((q) => q.isPending)) return <span aria-live="polite">Reading the board…</span>;
  if (settled.some((q) => q.isError)) return <span>Some racks did not load. Try them again below.</span>;

  const racks = jobs.data ? rackJobs(jobs.data.jobs ?? []) : null;
  const needs = sum(racks?.attention.length, failedDatasets.data?.datasets.length) ?? 0;
  const moving = sum(racks?.moving.length, validatingDatasets.data?.datasets.length);
  return (
    <span aria-live="polite" className="flex flex-wrap items-center gap-x-3 gap-y-1">
      <span
        className={cn(
          "rounded-sm px-2 py-0.5 font-semibold tabular-nums",
          needs > 0 ? "bg-accent text-on-accent" : "bg-faint text-ink",
        )}
      >
        {needs > 0 ? `${needs} need you` : "Nothing needs you"}
      </span>
      {moving !== undefined && <span className="tabular-nums">{moving} in motion</span>}
      {racks && (
        <>
          <span className="tabular-nums">{racks.ready.length} ready to run</span>
          <span className="tabular-nums">{racks.completed.length} completed, of the latest 50 jobs</span>
        </>
      )}
    </span>
  );
}

// At most three, with one primary: the next step most people take from here.
function QuickActions() {
  const { can } = useSession();
  return (
    <>
      {can("datasets.write") && (
        <ButtonLink href="/app/datasets/new" className="max-sm:hidden">
          New dataset
        </ButtonLink>
      )}
      {can("ingestion.write") && (
        <ButtonLink href="/app/ingestion/jobs/new" className="max-sm:hidden">
          New ingestion job
        </ButtonLink>
      )}
      {can("uploads.write") && (
        <ButtonLink href="/app/ingestion/uploads" variant="primary">
          Upload files
        </ButtonLink>
      )}
    </>
  );
}

type Mark = "failed" | "warning" | "active" | "done" | "neutral";

const cellTone: Record<Mark, string> = {
  failed: "bg-accent text-on-accent",
  // A stalled or partly published strip: an ink ring around an orange core, so failed stays the loudest.
  warning: "bg-faint text-ink",
  active: "bg-ink text-ground",
  done: "bg-faint text-ink",
  neutral: "bg-faint text-muted",
};

// One strip: a filled state cell, the strip's name and what is wrong, and its state and time on the right.
function Strip({
  mark,
  href,
  name,
  mono = false,
  detail,
  state,
  when,
  size = "md",
}: {
  mark: Mark;
  href?: string;
  name: ReactNode;
  mono?: boolean;
  detail?: ReactNode;
  state?: ReactNode;
  when?: string;
  size?: "md" | "lg";
}) {
  const nameClass = cn("font-semibold [overflow-wrap:anywhere]", mono && "mono-data", size === "lg" && "text-md");
  return (
    <li className="grid grid-cols-[2.5rem_1fr] overflow-hidden rounded-[var(--radius)] border border-line bg-ground">
      <span className={cn("flex justify-center", size === "lg" ? "pt-5" : "pt-3.5", cellTone[mark])}>
        <StateMark tone={mark} className="size-4" />
      </span>
      <div
        className={cn(
          "flex flex-wrap items-start justify-between gap-x-6 gap-y-1 px-4",
          size === "lg" ? "py-4" : "py-2.5",
        )}
      >
        <div className="min-w-0">
          {href ? (
            <Link
              href={href}
              translate={mono ? "no" : undefined}
              className={cn(nameClass, "underline decoration-line underline-offset-4 hover:decoration-ink")}
            >
              {name}
            </Link>
          ) : (
            <p className={nameClass} translate={mono ? "no" : undefined}>
              {name}
            </p>
          )}
          {detail && <p className="text-sm text-muted [overflow-wrap:anywhere]">{detail}</p>}
        </div>
        {(state || when) && (
          <div className="flex flex-col text-sm sm:items-end">
            {state && <span className="font-semibold">{state}</span>}
            {when && (
              <span className="text-muted tabular-nums">
                <Time value={when} />
              </span>
            )}
          </div>
        )}
      </div>
    </li>
  );
}

// An empty slot in a rack: dashed, so it never reads as a strip.
function EmptySlot({ children }: { children: ReactNode }) {
  return (
    <p className="rounded-[var(--radius)] border border-dashed border-edge px-4 py-3 text-base text-muted">
      {children}
    </p>
  );
}

function RackHeader({
  id,
  title,
  note,
  link,
  size = "md",
}: {
  id: string;
  title: string;
  note?: string;
  link?: { href: string; label: string };
  size?: "md" | "lg";
}) {
  return (
    <div className="mb-3 flex flex-wrap items-end justify-between gap-x-4 gap-y-1">
      <div className="min-w-0">
        <h2 id={id} className={cn("font-semibold", size === "lg" ? "text-xl" : "text-lg")}>
          {title}
        </h2>
        {note && <p className="text-sm text-muted">{note}</p>}
      </div>
      {link && (
        <Link href={link.href} className="text-sm underline underline-offset-2">
          {link.label}
        </Link>
      )}
    </div>
  );
}

// Shows the first few strips and keeps the rest one press away.
function ShortList<T>({ items, limit = 4, render }: { items: T[]; limit?: number; render: (item: T) => ReactNode }) {
  const [all, setAll] = useState(false);
  const shown = all ? items : items.slice(0, limit);
  const hidden = items.length - shown.length;
  return (
    <>
      <ul className="flex flex-col gap-2">{shown.map(render)}</ul>
      {(hidden > 0 || (all && items.length > limit)) && (
        <Button variant="ghost" className="mt-2 h-11" aria-expanded={all} onClick={() => setAll(!all)}>
          {all ? "Show fewer" : `Show ${hidden} more`}
        </Button>
      )}
    </>
  );
}

function NotAllowed({ permission, what }: { permission: string; what: string }) {
  return (
    <EmptySlot>
      Your role does not include <span className="mono-data">{permission}</span>, so {what} is not shown.
    </EmptySlot>
  );
}

// Loading, error and success for one query. A 403 renders as permission denied.
function Body<T>({
  query,
  rows = 3,
  children,
}: {
  query: UseQueryResult<T>;
  rows?: number;
  children: (data: T) => ReactNode;
}) {
  if (query.isPending) return <SkeletonRows rows={rows} />;
  if (query.isError) return <ErrorState error={query.error} onRetry={() => query.refetch()} />;
  return <>{children(query.data)}</>;
}

// "PARTIALLY_COMPLETED" reads as "Partially completed".
function words(state: string) {
  return state.charAt(0) + state.slice(1).toLowerCase().replaceAll("_", " ");
}

function SubRackHeader({ id, title, href, link }: { id: string; title: string; href: string; link: string }) {
  return (
    <div className="mb-2 flex flex-wrap items-baseline justify-between gap-2">
      <h3 id={id} className="text-md font-semibold">
        {title}
      </h3>
      <Link href={href} className="text-sm underline underline-offset-2">
        {link}
      </Link>
    </div>
  );
}

// The front bay: failed or stalled jobs and failed datasets, the biggest block on the board.
function NeedsYouBay({ data }: { data: OverviewData }) {
  const { can } = useSession();
  const { jobs, failedDatasets } = data;

  return (
    <section aria-labelledby="bay-title">
      <RackHeader
        id="bay-title"
        title="Needs you"
        size="lg"
        note="Failed, partly published, or stalled for over an hour."
      />
      <div className="flex flex-col gap-6">
        <section aria-labelledby="bay-jobs">
          <SubRackHeader id="bay-jobs" title="Ingestion jobs" href="/app/ingestion/jobs" link="All jobs" />
          {!can("ingestion.read") ? (
            <NotAllowed permission="ingestion.read" what="ingestion jobs" />
          ) : (
            <Body query={jobs}>
              {({ jobs: list }) => {
                const attention = rackJobs(list ?? []).attention;
                if (attention.length === 0) {
                  return (
                    <EmptySlot>No failed or stalled jobs in the latest 50. Older jobs are not checked here.</EmptySlot>
                  );
                }
                return (
                  <ShortList
                    items={attention}
                    render={({ job, reason }) => {
                      const stalled = job.state !== "FAILED" && job.state !== "PARTIALLY_COMPLETED";
                      return (
                        <Strip
                          key={job.id}
                          size="lg"
                          mark={job.state === "FAILED" ? "failed" : "warning"}
                          href={`/app/ingestion/jobs/${job.id}`}
                          name={shortId(job.id, 12)}
                          mono
                          detail={reason}
                          state={stalled ? `Stalled while ${words(job.state).toLowerCase()}` : words(job.state)}
                          when={job.updated_at}
                        />
                      );
                    }}
                  />
                );
              }}
            </Body>
          )}
        </section>

        <section aria-labelledby="bay-datasets">
          <SubRackHeader id="bay-datasets" title="Datasets" href="/app/datasets" link="All datasets" />
          {!can("datasets.read") ? (
            <NotAllowed permission="datasets.read" what="datasets" />
          ) : (
            <Body query={failedDatasets} rows={2}>
              {({ datasets }) =>
                (datasets ?? []).length === 0 ? (
                  <EmptySlot>No failed datasets. A dataset lands here when its validation fails.</EmptySlot>
                ) : (
                  <ShortList items={datasets} render={(d) => <DatasetStrip key={d.id} dataset={d} size="lg" />} />
                )
              }
            </Body>
          )}
        </section>
      </div>
    </section>
  );
}

function DatasetStrip({ dataset, size }: { dataset: DatasetSummary; size?: "md" | "lg" }) {
  const failed = dataset.lifecycle_status === "FAILED";
  return (
    <Strip
      size={size}
      mark={failed ? "failed" : "active"}
      href={`/app/datasets/${dataset.id}`}
      name={dataset.name}
      detail={failed ? "Validation failed" : "Validation running"}
      state={words(dataset.lifecycle_status)}
      when={dataset.updated_at}
    />
  );
}

// What is moving or waiting for a start: running jobs, datasets still validating, then ready jobs.
// A jobs error is already shown in the bay above, so here it is only a note.
function MovingRack({ data }: { data: OverviewData }) {
  const { can } = useSession();
  const { jobs, validatingDatasets } = data;
  const canJobs = can("ingestion.read");
  const canDatasets = can("datasets.read");
  if (!canJobs && !canDatasets) return null;

  let body: ReactNode;
  if ((canJobs && jobs.isPending) || (canDatasets && validatingDatasets.isPending)) {
    body = <SkeletonRows rows={2} />;
  } else if (canDatasets && validatingDatasets.isError) {
    body = <ErrorState error={validatingDatasets.error} onRetry={() => validatingDatasets.refetch()} />;
  } else {
    const racks = rackJobs(jobs.data?.jobs ?? []);
    const strips = [
      ...racks.moving.map((job) => (
        <Strip
          key={job.id}
          mark="active"
          href={`/app/ingestion/jobs/${job.id}`}
          name={shortId(job.id, 12)}
          mono
          detail={`${job.progress.published} of ${job.progress.total} published`}
          state={words(job.state)}
          when={job.updated_at}
        />
      )),
      ...(validatingDatasets.data?.datasets ?? []).map((d) => <DatasetStrip key={d.id} dataset={d} />),
      ...racks.ready.map((job) => (
        <Strip
          key={job.id}
          mark="neutral"
          href={`/app/ingestion/jobs/${job.id}`}
          name={shortId(job.id, 12)}
          mono
          detail={`${job.progress.total} files, waiting to be started`}
          state="Ready to run"
          when={job.updated_at}
        />
      )),
    ];
    body = (
      <>
        {strips.length === 0 ? (
          <EmptySlot>Nothing is running or waiting for a start.</EmptySlot>
        ) : (
          <ShortList items={strips} render={(s) => s} />
        )}
        {canJobs && jobs.isError && (
          <p className="mt-2 text-sm text-muted">Jobs did not load, so only datasets are shown.</p>
        )}
      </>
    );
  }

  return (
    <section aria-labelledby="moving-title">
      <RackHeader
        id="moving-title"
        title="In motion and ready"
        note="Running jobs and validations, then jobs waiting for a start."
      />
      {body}
    </section>
  );
}

function StorageRack({
  query,
  allowed,
}: {
  query: UseQueryResult<{
    connections: ConnectionSummary[];
    default_connection_id: string;
  }>;
  allowed: boolean;
}) {
  return (
    <section aria-labelledby="storage-title">
      <RackHeader
        id="storage-title"
        title="Storage"
        link={allowed ? { href: "/app/storage", label: "Manage" } : undefined}
      />
      {!allowed ? (
        <NotAllowed permission="storage.read" what="storage connections" />
      ) : (
        <Body query={query}>
          {({ connections, default_connection_id }) =>
            (connections ?? []).length === 0 ? (
              <EmptySlot>No storage connections yet.</EmptySlot>
            ) : (
              <ul className="flex flex-col gap-2">
                {connections.map((c) => {
                  const broken = lastTestFailed(c) || c.status !== "ACTIVE";
                  const isDefault = c.is_default || c.id === default_connection_id;
                  return (
                    <Strip
                      key={c.id}
                      // The filled square means a test passed; never-tested connections stay hollow.
                      mark={broken ? "failed" : c.last_test_ok_at ? "done" : "neutral"}
                      name={c.name}
                      detail={describeConnectionTest(c)}
                      state={c.status !== "ACTIVE" ? words(c.status) : isDefault ? "Default" : undefined}
                    />
                  );
                })}
              </ul>
            )
          }
        </Body>
      )}
    </section>
  );
}

function QuotaRack({ query, allowed }: { query: ReturnType<typeof useQuotaQuery>; allowed: boolean }) {
  return (
    <section aria-labelledby="quota-title">
      <RackHeader
        id="quota-title"
        title="Quota"
        link={allowed ? { href: "/app/settings/quota", label: "All limits" } : undefined}
      />
      {!allowed ? (
        <NotAllowed permission="quota.read" what="quota" />
      ) : (
        <Body query={query} rows={5}>
          {(quota) => (
            <div className="flex flex-col gap-4 rounded-[var(--radius)] border border-line p-4">
              <QuotaMeters quota={quota} />
              <p className="text-sm text-muted">
                From the quota ledger. Active ingestion jobs are not reported by the API.
              </p>
            </div>
          )}
        </Body>
      )}
    </section>
  );
}

function DeadLetterRack({ query }: { query: UseQueryResult<{ dead_letters: DeadLetterSummary[] }> }) {
  const missing = query.isError && isApiError(query.error) && query.error.isNotFound;
  return (
    <section aria-labelledby="dead-title">
      <RackHeader
        id="dead-title"
        title="Open dead letters"
        note="Platform admins only. The first 5 open items."
        link={{ href: "/app/operations/dead-letters", label: "Open queue" }}
      />
      {missing ? (
        <EmptySlot>Dead-letter handling is not enabled on this server.</EmptySlot>
      ) : (
        <Body query={query}>
          {({ dead_letters }) =>
            (dead_letters ?? []).length === 0 ? (
              <EmptySlot>No open dead letters. Events that could not be processed land here.</EmptySlot>
            ) : (
              <ul className="flex flex-col gap-2">
                {dead_letters.map((d) => (
                  <Strip
                    key={d.id}
                    mark="failed"
                    name={d.event_type ?? "Unknown event"}
                    mono
                    detail={d.category ?? "No category"}
                    when={d.created_at}
                  />
                ))}
              </ul>
            )
          }
        </Body>
      )}
    </section>
  );
}

// The log runs one way: newest first, time on the left.
function ActivityLog({ query, allowed }: { query: UseQueryResult<{ events: AuditEventSummary[] }>; allowed: boolean }) {
  return (
    <section aria-labelledby="log-title" className="mt-10">
      <RackHeader
        id="log-title"
        title="Recent activity"
        note="The 8 newest audit events."
        link={allowed ? { href: "/app/audit", label: "Open audit log" } : undefined}
      />
      {!allowed ? (
        <NotAllowed permission="audit.read" what="the audit log" />
      ) : (
        <Body query={query} rows={4}>
          {({ events }) =>
            (events ?? []).length === 0 ? (
              <EmptySlot>No audit events yet. Sign-ins, member changes and uploads are recorded here.</EmptySlot>
            ) : (
              <ol className="divide-y divide-line border-y border-line">
                {events.map((e) => (
                  <li
                    key={e.id}
                    className="grid gap-x-6 gap-y-0.5 py-2.5 sm:grid-cols-[11rem_1fr_auto] sm:items-center"
                  >
                    <span className="text-sm text-muted tabular-nums">
                      <Time value={e.created_at} />
                    </span>
                    <span className="min-w-0 [overflow-wrap:anywhere]">
                      <span className="mono-data" translate="no">
                        {e.action}
                      </span>
                      <span className="text-sm text-muted">
                        {" "}
                        by {e.actor_id.length > 16 ? shortId(e.actor_id, 8) : e.actor_id}
                      </span>
                    </span>
                    <span>
                      <StateBadge tone={e.outcome === "SUCCESS" ? "neutral" : "attention"}>
                        {words(e.outcome)}
                      </StateBadge>
                    </span>
                  </li>
                ))}
              </ol>
            )
          }
        </Body>
      )}
    </section>
  );
}
