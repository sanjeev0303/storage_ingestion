"use client";

import Link from "next/link";
import { useState } from "react";
import { Button } from "@/components/ui/button";
import { CopyButton } from "@/components/ui/copy-button";
import { Field, Select } from "@/components/ui/field";
import { LoadMore } from "@/components/ui/page";
import { Panel } from "@/components/ui/panel";
import { EmptyState, ErrorState, SkeletonRows } from "@/components/ui/states";
import { Table, Td, Th } from "@/components/ui/table";
import { Time } from "@/components/ui/time";
import { isApiError } from "@/lib/api/errors";
import { useVersions } from "@/features/validation/hooks";
import { datasetIdsByNode, KIND_LABELS, layoutGraph, MAX_GRAPH_EDGES, nodeHref, nodeKey, RELATION_LABELS, transformationNames } from "../graph";
import { nodeLabel, useLineage, useNodeLabels } from "../hooks";
import type { Direction, Edge, Ref } from "../types";
import { LineageGraph } from "./lineage-graph";

type View = "table" | "graph";

export function LineageScreen({ datasetId, versionId: initialVersionId }: { datasetId: string; versionId?: string }) {
  const [direction, setDirection] = useState<Direction>("upstream");
  const [depth, setDepth] = useState(3);
  const [versionId, setVersionId] = useState(initialVersionId ?? "");
  const [view, setView] = useState<View>("table");
  const [selectedKey, setSelectedKey] = useState<string | null>(null);

  const versions = useVersions(datasetId);
  const lineage = useLineage({ datasetId, versionId: versionId || undefined, direction, depth });
  const { edges, start } = lineage;

  const startRef = start;
  const known: Record<string, string> = versionId ? { [`DATASET_VERSION:${versionId}`]: datasetId } : {};
  const datasetIds = datasetIdsByNode(edges, known);
  const refs = collectRefs(startRef, edges);
  const labels = useNodeLabels(refs, datasetIds, transformationNames(edges));
  const layout = startRef ? layoutGraph(startRef, edges, direction) : null;
  const selected = refs.find((r) => nodeKey(r) === selectedKey) ?? null;

  return (
    <div className="flex flex-col gap-6">
      <section aria-labelledby="lineage-heading" className="flex flex-col gap-3">
        <h2 id="lineage-heading" className="text-lg font-semibold">
          Lineage
        </h2>
        <p className="max-w-prose text-base text-muted">
          What this {versionId ? "version" : "dataset"} contains, came from or was checked by (upstream), or what contains, imports or derives from it
          (downstream). Only relationships the platform recorded appear here. Individual object versions are not part of the graph; they are the
          version&apos;s members.
        </p>
        <div className="flex flex-wrap items-end gap-4">
          <Field label="Direction" className="w-52">
            {(props) => (
              <Select {...props} value={direction} onChange={(e) => setDirection(e.target.value as Direction)}>
                <option value="upstream">Upstream: came from</option>
                <option value="downstream">Downstream: used by</option>
              </Select>
            )}
          </Field>
          <Field label="Depth" className="w-28" hint="1 to 10 steps">
            {(props) => (
              <Select {...props} value={depth} onChange={(e) => setDepth(Number(e.target.value))}>
                {Array.from({ length: 10 }, (_, i) => i + 1).map((n) => (
                  <option key={n} value={n}>
                    {n}
                  </option>
                ))}
              </Select>
            )}
          </Field>
          <Field label="Start from" className="w-72">
            {(props) => (
              <Select {...props} value={versionId} onChange={(e) => setVersionId(e.target.value)}>
                <option value="">The dataset</option>
                {versionId && !versions.items.some((v) => v.id === versionId) && <option value={versionId}>{`Version ${versionId.slice(0, 8)}`}</option>}
                {versions.items.map((v) => (
                  <option key={v.id} value={v.id}>
                    {`Version ${v.version}`}
                  </option>
                ))}
              </Select>
            )}
          </Field>
          <div role="group" aria-label="View" className="neo-in-sm inline-flex gap-1 p-1">
            {(["table", "graph"] as const).map((v) => (
              <button
                key={v}
                type="button"
                aria-pressed={view === v}
                onClick={() => setView(v)}
                className={`rounded-sm px-3.5 py-1.5 text-base font-medium ${view === v ? "bg-ink text-ground" : "hover:bg-faint"}`}
              >
                {v === "table" ? "Table" : "Graph"}
              </button>
            ))}
          </div>
        </div>
      </section>

      {lineage.isPending ? (
        <SkeletonRows rows={5} />
      ) : lineage.isError ? (
        <LineageError error={lineage.error} onRetry={() => lineage.refetch()} />
      ) : edges.length === 0 ? (
        <EmptyState title="No recorded lineage">
          {direction === "upstream"
            ? "Nothing has been recorded upstream of this item yet. Snapshots, manifest imports, derived datasets and validations each add a relationship when they happen."
            : "Nothing is recorded as built from this item yet. A derived dataset or a validation of a version would appear here."}{" "}
          Try the other direction or a greater depth.
        </EmptyState>
      ) : (
        <>
          {lineage.truncated && (
            <p role="status" className="neo-in-sm max-w-prose p-3 text-base font-medium">
              More lineage exists. Narrow the depth or direction to see the rest.
            </p>
          )}
          <div className="grid gap-6 xl:grid-cols-3">
            <div className="min-w-0 xl:col-span-2">
              {view === "table" ? (
                <EdgeTable edges={edges} labels={labels} datasetIds={datasetIds} onSelect={setSelectedKey} />
              ) : (
                layout && (
                  <>
                    {layout.capped && (
                      <p role="status" className="mb-2 text-sm font-medium">
                        The graph draws the first {MAX_GRAPH_EDGES} of {edges.length} loaded edges. The table lists all of them.
                      </p>
                    )}
                    <LineageGraph layout={layout} labels={labels} selectedKey={selectedKey} onSelect={setSelectedKey} />
                  </>
                )
              )}
              <LoadMore
                hasNext={Boolean(lineage.hasNextPage)}
                loading={lineage.isFetchingNextPage}
                onLoad={() => lineage.fetchNextPage()}
                shown={edges.length}
              />
              <p className="mt-1 text-xs text-muted">Edges are loaded one page at a time. &quot;Load more&quot; fetches the next page of the same walk.</p>
            </div>
            <NodeDetails node={selected} labels={labels} datasetIds={datasetIds} edges={edges} />
          </div>
        </>
      )}
    </div>
  );
}

function collectRefs(start: Ref | undefined, edges: Edge[]): Ref[] {
  const seen = new Map<string, Ref>();
  if (start) seen.set(nodeKey(start), start);
  for (const e of edges) {
    seen.set(nodeKey(e.from), e.from);
    seen.set(nodeKey(e.to), e.to);
  }
  return [...seen.values()];
}

function LineageError({ error, onRetry }: { error: unknown; onRetry: () => void }) {
  if (isApiError(error) && error.code === "query_too_broad") {
    return (
      <div role="alert" className="neo-in flex max-w-prose flex-col items-start gap-3 p-8">
        <h3 className="text-md font-semibold">That lineage walk is too large</h3>
        <p className="text-base text-muted">
          The server stopped the walk because it reached too many edges or took too long. Lower the depth, or switch direction, then try again.
        </p>
        <Button onClick={onRetry}>Try again</Button>
      </div>
    );
  }
  return <ErrorState error={error} onRetry={onRetry} />;
}

function NodeCell({
  node,
  labels,
  datasetIds,
  onSelect,
}: {
  node: Ref;
  labels: Record<string, string>;
  datasetIds: Record<string, string>;
  onSelect: (key: string) => void;
}) {
  const href = nodeHref(node, datasetIds);
  return (
    <div className="max-w-64 whitespace-normal">
      <button type="button" onClick={() => onSelect(nodeKey(node))} className="text-left font-medium underline underline-offset-2 [overflow-wrap:anywhere]">
        {nodeLabel(node, labels)}
      </button>
      <span className="block text-xs text-muted">
        {KIND_LABELS[node.kind]} <span className="mono-data">{node.id.slice(0, 8)}</span>
      </span>
      {href && (
        <Link href={href} className="text-xs underline underline-offset-2">
          Open
        </Link>
      )}
    </div>
  );
}

function EdgeTable({
  edges,
  labels,
  datasetIds,
  onSelect,
}: {
  edges: Edge[];
  labels: Record<string, string>;
  datasetIds: Record<string, string>;
  onSelect: (key: string) => void;
}) {
  return (
    <Table aria-label="Lineage edges">
      <caption className="sr-only">Lineage edges, each read as from, relation, to, ordered by distance from the start</caption>
      <thead>
        <tr>
          <Th>From</Th>
          <Th>Relation</Th>
          <Th>To</Th>
          <Th className="text-right">Depth</Th>
          <Th>Recorded by</Th>
          <Th>Recorded</Th>
        </tr>
      </thead>
      <tbody>
        {edges.map((edge, i) => (
          <tr key={`${nodeKey(edge.from)}-${edge.relation}-${nodeKey(edge.to)}-${i}`}>
            <Td>
              <NodeCell node={edge.from} labels={labels} datasetIds={datasetIds} onSelect={onSelect} />
            </Td>
            <Td>{RELATION_LABELS[edge.relation]}</Td>
            <Td>
              <NodeCell node={edge.to} labels={labels} datasetIds={datasetIds} onSelect={onSelect} />
            </Td>
            <Td className="text-right tabular-nums">{edge.depth}</Td>
            <Td>
              <span className="mono-data text-xs">{edge.actor.slice(0, 8)}</span>
            </Td>
            <Td>
              <Time value={edge.created_at} />
            </Td>
          </tr>
        ))}
      </tbody>
    </Table>
  );
}

function NodeDetails({
  node,
  labels,
  datasetIds,
  edges,
}: {
  node: Ref | null;
  labels: Record<string, string>;
  datasetIds: Record<string, string>;
  edges: Edge[];
}) {
  if (!node) {
    return (
      <Panel title="Details" aria-live="polite">
        <p className="text-base text-muted">Select a node in the table or the graph to see what it is and how each relationship was recorded.</p>
      </Panel>
    );
  }
  const key = nodeKey(node);
  const href = nodeHref(node, datasetIds);
  const incident = edges.filter((e) => nodeKey(e.from) === key || nodeKey(e.to) === key);
  return (
    <Panel title={nodeLabel(node, labels)} description={KIND_LABELS[node.kind]} aria-live="polite">
      <div className="flex flex-col gap-4">
        <div className="flex flex-wrap items-center gap-2">
          <span className="mono-data text-xs [overflow-wrap:anywhere]">{node.id}</span>
          <CopyButton value={node.id} label="Copy id" />
        </div>
        {href && (
          <Link href={href} className="font-medium underline underline-offset-2">
            Open this {KIND_LABELS[node.kind].toLowerCase()}
          </Link>
        )}
        {!href && node.kind !== "TRANSFORMATION" && <p className="text-sm text-muted">The dataset this belongs to is not known from the loaded edges, so there is no link.</p>}
        <h3 className="text-sm font-semibold">How it was recorded</h3>
        <ul className="flex flex-col gap-3">
          {incident.map((edge, i) => (
            <li key={i} className="text-sm">
              <p className="font-medium">
                {nodeLabel(edge.from, labels)} {RELATION_LABELS[edge.relation]} {nodeLabel(edge.to, labels)}
              </p>
              <p className="text-muted">
                By <span className="mono-data">{edge.actor}</span> on <Time value={edge.created_at} />
                {edge.job_id && (
                  <>
                    , job <span className="mono-data">{edge.job_id}</span>
                  </>
                )}
              </p>
              {edge.details && Object.keys(edge.details).length > 0 && (
                <dl className="mt-1 grid grid-cols-[auto_1fr] gap-x-3 gap-y-0.5">
                  {Object.entries(edge.details).map(([k, v]) => (
                    <div key={k} className="contents">
                      <dt className="mono-data text-muted">{k}</dt>
                      <dd className="[overflow-wrap:anywhere]">{typeof v === "object" ? JSON.stringify(v) : String(v)}</dd>
                    </div>
                  ))}
                </dl>
              )}
            </li>
          ))}
        </ul>
      </div>
    </Panel>
  );
}
