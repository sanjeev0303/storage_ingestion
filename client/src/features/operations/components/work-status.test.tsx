import { screen, within } from "@testing-library/react";
import { http, HttpResponse } from "msw";
import { describe, expect, it } from "vitest";
import { API, server } from "@/test/server";
import { renderWithProviders } from "@/test/render";
import { WorkerHealthPanel } from "./work-status";

const counts = {
  at: "2026-11-12T10:00:00Z",
  processing: { waiting: 0, held: 0, oldest_wait_seconds: 0 },
  ingestion: { active_jobs: 0, entries_waiting: 0, entries_in_flight: 0 },
  imports: { active: 0 },
  validations: { queued: 0, running: 0 },
  deletions: { pending: 0 },
};

function mockStatus(workers: unknown) {
  server.use(http.get(`${API}/operations/status`, () => HttpResponse.json({ ...counts, workers })));
}

describe("WorkerHealthPanel", () => {
  it("tells running, silent and never-seen workers apart by word", async () => {
    mockStatus([
      { kind: "processing", state: "RUNNING", last_seen_at: "2026-11-12T09:59:50Z" },
      { kind: "upload", state: "STALE", last_seen_at: "2026-11-12T09:00:00Z" },
      { kind: "webhook", state: "NOT_SEEN" },
    ]);
    renderWithProviders(<WorkerHealthPanel />);

    const processing = (await screen.findByText("Processing")).closest("tr")!;
    expect(within(processing).getByText("Running")).toBeInTheDocument();
    const upload = screen.getByText("Upload").closest("tr")!;
    expect(within(upload).getByText("No recent heartbeat")).toBeInTheDocument();
    const webhook = screen.getByText("Webhook").closest("tr")!;
    expect(within(webhook).getByText("Not seen")).toBeInTheDocument();
    expect(within(webhook).getByText("Never in the last day")).toBeInTheDocument();
    // Members get no host names.
    expect(screen.queryByRole("columnheader", { name: "Processes" })).not.toBeInTheDocument();
  });

  it("lists processes when the API sends them to a platform administrator", async () => {
    mockStatus([
      {
        kind: "processing",
        state: "RUNNING",
        last_seen_at: "2026-11-12T09:59:50Z",
        instances: [{ instance: "host-a-41", started_at: "2026-11-12T08:00:00Z", last_seen_at: "2026-11-12T09:59:50Z", stale: false }],
      },
    ]);
    renderWithProviders(<WorkerHealthPanel />, { user: { platform_role: "ADMIN" } });

    expect(await screen.findByRole("columnheader", { name: "Processes" })).toBeInTheDocument();
    expect(screen.getByText("host-a-41")).toBeInTheDocument();
  });

  it("says so when the API reports no heartbeats", async () => {
    server.use(http.get(`${API}/operations/status`, () => HttpResponse.json(counts)));
    renderWithProviders(<WorkerHealthPanel />);
    expect(await screen.findByText("This API does not report worker heartbeats.")).toBeInTheDocument();
  });
});
