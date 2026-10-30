"use client";

import { useQuery } from "@tanstack/react-query";
import { Button } from "@/components/ui/button";
import { DefList } from "@/components/ui/page";
import { Panel } from "@/components/ui/panel";
import { ErrorState, SkeletonRows } from "@/components/ui/states";
import { useOrganizationId } from "@/lib/auth/session";
import { orgKey } from "@/lib/query/keys";
import { formatBytes, formatNumber } from "@/lib/utils/format";
import { getShards, getSignedManifest, type SignedManifest } from "../api";

function saveManifest(manifest: SignedManifest, versionId: string) {
  const blob = new Blob([JSON.stringify(manifest, null, 2)], { type: "application/json" });
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download = `signed-manifest-${versionId}.json`;
  a.click();
  URL.revokeObjectURL(url);
}

// How a training job reads this version: a shard at a time, every file
// checked, and the signed manifest it can check the snapshot against.
export function ReadForTraining({ id, versionId }: { id: string; versionId: string }) {
  const organizationId = useOrganizationId();
  const shards = useQuery({
    queryKey: orgKey(organizationId, "streaming", versionId, "shards"),
    queryFn: ({ signal }) => getShards(id, versionId, signal),
    enabled: Boolean(organizationId),
  });
  const signed = useQuery({
    queryKey: orgKey(organizationId, "streaming", versionId, "signed-manifest"),
    queryFn: ({ signal }) => getSignedManifest(id, versionId, signal),
    enabled: Boolean(organizationId),
  });

  const list = shards.data?.shards ?? [];
  const more = Boolean(shards.data?.next_cursor);
  const files = list.reduce((n, s) => n + s.files, 0);
  const bytes = list.reduce((n, s) => n + s.bytes, 0);
  const packs = list.reduce((n, s) => n + s.packs, 0);
  const example = `from osp.stream import SnapshotStream

stream = SnapshotStream(client, "${id}", "${versionId}", shuffle_seed=0)
for sample in stream.samples("samples"):  # or "files"
    ...
stream.report(run_id)  # what was re-hashed, and on which host

# From a shell: check the signature and every file
osp verify ${id} ${versionId}`;

  return (
    <Panel
      title="Read for training"
      description="Data loaders read this version a shard at a time: a run of files in path order in one response, files packed together read from their pack at once, and every file checked against its SHA-256."
    >
      {shards.isPending ? (
        <SkeletonRows rows={3} />
      ) : shards.isError ? (
        <ErrorState error={shards.error} onRetry={() => shards.refetch()} />
      ) : (
        <div className="flex flex-col gap-4">
          <DefList
            items={[
              { label: "Shards", value: `${formatNumber(list.length)}${more ? " or more" : ""}` },
              {
                label: "Average shard",
                value: list.length ? `${formatBytes(bytes / list.length)}, ${formatNumber(Math.round(files / list.length))} files` : "None",
              },
              { label: "Packs read", value: packs ? formatNumber(packs) : "None: every file is read on its own" },
              {
                label: "Signature",
                value: signed.isPending ? (
                  "Checking…"
                ) : signed.isError ? (
                  <span className="text-muted">Could not be read</span>
                ) : signed.data?.signature ? (
                  `Signed with ${signed.data.algorithm} key ${signed.data.key_id}`
                ) : (
                  <span className="text-muted">Not signed: this platform has no signing key</span>
                ),
              },
            ]}
          />
          {signed.data && (
            <div>
              <Button onClick={() => saveManifest(signed.data, versionId)}>Download signed manifest</Button>
            </div>
          )}
          <pre className="neo-in-sm mono-data overflow-x-auto p-4 text-sm" data-lenis-prevent-horizontal>
            {example}
          </pre>
        </div>
      )}
    </Panel>
  );
}
