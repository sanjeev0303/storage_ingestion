import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { describe, expect, it } from "vitest";
import { API, server } from "@/test/server";
import { renderWithProviders } from "@/test/render";
import { HoldsInEffect } from "./retention-current";

const hold = (id: string, reason: string) => ({
  id,
  scope: "DATASET",
  scope_key: "pets",
  reason,
  placed_by: "user-1",
  placed_at: "2026-11-01T10:00:00Z",
});

describe("HoldsInEffect", () => {
  it("loads the next page of holds with the cursor the API gave", async () => {
    const asked: string[] = [];
    server.use(
      http.get(`${API}/legal-holds`, ({ request }) => {
        const url = new URL(request.url);
        asked.push(url.search);
        if (url.searchParams.get("cursor") === "page-2") {
          return HttpResponse.json({ holds: [hold("hold-2", "older matter")] });
        }
        return HttpResponse.json({ holds: [hold("hold-1", "newer matter")], next_cursor: "page-2" });
      }),
    );
    renderWithProviders(<HoldsInEffect />);

    expect(await screen.findByText("newer matter")).toBeInTheDocument();
    expect(screen.getByText("1 loaded, more available")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Load more" }));
    expect(await screen.findByText("older matter")).toBeInTheDocument();
    expect(screen.getByText("2 loaded, end of list")).toBeInTheDocument();
    expect(asked[1]).toContain("cursor=page-2");
  });

  it("shows no paging controls when one page holds everything", async () => {
    server.use(http.get(`${API}/legal-holds`, () => HttpResponse.json({ holds: [hold("hold-1", "court order")] })));
    renderWithProviders(<HoldsInEffect />);
    expect(await screen.findByText("court order")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Load more" })).not.toBeInTheDocument();
  });
});
