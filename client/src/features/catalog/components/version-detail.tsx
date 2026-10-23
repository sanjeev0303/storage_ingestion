"use client";

import Link from "next/link";
import { useState } from "react";
import { Button } from "@/components/ui/button";
import { CopyButton } from "@/components/ui/copy-button";
import { ConfirmDialog } from "@/components/ui/dialog";
import { DefList } from "@/components/ui/page";
import { Panel } from "@/components/ui/panel";
import { ErrorState, SkeletonRows } from "@/components/ui/states";
import { StateBadge } from "@/components/ui/status";
import { Time } from "@/components/ui/time";
import { notify } from "@/components/ui/toast";
import { describeError, isApiError } from "@/lib/api/errors";
import { useSession } from "@/lib/auth/session";
import { formatBytes } from "@/lib/utils/format";
import { useConnectionNames, useDeleteVersion, useLifecycle, usePromote, useVersion } from "../hooks";
import { isTombstone } from "../preview";
import type { CatalogObject, ObjectVersion } from "../types";
import { DownloadControl } from "./download-control";
import { Preview } from "./preview";

export function statusLabel(version: ObjectVersion) {
  if (version.status === "SOFT_DELETED") return "Soft-deleted";
  if (version.status === "DELETED") return "Deleted";
  if (version.status === "AVAILABLE") return "Available";
  return version.status.charAt(0) + version.status.slice(1).toLowerCase();
}

function Mono({ value }: { value: string }) {
  return (
    <span className="flex flex-wrap items-center gap-2">
      <span className="mono-data">{value}</span>
      <CopyButton value={value} />
    </span>
  );
}

function Evidence({ version, connectionId }: { version: ObjectVersion; connectionId?: string }) {
  const names = useConnectionNames();
  const connection = connectionId ? (names.data?.[connectionId] ?? connectionId) : undefined;
  return (
    <DefList
      items={[
        { label: "Version", value: `${version.version}${version.current ? " (current)" : ""}` },
        { label: "Status", value: <StateBadge tone={isTombstone(version.status) ? "attention" : "neutral"}>{statusLabel(version)}</StateBadge> },
        { label: "Size", value: formatBytes(version.size_bytes) },
        { label: "Content type", value: version.content_type },
        {
          label: "SHA-256 checksum",
          value: version.checksum_sha256 ? <Mono value={version.checksum_sha256} /> : <span className="text-muted">Not recorded</span>,
        },
        { label: "Published", value: <Time value={version.published_at} /> },
        {
          label: "Provider ETag (store validator, not a checksum)",
          value: version.provider_etag ? <span className="mono-data">{version.provider_etag}</span> : <span className="text-muted">None</span>,
        },
        ...(version.provider_version_id
          ? [{ label: "Provider version ID", value: <span className="mono-data">{version.provider_version_id}</span> }]
          : []),
        ...(connection ? [{ label: "Storage connection", value: connection }] : []),
        { label: "File name", value: version.file_name },
        { label: "Version ID", value: <Mono value={version.asset_id} /> },
      ]}
    />
  );
}

function Lifecycle({ version }: { version: ObjectVersion }) {
  const lifecycle = useLifecycle(version.asset_id);
  if (version.status === "DELETED") {
    return <p className="text-sm text-muted">The bytes of this version have been purged. Nothing can restore it.</p>;
  }
  if (lifecycle.isPending) return <SkeletonRows rows={2} />;
  if (lifecycle.isError) {
    const gone = isApiError(lifecycle.error) && lifecycle.error.isNotFound;
    return <p className="text-sm text-muted">{gone ? "No lifecycle record for this version." : describeError(lifecycle.error)}</p>;
  }
  const { retain_until, expires_at, deletion } = lifecycle.data;
  return (
    <div className="flex flex-col gap-3">
      <DefList
        items={[
          { label: "Retained until", value: <Time value={retain_until} /> },
          { label: "Expires at", value: expires_at ? <Time value={expires_at} /> : <span className="text-muted">No expiry</span> },
          ...(deletion
            ? [
                { label: "Deletion state", value: deletion.state.replaceAll("_", " ").toLowerCase() },
                { label: "Bytes removed no earlier than", value: <Time value={deletion.purge_after} /> },
                { label: "Restorable", value: deletion.restorable ? "Yes, until the purge time" : "No" },
              ]
            : []),
        ]}
      />
      {deletion && (
        <p className="text-sm text-muted">
          Retention rules or a hold can keep the bytes longer than the time shown. The catalog does not reveal holds.
        </p>
      )}
    </div>
  );
}

export function VersionDetail({
  object,
  selector,
  title,
}: {
  object: CatalogObject;
  selector: number | "current";
  title: string;
}) {
  const { can } = useSession();
  const query = useVersion(object.id, selector);
  const promote = usePromote(object.id);
  const remove = useDeleteVersion(object.id);
  const [confirmPromote, setConfirmPromote] = useState(false);
  const [confirmDelete, setConfirmDelete] = useState(false);

  if (query.isPending) {
    return (
      <Panel title={title}>
        <SkeletonRows rows={4} />
      </Panel>
    );
  }
  if (query.isError) {
    if (isApiError(query.error) && query.error.isNotFound) {
      return (
        <Panel title={title}>
          <p className="text-base text-muted">
            {selector === "current"
              ? "This object has no current version. It may be deleted, or none of its uploads has been published yet."
              : `Version ${selector} does not exist on this object.`}
          </p>
        </Panel>
      );
    }
    return (
      <Panel title={title}>
        <ErrorState error={query.error} onRetry={() => query.refetch()} />
      </Panel>
    );
  }

  const version = query.data;
  const tombstone = isTombstone(version.status);
  const available = version.status === "AVAILABLE";

  return (
    <Panel title={title}>
      <div className="flex flex-col gap-5">
        {tombstone && (
          <p role="note" className="neo-in-sm p-4 text-base">
            {version.status === "SOFT_DELETED"
              ? "This version is soft-deleted. It stays in the history but cannot be downloaded or previewed. It can be restored until its purge time."
              : "This version was deleted and its bytes purged. It stays in the history as a record only."}
          </p>
        )}

        <Evidence version={version} connectionId={version.current ? object.current_version?.storage_connection_id : undefined} />

        <section aria-label="Retention and deletion">
          <h3 className="mb-2 text-md font-semibold">Retention and deletion</h3>
          <Lifecycle version={version} />
        </section>

        {available && (
          <section aria-label="Download and preview" className="flex flex-col gap-4">
            <h3 className="text-md font-semibold">Download and preview</h3>
            <DownloadControl
              assetId={version.asset_id}
              fileName={version.file_name}
              canDownload={can("assets.download")}
              canRevoke={can("assets.delete")}
            />
            {can("assets.download") && (
              <Preview
                assetId={version.asset_id}
                contentType={version.content_type}
                sizeBytes={version.size_bytes}
                status={version.status}
              />
            )}
          </section>
        )}

        <div className="flex flex-wrap gap-2">
          {available && !version.current && can("catalog.write") && (
            <Button onClick={() => setConfirmPromote(true)}>Make this the current version</Button>
          )}
          {version.status === "SOFT_DELETED" && can("assets.delete") && (
            <Link
              href={`/app/operations/assets?id=${encodeURIComponent(version.asset_id)}`}
              className="inline-flex h-9 items-center rounded-md px-4 text-base font-medium underline underline-offset-2"
            >
              Restore in Operations
            </Link>
          )}
          {!tombstone && can("assets.delete") && (
            <Button variant="accent" onClick={() => setConfirmDelete(true)}>
              Delete this version
            </Button>
          )}
        </div>
      </div>

      <ConfirmDialog
        open={confirmPromote}
        onOpenChange={setConfirmPromote}
        title={`Make version ${version.version} current`}
        description="Readers of this path will get this version from now on. Later versions stay in the history."
        confirmLabel="Make current"
        onConfirm={async () => {
          try {
            await promote.mutateAsync({ revision: object.revision, version: version.version });
          } catch (e) {
            if (isApiError(e) && e.status === 412) {
              throw new Error("The object changed while you were looking at it. It has been reloaded; try again.", { cause: e });
            }
            throw e;
          }
          notify.success(`Version ${version.version} is now current`);
        }}
      />

      <ConfirmDialog
        open={confirmDelete}
        onOpenChange={setConfirmDelete}
        title={`Delete version ${version.version}`}
        confirmText={object.path}
        confirmLabel="Delete version"
        description={
          <div className="flex flex-col gap-2">
            <p>
              This soft-deletes the version. It stays restorable until its purge time, which comes from the
              organization&apos;s deletion grace period (7 days by default).
            </p>
            <p>
              Retention rules or a legal hold may keep its bytes for longer than you request here.
              {version.current && " It is the current version, so the newest remaining available version becomes current."}
            </p>
          </div>
        }
        onConfirm={async () => {
          await remove.mutateAsync(version.version);
          notify.success(`Version ${version.version} deleted. It is restorable until its purge time.`);
        }}
      />
    </Panel>
  );
}
