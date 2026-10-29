"use client";

import { useState } from "react";
import { Field, Select } from "@/components/ui/field";
import { LoadMore } from "@/components/ui/page";
import { Panel } from "@/components/ui/panel";
import { EmptyState, ErrorState, SkeletonRows } from "@/components/ui/states";
import { StateBadge } from "@/components/ui/status";
import { Time } from "@/components/ui/time";
import { api } from "@/lib/api/client";
import { useOrganizationId } from "@/lib/auth/session";
import { orgKey } from "@/lib/query/keys";
import { usePagedQuery } from "@/lib/query/paged";
import { formatBytes } from "@/lib/utils/format";

type UploadListItem = {
  upload_id: string;
  status: string;
  file_name: string;
  content_type: string;
  size_bytes: number;
  multipart: boolean;
  created_at: string;
  expires_at: string;
};

const TONE: Record<string, "done" | "failed" | "active" | "neutral"> = {
  COMPLETED: "done",
  VERIFIED: "done",
  FAILED: "failed",
  EXPIRED: "neutral",
  ABORTED: "neutral",
  ABORTING: "neutral",
  PENDING: "active",
  UPLOADING: "active",
  COMPLETING: "active",
};

// The organization's uploads, from any browser or client, newest first.
export function RecentUploads() {
  const organizationId = useOrganizationId();
  const [status, setStatus] = useState("");
  const uploads = usePagedQuery<UploadListItem, string>({
    queryKey: orgKey(organizationId, "uploads", "list", status),
    enabled: Boolean(organizationId),
    fetchPage: async (cursor, signal) => {
      const page = await api<{ uploads: UploadListItem[]; next_cursor?: string }>("/uploads", {
        query: { cursor, status, limit: 20 },
        signal,
      });
      return { items: page.uploads, next: page.next_cursor };
    },
  });

  return (
    <Panel title="Recent uploads" description="Everyone's, from any browser, the SDK or the API.">
      <Field label="Show" className="mb-4">
        {(p) => (
          <Select {...p} value={status} onChange={(e) => setStatus(e.target.value)}>
            <option value="">Every upload</option>
            <option value="UPLOADING">Uploading</option>
            <option value="COMPLETED">Waiting for processing</option>
            <option value="VERIFIED">Processed</option>
            <option value="FAILED">Failed</option>
            <option value="EXPIRED">Expired</option>
            <option value="ABORTED">Cancelled</option>
          </Select>
        )}
      </Field>
      {uploads.isPending ? (
        <SkeletonRows rows={4} />
      ) : uploads.isError ? (
        <ErrorState error={uploads.error} onRetry={() => uploads.refetch()} />
      ) : uploads.items.length === 0 ? (
        <EmptyState title={status ? "None in this state" : "No uploads yet"}>
          Files you drop here, or send with the SDK, are listed as they arrive.
        </EmptyState>
      ) : (
        <>
          <ul className="flex flex-col gap-3">
            {uploads.items.map((u) => (
              <li key={u.upload_id} className="flex items-start justify-between gap-3">
                <div className="min-w-0">
                  <p className="truncate font-medium" title={u.file_name}>
                    {u.file_name}
                  </p>
                  <p className="text-sm text-muted">
                    {formatBytes(u.size_bytes)}
                    {u.multipart && ", multipart"}, <Time value={u.created_at} />
                  </p>
                </div>
                <StateBadge tone={TONE[u.status] ?? "neutral"}>{u.status.toLowerCase()}</StateBadge>
              </li>
            ))}
          </ul>
          <LoadMore
            hasNext={Boolean(uploads.hasNextPage)}
            loading={uploads.isFetchingNextPage}
            onLoad={() => uploads.fetchNextPage()}
            shown={uploads.items.length}
          />
        </>
      )}
    </Panel>
  );
}
