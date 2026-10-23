"use client";

import { useIsFetching, useQueryClient, type UseQueryResult } from "@tanstack/react-query";
import { ArrowClockwise } from "@phosphor-icons/react";
import Link from "next/link";
import { useState, type ReactNode } from "react";
import { Button, ButtonLink } from "@/components/ui/button";
import { PageHeader } from "@/components/ui/page";
import { Panel } from "@/components/ui/panel";
import { EmptyState, ErrorState, SkeletonRows } from "@/components/ui/states";
import { StateBadge } from "@/components/ui/status";
import { Time } from "@/components/ui/time";
import { isApiError } from "@/lib/api/errors";
import { useSession } from "@/lib/auth/session";
import { useQuotaQuery } from "@/features/organization/hooks";
import { QuotaMeters } from "@/features/organization/components/quota-view";
import { shortId } from "@/lib/utils/format";
import { useOverviewData, overviewKey } from "../hooks";
import { findAttentionJobs, describeConnectionTest } from "../attention";
import type { AuditEventSummary, ConnectionSummary, DatasetSummary, DeadLetterSummary } from "../types";

export function OverviewScreen() {
  const { can, organizationId, isPlatformAdmin } = useSession();
  const client = useQueryClient();
  const data = useOverviewData();
  const quota = useQuotaQuery({ enabled: can("quota.read"), keySuffix: "overview" });
  const fetching = useIsFetching({ queryKey: overviewKey(organizationId) }) > 0;

  const all = [...Object.values(data), quota];
  const updatedAt = Math.max(0, ...all.map((q) => q.dataUpdatedAt));

  return (
    <>
      <PageHeader
        title="Overview"
        description="What needs a look right now, from the API as of the last refresh."
        actions={
          <>
            <span className="text-sm text-muted" aria-live="polite">
              {updatedAt > 0 ? (
                <>
                  Updated <Time value={new Date(updatedAt).toISOString()} />
                </>
              ) : (
                "Loading"
              )}
            </span>
            <Button loading={fetching} onClick={() => client.refetchQueries({ queryKey: overviewKey(organizationId) })}>
              {!fetching && <ArrowClockwise aria-hidden />}
              Refresh
            </Button>
          </>
        }
      />
      <div className="mb-6 flex flex-wrap items-center justify-between gap-4">
        <StatusLine data={data} />
        <QuickActions />
      </div>
      <div className="grid gap-6 lg:grid-cols-3">
        <div className="flex min-w-0 flex-col gap-6 lg:col-span-2">
          <AttentionPanel data={data} />
          <ActivityPanel query={data.audit} allowed={can("audit.read")} />
        </div>
        <div className="flex min-w-0 flex-col gap-6">
          <QuotaPanel query={quota} allowed={can("quota.read")} />
          <StoragePanel query={data.connections} allowed={can("storage.read")} />
          {isPlatformAdmin && <DeadLettersPanel query={data.deadLetters} />}
        </div>
      </div>
    </>
  );
}

// The one sentence a person opens this page for. It stays quiet when all is well
// and takes the accent only when something needs them.
function StatusLine({ data }: { data: ReturnType<typeof useOverviewData> }) {
  const { jobs, failedDatasets } = data;
  const settled = [jobs, failedDatasets].filter((q) => q.fetchStatus !== "idle" || q.data);
  // Reserve the line's height so the page does not jump when the answer arrives.
  if (settled.length === 0 || settled.some((q) => q.isPending)) return <p className="min-h-9 text-base text-muted" aria-live="polite" />;
  if (settled.some((q) => q.isError)) return <p className="min-h-9 text-base text-muted">Some checks did not load. Try them again below.</p>;

  const count =
    findAttentionJobs(jobs.data?.jobs ?? []).length + (failedDatasets.data?.datasets ?? []).length;
  if (count === 0) {
    return (
      <p className="min-h-9 text-md font-semibold" aria-live="polite">
        All clear. Nothing failed or stalled.
      </p>
    );
  }
  return (
    <p aria-live="polite" className="inline-flex min-h-9 items-center bg-accent px-3 text-md font-semibold text-on-accent">
      {count} {count === 1 ? "item needs" : "items need"} attention
    </p>
  );
}

// At most three, with one primary: the next step most people take from here.
function QuickActions() {
  const { can } = useSession();
  return (
    <div className="flex flex-wrap items-center gap-2">
      {can("uploads.write") && (
        <ButtonLink href="/app/ingestion/uploads" variant="primary" className="h-11">
          Upload files
        </ButtonLink>
      )}
      {can("datasets.write") && (
        <ButtonLink href="/app/datasets/new" className="h-11">
          New dataset
        </ButtonLink>
      )}
      {can("ingestion.write") && (
        <ButtonLink href="/app/ingestion/jobs/new" className="h-11">
          New ingestion job
        </ButtonLink>
      )}
    </div>
  );
}

// Shows the first few rows and keeps the rest one press away.
function ShortList<T>({
  items,
  limit = 4,
  render,
}: {
  items: T[];
  limit?: number;
  render: (item: T) => ReactNode;
}) {
  const [all, setAll] = useState(false);
  const shown = all ? items : items.slice(0, limit);
  const hidden = items.length - shown.length;
  return (
    <>
      <ul className="flex flex-col divide-y divide-faint">{shown.map(render)}</ul>
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
    <p className="text-base text-muted">
      Your role does not include <span className="mono-data">{permission}</span>, so {what} is not shown.
    </p>
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

function AttentionPanel({ data }: { data: ReturnType<typeof useOverviewData> }) {
  const { can } = useSession();
  const { jobs, failedDatasets, validatingDatasets } = data;

  return (
    <Panel title="Needs attention" description="Failed or stalled work. Only the latest 50 jobs and the first 5 datasets of each kind are checked.">
      <div className="flex flex-col gap-8">
        <section aria-labelledby="attention-jobs" className="flex flex-col gap-3">
          <div className="flex flex-wrap items-baseline justify-between gap-2">
            <h3 id="attention-jobs" className="text-md font-semibold">
              Ingestion jobs <span className="text-sm font-normal text-muted">latest 50 jobs</span>
            </h3>
            <Link href="/app/ingestion/jobs" className="text-sm underline underline-offset-2">
              All jobs
            </Link>
          </div>
          {!can("ingestion.read") ? (
            <NotAllowed permission="ingestion.read" what="ingestion jobs" />
          ) : (
            <Body query={jobs}>
              {({ jobs: list }) => {
                const attention = findAttentionJobs(list ?? []);
                if (attention.length === 0) {
                  return (
                    <EmptyState title="No failed or stalled jobs">
                      None of the latest 50 jobs failed, finished partially or stopped moving. Older jobs are not checked
                      here.
                    </EmptyState>
                  );
                }
                return (
                  <ShortList
                    items={attention}
                    render={({ job, reason }) => (
                      <li key={job.id} className="flex flex-wrap items-center justify-between gap-3 py-2.5">
                        <div className="min-w-0">
                          <Link href={`/app/ingestion/jobs/${job.id}`} className="mono-data underline underline-offset-2">
                            {shortId(job.id, 12)}
                          </Link>
                          <p className="text-sm text-muted">
                            {reason}, <Time value={job.updated_at} />
                          </p>
                        </div>
                        <StateBadge tone="attention">{job.state.replaceAll("_", " ")}</StateBadge>
                      </li>
                    )}
                  />
                );
              }}
            </Body>
          )}
        </section>

        <section aria-labelledby="attention-datasets" className="flex flex-col gap-3">
          <div className="flex flex-wrap items-baseline justify-between gap-2">
            <h3 id="attention-datasets" className="text-md font-semibold">
              Datasets <span className="text-sm font-normal text-muted">failed or validating, first 5 of each</span>
            </h3>
            <Link href="/app/datasets" className="text-sm underline underline-offset-2">
              All datasets
            </Link>
          </div>
          {!can("datasets.read") ? (
            <NotAllowed permission="datasets.read" what="datasets" />
          ) : (
            <DatasetList failed={failedDatasets} validating={validatingDatasets} />
          )}
        </section>
      </div>
    </Panel>
  );
}

function DatasetList({
  failed,
  validating,
}: {
  failed: UseQueryResult<{ datasets: DatasetSummary[] }>;
  validating: UseQueryResult<{ datasets: DatasetSummary[] }>;
}) {
  if (failed.isPending || validating.isPending) return <SkeletonRows rows={2} />;
  const error = failed.error ?? validating.error;
  if (error) {
    return (
      <ErrorState
        error={error}
        onRetry={() => {
          void failed.refetch();
          void validating.refetch();
        }}
      />
    );
  }
  const items = [...(failed.data?.datasets ?? []), ...(validating.data?.datasets ?? [])];
  if (items.length === 0) {
    return (
      <EmptyState title="No failed or validating datasets">
        Datasets move here when a validation fails or is still running.
      </EmptyState>
    );
  }
  return (
    <ShortList
      items={items}
      render={(d) => (
        <li key={d.id} className="flex flex-wrap items-center justify-between gap-3 py-2.5">
          <div className="min-w-0">
            <Link href={`/app/datasets/${d.id}`} className="font-medium underline underline-offset-2 [overflow-wrap:anywhere]">
              {d.name}
            </Link>
            <p className="text-sm text-muted">
              Updated <Time value={d.updated_at} />
            </p>
          </div>
          <StateBadge tone={d.lifecycle_status === "FAILED" ? "attention" : "active"}>{d.lifecycle_status}</StateBadge>
        </li>
      )}
    />
  );
}

function ActivityPanel({ query, allowed }: { query: UseQueryResult<{ events: AuditEventSummary[] }>; allowed: boolean }) {
  return (
    <Panel
      title="Recent activity"
      description="The 8 newest audit events."
      actions={
        allowed && (
          <Link href="/app/audit" className="text-sm underline underline-offset-2">
            Open audit log
          </Link>
        )
      }
    >
      {!allowed ? (
        <NotAllowed permission="audit.read" what="the audit log" />
      ) : (
        <Body query={query} rows={4}>
          {({ events }) =>
            (events ?? []).length === 0 ? (
              <EmptyState title="No audit events yet">Sign-ins, member changes and uploads are recorded here.</EmptyState>
            ) : (
              <ShortList
                items={events}
                limit={5}
                render={(e) => (
                  <li key={e.id} className="flex flex-wrap items-center justify-between gap-3 py-2.5">
                    <div className="min-w-0">
                      <p className="mono-data text-sm [overflow-wrap:anywhere]">{e.action}</p>
                      <p className="text-sm text-muted">
                        {e.actor_id.length > 16 ? shortId(e.actor_id, 8) : e.actor_id}, <Time value={e.created_at} />
                      </p>
                    </div>
                    <StateBadge tone={e.outcome === "SUCCESS" ? "neutral" : "attention"}>{e.outcome}</StateBadge>
                  </li>
                )}
              />
            )
          }
        </Body>
      )}
    </Panel>
  );
}

function QuotaPanel({ query, allowed }: { query: ReturnType<typeof useQuotaQuery>; allowed: boolean }) {
  return (
    <Panel
      title="Quota"
      actions={
        allowed && (
          <Link href="/app/settings/quota" className="text-sm underline underline-offset-2">
            All limits
          </Link>
        )
      }
    >
      {!allowed ? (
        <NotAllowed permission="quota.read" what="quota" />
      ) : (
        <Body query={query} rows={5}>
          {(quota) => (
            <div className="flex flex-col gap-4">
              <QuotaMeters quota={quota} />
              <p className="text-sm text-muted">
                Stored objects and bytes come from the quota ledger. Active ingestion jobs: Not available from the API.
              </p>
            </div>
          )}
        </Body>
      )}
    </Panel>
  );
}

function StoragePanel({
  query,
  allowed,
}: {
  query: UseQueryResult<{ connections: ConnectionSummary[]; default_connection_id: string }>;
  allowed: boolean;
}) {
  return (
    <Panel
      title="Storage"
      actions={
        allowed && (
          <Link href="/app/storage" className="text-sm underline underline-offset-2">
            Manage
          </Link>
        )
      }
    >
      {!allowed ? (
        <NotAllowed permission="storage.read" what="storage connections" />
      ) : (
        <Body query={query}>
          {({ connections, default_connection_id }) => (
            <ul className="flex flex-col divide-y divide-faint">
              {(connections ?? []).map((c) => (
                <li key={c.id} className="flex flex-col gap-1 py-2.5">
                  <div className="flex flex-wrap items-center justify-between gap-2">
                    <span className="font-medium [overflow-wrap:anywhere]">{c.name}</span>
                    <span className="flex gap-1.5">
                      {(c.is_default || c.id === default_connection_id) && <StateBadge tone="done">Default</StateBadge>}
                      <StateBadge tone={c.status === "ACTIVE" ? "neutral" : "attention"}>{c.status}</StateBadge>
                    </span>
                  </div>
                  <p className="text-sm text-muted">{describeConnectionTest(c)}</p>
                </li>
              ))}
            </ul>
          )}
        </Body>
      )}
    </Panel>
  );
}

function DeadLettersPanel({ query }: { query: UseQueryResult<{ dead_letters: DeadLetterSummary[] }> }) {
  const missing = query.isError && isApiError(query.error) && query.error.isNotFound;
  return (
    <Panel
      title="Open dead letters"
      description="Platform admins only. The first 5 open items."
      actions={
        <Link href="/app/operations/dead-letters" className="text-sm underline underline-offset-2">
          Open queue
        </Link>
      }
    >
      {missing ? (
        <p className="text-base text-muted">Dead-letter handling is not enabled on this server.</p>
      ) : (
        <Body query={query}>
          {({ dead_letters }) =>
            (dead_letters ?? []).length === 0 ? (
              <EmptyState title="No open dead letters">Events that could not be processed land here.</EmptyState>
            ) : (
              <ul className="flex flex-col divide-y divide-faint">
                {dead_letters.map((d) => (
                  <li key={d.id} className="py-2.5">
                    <p className="mono-data text-sm [overflow-wrap:anywhere]">{d.event_type ?? "Unknown event"}</p>
                    <p className="text-sm text-muted">
                      {d.category ?? "No category"}, <Time value={d.created_at} />
                    </p>
                  </li>
                ))}
              </ul>
            )
          }
        </Body>
      )}
    </Panel>
  );
}
