"use client";

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { Button } from "@/components/ui/button";
import { Field, Input, Textarea } from "@/components/ui/field";
import { DefList, PageHeader } from "@/components/ui/page";
import { Panel } from "@/components/ui/panel";
import { EmptyState, ErrorState, SkeletonRows } from "@/components/ui/states";
import { StateBadge } from "@/components/ui/status";
import { SubmitButton } from "@/components/ui/submit-button";
import { Table, Td, Th } from "@/components/ui/table";
import { Time } from "@/components/ui/time";
import { notify } from "@/components/ui/toast";
import { useOrganizationId } from "@/lib/auth/session";
import { orgKey } from "@/lib/query/keys";
import { formatNumber } from "@/lib/utils/format";
import { createReview, fetchFile, listReviews, saveBlob, signReview, type FindingKind, type Review } from "../api";
import { AuditTabs } from "./audit-tabs";

const KIND_TEXT: Record<FindingKind, string> = {
  DELETION: "Deletions",
  RETENTION_CHANGE: "Retention changes",
  PERMISSION_CHANGE: "Permission changes",
  FAILED_HASH: "Failed hashes",
  AFTER_HOURS_READS: "After-hours bulk reads",
};

function useReviewKeys() {
  const organizationId = useOrganizationId();
  const client = useQueryClient();
  return { organizationId, refresh: () => client.invalidateQueries({ queryKey: orgKey(organizationId, "evidence", "reviews") }) };
}

export function ReviewsScreen() {
  const { organizationId } = useReviewKeys();
  const [openId, setOpenId] = useState<string | null>(null);
  const reviews = useQuery({
    queryKey: orgKey(organizationId, "evidence", "reviews"),
    queryFn: ({ signal }) => listReviews(signal),
    enabled: Boolean(organizationId),
  });
  const list = reviews.data?.reviews ?? [];
  const open = list.find((r) => r.id === openId) ?? list[0];

  return (
    <>
      <PageHeader
        title="Exception reviews"
        description="Deletions, retention and permission changes, failed hashes and after-hours bulk reads for a period, signed off by a reviewer in the audit chain itself."
      />
      <AuditTabs />
      <div className="grid gap-6 lg:grid-cols-3">
        <div className="flex min-w-0 flex-col gap-6 lg:col-span-2">
          <Panel title="Reviews" description="The worker builds one on the schedule in the settings.">
            {reviews.isPending ? (
              <SkeletonRows rows={4} />
            ) : reviews.isError ? (
              <ErrorState error={reviews.error} onRetry={() => reviews.refetch()} />
            ) : list.length === 0 ? (
              <EmptyState title="No reviews yet">Build one for a period to see what needs review.</EmptyState>
            ) : (
              <Table>
                <thead>
                  <tr>
                    <Th>Period</Th>
                    <Th>Findings</Th>
                    <Th>State</Th>
                  </tr>
                </thead>
                <tbody>
                  {list.map((r) => (
                    <tr key={r.id} className={r.id === open?.id ? "bg-faint" : undefined}>
                      <Td>
                        <button
                          type="button"
                          aria-current={r.id === open?.id ? "true" : undefined}
                          className="text-left underline underline-offset-2"
                          onClick={() => setOpenId(r.id)}
                        >
                          <Time value={r.period_start} /> to <Time value={r.period_end} />
                        </button>
                      </Td>
                      <Td className="tabular-nums">{formatNumber(r.findings.length)}</Td>
                      <Td>
                        <StateBadge tone={r.state === "SIGNED" ? "done" : "warning"}>{r.state === "SIGNED" ? "Signed" : "Open"}</StateBadge>
                      </Td>
                    </tr>
                  ))}
                </tbody>
              </Table>
            )}
          </Panel>
          {open && <ReviewDetail key={open.id} review={open} />}
        </div>
        <div className="flex min-w-0 flex-col gap-6">
          <CreatePanel onCreated={setOpenId} />
        </div>
      </div>
    </>
  );
}

function CreatePanel({ onCreated }: { onCreated: (id: string) => void }) {
  const { refresh } = useReviewKeys();
  const [days, setDays] = useState("7");
  const create = useMutation({
    mutationFn: () => createReview(new Date(Date.now() - Number(days) * 86_400_000).toISOString()),
    onSuccess: (r) => {
      notify.success(`Review built: ${formatNumber(r.findings.length)} findings`);
      refresh();
      onCreated(r.id);
    },
    onError: notify.error,
  });
  const n = Number(days);
  const error = !Number.isInteger(n) || n < 1 || n > 366 ? "Between 1 and 366 days." : undefined;

  function submit(e: FormEvent) {
    e.preventDefault();
    if (!error) create.mutate();
  }

  return (
    <Panel title="Build a review" description="From the chain as it stands; up to now.">
      <form onSubmit={submit} className="flex flex-col gap-4">
        <Field label="Days back" error={error}>
          {(props) => <Input {...props} type="number" min={1} max={366} value={days} onChange={(e) => setDays(e.target.value)} />}
        </Field>
        <div>
          <SubmitButton valid={!error} loading={create.isPending}>
            Build review
          </SubmitButton>
        </div>
      </form>
    </Panel>
  );
}

function ReviewDetail({ review }: { review: Review }) {
  const { refresh } = useReviewKeys();
  const [note, setNote] = useState("");
  const sign = useMutation({
    mutationFn: () => signReview(review.id, note.trim()),
    onSuccess: () => {
      notify.success("Signed; the sign-off is now an entry in the audit chain");
      refresh();
    },
    onError: notify.error,
  });
  const download = useMutation({
    mutationFn: () => fetchFile(`/audit-reviews/${encodeURIComponent(review.id)}/export`),
    onSuccess: (blob) => saveBlob(blob, `review-${review.id}.zip`),
    onError: notify.error,
  });
  const counts = Object.entries(KIND_TEXT).map(([kind, label]) => ({
    label,
    value: formatNumber(review.counts[kind as FindingKind] ?? 0),
  }));

  return (
    <Panel
      title="Review"
      description="Findings for the period, from the audit chain."
      actions={
        <Button onClick={() => download.mutate()} loading={download.isPending}>
          Download for an inspector
        </Button>
      }
    >
      <div className="flex flex-col gap-5">
        <p className="text-sm text-muted">
          Report SHA-256 <span className="mono-data break-all">{review.report_sha256}</span>
        </p>
        <DefList items={counts} />
        {review.findings.length > 0 && (
          <Table>
            <thead>
              <tr>
                <Th>When</Th>
                <Th>What</Th>
                <Th>Who</Th>
                <Th>Detail</Th>
              </tr>
            </thead>
            <tbody>
              {review.findings.slice(0, 200).map((f, i) => (
                <tr key={`${f.audit_seq ?? i}-${f.kind}`}>
                  <Td>
                    <Time value={f.at} />
                  </Td>
                  <Td className="text-sm">{KIND_TEXT[f.kind] ?? f.kind}</Td>
                  <Td className="mono-data text-sm break-all">{f.actor || <span className="text-muted">System</span>}</Td>
                  <Td className="text-sm">
                    {f.detail}
                    {f.audit_seq ? <span className="block text-muted">Entry {formatNumber(f.audit_seq)}</span> : null}
                  </Td>
                </tr>
              ))}
            </tbody>
          </Table>
        )}
        {review.findings.length > 200 && (
          <p className="text-sm text-muted">Showing 200 of {formatNumber(review.findings.length)}; the download has them all.</p>
        )}
        {review.state === "SIGNED" ? (
          <p className="text-base">
            Signed by <span className="mono-data">{review.signed_by}</span> {review.signed_at && <Time value={review.signed_at} />}
            {review.note && <span className="block text-muted">{review.note}</span>}
          </p>
        ) : (
          <form
            onSubmit={(e) => {
              e.preventDefault();
              sign.mutate();
            }}
            className="flex flex-col gap-3"
          >
            <Field label="Reviewer's note" hint="What you looked at and what follows. A review is signed once.">
              {(props) => <Textarea {...props} rows={3} value={note} onChange={(e) => setNote(e.target.value)} />}
            </Field>
            <div>
              <SubmitButton valid loading={sign.isPending}>
                Sign off review
              </SubmitButton>
            </div>
          </form>
        )}
      </div>
    </Panel>
  );
}
