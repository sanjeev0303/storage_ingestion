import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { describe, expect, it } from "vitest";
import { renderWithProviders } from "@/test/render";
import { API, server } from "@/test/server";
import { VersionChunks } from "./version-chunks";

const savings = (extra: object = {}) => ({
  enabled: true, logical_bytes: 300, unique_file_bytes: 200, indexed_files: 2, indexed_file_bytes: 200, unique_chunk_bytes: 100, unique_chunks: 4,
  not_indexed_files: 0, not_indexed_bytes: 0, would_save_bytes: 100, ...extra,
});

const storage = (extra: object = {}) => ({ enabled: false, files: 0, logical_bytes: 0, retired_source_bytes: 0, chunk_bytes: 0, chunk_objects: 0, rehydrating: 0, ...extra });

function routes(body: object, status: object = { state: "DONE", files_indexed: 2, bytes_read: 200, skipped: [] }, held: object = storage()) {
  server.use(
    http.get(`${API}/datasets/d1/chunk-storage`, () => HttpResponse.json(held)),
    http.get(`${API}/datasets/d1/chunk-savings`, () => HttpResponse.json(body)),
    http.get(`${API}/datasets/d1/versions/v2/chunks/status`, () => HttpResponse.json(status)),
    http.get(`${API}/datasets/d1/versions`, () => HttpResponse.json({ versions: [{ id: "v2", version: 2 }, { id: "v1", version: 1 }], next_cursor: "" })),
    http.get(`${API}/datasets/d1/versions/v2/delta`, ({ request }) => {
      expect(new URL(request.url).searchParams.get("from")).toBe("v1");
      return HttpResponse.json({ total_bytes: 1048576, unchanged_files: 2, unchanged_bytes: 0, changed_files: 1, changed_bytes: 10, reused_bytes: 8, fetch_bytes: 2048, not_indexed_files: 1, largest_changed: [] });
    }),
  );
}

describe("VersionChunks", () => {
  it("reports the savings, and what a delta download would fetch", async () => {
    routes(savings());
    renderWithProviders(<VersionChunks id="d1" versionId="v2" version={2} />, { permissions: ["datasets.read"] });
    expect(await screen.findByText("Indexed")).toBeInTheDocument();
    expect(await screen.findByText(/\(50%\)/)).toBeInTheDocument();
    expect(await screen.findByText(/A delta download fetches/)).toBeInTheDocument();
    expect(screen.getByText(/1 changed files are not indexed/)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Stop indexing new versions" })).not.toBeInTheDocument();
  });

  it("asks before storing chunks once, and says what it holds", async () => {
    routes(savings());
    let sent: unknown;
    server.use(
      http.put(`${API}/datasets/d1/chunk-storage`, async ({ request }) => {
        sent = await request.json();
        return HttpResponse.json({ enabled: true });
      }),
    );
    renderWithProviders(<VersionChunks id="d1" versionId="v2" version={2} />, { permissions: ["datasets.manage"] });
    const user = userEvent.setup();
    await user.click(await screen.findByRole("button", { name: "Store chunks once" }));
    const dialog = await screen.findByRole("alertdialog");
    expect(dialog).toHaveTextContent(/read through the API/);
    expect(sent).toBeUndefined();
    const confirm = Array.from(dialog.querySelectorAll("button")).find((b) => b.textContent === "Store chunks once")!;
    await user.click(confirm);
    await waitFor(() => expect(sent).toEqual({ enabled: true }));
  });

  it("shows what chunks hold, and offers the way back", async () => {
    routes(savings(), undefined, storage({ enabled: true, files: 3, logical_bytes: 3072, retired_source_bytes: 1024, chunk_bytes: 2048, chunk_objects: 4 }));
    let asked = false;
    server.use(
      http.post(`${API}/datasets/d1/chunk-storage/rehydrate`, () => {
        asked = true;
        return HttpResponse.json({ files: 3 }, { status: 202 });
      }),
    );
    renderWithProviders(<VersionChunks id="d1" versionId="v2" version={2} />, { permissions: ["datasets.manage"] });
    expect(await screen.findByText(/in 4 objects/)).toBeInTheDocument();
    const user = userEvent.setup();
    await user.click(screen.getByRole("button", { name: "Make whole objects again" }));
    const dialog = await screen.findByRole("alertdialog");
    await user.click(Array.from(dialog.querySelectorAll("button")).find((b) => b.textContent === "Make whole again")!);
    await waitFor(() => expect(asked).toBe(true));
  });

  it("says what indexing costs before it is turned on", async () => {
    routes(savings({ enabled: false, indexed_files: 0 }), { state: "NONE", skipped: [] });
    let sent: unknown;
    server.use(
      http.put(`${API}/datasets/d1/chunking`, async ({ request }) => {
        sent = await request.json();
        return HttpResponse.json({ enabled: true });
      }),
    );
    renderWithProviders(<VersionChunks id="d1" versionId="v2" version={2} />, { permissions: ["datasets.manage"] });
    expect(await screen.findByText(/Indexing reads each file once/)).toBeInTheDocument();
    await userEvent.setup().click(screen.getByRole("button", { name: "Index this dataset" }));
    await waitFor(() => expect(sent).toEqual({ enabled: true }));
  });
});
