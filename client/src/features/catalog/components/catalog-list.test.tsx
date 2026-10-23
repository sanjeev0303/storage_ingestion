import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { API, server } from "@/test/server";
import { renderWithProviders } from "@/test/render";
import { makeObject } from "../fixtures";
import { CatalogList } from "./catalog-list";

const push = vi.fn();
let search = "";

vi.mock("next/navigation", () => ({
  useRouter: () => ({ push }),
  useSearchParams: () => new URLSearchParams(search),
}));

beforeEach(() => {
  push.mockReset();
  search = "";
  server.use(http.get(`${API}/catalog/capabilities`, () => new HttpResponse(null, { status: 404 })));
});

describe("CatalogList", () => {
  it("pages with the cursor and keeps the same filters on the next page", async () => {
    const seen: URL[] = [];
    search = "prefix=2026%2F&dataset=images";
    server.use(
      http.get(`${API}/catalog/objects`, ({ request }) => {
        const url = new URL(request.url);
        seen.push(url);
        if (!url.searchParams.get("cursor")) {
          return HttpResponse.json({ objects: [makeObject({ id: "a", path: "2026/a.png" })], next_cursor: "c1" });
        }
        return HttpResponse.json({ objects: [makeObject({ id: "b", path: "2026/b.png" })] });
      }),
    );
    renderWithProviders(<CatalogList />);

    expect(await screen.findByText("2026/a.png")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Load more" }));
    expect(await screen.findByText("2026/b.png")).toBeInTheDocument();

    expect(seen[0].searchParams.get("cursor")).toBeNull();
    expect(seen[1].searchParams.get("cursor")).toBe("c1");
    expect(seen[1].searchParams.get("prefix")).toBe("2026/");
    expect(seen[1].searchParams.get("dataset")).toBe("images");
    expect(screen.getAllByText("SHA-256").length).toBeGreaterThan(0);
  });

  it("puts applied filters in the address", async () => {
    server.use(http.get(`${API}/catalog/objects`, () => HttpResponse.json({ objects: [] })));
    renderWithProviders(<CatalogList />);
    await screen.findByText("The catalog is empty");

    await userEvent.type(screen.getByLabelText("Path prefix"), "logs/");
    await userEvent.selectOptions(screen.getByLabelText("Dataset"), "root");
    await userEvent.click(screen.getByRole("button", { name: "Apply filters" }));

    expect(push).toHaveBeenCalledWith("/app/catalog?dataset=&prefix=logs%2F");
  });

  it("starts from the first page when the filters change", async () => {
    const cursors: (string | null)[] = [];
    server.use(
      http.get(`${API}/catalog/objects`, ({ request }) => {
        cursors.push(new URL(request.url).searchParams.get("cursor"));
        return HttpResponse.json({ objects: [makeObject()], next_cursor: "c1" });
      }),
    );
    const { rerender } = renderWithProviders(<CatalogList />);
    await screen.findByText("2026/cat.png");
    await userEvent.click(screen.getByRole("button", { name: "Load more" }));
    await waitFor(() => expect(cursors).toEqual([null, "c1"]));

    search = "prefix=other";
    rerender(<CatalogList />);
    await waitFor(() => expect(cursors).toHaveLength(3));
    expect(cursors[2]).toBeNull();
  });

  it("tells the user to narrow a query that is too broad", async () => {
    server.use(
      http.get(`${API}/catalog/objects`, () =>
        HttpResponse.json({ error: "query too broad", code: "query_too_broad" }, { status: 422 }),
      ),
    );
    renderWithProviders(<CatalogList />);
    expect(await screen.findByText("That filter is too broad")).toBeInTheDocument();
    expect(screen.getByText(/Narrow it with a dataset/)).toBeInTheDocument();
  });

  it("says there is no free-text search and shows a missing exact path", async () => {
    server.use(
      http.get(`${API}/catalog/objects`, () => HttpResponse.json({ objects: [] })),
      http.get(`${API}/catalog/lookup`, () => HttpResponse.json({ error: "not found", code: "not_found" }, { status: 404 })),
    );
    renderWithProviders(<CatalogList />);
    expect(await screen.findByText(/no free-text search/)).toBeInTheDocument();
    await userEvent.type(screen.getByLabelText(/Exact path/), "missing.txt");
    await userEvent.click(screen.getByRole("button", { name: "Open object" }));
    expect(await screen.findByText("No object has that dataset and path.")).toBeInTheDocument();
  });

  it("shows a capabilities line when the API answers", async () => {
    server.use(
      http.get(`${API}/catalog/objects`, () => HttpResponse.json({ objects: [] })),
      http.get(`${API}/catalog/capabilities`, () =>
        HttpResponse.json({ logical_versioning: true, provider_versioning: false }),
      ),
    );
    renderWithProviders(<CatalogList />);
    expect(await screen.findByText(/provider reports versioning as off/)).toBeInTheDocument();
  });

  it("keeps Open object disabled until a path is typed", async () => {
    server.use(http.get(`${API}/catalog/objects`, () => HttpResponse.json({ objects: [] })));
    renderWithProviders(<CatalogList />);
    const open = await screen.findByRole("button", { name: "Open object" });
    expect(open).toBeDisabled();
    await userEvent.type(screen.getByLabelText(/Exact path/), "a.txt");
    expect(open).toBeEnabled();
  });

  it("disables Apply filters when the size range is backwards, but not when empty", async () => {
    server.use(http.get(`${API}/catalog/objects`, () => HttpResponse.json({ objects: [] })));
    renderWithProviders(<CatalogList />);
    const apply = await screen.findByRole("button", { name: "Apply filters" });
    expect(apply).toBeEnabled();
    await userEvent.click(screen.getByText("More filters"));
    await userEvent.type(screen.getByLabelText("Minimum size (bytes)"), "2,000");
    await userEvent.type(screen.getByLabelText("Maximum size (bytes)"), "10");
    expect(apply).toBeDisabled();
  });
});
