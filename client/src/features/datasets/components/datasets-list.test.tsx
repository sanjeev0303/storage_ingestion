import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { describe, expect, it } from "vitest";
import { API, server } from "@/test/server";
import { renderWithProviders } from "@/test/render";
import { makeDataset } from "../fixtures";
import { DatasetsList } from "./datasets-list";

describe("DatasetsList", () => {
  it("lists datasets by lifecycle status and pages with the cursor", async () => {
    const cursors: (string | null)[] = [];
    server.use(
      http.get(`${API}/datasets`, ({ request }) => {
        const url = new URL(request.url);
        cursors.push(url.searchParams.get("cursor"));
        if (!url.searchParams.get("cursor")) {
          return HttpResponse.json({
            datasets: [makeDataset({ lifecycle_status: "READY_WITH_WARNINGS", current_version_id: "v-1234567890" })],
            next_cursor: "c1.abc",
          });
        }
        return HttpResponse.json({ datasets: [makeDataset({ id: "ds-2", name: "cats" })] });
      }),
    );

    renderWithProviders(<DatasetsList />);
    expect(await screen.findByRole("link", { name: "pets" })).toBeInTheDocument();
    expect(within(screen.getByRole("table")).getByText("Ready with warnings")).toBeInTheDocument();
    expect(screen.getByText("v-123456")).toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: "Load more" }));
    expect(await screen.findByRole("link", { name: "cats" })).toBeInTheDocument();
    expect(cursors).toEqual([null, "c1.abc"]);
    expect(screen.getByText("No snapshot yet")).toBeInTheDocument();
  });

  it("sends the chosen lifecycle status as the filter", async () => {
    const seen: (string | null)[] = [];
    server.use(
      http.get(`${API}/datasets`, ({ request }) => {
        seen.push(new URL(request.url).searchParams.get("status"));
        return HttpResponse.json({ datasets: [] });
      }),
    );
    renderWithProviders(<DatasetsList />);
    await screen.findByText("No datasets yet");
    await userEvent.selectOptions(screen.getByLabelText("State"), "ARCHIVED");
    expect(await screen.findByText("No datasets in this state")).toBeInTheDocument();
    expect(seen).toContain("ARCHIVED");
  });

  it("teaches what a dataset is when there are none", async () => {
    server.use(http.get(`${API}/datasets`, () => HttpResponse.json({ datasets: [] })));
    renderWithProviders(<DatasetsList />);
    expect(await screen.findByText(/immutable version/)).toBeInTheDocument();
  });

  it("hides New dataset without datasets.write", async () => {
    server.use(http.get(`${API}/datasets`, () => HttpResponse.json({ datasets: [] })));
    renderWithProviders(<DatasetsList />, { permissions: ["datasets.read"] });
    await screen.findByText("No datasets yet");
    expect(screen.queryByRole("link", { name: "New dataset" })).not.toBeInTheDocument();
  });

  it("shows New dataset with datasets.write", async () => {
    server.use(http.get(`${API}/datasets`, () => HttpResponse.json({ datasets: [] })));
    renderWithProviders(<DatasetsList />, { permissions: ["datasets.read", "datasets.write"] });
    expect(await screen.findByRole("link", { name: "New dataset" })).toHaveAttribute("href", "/app/datasets/new");
  });

  it("shows permission denied on 403 and an error with retry otherwise", async () => {
    server.use(http.get(`${API}/datasets`, () => HttpResponse.json({ error: "permission denied", code: "forbidden" }, { status: 403 })));
    const first = renderWithProviders(<DatasetsList />);
    expect(await screen.findByText("Your role cannot do this")).toBeInTheDocument();
    first.unmount();

    let calls = 0;
    server.use(
      http.get(`${API}/datasets`, () => {
        calls += 1;
        return calls === 1
          ? HttpResponse.json({ error: "boom", code: "internal" }, { status: 500 })
          : HttpResponse.json({ datasets: [makeDataset()] });
      }),
    );
    renderWithProviders(<DatasetsList />);
    await userEvent.click(await screen.findByRole("button", { name: "Try again" }));
    await waitFor(() => expect(screen.getByRole("link", { name: "pets" })).toBeInTheDocument());
  });
});
