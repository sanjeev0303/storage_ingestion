"use client";

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { Button } from "@/components/ui/button";
import { ConfirmDialog } from "@/components/ui/dialog";
import { Field, Select } from "@/components/ui/field";
import { DefList } from "@/components/ui/page";
import { Panel } from "@/components/ui/panel";
import { ErrorState, SkeletonRows } from "@/components/ui/states";
import { StateBadge } from "@/components/ui/status";
import { notify } from "@/components/ui/toast";
import { useVersions } from "@/features/datasets/hooks";
import { useOrganizationId, useSession } from "@/lib/auth/session";
import { orgKey } from "@/lib/query/keys";
import { formatBytes, formatNumber } from "@/lib/utils/format";
import { chunkDelta, chunkSavings, chunkStatus, chunkStorage, indexVersion, rehydrate, setChunking, setChunkStorage } from "../api";

const STATE_TEXT = {
  NONE: { text: "Not indexed", tone: "neutral" },
  PENDING: { text: "Waiting", tone: "active" },
  RUNNING: { text: "Indexing", tone: "active" },
  DONE: { text: "Indexed", tone: "done" },
  FAILED: { text: "Failed", tone: "failed" },
} as const;

function percent(part: number, whole: number) {
  if (whole <= 0) return "0%";
  return `${Math.round((part / whole) * 100)}%`;
}

// What storing each chunk once would save, and what moving between two
// versions fetches.
export function VersionChunks({ id, versionId, version }: { id: string; versionId: string; version: number }) {
  const organizationId = useOrganizationId();
  const { can } = useSession();
  const client = useQueryClient();
  const key = orgKey(organizationId, "chunks", id);
  const refresh = () => client.invalidateQueries({ queryKey: key });
  const savings = useQuery({ queryKey: [...key, "savings"], queryFn: ({ signal }) => chunkSavings(id, signal), enabled: Boolean(organizationId) });
  const status = useQuery({
    queryKey: [...key, "status", versionId],
    queryFn: ({ signal }) => chunkStatus(id, versionId, signal),
    enabled: Boolean(organizationId),
    refetchInterval: (q) => (q.state.data?.state === "PENDING" || q.state.data?.state === "RUNNING" ? 5000 : false),
  });
  const versions = useVersions(id);
  const older = versions.items.filter((v) => v.version < version);
  const [picked, setPicked] = useState("");
  const from = picked || older[0]?.id || "";
  const delta = useQuery({
    queryKey: [...key, "delta", versionId, from],
    queryFn: ({ signal }) => chunkDelta(id, versionId, from, signal),
    enabled: Boolean(organizationId && from),
  });
  const stored = useQuery({ queryKey: [...key, "storage"], queryFn: ({ signal }) => chunkStorage(id, signal), enabled: Boolean(organizationId) });
  const [turningOn, setTurningOn] = useState(false);
  const [restoring, setRestoring] = useState(false);
  const stopStoring = useMutation({ mutationFn: () => setChunkStorage(id, false), onSuccess: refresh, onError: notify.error });
  const toggle = useMutation({ mutationFn: (on: boolean) => setChunking(id, on), onSuccess: refresh, onError: notify.error });
  const index = useMutation({ mutationFn: () => indexVersion(id, versionId), onSuccess: refresh, onError: notify.error });

  const s = savings.data;
  const held = stored.data;
  const job = status.data;
  const state = STATE_TEXT[job?.state ?? "NONE"];
  return (
    <Panel
      title="Chunk savings"
      description="Splits files at content-defined boundaries to measure what storing each chunk once would save, and lets the SDK fetch a new version as the difference from a copy of an old one. Files stay stored whole."
    >
      {savings.isPending ? (
        <SkeletonRows rows={2} />
      ) : savings.isError ? (
        <ErrorState error={savings.error} onRetry={() => savings.refetch()} />
      ) : (
        s && (
          <div className="flex flex-col gap-5">
            <div className="flex flex-wrap items-center gap-3">
              <StateBadge tone={state.tone}>{state.text}</StateBadge>
              {job && job.state === "DONE" && (
                <span className="text-sm text-muted tabular-nums">
                  {formatNumber(job.files_indexed)} files, {formatBytes(job.bytes_read)} read
                </span>
              )}
              {can("datasets.manage") && (
                <>
                  <Button variant="secondary" onClick={() => toggle.mutate(!s.enabled)} loading={toggle.isPending}>
                    {s.enabled ? "Stop indexing new versions" : "Index this dataset"}
                  </Button>
                  {!s.enabled && (
                    <Button variant="ghost" onClick={() => index.mutate()} loading={index.isPending}>
                      Index this version only
                    </Button>
                  )}
                </>
              )}
            </div>
            {!s.enabled && <p className="max-w-prose text-base text-muted">Indexing reads each file once. On a bucket of your own, that costs what reading it costs.</p>}
            {job && job.skipped.length > 0 && (
              <div className="text-sm">
                <p>{`${formatNumber(job.skipped.length)} files were not indexed:`}</p>
                <ul className="mt-1 text-muted">
                  {job.skipped.slice(0, 5).map((f) => (
                    <li key={f.path} className="break-all">
                      <span className="mono-data">{f.path}</span>: {f.reason}
                    </li>
                  ))}
                </ul>
              </div>
            )}
            {s.indexed_files > 0 && (
              <DefList
                items={[
                  { label: "Each distinct file once, as stored today", value: formatBytes(s.indexed_file_bytes) },
                  { label: "Each distinct chunk once", value: formatBytes(s.unique_chunk_bytes) },
                  {
                    label: "Chunk dedup would save",
                    value: `${formatBytes(s.would_save_bytes)} (${percent(s.would_save_bytes, s.indexed_file_bytes)})`,
                  },
                  ...(s.not_indexed_files > 0
                    ? [{ label: "Not indexed yet", value: `${formatNumber(s.not_indexed_files)} files, ${formatBytes(s.not_indexed_bytes)}` }]
                    : []),
                ]}
              />
            )}
            {held && (
              <div className="flex flex-col gap-3">
                <h3 className="text-base font-medium">Store chunks once</h3>
                {held.files > 0 ? (
                  <DefList
                    items={[
                      { label: "Files kept as chunks", value: `${formatNumber(held.files)}, ${formatBytes(held.logical_bytes)}` },
                      { label: "Space their chunks take", value: `${formatBytes(held.chunk_bytes)} in ${formatNumber(held.chunk_objects)} objects` },
                      { label: "Originals removed so far", value: formatBytes(held.retired_source_bytes) },
                      ...(held.rehydrating > 0 ? [{ label: "Becoming whole objects again", value: formatNumber(held.rehydrating) }] : []),
                    ]}
                  />
                ) : (
                  <p className="max-w-prose text-base text-muted">
                    {held.enabled ? "On. Files sharing enough with others are converted a few at a time." : "Off. Every file is one object."}
                  </p>
                )}
                {can("datasets.manage") && (
                  <div className="flex flex-wrap gap-2">
                    {held.enabled ? (
                      <Button variant="secondary" onClick={() => stopStoring.mutate()} loading={stopStoring.isPending}>
                        Stop converting
                      </Button>
                    ) : (
                      <Button variant="secondary" onClick={() => setTurningOn(true)}>
                        Store chunks once
                      </Button>
                    )}
                    {held.files > 0 && (
                      <Button variant="ghost" onClick={() => setRestoring(true)}>
                        Make whole objects again
                      </Button>
                    )}
                  </div>
                )}
              </div>
            )}
            {older.length > 0 && (
              <div className="flex flex-col gap-3">
                <Field label="Compare with">
                  {(f) => (
                    <Select {...f} value={from} onChange={(e) => setPicked(e.target.value)}>
                      {older.map((v) => (
                        <option key={v.id} value={v.id}>
                          Version {v.version}
                        </option>
                      ))}
                    </Select>
                  )}
                </Field>
                {delta.isError ? (
                  <ErrorState error={delta.error} onRetry={() => delta.refetch()} />
                ) : (
                  delta.data && (
                    <>
                      <p className="text-base tabular-nums">
                        A delta download fetches {formatBytes(delta.data.fetch_bytes)} of {formatBytes(delta.data.total_bytes)}: {formatNumber(delta.data.unchanged_files)} files are
                        unchanged, and {formatBytes(delta.data.reused_bytes)} of the {formatNumber(delta.data.changed_files)} changed files are in chunks you already hold.
                      </p>
                      {delta.data.not_indexed_files > 0 && (
                        <p className="text-sm text-muted">{`${formatNumber(delta.data.not_indexed_files)} changed files are not indexed, and would be fetched whole.`}</p>
                      )}
                    </>
                  )
                )}
              </div>
            )}
          </div>
        )
      )}
      <ConfirmDialog
        open={turningOn}
        onOpenChange={setTurningOn}
        title="Store this dataset's chunks once?"
        description="Files sharing at least a fifth of their bytes with others are kept as chunks, and read through the API instead of a direct storage link. Reads of those files take more requests. Their originals are removed after a day. Files under a hold or lock stay whole, and you can undo this."
        confirmLabel="Store chunks once"
        onConfirm={async () => {
          await setChunkStorage(id, true);
          refresh();
        }}
      />
      <ConfirmDialog
        open={restoring}
        onOpenChange={setRestoring}
        title="Make these files whole objects again?"
        description="Each file is written back as one object, which takes the space chunks were saving. New conversions stop."
        confirmLabel="Make whole again"
        onConfirm={async () => {
          await rehydrate(id);
          refresh();
        }}
      />
    </Panel>
  );
}
