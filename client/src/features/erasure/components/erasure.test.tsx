import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { describe, expect, it } from "vitest";
import { renderWithProviders } from "@/test/render";
import { API, server } from "@/test/server";
import { ErasureScreen } from "./erasure-screen";

const plan = {
  objects: [
    { asset_id: "a1", object_id: "o1", version: 1, path: "cohort/p1-scan.txt", sha256: "ab".repeat(32), size_bytes: 300, connection_id: "platform",
      status: "AVAILABLE", held_by: [] },
    { asset_id: "a2", object_id: "o2", version: 1, path: "cohort/p1-notes.txt", sha256: "cd".repeat(32), size_bytes: 200, connection_id: "sc_aws",
      status: "AVAILABLE", held_by: ["legal hold: case 7"] },
  ],
  bytes: 500,
  connections: ["platform", "sc_aws"],
  versions: [{ dataset_id: "d1", dataset_name: "cohort", version_id: "v1", version: 1, members: 2, current: true }],
  drafts: [{ dataset_id: "d1", dataset_name: "cohort", members: 2 }],
  models: [{ id: "m1", name: "risk-model", version: "1", run_id: "r1", run_name: "cohort-model" }],
  not_searched: ["near copies: only exact SHA-256 copies were matched"],
};

const erasure = (state: string, extra: object = {}) => ({
  id: "e1", kind: "SUBJECT_WITHDRAWAL", state, reason: "participant withdrew", reference: "WD-1", institution: "Example Biobank",
  selector: { subject_ids: ["P-1"], sha256s: [], object_ids: [] }, subject_hashes: ["x"], plan, successors: [], items: [],
  progress: { total: 0, destroyed: 0, held: 0, failed: 0, pending: 0 }, requested_by: "someone-else", certified: false,
  created_at: "2026-10-30T10:00:00Z", ...extra,
});

describe("ErasureScreen", () => {
  it("finds a subject, shows what it reaches and records the request", async () => {
    let searched: unknown;
    let created: Record<string, unknown> | undefined;
    server.use(
      http.get(`${API}/erasures`, () => HttpResponse.json({ erasures: [] })),
      http.post(`${API}/erasures/search`, async ({ request }) => {
        searched = await request.json();
        return HttpResponse.json(plan);
      }),
      http.post(`${API}/erasures`, async ({ request }) => {
        created = (await request.json()) as Record<string, unknown>;
        return HttpResponse.json(erasure("PLANNED"), { status: 201 });
      }),
      http.get(`${API}/erasures/e1`, () => HttpResponse.json(erasure("PLANNED"))),
    );
    renderWithProviders(<ErasureScreen />, { permissions: ["retention.manage"] });
    expect(await screen.findByText("No erasure requests")).toBeInTheDocument();

    await userEvent.type(screen.getByLabelText("SHA-256 hashes"), "not-a-hash");
    expect(screen.getByRole("button", { name: "Find" })).toBeDisabled();
    await userEvent.clear(screen.getByLabelText("SHA-256 hashes"));
    await userEvent.type(screen.getByLabelText("Subjects"), "P-1{enter}P-1 ");
    await userEvent.click(screen.getByRole("button", { name: "Find" }));
    expect(await screen.findByText("legal hold: case 7")).toBeInTheDocument();
    expect(searched).toEqual({ subject_ids: ["P-1", "P-1"], sha256s: [], object_ids: [], subject_key: "subject_id" });
    expect(screen.getByText("risk-model 1")).toBeInTheDocument();
    expect(screen.getByText(/Not searched: near copies/)).toBeInTheDocument();

    await userEvent.type(screen.getByLabelText(/^Reason/), "participant withdrew");
    await userEvent.type(screen.getByLabelText(/^Institution/), "Example Biobank");
    await userEvent.click(screen.getByRole("button", { name: "Record erasure request" }));
    await waitFor(() => expect(created).toMatchObject({ kind: "SUBJECT_WITHDRAWAL", reason: "participant withdrew", institution: "Example Biobank" }));
    expect(await screen.findByRole("button", { name: "Approve" })).toBeInTheDocument();
  });

  it("erases only after the typed confirmation, then follows the deletion", async () => {
    let executed = false;
    let state = "APPROVED";
    server.use(
      http.get(`${API}/erasures`, () => HttpResponse.json({ erasures: [erasure(state)] })),
      http.get(`${API}/erasures/e1`, () =>
        HttpResponse.json(
          state === "APPROVED"
            ? erasure(state)
            : erasure(state, {
                items: [
                  { asset_id: "a1", path: "cohort/p1-scan.txt", sha256: "ab", size_bytes: 300, connection_id: "platform", state: "DESTROYED", reason: "" },
                  { asset_id: "a2", path: "cohort/p1-notes.txt", sha256: "cd", size_bytes: 200, connection_id: "sc_aws", state: "HELD", reason: "legal hold h1" },
                ],
                progress: { total: 2, destroyed: 1, held: 1, failed: 0, pending: 0 },
                successors: [{ dataset_id: "d1", version_id: "v1", version: 1, successor_id: "v2", successor_version: 2, removed: 2 }],
              }),
        )),
      http.post(`${API}/erasures/e1/execute`, () => {
        executed = true;
        state = "WAITING_ON_HOLD";
        return HttpResponse.json(erasure(state), { status: 202 });
      }),
    );
    renderWithProviders(<ErasureScreen />, { permissions: ["retention.manage", "assets.delete"] });
    await userEvent.click(await screen.findByRole("button", { name: /Subject withdrawal/ }));
    await userEvent.click(await screen.findByRole("button", { name: "Erase" }));
    const dialog = await screen.findByRole("alertdialog");
    expect(within(dialog).getByRole("button", { name: "Erase" })).toBeDisabled();
    await userEvent.type(within(dialog).getByRole("textbox"), "ERASE");
    await userEvent.click(within(dialog).getByRole("button", { name: "Erase" }));
    await waitFor(() => expect(executed).toBe(true));
    expect(await screen.findByText(/Version 1 is superseded by version 2/)).toBeInTheDocument();
    expect(screen.getByText(/legal hold h1/)).toBeInTheDocument();
    expect(screen.getByText(/Retrain or retire: risk-model 1/)).toBeInTheDocument();
  });
});
