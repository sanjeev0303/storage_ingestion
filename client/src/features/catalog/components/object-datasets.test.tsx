import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { describe, expect, it } from "vitest";
import { API, server } from "@/test/server";
import { renderWithProviders } from "@/test/render";
import { ObjectDatasets } from "./object-datasets";

const draft = (id: string, name: string) => ({ dataset_id: id, dataset_name: name, added_at: "2026-11-01T00:00:00Z" });

describe("ObjectDatasets", () => {
  it("pages the drafts on their own with the cursor the API gave", async () => {
    const seen: string[] = [];
    server.use(
      http.get(`${API}/catalog/objects/obj-1/datasets`, ({ request }) => {
        const url = new URL(request.url);
        seen.push(url.search);
        if (url.searchParams.get("part") === "snapshots") {
          return HttpResponse.json({
            datasets: [],
            snapshots: [{ dataset_id: "ds-1", dataset_name: "pets", version_id: "v-2", version: 2, object_version: 1 }],
          });
        }
        if (url.searchParams.get("cursor") === "next-1") {
          return HttpResponse.json({ datasets: [draft("ds-2", "zoo")], snapshots: [] });
        }
        return HttpResponse.json({ datasets: [draft("ds-1", "pets")], snapshots: [], next_datasets_cursor: "next-1" });
      }),
    );
    renderWithProviders(<ObjectDatasets objectId="obj-1" />);

    expect(await screen.findByRole("link", { name: "pets" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "pets version 2" })).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Load more" }));
    expect(await screen.findByRole("link", { name: "zoo" })).toBeInTheDocument();
    expect(screen.getByText("2 loaded, end of list")).toBeInTheDocument();
    expect(seen.some((q) => q.includes("part=datasets") && q.includes("cursor=next-1"))).toBe(true);
  });

  it("says plainly when nothing uses the object", async () => {
    server.use(http.get(`${API}/catalog/objects/obj-1/datasets`, () => HttpResponse.json({ datasets: [], snapshots: [] })));
    renderWithProviders(<ObjectDatasets objectId="obj-1" />);
    expect(await screen.findByText("No dataset or snapshot uses this object.")).toBeInTheDocument();
  });
});
