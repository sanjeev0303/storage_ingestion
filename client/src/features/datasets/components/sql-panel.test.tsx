import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { describe, expect, it, vi } from "vitest";
import { API, server } from "@/test/server";
import { renderWithProviders } from "@/test/render";
import { SqlPanel } from "./sql-panel";

// DuckDB itself is WebAssembly and does not run in jsdom. This stand-in keeps
// one catalog for the whole tab, as the real one does, and refuses a table
// name that is already taken.
const duck = vi.hoisted(() => {
  const tables = new Set<string>();
  const closed: number[] = [];
  let connections = 0;
  const result = (rows: object[]) => ({ toArray: () => rows, schema: { fields: [] }, numRows: rows.length });
  function connect() {
    const id = ++connections;
    return {
      async query(sql: string) {
        const drop = sql.match(/^DROP TABLE IF EXISTS "?(\w+)"?/i);
        if (drop) {
          tables.delete(drop[1]);
          return result([]);
        }
        const create = sql.match(/^CREATE (OR REPLACE )?TABLE "(\w+)"/i);
        if (create) {
          if (!create[1] && tables.has(create[2])) throw new Error(`Table with name ${create[2]} already exists!`);
          tables.add(create[2]);
          return result([]);
        }
        return result([{ n: 1 }]);
      },
      async insertJSONFromPath(_path: string, options: { name: string }) {
        if (tables.has(options.name)) throw new Error(`Table with name ${options.name} already exists!`);
        tables.add(options.name);
      },
      async close() {
        closed.push(id);
      },
    };
  }
  const db = { connect: async () => connect(), registerFileText: async () => undefined, registerFileBuffer: async () => undefined };
  return { db, tables, closed, opened: () => connections };
});

vi.mock("../sql/duckdb", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../sql/duckdb")>()),
  openDuckDB: async () => duck.db,
}));

// jsdom blobs cannot stream; the member below pins no checksum anyway.
vi.mock("@/lib/upload/sha256", () => ({ sha256Blob: async () => "0".repeat(64) }));

const member = {
  ordinal: 1,
  object_id: "o1",
  version_id: "a1",
  object_version: 1,
  path: "tables/data.csv",
  size_bytes: 8,
  content_type: "text/csv",
  availability: "AVAILABLE",
  metadata: {},
};

describe("SqlPanel", () => {
  it("starts again on another version without colliding with the tables the first one left", async () => {
    server.use(
      http.get(`${API}/datasets/ds-1/versions/:vid/members`, () => HttpResponse.json({ members: [member] })),
      http.get(`${API}/assets/a1/content`, () => new HttpResponse("a,b\n1,2\n", { headers: { "Content-Type": "text/csv" } })),
    );

    async function startAndLoad() {
      await userEvent.click(screen.getByRole("button", { name: "Start SQL" }));
      await userEvent.selectOptions(await screen.findByLabelText("Load a file as a table"), "tables/data.csv");
      await userEvent.click(screen.getByRole("button", { name: "Load file" }));
      expect(await screen.findByText("tables/data.csv", { selector: "span" })).toBeInTheDocument();
      expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    }

    const first = renderWithProviders(<SqlPanel datasetId="ds-1" versionId="v1" />);
    await startAndLoad();
    first.unmount();

    renderWithProviders(<SqlPanel datasetId="ds-1" versionId="v2" />);
    await startAndLoad();
    expect([...duck.tables].sort()).toEqual(["data", "members"]);
    // The first panel's connection was closed when it unmounted.
    await waitFor(() => expect(duck.closed).toEqual([1]));
  });

  it("closes the connection when the members cannot be read", async () => {
    server.use(
      http.get(`${API}/datasets/ds-1/versions/:vid/members`, () => HttpResponse.json({ error: "no access", code: "forbidden" }, { status: 403 })),
    );
    renderWithProviders(<SqlPanel datasetId="ds-1" versionId="v3" />);
    await userEvent.click(screen.getByRole("button", { name: "Start SQL" }));
    expect(await screen.findByText(/DuckDB could not start/)).toBeInTheDocument();
    expect(duck.closed).toContain(duck.opened());
  });
});
