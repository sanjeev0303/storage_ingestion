"use client";

import { useState, type FormEvent, type ReactNode } from "react";
import { Button } from "@/components/ui/button";
import { CopyButton } from "@/components/ui/copy-button";
import { ConfirmDialog } from "@/components/ui/dialog";
import { Field, Input } from "@/components/ui/field";
import { DefList } from "@/components/ui/page";
import { Panel } from "@/components/ui/panel";
import { EmptyState, ErrorState, PermissionDenied, SkeletonRows } from "@/components/ui/states";
import { StateBadge } from "@/components/ui/status";
import { SubmitButton } from "@/components/ui/submit-button";
import { Time } from "@/components/ui/time";
import { useSession } from "@/lib/auth/session";
import { formatBytes } from "@/lib/utils/format";
import { isNotFound, useAsset, useAssetActions, useLifecycle, useProcessing } from "../hooks";
import type { Deletion, ProcessingRecord } from "../types";

function statusTone(status: string) {
  if (status === "AVAILABLE") return "done";
  if (status === "REJECTED" || status === "SOFT_DELETED") return "attention";
  return "active";
}

export function AssetsScreen({ initialId = "" }: { initialId?: string }) {
  const [draft, setDraft] = useState(initialId);
  const [id, setId] = useState(initialId.trim());
  // Ids pasted with spaces or line breaks still work.
  const next = draft.replace(/\s+/g, "");

  function lookUp(e: FormEvent) {
    e.preventDefault();
    if (next === "") return;
    setId(next);
    // keep the address shareable without a navigation
    window.history.replaceState(null, "", next ? `?id=${encodeURIComponent(next)}` : window.location.pathname);
  }

  return (
    <div className="flex flex-col gap-6">
      <form onSubmit={lookUp} noValidate className="neo flex flex-wrap items-end gap-3 p-5">
        <Field
          label="Asset id"
          required
          error={next === "" ? "Enter the id of an asset." : undefined}
          className="min-w-64 flex-1"
          hint="Copy it from the catalog or an ingestion entry. Deleted assets stay findable until they are purged."
        >
          {(props) => (
            <Input
              {...props}
              value={draft}
              onChange={(e) => setDraft(e.target.value)}
              className="mono-data"
              autoComplete="off"
              spellCheck={false}
            />
          )}
        </Field>
        <SubmitButton valid={next !== ""} hint="Enter an asset id to look it up.">
          Look up asset
        </SubmitButton>
      </form>

      {id === "" ? (
        <EmptyState title="Look up an asset to inspect or change its lifecycle">
          You will see its status, whether it can be delivered, its retention dates and any quarantine record. From
          here you can schedule it for deletion, restore it during the grace period, or ask for a held asset to be
          processed again.
        </EmptyState>
      ) : (
        <AssetDetail key={id} id={id} />
      )}
    </div>
  );
}

function AssetDetail({ id }: { id: string }) {
  const { can } = useSession();
  const asset = useAsset(id);
  const lifecycle = useLifecycle(id, asset.isSuccess);
  const canReadQuarantine = can("quarantine.read");
  const processing = useProcessing(id, asset.isSuccess && canReadQuarantine);
  const actions = useAssetActions(id);
  const [confirm, setConfirm] = useState<"delete" | "restore" | "release" | null>(null);

  if (asset.isPending) return <SkeletonRows rows={6} />;
  if (asset.isError) {
    return (
      <ErrorState
        error={asset.error}
        title={isNotFound(asset.error) ? "No asset with that id" : undefined}
        onRetry={() => asset.refetch()}
      />
    );
  }

  const a = asset.data;
  const deletion = lifecycle.data?.deletion;
  const canHaveRelease = a.status === "QUARANTINED" || a.status === "PROCESSING";

  return (
    <div className="grid gap-6 lg:grid-cols-3">
      <div className="flex min-w-0 flex-col gap-6 lg:col-span-2">
        <Panel
          title={a.file_name}
          description={a.content_type}
          actions={<StateBadge tone={statusTone(a.status)}>{a.status}</StateBadge>}
        >
          <DefList
            items={[
              {
                label: "Id",
                value: (
                  <span className="flex flex-wrap items-center gap-2">
                    <span className="mono-data">{a.id}</span>
                    <CopyButton value={a.id} />
                  </span>
                ),
              },
              { label: "Deliverable", value: a.deliverable ? "Yes, downloads are served" : "No, downloads are refused" },
              { label: "Size", value: formatBytes(a.size_bytes) },
              { label: "Dataset", value: a.dataset ?? "None" },
              {
                label: "SHA-256",
                value: a.checksum_sha256 ? <span className="mono-data">{a.checksum_sha256}</span> : "Not recorded",
              },
              { label: "Published", value: <Time value={a.published_at} /> },
              { label: "Created", value: <Time value={a.created_at} /> },
              { label: "Updated", value: <Time value={a.updated_at} /> },
              ...(a.rejection_reason ? [{ label: "Rejection reason", value: a.rejection_reason }] : []),
            ]}
          />
        </Panel>

        <Panel title="Lifecycle" description="Retention dates and the state of any scheduled deletion.">
          {lifecycle.isPending ? (
            <SkeletonRows rows={3} />
          ) : lifecycle.isError ? (
            <ErrorState error={lifecycle.error} onRetry={() => lifecycle.refetch()} />
          ) : (
            <>
              <DefList
                items={[
                  { label: "Retained until", value: <Time value={lifecycle.data.retain_until} /> },
                  {
                    label: "Expires at",
                    value: lifecycle.data.expires_at ? <Time value={lifecycle.data.expires_at} /> : "No expiry",
                  },
                  { label: "Deletion grace period", value: formatGrace(lifecycle.data.deletion_grace_seconds) },
                  ...(lifecycle.data.locked_until
                    ? [
                        {
                          label: "Locked at the provider until",
                          value: (
                            <span>
                              <Time value={lifecycle.data.locked_until} />
                              <span className="block text-sm text-muted">
                                S3 Object Lock: the bucket itself refuses to delete this version earlier.
                              </span>
                            </span>
                          ),
                        },
                      ]
                    : []),
                  { label: "Deletion", value: deletion ? <DeletionSummary deletion={deletion} /> : "Not scheduled" },
                ]}
              />
              <p className="mt-4 max-w-prose text-sm text-muted">
                Legal holds are not shown to organization owners. A hold can keep an asset after the dates above.
              </p>
            </>
          )}
        </Panel>

        <ProcessingPanel
          allowed={canReadQuarantine}
          isPending={processing.isPending && canReadQuarantine}
          error={processing.error}
          data={processing.data}
          onRetry={() => processing.refetch()}
        />
      </div>

      <div className="flex min-w-0 flex-col gap-6">
        <Panel title="Actions" description="Each action is shown as done only after the API confirms it.">
          <div className="flex flex-col items-start gap-3">
            {a.status === "SOFT_DELETED" ? (
              <Button
                onClick={() => setConfirm("restore")}
                disabled={!can("assets.delete") || deletion?.restorable === false}
              >
                Restore asset
              </Button>
            ) : (
              <Button onClick={() => setConfirm("delete")} disabled={!can("assets.delete")}>
                Delete asset
              </Button>
            )}
            {!can("assets.delete") && <p className="text-sm text-muted">Deleting and restoring need assets.delete.</p>}
            {a.status === "SOFT_DELETED" && deletion?.restorable === false && (
              <p className="text-sm text-muted">The deletion is past the point where it can be restored.</p>
            )}

            {canHaveRelease && (
              <>
                <Button onClick={() => setConfirm("release")} disabled={!can("quarantine.release")}>
                  Release from quarantine
                </Button>
                <p className="text-sm text-muted">
                  {can("quarantine.release")
                    ? "This queues processing again. The malware scan still decides whether the asset becomes available."
                    : "Releasing needs quarantine.release."}
                </p>
              </>
            )}
          </div>

          {actions.remove.data && (
            <ActionResult title={actions.remove.data.created ? "Deletion scheduled" : "Already scheduled for deletion"}>
              <DeletionSummary deletion={actions.remove.data.deletion} />
            </ActionResult>
          )}
          {actions.restore.data && (
            <ActionResult title="Restored">
              Status returned to <StateBadge tone={statusTone(actions.restore.data.status)}>{actions.restore.data.status}</StateBadge>
            </ActionResult>
          )}
          {actions.release.data && (
            <ActionResult title="Release accepted">
              Processing job is <span className="mono-data">{actions.release.data.job_state}</span> at stage{" "}
              <span className="mono-data">{actions.release.data.stage}</span>. The scan has not finished yet.
            </ActionResult>
          )}
        </Panel>
      </div>

      <ConfirmDialog
        open={confirm === "delete"}
        onOpenChange={(open) => !open && setConfirm(null)}
        title="Delete this asset?"
        description={
          <>
            <span className="mono-data">{a.file_name}</span> stops being served and is scheduled for purge once the
            deletion grace period ends
            {lifecycle.data ? ` (${formatGrace(lifecycle.data.deletion_grace_seconds)})` : ""}. Until then you can
            restore it. After the purge its stored bytes are removed for good.
          </>
        }
        confirmLabel="Schedule deletion"
        onConfirm={() => actions.remove.mutateAsync()}
      />
      <ConfirmDialog
        open={confirm === "restore"}
        onOpenChange={(open) => !open && setConfirm(null)}
        title="Restore this asset?"
        description="The asset returns to the status it had before deletion and is served again, if its objects are still in storage."
        confirmLabel="Restore asset"
        onConfirm={() => actions.restore.mutateAsync()}
      />
      <ConfirmDialog
        open={confirm === "release"}
        onOpenChange={(open) => !open && setConfirm(null)}
        title="Release from quarantine?"
        description="This requeues the processing job for this asset. Verification and the malware scan run again, and an asset that fails them stays unavailable."
        confirmLabel="Release"
        onConfirm={() => actions.release.mutateAsync()}
      />
    </div>
  );
}

function ActionResult({ title, children }: { title: string; children: ReactNode }) {
  return (
    <div role="status" className="neo-in-sm mt-4 p-3 text-base">
      <p className="font-semibold">{title}</p>
      <div className="mt-1 text-muted">{children}</div>
    </div>
  );
}

function DeletionSummary({ deletion }: { deletion: Deletion }) {
  return (
    <span className="flex flex-col gap-1">
      <span>
        <StateBadge tone={deletion.state === "FAILED" ? "attention" : "active"}>{deletion.state}</StateBadge>
      </span>
      <span>
        Purge after <Time value={deletion.purge_after} />
      </span>
      <span>{deletion.restorable ? "Can be restored until then" : "Can no longer be restored"}</span>
    </span>
  );
}

function ProcessingPanel({
  allowed,
  isPending,
  error,
  data,
  onRetry,
}: {
  allowed: boolean;
  isPending: boolean;
  error: unknown;
  data: ReturnType<typeof useProcessing>["data"];
  onRetry: () => void;
}) {
  return (
    <Panel title="Processing and quarantine" description="The verification and scan job that decides whether an upload becomes available.">
      {!allowed ? (
        <PermissionDenied detail="Reading the processing record needs the quarantine.read permission." />
      ) : isPending ? (
        <SkeletonRows rows={3} />
      ) : error && isNotFound(error) ? (
        <EmptyState title="No processing record">
          Only assets that went through the upload pipeline have one. Assets loaded by bulk ingestion, or assets that
          finished long ago, can have none. This is expected and not an error.
        </EmptyState>
      ) : error ? (
        <ErrorState error={error} onRetry={onRetry} />
      ) : data ? (
        <div className="flex flex-col gap-4">
          <DefList
            items={[
              { label: "Job state", value: <StateBadge tone={data.job_state === "SUCCEEDED" ? "done" : data.job_state === "PENDING" ? "active" : "attention"}>{data.job_state}</StateBadge> },
              { label: "Stage", value: data.job_stage },
              { label: "Attempts", value: data.attempts },
              ...(data.last_error ? [{ label: "Last error", value: data.last_error }] : []),
              ...(data.scan ? [{ label: "Malware scan", value: <ScanVerdict scan={data.scan} /> }] : []),
            ]}
          />
          {data.events.length > 0 && (
            <details>
              <summary className="cursor-pointer text-base">Stage history ({data.events.length})</summary>
              <ol className="mt-2 flex flex-col gap-2">
                {data.events.map((e, i) => (
                  <li key={i} className="text-sm">
                    <span className="mono-data">{e.event}</span> <Time value={e.at} />
                    {e.detail && <span className="block text-muted [overflow-wrap:anywhere]">{e.detail}</span>}
                  </li>
                ))}
              </ol>
            </details>
          )}
          {data.quarantine.length === 0 ? (
            <p className="text-base text-muted">Nothing is held in quarantine for this asset.</p>
          ) : (
            <ul className="flex flex-col gap-3">
              {data.quarantine.map((q) => (
                <li key={q.id} className="neo-in-sm p-3">
                  <div className="flex flex-wrap items-center gap-2">
                    <StateBadge tone={q.status === "HELD" ? "attention" : "neutral"}>{q.status}</StateBadge>
                    <span className="font-medium">{q.category.replaceAll("_", " ").toLowerCase()}</span>
                  </div>
                  <p className="mt-1 [overflow-wrap:anywhere]">{q.reason}</p>
                  <p className="mt-1 text-sm text-muted">
                    Scan result <span className="mono-data">{q.scan_result}</span>, since <Time value={q.created_at} />
                    {q.retain_until && (
                      <>
                        , kept until <Time value={q.retain_until} />
                      </>
                    )}
                  </p>
                </li>
              ))}
            </ul>
          )}
        </div>
      ) : null}
    </Panel>
  );
}

function formatGrace(seconds: number) {
  const hours = seconds / 3600;
  if (hours >= 48) return `${Math.round(hours)} hours (${Math.round(hours / 24)} days)`;
  return `${Math.round(hours * 10) / 10} hours`;
}

function ScanVerdict({ scan }: { scan: NonNullable<ProcessingRecord["scan"]> }) {
  return (
    <span>
      <StateBadge tone={scan.result === "CLEAN" ? "done" : "failed"}>{scan.result}</StateBadge>{" "}
      <span className="text-sm">
        by {scan.scanner}
        {scan.scanner_version && <span className="mono-data"> {scan.scanner_version}</span>}
      </span>
      {scan.reused_from && (
        <span className="block text-sm text-muted">
          Not scanned again: asset <span className="mono-data">{scan.reused_from}</span> has the same bytes and was scanned clean by
          the same engine version.
        </span>
      )}
    </span>
  );
}
