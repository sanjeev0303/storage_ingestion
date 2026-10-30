import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { describe, expect, it } from "vitest";
import { renderWithProviders } from "@/test/render";
import { API, server } from "@/test/server";
import { ProtectionScreen } from "./protection-screen";

const dataset = {
  id: "ds1", name: "scans", completion_policy: "ALL", status: "READY", max_bytes: 0, reserved_bytes: 0, stored_bytes: 0,
  created_at: "2026-10-30T09:00:00Z", updated_at: "2026-10-30T09:00:00Z", description: "", tags: [], metadata: {},
  lifecycle_status: "ACTIVE", revision: 7, member_count: 3, created_by: "u1", residency: [], require_customer_key: false,
  escrow: false, vault_retention_days: 0, scrub_days: 0, storage_connection_id: "sc_a",
};

function base(extra: Record<string, unknown> = {}) {
  server.use(
    http.get(`${API}/datasets/ds1`, () => HttpResponse.json({ ...dataset, ...extra })),
    http.get(`${API}/datasets/ds1/replication`, () =>
      HttpResponse.json({ connection_id: "sc_b", objects: 10, replicated: 8, pending: 2, failing: 0, replicated_bytes: 4096,
        recovery_point_seconds: 300, escrow: true, vault_retention_days: 0, scrub_days: 0 }),
    ),
    http.get(`${API}/copy-jobs`, () => HttpResponse.json({ jobs: [{ id: "j1", kind: "DRILL", state: "COMPLETED", total_items: 10,
      done_items: 10, failed_items: 0, total_bytes: 100, done_bytes: 100, created_by: "u1", created_at: "2026-10-30T10:00:00Z",
      report: { intact: 10, sampled: 10, estimated_restore_seconds: 5400 } }] })),
    http.get(`${API}/datasets/ds1/scrubs`, () => HttpResponse.json({ scrubs: [] })),
    http.get(`${API}/datasets/ds1/versions`, () => HttpResponse.json({ versions: [{ id: "v1", version: 1 }] })),
    http.get(`${API}/storage-connections`, () => HttpResponse.json({ default_connection_id: "sc_a", connections: [
      { id: "sc_a", name: "Primary", status: "ACTIVE" }, { id: "sc_b", name: "Second provider", status: "ACTIVE" }] })),
  );
}

describe("ProtectionScreen", () => {
  it("shows the recovery point and what a drill measured", async () => {
    base({ replica_connection_id: "sc_b", escrow: true });
    renderWithProviders(<ProtectionScreen id="ds1" />, { permissions: ["datasets.read", "datasets.manage"] });
    expect(await screen.findByText("5 minutes behind (2 objects waiting)")).toBeInTheDocument();
    expect(screen.getByText(/10 of 10 read back intact; a full restore would take about 90 minutes/)).toBeInTheDocument();
  });

  it("sets a replica, escrow and scrubs with the dataset's revision", async () => {
    let sent: unknown;
    base();
    server.use(http.patch(`${API}/datasets/ds1`, async ({ request }) => {
      sent = await request.json();
      return HttpResponse.json({ ...dataset, revision: 8 });
    }));
    renderWithProviders(<ProtectionScreen id="ds1" />, { permissions: ["datasets.read", "datasets.manage"] });
    await screen.findByRole("option", { name: "Second provider" });
    await userEvent.selectOptions(screen.getByLabelText("Replica connection"), "sc_b");
    expect(within(screen.getByLabelText("Replica connection")).queryByText("Primary")).not.toBeInTheDocument();
    await userEvent.click(screen.getByLabelText(/Escrow: nothing in the dataset is deleted/));
    await userEvent.clear(screen.getByLabelText(/Scrub every, days/));
    await userEvent.type(screen.getByLabelText(/Scrub every, days/), "30");
    await userEvent.click(screen.getByRole("button", { name: "Save protection" }));
    await waitFor(() => expect(sent).toEqual({ revision: 7, replica_connection_id: "sc_b", escrow: true, scrub_days: 30 }));
  });

  it("estimates a migration before starting it", async () => {
    let asked: Record<string, unknown>[] = [];
    base();
    server.use(http.post(`${API}/datasets/ds1/migrations`, async ({ request }) => {
      const body = (await request.json()) as Record<string, unknown>;
      asked.push(body);
      return HttpResponse.json({ plan: { objects: 3, bytes: 3072, already_there: 0, egress_estimate_usd: 0.27, egress_note: "list prices" },
        ...(body.dry_run ? {} : { job: { id: "j2" } }) }, { status: body.dry_run ? 200 : 202 });
    }));
    renderWithProviders(<ProtectionScreen id="ds1" />, { permissions: ["datasets.read", "datasets.manage", "storage.manage"] });
    await userEvent.click(await screen.findByRole("button", { name: "Migrate" }));
    await userEvent.selectOptions(screen.getByLabelText("Target connection"), "sc_b");
    await userEvent.click(screen.getByRole("button", { name: "Estimate" }));
    expect(await screen.findByText("$0.27")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Migrate 3 objects" }));
    await userEvent.click(within(await screen.findByRole("alertdialog")).getByRole("button", { name: "Start migration" }));
    await waitFor(() => expect(asked).toEqual([{ target_connection_id: "sc_b", dry_run: true }, { target_connection_id: "sc_b" }]));
  });
});
