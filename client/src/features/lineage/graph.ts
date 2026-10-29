import type { Direction, Edge, NodeKind, Ref, Relation } from "./types";

// The SVG never draws more than this many edges. The table keeps all of them.
export const MAX_GRAPH_EDGES = 300;

export const NODE_W = 190;
export const NODE_H = 46;
export const COL_GAP = 110;
export const ROW_GAP = 22;

export const KIND_LABELS: Record<NodeKind, string> = {
  DATASET: "Dataset",
  DATASET_VERSION: "Dataset version",
  MANIFEST_IMPORT: "Manifest import",
  VALIDATION_REPORT: "Validation report",
  TRANSFORMATION: "Transformation",
  OBJECT_VERSION: "Object version",
  TRAINING_RUN: "Training run",
  MODEL: "Model",
};

export const RELATION_LABELS: Record<Relation, string> = {
  CONTAINS: "contains",
  DERIVED_FROM: "derived from",
  IMPORTED_FROM: "imported from",
  VALIDATED_BY: "validated by",
  CREATED_FROM_SNAPSHOT: "created from snapshot",
  TRANSFORMED_BY: "transformed by",
  TRAINED_ON: "trained on",
  PRODUCED_BY: "produced by",
};

export const nodeKey = (ref: Ref) => `${ref.kind}:${ref.id}`;

export type LineageNode = Ref & { key: string; layer: number };

// Nodes are only refs (kind and id). The far end of an edge sits at edge.depth
// from the start: that is `to` going upstream and `from` going downstream.
export function deriveNodes(start: Ref, edges: Edge[], direction: Direction): LineageNode[] {
  const nodes = new Map<string, LineageNode>();
  nodes.set(nodeKey(start), { ...start, key: nodeKey(start), layer: 0 });

  const place = (ref: Ref, layer: number) => {
    const key = nodeKey(ref);
    const existing = nodes.get(key);
    if (!existing) nodes.set(key, { ...ref, key, layer });
    else if (key !== nodeKey(start) && layer < existing.layer) existing.layer = layer;
  };

  for (const edge of edges) {
    const far = direction === "upstream" ? edge.to : edge.from;
    const near = direction === "upstream" ? edge.from : edge.to;
    place(far, edge.depth);
    // The near end is one step closer; it is already placed unless the page cut the walk short.
    place(near, Math.max(0, edge.depth - 1));
  }
  return [...nodes.values()];
}

export type Placed = LineageNode & { x: number; y: number };
export type Link = { edge: Edge; x1: number; y1: number; x2: number; y2: number };
export type Layout = {
  nodes: Placed[];
  links: Link[];
  width: number;
  height: number;
  capped: boolean;
  columns: number;
};

// Columns by distance from the start. Every edge reads from -> to, so the
// columns run so that arrows always point right: upstream puts the start on
// the left, downstream puts it on the right.
export function layoutGraph(start: Ref, allEdges: Edge[], direction: Direction): Layout {
  const capped = allEdges.length > MAX_GRAPH_EDGES;
  const edges = capped ? allEdges.slice(0, MAX_GRAPH_EDGES) : allEdges;
  const nodes = deriveNodes(start, edges, direction);
  const maxLayer = Math.max(0, ...nodes.map((n) => n.layer));

  // Start from the depth columns, then push a node right whenever an edge would
  // otherwise point left or straight down a column. Stops after one pass per node,
  // so even a cycle (the server forbids them) cannot loop.
  const column = new Map<string, number>();
  for (const node of nodes) column.set(node.key, direction === "upstream" ? node.layer : maxLayer - node.layer);
  for (let pass = 0; pass < nodes.length; pass++) {
    let moved = false;
    for (const edge of edges) {
      const from = column.get(nodeKey(edge.from));
      const to = column.get(nodeKey(edge.to));
      if (from === undefined || to === undefined || to > from) continue;
      column.set(nodeKey(edge.to), from + 1);
      moved = true;
    }
    if (!moved) break;
  }
  const lastColumn = Math.max(0, ...column.values());

  const rows = new Map<number, number>();
  const placed = new Map<string, Placed>();
  for (const node of nodes) {
    const col = column.get(node.key) ?? 0;
    const row = rows.get(col) ?? 0;
    rows.set(col, row + 1);
    placed.set(node.key, { ...node, x: col * (NODE_W + COL_GAP), y: row * (NODE_H + ROW_GAP) });
  }

  const links: Link[] = [];
  for (const edge of edges) {
    const a = placed.get(nodeKey(edge.from));
    const b = placed.get(nodeKey(edge.to));
    if (!a || !b) continue;
    links.push({ edge, x1: a.x + NODE_W, y1: a.y + NODE_H / 2, x2: b.x, y2: b.y + NODE_H / 2 });
  }

  const tallest = Math.max(1, ...rows.values());
  return {
    nodes: [...placed.values()],
    links,
    width: (lastColumn + 1) * NODE_W + lastColumn * COL_GAP,
    height: tallest * NODE_H + (tallest - 1) * ROW_GAP,
    capped,
    columns: lastColumn + 1,
  };
}

// Dataset ids for the other node kinds, read from the edges that name both.
// CONTAINS and CREATED_FROM_SNAPSHOT: DATASET -> DATASET_VERSION; IMPORTED_FROM: DATASET -> MANIFEST_IMPORT.
export function datasetIdsByNode(edges: Edge[], known: Record<string, string> = {}): Record<string, string> {
  const map: Record<string, string> = { ...known };
  for (const edge of edges) {
    if (edge.from.kind !== "DATASET") continue;
    if (edge.relation === "CONTAINS" || edge.relation === "IMPORTED_FROM") {
      map[nodeKey(edge.to)] = edge.from.id;
    }
  }
  return map;
}

export function nodeHref(node: Ref, datasetIds: Record<string, string>): string | null {
  const datasetId = datasetIds[nodeKey(node)];
  switch (node.kind) {
    case "DATASET":
      return `/app/datasets/${node.id}`;
    case "DATASET_VERSION":
      return datasetId ? `/app/datasets/${datasetId}/versions/${node.id}` : null;
    case "VALIDATION_REPORT":
      return `/app/validation/reports/${node.id}`;
    case "MANIFEST_IMPORT":
      return datasetId ? `/app/datasets/${datasetId}/imports/${node.id}` : null;
    default:
      return null;
  }
}

// Names are not part of the lineage response. Transformations, training
// runs and models carry theirs in the details of the edge that names them.
export function edgeNames(edges: Edge[]): Record<string, string> {
  const names: Record<string, string> = {};
  for (const edge of edges) {
    if (typeof edge.details?.name !== "string") continue;
    const version = typeof edge.details.version === "string" ? ` ${edge.details.version}` : "";
    if (edge.to.kind === "TRANSFORMATION") names[nodeKey(edge.to)] = `${edge.details.name}${version}`;
    if (edge.relation === "TRAINED_ON") names[nodeKey(edge.from)] = edge.details.name;
    if (edge.relation === "PRODUCED_BY") names[nodeKey(edge.from)] = `${edge.details.name}${version}`;
  }
  return names;
}
