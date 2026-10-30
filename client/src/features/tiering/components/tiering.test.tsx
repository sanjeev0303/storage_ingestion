import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { describe, expect, it } from "vitest";
import { renderWithProviders } from "@/test/render";
import { API, server } from "@/test/server";
import { CostsScreen } from "./costs-screen";
import { DatasetLifecycle } from "./dataset-lifecycle";
import { VersionCosts } from "./version-costs";

const estimate = (total: number, extra: Record<string, number> = {}) => ({
  operation: "x", requests: 10, bytes: 100, storage_usd: 0, request_usd: 0, egress_usd: 0, retrieval_usd: 0, total_usd: total, note: "list prices", ...extra,
});

const report = {
  month: "2026-10", stored: [{ connection_id: "sc_a", provider: "AWS_S3", tier: "ARCHIVE", objects: 3, bytes: 3072, monthly_usd: 0.01 }],
  storage_usd: 1.5, reads: 40, egress_bytes: 2048, writes: 9, restored_bytes: 0, request_usd: 0.02, egress_usd: 0.3, restore_usd: 0,
  total_usd: 1.82, savings: { packed_files: 80, packs: 2, reads_saved_per_pass: 78, shared_copies: 38, shared_bytes: 114, dedup_usd_month: 0, tiering_usd_month: 0.05 },
  note: "Amazon S3 list prices",
};

describe("CostsScreen", () => {
  it("shows the month and saves a budget and a rule", async () => {
    let budget: unknown;
    let rule: unknown;
    server.use(
      http.get(`${API}/cost-report`, () => HttpResponse.json(report)),
      http.get(`${API}/cost-budget`, () => HttpResponse.json({ monthly_usd: 0, monthly_requests: 0, action: "ALERT" })),
      http.put(`${API}/cost-budget`, async ({ request }) => {
        budget = await request.json();
        return HttpResponse.json(budget as Record<string, unknown>);
      }),
      http.get(`${API}/tier-rules`, () => HttpResponse.json({ rules: [] })),
      http.get(`${API}/tier-moves`, () => HttpResponse.json({ moves: [] })),
      http.post(`${API}/tier-rules`, async ({ request }) => {
        rule = await request.json();
        return HttpResponse.json({ id: "r1" }, { status: 201 });
      }),
    );
    renderWithProviders(<CostsScreen />, { permissions: ["org.read", "org.manage", "storage.read", "storage.manage"] });
    expect(await screen.findByText("$1.82")).toBeInTheDocument();
    expect(screen.getByText(/so a full read makes 78 fewer requests/)).toBeInTheDocument();

    await userEvent.type(screen.getByLabelText(/Dollars a month/), "50");
    await userEvent.selectOptions(screen.getByLabelText("Past the budget"), "BLOCK");
    await userEvent.click(screen.getByRole("button", { name: "Save budget" }));
    await waitFor(() => expect(budget).toEqual({ monthly_usd: 50, monthly_requests: 0, action: "BLOCK" }));

    await userEvent.type(screen.getByLabelText(/Rule name/), "Cold");
    await userEvent.click(screen.getByRole("button", { name: "Add rule" }));
    await waitFor(() => expect(rule).toEqual({ name: "Cold", tier: "ARCHIVE", min_age_days: 90, unread_days: 30 }));
  });
});

describe("DatasetLifecycle", () => {
  it("previews an expiry and carries it out after the typed confirmation", async () => {
    const sent: Record<string, unknown>[] = [];
    server.use(
      http.get(`${API}/datasets/ds1/costs`, () =>
        HttpResponse.json({ stored: [], monthly_usd: 0.5, years: 10, retain_usd: 60, archived_usd: 9, note: "" })),
      http.post(`${API}/datasets/ds1/version-expiry`, async ({ request }) => {
        const body = (await request.json()) as Record<string, unknown>;
        sent.push(body);
        return HttpResponse.json({
          expire: [{ id: "v1", version: 1, members: 3, bytes: 10, created_at: "2026-01-01T00:00:00Z" }],
          keep: [{ id: "v2", version: 2, members: 3, bytes: 10, created_at: "2026-10-01T00:00:00Z", kept: "a training run read it" }],
          freed_objects: 1, freed_bytes: 120, freed_usd_month: 0, dry_run: body.dry_run, deletions_requested: body.dry_run ? 0 : 1,
        });
      }),
    );
    renderWithProviders(<DatasetLifecycle datasetId="ds1" />, { permissions: ["datasets.read", "datasets.manage"] });
    expect(await screen.findByText("$60.00")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Preview" }));
    expect(await screen.findByText(/1 snapshot expires \(versions 1\), freeing 1 object /)).toBeInTheDocument();
    expect(screen.getByText("Version 2 stays: a training run read it")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Expire it" }));
    const dialog = await screen.findByRole("alertdialog");
    await userEvent.type(within(dialog).getByRole("textbox"), "expire");
    await userEvent.click(within(dialog).getByRole("button", { name: "Expire snapshots" }));
    await waitFor(() => expect(sent.map((b) => b.dry_run)).toEqual([true, false]));
  });
});

describe("VersionCosts", () => {
  it("prices reads and restores archived files", async () => {
    let restored: unknown;
    server.use(
      http.get(`${API}/datasets/ds1/versions/v1/cost-estimate`, () =>
        HttpResponse.json({ files: 4, bytes: 400, packed_files: 0, packs: 0, download: estimate(0.09, { egress_usd: 0.09 }),
          epoch: estimate(0), archived_files: 2, archived_bytes: 200, restore: estimate(0.03, { retrieval_usd: 0.03, hours: 5 }) })),
      http.post(`${API}/datasets/ds1/versions/v1/archive-restores`, async ({ request }) => {
        restored = await request.json();
        return HttpResponse.json({ archived: 2, requested: 2, already_open: 0, readable: 0, estimate: estimate(0.03) }, { status: 202 });
      }),
    );
    renderWithProviders(<VersionCosts id="ds1" versionId="v1" />, { permissions: ["datasets.read", "assets.download"] });
    expect(await screen.findByText(/takes about 5 hours/)).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Restore archived files for 7 days" }));
    await waitFor(() => expect(restored).toEqual({ days: 7, speed: "STANDARD" }));
  });
});
