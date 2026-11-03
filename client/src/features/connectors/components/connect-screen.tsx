"use client";

import { useQuery } from "@tanstack/react-query";
import { CopyButton } from "@/components/ui/copy-button";
import { Panel } from "@/components/ui/panel";
import { EmptyState, ErrorState, SkeletonRows } from "@/components/ui/states";
import { StateBadge } from "@/components/ui/status";
import { Table, Td, Th } from "@/components/ui/table";
import { Time } from "@/components/ui/time";
import { useDataset } from "@/features/datasets/hooks";
import { listUpstream, type UpstreamSource } from "@/features/domain/api";
import { useOrganizationId } from "@/lib/auth/session";
import { orgKey } from "@/lib/query/keys";
import { formatNumber } from "@/lib/utils/format";
import { listCollections, type Collection } from "../api";

const FORMAT_TEXT: Record<Collection["format"], string> = {
  COMMS_JSONL: "Comms archive",
  RSMF: "RSMF",
  UFDR: "Cellebrite UFDR",
  MANIFEST: "Hash manifest",
};

function Snippet({ text }: { text: string }) {
  return (
    <div className="neo-in-sm flex items-start justify-between gap-3 p-3">
      <pre className="mono-data min-w-0 overflow-x-auto text-sm whitespace-pre" translate="no">
        {text}
      </pre>
      <CopyButton value={text} variant="ghost" />
    </div>
  );
}

const UPSTREAM_STATE: Record<UpstreamSource["state"], { text: string; tone: "done" | "warning" | "failed" }> = {
  WATCHING: { text: "Unchanged", tone: "done" },
  CHANGED: { text: "Changed", tone: "warning" },
  GONE: { text: "Gone", tone: "failed" },
};

// How the tools around the platform reach this dataset, and the exports
// brought into it.
export function ConnectScreen({ id }: { id: string }) {
  const organizationId = useOrganizationId();
  const dataset = useDataset(id);
  const collections = useQuery({
    queryKey: orgKey(organizationId, "connectors", "collections", id),
    queryFn: ({ signal }) => listCollections(id, signal),
    enabled: Boolean(organizationId),
  });
  const upstream = useQuery({
    queryKey: orgKey(organizationId, "connectors", "upstream", id),
    queryFn: ({ signal }) => listUpstream(id, signal),
    enabled: Boolean(organizationId),
  });
  const sources = upstream.data?.sources ?? [];
  const name = dataset.data?.name ?? "DATASET";
  const list = collections.data?.collections ?? [];
  return (
    <div className="flex flex-col gap-6">
      <Panel
        title="Git LFS"
        description="Keep a repository's large files in this dataset. Use an API token as the password when Git asks; $OSP_API is your API's address."
      >
        <div className="flex flex-col gap-3">
          <Snippet text={`git lfs install\ngit config -f .lfsconfig lfs.url $OSP_API/lfs/${name}\ngit lfs track "*.psd" "*.pak"`} />
          <p className="text-base">Promote what is at HEAD to a release that cannot change:</p>
          <Snippet text={`osp lfs-promote ${name} v1.2.0 --hold SHIP-1.2.0`} />
        </div>
      </Panel>
      <Panel title="Imported exports" description="Comms-archive and forensic exports, with what each said it held and what the import found.">
        <div className="flex flex-col gap-4">
          <Snippet text={`osp import-export RSMF slack-export.rsmf ${name} --retain 61320h --lock COMPLIANCE`} />
          {collections.isPending ? (
            <SkeletonRows rows={2} />
          ) : collections.isError ? (
            <ErrorState error={collections.error} onRetry={() => collections.refetch()} />
          ) : list.length === 0 ? (
            <EmptyState title="Nothing imported yet">Exports brought in with the command above appear here.</EmptyState>
          ) : (
            <Table>
              <thead>
                <tr>
                  <Th>Source</Th>
                  <Th>Verified</Th>
                  <Th>Retention</Th>
                  <Th>Imported</Th>
                </tr>
              </thead>
              <tbody>
                {list.map((c) => (
                  <tr key={c.id}>
                    <Td className="text-sm">
                      <span className="block break-words">{c.source}</span>
                      <span className="text-muted">{FORMAT_TEXT[c.format]}</span>
                    </Td>
                    <Td className="text-sm">
                      <span className="tabular-nums">
                        {formatNumber(c.verified)} of {formatNumber(c.items)}
                      </span>
                      {c.mismatches.length > 0 && (
                        <span className="mt-1 block">
                          <StateBadge tone="warning">{`${formatNumber(c.mismatches.length)} to look at`}</StateBadge>
                          <span className="mt-1 block break-words text-muted">{c.mismatches[0]}</span>
                        </span>
                      )}
                    </Td>
                    <Td className="text-sm">{c.retention || <span className="text-muted">None set</span>}</Td>
                    <Td>
                      <Time value={c.created_at} />
                    </Td>
                  </tr>
                ))}
              </tbody>
            </Table>
          )}
        </div>
      </Panel>
      <Panel
        title="Upstream sources"
        description="Where files imported by URL or from STAC came from. Each is asked once a day whether it is still what was imported. Your copy is unaffected either way."
      >
        {upstream.isPending ? (
          <SkeletonRows rows={2} />
        ) : upstream.isError ? (
          <ErrorState error={upstream.error} onRetry={() => upstream.refetch()} />
        ) : sources.length === 0 ? (
          <EmptyState title="Nothing to watch">Files imported by URL or from a STAC catalogue appear here once they are in a snapshot.</EmptyState>
        ) : (
          <Table>
            <thead>
              <tr>
                <Th>File</Th>
                <Th>Upstream</Th>
                <Th>Last asked</Th>
              </tr>
            </thead>
            <tbody>
              {sources.map((u) => (
                <tr key={u.url}>
                  <Td className="text-sm">
                    <span className="mono-data block break-all">{u.path}</span>
                    <span className="mono-data block break-all text-muted">{u.url}</span>
                  </Td>
                  <Td className="text-sm">
                    <StateBadge tone={UPSTREAM_STATE[u.state].tone}>{UPSTREAM_STATE[u.state].text}</StateBadge>
                    {u.note && <span className="mt-1 block break-words text-muted">{u.note}</span>}
                  </Td>
                  <Td>{u.checked_at ? <Time value={u.checked_at} /> : <span className="text-muted">Not yet</span>}</Td>
                </tr>
              ))}
            </tbody>
          </Table>
        )}
      </Panel>
    </div>
  );
}

// The read-only DICOMweb root of a snapshot.
export function DicomwebLink({ datasetId, versionId }: { datasetId: string; versionId: string }) {
  return (
    <Panel title="DICOMweb" description="DICOM files in this version, read-only over QIDO-RS and WADO-RS with a bearer token. $OSP_API is your API's address.">
      <Snippet text={`$OSP_API/dicomweb/${datasetId}/${versionId}/studies`} />
    </Panel>
  );
}
