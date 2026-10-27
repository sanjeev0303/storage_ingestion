"use client";

import { Button } from "@/components/ui/button";
import { Panel } from "@/components/ui/panel";
import { DefList, Meter } from "@/components/ui/page";
import { EmptyState, SkeletonRows } from "@/components/ui/states";
import { useStorageConnections } from "@/features/storage-connections/hooks";
import { isApiError } from "@/lib/api/errors";
import { useSession } from "@/lib/auth/session";
import { MIB } from "@/lib/upload/parts";
import { formatBytes, formatNumber } from "@/lib/utils/format";
import { useQuota } from "../api";
import { SINGLE_LIMIT_BYTES } from "../engine/file-upload";
import type { UploadSnapshot } from "../types";
import { ConnectionSelect } from "./connection-select";
import { Dropzone } from "./dropzone";
import { QueueTable } from "./queue-table";
import { ResumePanel } from "./resume-panel";
import { useUploadQueue } from "./use-upload-queue";

const FINISHED = ["available", "rejected", "failed", "cancelled"];
const LARGE_BYTES = 1024 * MIB;

function sentBytes(snap: UploadSnapshot) {
  return ["finalizing", "verifying", "processing", "available", "rejected"].includes(snap.state) ? snap.fileSize : snap.sentBytes;
}

function Summary({ snapshots, waiting, needFix }: { snapshots: UploadSnapshot[]; waiting: number; needFix: number }) {
  const total = snapshots.reduce((sum, s) => sum + s.fileSize, 0);
  const sent = snapshots.reduce((sum, s) => sum + sentBytes(s), 0);
  const count = (...states: string[]) => snapshots.filter((s) => states.includes(s.state)).length;
  const ready = count("queued") - waiting - needFix;

  return (
    <div className="flex flex-col gap-4" aria-live="polite">
      <Meter label="All files uploaded" used={sent} limit={total} format={formatBytes} />
      <p className="text-sm text-muted">
        {formatNumber(snapshots.length)} {snapshots.length === 1 ? "file" : "files"}: {ready} ready to start, {waiting} waiting for a slot,{" "}
        {needFix} {needFix === 1 ? "needs" : "need"} a fix,{" "}
        {count("hashing", "uploading", "finalizing")} uploading,{" "}
        {count("verifying", "processing")} being checked by the server, {count("available")} available, {count("rejected")} rejected,{" "}
        {count("failed", "cancelled")} failed or cancelled.
      </p>
    </div>
  );
}

function LimitsPanel() {
  const quota = useQuota();

  return (
    <Panel title="Limits for this session" description="Set by your organization and enforced by the server.">
      {quota.isPending && <SkeletonRows rows={3} />}
      {quota.isError && (
        <p className="text-sm text-muted">
          {isApiError(quota.error) && quota.error.isForbidden
            ? "Your role cannot read organization limits. The server still refuses files over the limit."
            : "The limits could not be loaded. The server still enforces them."}
        </p>
      )}
      {quota.data && (
        <DefList
          items={[
            { label: "Largest file", value: formatBytes(quota.data.limits.max_object_bytes) },
            {
              label: "Uploads in progress",
              value: `${formatNumber(quota.data.usage.active_uploads)} of ${formatNumber(quota.data.limits.max_active_uploads)}`,
            },
            {
              label: "Multipart uploads in progress",
              value: `${formatNumber(quota.data.usage.active_multipart)} of ${formatNumber(quota.data.limits.max_active_multipart)}`,
            },
            { label: "Single request up to", value: formatBytes(SINGLE_LIMIT_BYTES) },
          ]}
        />
      )}
    </Panel>
  );
}

export function UploadsScreen() {
  const { organizationId, can } = useSession();
  const connections = useStorageConnections();
  const quota = useQuota();

  const list = connections.data?.connections ?? [];
  const defaultId = connections.data?.default_connection_id ?? "";

  const queue = useUploadQueue({
    connections: list,
    defaultConnectionId: defaultId,
    maxObjectBytes: quota.data?.limits.max_object_bytes,
  });
  const { entries, snapshots } = queue;
  const needFix = entries.filter(
    (entry, i) => snapshots[i]?.state === "queued" && Object.values(queue.errors.get(entry.id) ?? {}).some(Boolean),
  ).length;

  const canUpload = can("uploads.write");
  const anyLarge = snapshots.some((s) => s.fileSize > LARGE_BYTES);
  const anyMultipart = snapshots.some((s) => s.fileSize > SINGLE_LIMIT_BYTES);
  const runningIds = snapshots.flatMap((s) => (s.uploadId ? [s.uploadId] : []));

  return (
    <div className="grid gap-6 lg:grid-cols-3">
      <div className="flex min-w-0 flex-col gap-6 lg:col-span-2">
        {!canUpload && (
          <p role="alert" className="neo-in p-4 text-base">
            Your role does not include uploads. Ask an organization admin for the uploads.write permission.
          </p>
        )}
        <Dropzone onFiles={queue.addFiles} disabled={!canUpload} />

        {entries.length === 0 ? (
          <EmptyState title="No files yet">
            Add one file or many. Each file can get a dataset name, a catalog path and metadata before it starts. Small files go
            up in one request; large files are cut into parts and can be resumed after a reload.
          </EmptyState>
        ) : (
          <Panel
            title="Upload queue"
            actions={
              <>
                <Button
                  variant="secondary"
                  onClick={queue.clearFinished}
                  disabled={!snapshots.some((s) => FINISHED.includes(s.state))}
                >
                  Clear finished
                </Button>
                <Button variant="primary" onClick={queue.start} disabled={queue.startableCount === 0}>
                  {queue.waitingCount > 1 ? `Start ${queue.startableCount} uploads` : "Start upload"}
                </Button>
              </>
            }
          >
            <div className="flex flex-col gap-5">
              <Summary snapshots={snapshots} waiting={queue.waitingIds.length} needFix={needFix} />
              {(anyLarge || anyMultipart) && (
                <p className="neo-in-sm p-3 text-sm">
                  {anyLarge ? "Large files take a while. " : ""}Files over {formatBytes(SINGLE_LIMIT_BYTES)} upload in parts, four at a
                  time. Keep this tab open. If you close it, the upload stops, and you can continue it from Unfinished uploads
                  within about 30 minutes.
                </p>
              )}
              <QueueTable
                entries={entries}
                snapshots={snapshots}
                errors={queue.errors}
                connections={list}
                defaultConnectionId={defaultId}
                onDraft={queue.updateDraft}
                onRemove={queue.remove}
                waitingIds={queue.waitingIds}
              />
            </div>
          </Panel>
        )}
      </div>

      <div className="flex min-w-0 flex-col gap-6">
        <Panel title="Destination" description="Used for files you add next. Each file can override it.">
          {connections.isPending && <SkeletonRows rows={1} />}
          {connections.isError && (
            <p className="text-sm text-muted">
              {isApiError(connections.error) && connections.error.isForbidden
                ? "Your role cannot list storage connections, so files go to the organization default."
                : "Storage connections could not be loaded. Files go to the organization default."}
            </p>
          )}
          {connections.data && (
            <ConnectionSelect
              label="Storage connection"
              connections={list}
              defaultConnectionId={defaultId}
              value={queue.destination}
              onChange={queue.setDestination}
            />
          )}
        </Panel>
        <LimitsPanel />
        {organizationId && (
          <ResumePanel store={queue.store} organizationId={organizationId} runningIds={runningIds} onResume={queue.resume} />
        )}
      </div>
    </div>
  );
}
