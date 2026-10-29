"use client";

import { useSearchParams } from "next/navigation";
import { ButtonLink } from "@/components/ui/button";
import { CopyButton } from "@/components/ui/copy-button";
import { PageHeader } from "@/components/ui/page";
import { Panel } from "@/components/ui/panel";
import { EmptyState, ErrorState, SkeletonRows } from "@/components/ui/states";
import { StateBadge } from "@/components/ui/status";
import { Time } from "@/components/ui/time";
import { isApiError } from "@/lib/api/errors";
import { useSession } from "@/lib/auth/session";
import { useObject } from "../hooks";
import { MetadataEditor } from "./metadata-editor";
import { DeleteObjectButton, RenameButton, VersioningToggle } from "./object-actions";
import { VersionDetail } from "./version-detail";
import { ObjectDatasets } from "./object-datasets";
import { VersionHistory } from "./version-history";

const crumbs = [{ label: "Catalog", href: "/app/catalog" }];

function BackToList() {
  return (
    <ButtonLink href="/app/catalog" variant="primary">
      Back to the catalog
    </ButtonLink>
  );
}

export function ObjectDetail({ id }: { id: string }) {
  const { can } = useSession();
  const selectedParam = Number(useSearchParams().get("version"));
  const selected = Number.isInteger(selectedParam) && selectedParam > 0 ? selectedParam : undefined;
  const query = useObject(id);

  if (query.isPending) {
    return (
      <>
        <PageHeader title="Loading object" crumbs={crumbs} />
        <SkeletonRows rows={6} />
      </>
    );
  }

  if (query.isError) {
    const missing = isApiError(query.error) && query.error.isNotFound;
    return (
      <>
        <PageHeader title={missing ? "Object not found" : "Object unavailable"} crumbs={crumbs} />
        {missing ? (
          <EmptyState title="This object does not exist, or was removed" action={<BackToList />}>
            The link may be old, or the object may belong to another organization. Open it again from the catalog.
          </EmptyState>
        ) : (
          <ErrorState error={query.error} onRetry={() => query.refetch()} />
        )}
      </>
    );
  }

  const object = query.data;
  const canWrite = can("catalog.write");
  const showSelected = selected !== undefined && selected !== object.current_version?.version;

  return (
    <>
      <PageHeader
        title={<span className="mono-data text-xl">{object.path}</span>}
        description={
          <>
            {object.dataset ? `Dataset ${object.dataset}` : "Root namespace"}. Revision {object.revision}.
          </>
        }
        crumbs={[...crumbs, { label: object.path }]}
        actions={
          <>
            {canWrite && <RenameButton object={object} />}
            {can("assets.delete") && !object.deleted_at && <DeleteObjectButton object={object} />}
          </>
        }
      />

      <div className="mb-6 flex flex-wrap items-center gap-3">
        <StateBadge tone={object.versioning === "ENABLED" ? "neutral" : "attention"}>
          Versioning {object.versioning}
        </StateBadge>
        {object.deleted_at && (
          <StateBadge tone="attention">Deleted</StateBadge>
        )}
        <span className="text-sm text-muted">
          Object ID <span className="mono-data">{object.id}</span>
        </span>
        <CopyButton value={object.id} label="Copy ID" />
      </div>

      {object.deleted_at && (
        <p role="note" className="neo-in-sm mb-6 p-4 text-base">
          This object was deleted on <Time value={object.deleted_at} />. Its path and history are still reserved, and its
          versions can be restored until their purge time. The next upload to this path becomes the next version.
        </p>
      )}

      <div className="grid gap-6 lg:grid-cols-3">
        <div className="flex min-w-0 flex-col gap-6 lg:col-span-2">
          <VersionDetail object={object} selector="current" title="Current version" />
          {showSelected && <VersionDetail object={object} selector={selected} title={`Version ${selected}`} />}
          <VersionHistory objectId={object.id} selected={selected ?? object.current_version?.version} />
        </div>

        <div className="flex min-w-0 flex-col gap-6">
          <MetadataEditor key={object.id} object={object} canEdit={canWrite} />

          <Panel title="Versioning" description="Whether new uploads add to the history or replace the current version.">
            <div className="flex flex-col gap-3">
              <p className="text-base">
                {object.versioning === "ENABLED"
                  ? "Every published version is kept, and any available version can be made current again."
                  : "A new upload replaces the current version. The replaced version is handed to deletion."}
              </p>
              {canWrite && <VersioningToggle object={object} />}
            </div>
          </Panel>

          <ObjectDatasets objectId={object.id} />

          <Panel title="Record">
            <dl className="grid gap-2 text-sm">
              <div>
                <dt className="text-muted">Created</dt>
                <dd>
                  <Time value={object.created_at} />
                </dd>
              </div>
              <div>
                <dt className="text-muted">Updated</dt>
                <dd>
                  <Time value={object.updated_at} />
                </dd>
              </div>
              <div>
                <dt className="text-muted">Versions published</dt>
                <dd>{object.versions}</dd>
              </div>
            </dl>
          </Panel>
        </div>
      </div>
    </>
  );
}
