import { configure, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { describe, expect, it } from "vitest";
import { axe } from "vitest-axe";
import { renderWithProviders } from "@/test/render";
import { API, server } from "@/test/server";
import type { Edge } from "../types";
import { LineageScreen } from "./lineage-screen";

// Other work on this machine can slow the first renders; the default 1s is too tight.
configure({ asyncUtilTimeout: 5000 });

const ds = { kind: "DATASET", id: "ds-1" } as const;

function edge(n: number, extra: Partial<Edge> = {}): Edge {
  return {
    from: ds,
    to: { kind: "DATASET_VERSION", id: `version-${n}` },
    relation: "CONTAINS",
    depth: 1,
    actor: "user-1",
    created_at: "2026-10-22T10:00:00Z",
    ...extra,
  };
}

function mockVersions() {
  server.use(http.get(`${API}/datasets/ds-1/versions`, () => HttpResponse.json({ versions: [] })));
  server.use(http.get(`${API}/datasets/ds-1`, () => HttpResponse.json({ name: "pets" })));
  server.use(http.get(`${API}/datasets/ds-1/versions/:vid`, () => HttpResponse.json({ version: 1 })));
}

function mockLineage(handler: (params: URLSearchParams) => object) {
  server.use(http.get(`${API}/datasets/ds-1/lineage`, ({ request }) => HttpResponse.json(handler(new URL(request.url).searchParams))));
}

describe("LineageScreen", () => {
  it("shows edges as a table by default, with friendly names and links", async () => {
    mockVersions();
    mockLineage(() => ({
      start: ds,
      direction: "upstream",
      truncated: false,
      edges: [edge(1), edge(2, { from: { kind: "DATASET_VERSION", id: "version-2" }, relation: "VALIDATED_BY", to: { kind: "VALIDATION_REPORT", id: "rep-1" }, depth: 2 })],
    }));
    const { container } = renderWithProviders(<LineageScreen datasetId="ds-1" />);

    const table = await screen.findByRole("table", { name: "Lineage edges" });
    expect(within(table).getAllByRole("row")).toHaveLength(3);
    expect(within(table).getAllByText("contains").length).toBeGreaterThan(0);
    expect(within(table).getByText("validated by")).toBeInTheDocument();
    expect(await within(table).findAllByText("pets")).not.toHaveLength(0);
    expect(within(table).getAllByRole("link", { name: "Open" }).some((a) => a.getAttribute("href") === "/app/validation/reports/rep-1")).toBe(true);
    expect(screen.queryByRole("group", { name: /Lineage graph/ })).not.toBeInTheDocument();
    expect(await axe(container)).toHaveNoViolations();
  });

  it("requests upstream with depth 3 first and re-requests with the chosen direction and depth", async () => {
    mockVersions();
    const seen: URLSearchParams[] = [];
    mockLineage((params) => {
      seen.push(params);
      return { start: ds, direction: params.get("direction"), truncated: false, edges: [edge(1)] };
    });
    renderWithProviders(<LineageScreen datasetId="ds-1" />);
    await screen.findByRole("table", { name: "Lineage edges" });
    expect(seen[0].get("direction")).toBe("upstream");
    expect(seen[0].get("depth")).toBe("3");

    await userEvent.selectOptions(screen.getByLabelText("Direction"), "downstream");
    await userEvent.selectOptions(screen.getByLabelText("Depth"), "5");
    await screen.findByRole("table", { name: "Lineage edges" });
    const last = seen[seen.length - 1];
    expect(last.get("direction")).toBe("downstream");
    expect(last.get("depth")).toBe("5");
  });

  it("uses the version endpoint when a version is given", async () => {
    mockVersions();
    let hit = false;
    server.use(
      http.get(`${API}/dataset-versions/version-1/lineage`, () => {
        hit = true;
        return HttpResponse.json({ start: { kind: "DATASET_VERSION", id: "version-1" }, direction: "upstream", truncated: false, edges: [] });
      }),
    );
    renderWithProviders(<LineageScreen datasetId="ds-1" versionId="version-1" />);
    expect(await screen.findByText("No recorded lineage")).toBeInTheDocument();
    expect(hit).toBe(true);
  });

  it("loads more edges with the cursor and reports truncation honestly", async () => {
    mockVersions();
    const cursors: (string | null)[] = [];
    mockLineage((params) => {
      cursors.push(params.get("cursor"));
      if (params.get("cursor") === "next") return { start: ds, direction: "upstream", truncated: true, edges: [edge(3)] };
      return { start: ds, direction: "upstream", truncated: false, next_cursor: "next", edges: [edge(1), edge(2)] };
    });
    renderWithProviders(<LineageScreen datasetId="ds-1" />);
    await screen.findByRole("table", { name: "Lineage edges" });
    expect(screen.queryByText(/More lineage exists/)).not.toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: "Load more" }));
    expect(await screen.findByText(/More lineage exists. Narrow the depth or direction/)).toBeInTheDocument();
    expect(cursors).toEqual([null, "next"]);
    expect(within(screen.getByRole("table", { name: "Lineage edges" })).getAllByRole("row")).toHaveLength(4);
  });

  it("caps the SVG at 300 edges, says so, and keeps every edge in the table", async () => {
    mockVersions();
    const edges = Array.from({ length: 350 }, (_, i) => edge(i));
    mockLineage(() => ({ start: ds, direction: "upstream", truncated: false, edges }));
    renderWithProviders(<LineageScreen datasetId="ds-1" />);
    const table = await screen.findByRole("table", { name: "Lineage edges" });
    expect(within(table).getAllByRole("row")).toHaveLength(351);

    await userEvent.click(screen.getByRole("button", { name: "Graph" }));
    expect(screen.getByText(/draws the first 300 of 350 loaded edges/)).toBeInTheDocument();
    const graph = screen.getByRole("group", { name: /Lineage graph/ });
    expect(graph.querySelectorAll("[data-node]")).toHaveLength(301);
    expect(graph.querySelectorAll("path[marker-end]")).toHaveLength(300);

    await userEvent.click(screen.getByRole("button", { name: "Table" }));
    expect(screen.getByRole("table", { name: "Lineage edges" })).toBeInTheDocument();
  }, 30000);

  it("selects a node from the graph and shows how its edge was recorded", async () => {
    mockVersions();
    mockLineage(() => ({
      start: ds,
      direction: "upstream",
      truncated: false,
      edges: [edge(1, { job_id: "job-9", details: { manifest_sha256: "abc" } })],
    }));
    renderWithProviders(<LineageScreen datasetId="ds-1" />);
    await screen.findByRole("table", { name: "Lineage edges" });
    await userEvent.click(screen.getByRole("button", { name: "Graph" }));
    const nodes = screen.getByRole("group", { name: /Lineage graph/ }).querySelectorAll("[data-node]");
    await userEvent.click(nodes[1]);
    expect(await screen.findByText("How it was recorded")).toBeInTheDocument();
    expect(screen.getByText("job-9")).toBeInTheDocument();
    expect(screen.getByText("manifest_sha256")).toBeInTheDocument();
  });

  it("gives guidance for 422 query_too_broad", async () => {
    mockVersions();
    server.use(http.get(`${API}/datasets/ds-1/lineage`, () => HttpResponse.json({ error: "too broad", code: "query_too_broad" }, { status: 422 })));
    renderWithProviders(<LineageScreen datasetId="ds-1" />);
    expect(await screen.findByText("That lineage walk is too large")).toBeInTheDocument();
    expect(screen.getByText(/Lower the depth/)).toBeInTheDocument();
  });

  it("shows an honest empty state", async () => {
    mockVersions();
    mockLineage(() => ({ start: ds, direction: "upstream", truncated: false, edges: [] }));
    renderWithProviders(<LineageScreen datasetId="ds-1" />);
    expect(await screen.findByText("No recorded lineage")).toBeInTheDocument();
  });

  it("shows permission denied on 403", async () => {
    mockVersions();
    server.use(http.get(`${API}/datasets/ds-1/lineage`, () => HttpResponse.json({ error: "forbidden", code: "forbidden" }, { status: 403 })));
    renderWithProviders(<LineageScreen datasetId="ds-1" />);
    expect(await screen.findByText("Your role cannot do this")).toBeInTheDocument();
  });
});
