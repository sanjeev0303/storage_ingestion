import { describe, expect, it } from "vitest";
import { datasetIdsByNode, deriveNodes, layoutGraph, MAX_GRAPH_EDGES, nodeHref, nodeKey, transformationNames } from "./graph";
import type { Edge, Ref } from "./types";

const ds: Ref = { kind: "DATASET", id: "ds-1" };
const v1: Ref = { kind: "DATASET_VERSION", id: "v1" };
const src: Ref = { kind: "DATASET_VERSION", id: "src" };
const report: Ref = { kind: "VALIDATION_REPORT", id: "rep" };

function edge(from: Ref, relation: Edge["relation"], to: Ref, depth: number, details?: Edge["details"]): Edge {
  return { from, to, relation, depth, actor: "user-1", created_at: "2026-10-22T10:00:00Z", details };
}

// ds CONTAINS v1, v1 VALIDATED_BY rep, v1 DERIVED_FROM src
const upstream = [edge(ds, "CONTAINS", v1, 1), edge(v1, "VALIDATED_BY", report, 2), edge(v1, "DERIVED_FROM", src, 2)];

describe("deriveNodes", () => {
  it("derives nodes from refs only, once each, with the start at layer 0", () => {
    const nodes = deriveNodes(ds, upstream, "upstream");
    expect(nodes.map((n) => n.key).sort()).toEqual([nodeKey(ds), nodeKey(v1), nodeKey(report), nodeKey(src)].sort());
    const layer = Object.fromEntries(nodes.map((n) => [n.id, n.layer]));
    expect(layer).toEqual({ "ds-1": 0, v1: 1, rep: 2, src: 2 });
  });

  it("measures downstream depth from the `from` end", () => {
    // start is a version; the dataset that contains it is one step away
    const nodes = deriveNodes(v1, [edge(ds, "CONTAINS", v1, 1)], "downstream");
    expect(Object.fromEntries(nodes.map((n) => [n.id, n.layer]))).toEqual({ v1: 0, "ds-1": 1 });
  });

  it("keeps the start in the graph when there are no edges", () => {
    expect(deriveNodes(ds, [], "upstream")).toHaveLength(1);
  });
});

describe("layoutGraph", () => {
  it("places upstream columns left to right by depth, so arrows point right", () => {
    const layout = layoutGraph(ds, upstream, "upstream");
    const x = Object.fromEntries(layout.nodes.map((n) => [n.id, n.x]));
    expect(x["ds-1"]).toBeLessThan(x.v1);
    expect(x.v1).toBeLessThan(x.rep);
    expect(x.rep).toBe(x.src);
    expect(layout.columns).toBe(3);
    for (const link of layout.links) expect(link.x2).toBeGreaterThan(link.x1);
  });

  it("puts the start on the right going downstream, so arrows still point right", () => {
    const layout = layoutGraph(v1, [edge(ds, "CONTAINS", v1, 1)], "downstream");
    const x = Object.fromEntries(layout.nodes.map((n) => [n.id, n.x]));
    expect(x["ds-1"]).toBeLessThan(x.v1);
    expect(layout.links[0].x2).toBeGreaterThan(layout.links[0].x1);
  });

  it("never lets an edge point left or down a column, even when depths tie", () => {
    // derived dataset: ds -> snapshot source src (depth 1), ds -> v1 (depth 1), v1 -> src (depth 2)
    const edges = [edge(ds, "CREATED_FROM_SNAPSHOT", src, 1), edge(ds, "CONTAINS", v1, 1), edge(v1, "DERIVED_FROM", src, 2)];
    const layout = layoutGraph(ds, edges, "upstream");
    for (const link of layout.links) expect(link.x2).toBeGreaterThan(link.x1);
  });

  it("stacks nodes of one column in separate rows", () => {
    const layout = layoutGraph(ds, upstream, "upstream");
    const column = layout.nodes.filter((n) => n.layer === 2);
    expect(new Set(column.map((n) => n.y)).size).toBe(2);
  });

  it("draws at most 300 edges and says it capped", () => {
    const many = Array.from({ length: 450 }, (_, i) => edge(ds, "CONTAINS", { kind: "DATASET_VERSION", id: `v${i}` }, 1));
    const layout = layoutGraph(ds, many, "upstream");
    expect(MAX_GRAPH_EDGES).toBe(300);
    expect(layout.links).toHaveLength(300);
    expect(layout.nodes).toHaveLength(301);
    expect(layout.capped).toBe(true);
    expect(layoutGraph(ds, upstream, "upstream").capped).toBe(false);
  });
});

describe("links and labels", () => {
  it("finds the dataset of versions and imports from the edges that name both", () => {
    const imp: Ref = { kind: "MANIFEST_IMPORT", id: "imp" };
    const ids = datasetIdsByNode([edge(ds, "CONTAINS", v1, 1), edge(ds, "IMPORTED_FROM", imp, 1)]);
    expect(nodeHref(ds, ids)).toBe("/app/datasets/ds-1");
    expect(nodeHref(v1, ids)).toBe("/app/datasets/ds-1/versions/v1");
    expect(nodeHref(imp, ids)).toBe("/app/datasets/ds-1/imports/imp");
    expect(nodeHref(report, ids)).toBe("/app/validation/reports/rep");
  });

  it("offers no link to a version whose dataset is not known", () => {
    expect(nodeHref(src, {})).toBeNull();
  });

  it("names a transformation from its edge details", () => {
    const t: Ref = { kind: "TRANSFORMATION", id: "t1" };
    expect(transformationNames([edge(ds, "TRANSFORMED_BY", t, 1, { name: "dedupe", version: "1" })])).toEqual({ [nodeKey(t)]: "dedupe 1" });
  });
});
