"use client";

import { useMutation, useQuery } from "@tanstack/react-query";
import { Button } from "@/components/ui/button";
import { DefList, PageHeader } from "@/components/ui/page";
import { Panel } from "@/components/ui/panel";
import { EmptyState, ErrorState, SkeletonRows } from "@/components/ui/states";
import { Time } from "@/components/ui/time";
import { notify } from "@/components/ui/toast";
import { isApiError } from "@/lib/api/errors";
import { useOrganizationId } from "@/lib/auth/session";
import { orgKey } from "@/lib/query/keys";
import { formatBytes } from "@/lib/utils/format";
import { getShare, openShare } from "../api";

// Where a chat message's link lands, after sign-in.
export function SharePage({ id }: { id: string }) {
  const organizationId = useOrganizationId();
  const share = useQuery({
    queryKey: orgKey(organizationId, "chat", "share", id),
    queryFn: ({ signal }) => getShare(id, signal),
    enabled: Boolean(organizationId),
    retry: false,
  });
  const download = useMutation({
    mutationFn: () => openShare(id),
    onSuccess: (r) => window.location.assign(r.url),
    onError: notify.error,
  });
  const s = share.data;
  return (
    <>
      <PageHeader title="Shared file" description="Shared to a chat channel. Your download is recorded in the audit log." />
      {share.isPending ? (
        <SkeletonRows rows={3} />
      ) : share.isError ? (
        isApiError(share.error) && share.error.status === 410 ? (
          <EmptyState title="This link has stopped working">It expired or was revoked. Ask whoever shared it for a new one.</EmptyState>
        ) : (
          <ErrorState error={share.error} onRetry={() => share.refetch()} />
        )
      ) : (
        s && (
          <Panel title={s.path.slice(s.path.lastIndexOf("/") + 1)} description={s.note || undefined}>
            <div className="flex flex-col gap-4">
              <DefList
                items={[
                  { label: "Path", value: <span className="mono-data break-all">{s.path}</span> },
                  { label: "Size", value: formatBytes(s.size_bytes) },
                  { label: "Link stops working", value: <Time value={s.expires_at} /> },
                ]}
              />
              <div>
                <Button variant="primary" onClick={() => download.mutate()} loading={download.isPending}>
                  Download
                </Button>
              </div>
            </div>
          </Panel>
        )
      )}
    </>
  );
}
