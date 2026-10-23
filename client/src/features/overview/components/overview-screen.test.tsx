import { screen } from "@testing-library/react";
import { http, HttpResponse } from "msw";
import { describe, expect, it } from "vitest";
import { renderWithProviders } from "@/test/render";
import { API, server } from "@/test/server";
import { OverviewScreen } from "./overview-screen";

const progress = { total: 1, published: 0, failed: 0, invalid: 0 };

describe("OverviewScreen", () => {
  it("lists failed jobs from the latest page and links to them", async () => {
    server.use(
      http.get(`${API}/ingestion/jobs`, () =>
        HttpResponse.json({
          jobs: [
            { id: "job-fail", dataset_id: "d", state: "FAILED", error: "manifest rejected", progress, updated_at: "2026-01-01T00:00:00Z" },
            { id: "job-ok", dataset_id: "d", state: "COMPLETED", progress, updated_at: "2026-01-01T00:00:00Z" },
          ],
        }),
      ),
      http.get(`${API}/datasets`, () => HttpResponse.json({ datasets: [] })),
      http.get(`${API}/storage-connections`, () => HttpResponse.json({ connections: [], default_connection_id: "platform" })),
      http.get(`${API}/organization/audit`, () => HttpResponse.json({ events: [] })),
    );
    renderWithProviders(<OverviewScreen />, { permissions: ["ingestion.read", "datasets.read", "storage.read", "audit.read"] });

    const link = await screen.findByRole("link", { name: "job-fail" });
    expect(link).toHaveAttribute("href", "/app/ingestion/jobs/job-fail");
    expect(screen.queryByRole("link", { name: "job-ok" })).not.toBeInTheDocument();
  });

  it("says when a panel is hidden by permissions instead of showing zero", async () => {
    renderWithProviders(<OverviewScreen />, { permissions: ["org.read"] });
    expect(await screen.findByText(/ingestion\.read/)).toBeInTheDocument();
    expect(screen.getByText(/audit\.read/)).toBeInTheDocument();
    expect(screen.getByText(/quota\.read/)).toBeInTheDocument();
    expect(screen.queryByText(/No failed or stalled jobs/)).not.toBeInTheDocument();
  });

  it("gives one failing panel its own error and retry", async () => {
    server.use(http.get(`${API}/ingestion/jobs`, () => HttpResponse.json({ error: "boom" }, { status: 500 })));
    renderWithProviders(<OverviewScreen />, { permissions: ["ingestion.read"] });
    expect(await screen.findByRole("button", { name: "Try again" })).toBeInTheDocument();
  });
});
