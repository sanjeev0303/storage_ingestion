import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { describe, expect, it } from "vitest";
import { renderWithProviders } from "@/test/render";
import { API, server } from "@/test/server";
import { ReadForTraining } from "./read-for-training";
import { RunChecks } from "./run-checks";
import { StorageLayoutPanel } from "./storage-layout-panel";

describe("ReadForTraining", () => {
  it("shows the shards and whether the manifest is signed", async () => {
    server.use(
      http.get(`${API}/datasets/ds1/versions/v1/shards`, () =>
        HttpResponse.json({ shards: [
          { ordinals: [1, 2], files: 2, bytes: 2048, packs: 1, first_path: "a/1.jpg", last_path: "a/1.txt" },
          { ordinals: [3, 4], files: 2, bytes: 2048, packs: 1, first_path: "a/2.jpg", last_path: "a/2.txt" },
        ] }),
      ),
      http.get(`${API}/datasets/ds1/versions/v1/signed-manifest`, () =>
        HttpResponse.json({ body: "{}", algorithm: "Ed25519", key_id: "k1", signature: "c2ln" }),
      ),
    );
    renderWithProviders(<ReadForTraining id="ds1" versionId="v1" />, { permissions: ["datasets.read"] });
    expect(await screen.findByText("Signed with Ed25519 key k1")).toBeInTheDocument();
    expect(screen.getByText(/KiB, 2 files$/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Download signed manifest" })).toBeInTheDocument();
  });
});

describe("StorageLayoutPanel", () => {
  it("lets an owner set the pack size and queue a repack", async () => {
    let size: unknown;
    let repacked = false;
    server.use(
      http.get(`${API}/packing-settings`, () => HttpResponse.json({ target_pack_bytes: 0 })),
      http.put(`${API}/packing-settings`, async ({ request }) => {
        size = await request.json();
        return HttpResponse.json(size as { target_pack_bytes: number });
      }),
      http.get(`${API}/datasets/ds1/repacks`, () =>
        HttpResponse.json({ repacks: repacked
          ? [{ id: "r1", dataset_id: "ds1", state: "PENDING", packs_planned: 0, entries_planned: 0, requested_by: "u1", created_at: "2026-10-30T10:00:00Z" }]
          : [] }),
      ),
      http.post(`${API}/datasets/ds1/repacks`, () => {
        repacked = true;
        return HttpResponse.json({ id: "r1", state: "PENDING" }, { status: 202 });
      }),
    );
    renderWithProviders(<StorageLayoutPanel datasetId="ds1" />, { permissions: ["datasets.read", "datasets.manage", "org.manage"] });
    await userEvent.type(await screen.findByLabelText(/Target pack size, MiB/), "256");
    await userEvent.click(screen.getByRole("button", { name: "Save size" }));
    await waitFor(() => expect(size).toEqual({ target_pack_bytes: 256 * 1024 * 1024 }));
    await userEvent.click(screen.getByRole("button", { name: "Repack in path order" }));
    expect(await screen.findByRole("button", { name: "A repack is under way" })).toBeDisabled();
    expect(screen.getByText("Planning")).toBeInTheDocument();
  });

  it("shows members the size without a form", async () => {
    server.use(
      http.get(`${API}/packing-settings`, () => HttpResponse.json({ target_pack_bytes: 64 * 1024 * 1024 })),
      http.get(`${API}/datasets/ds1/repacks`, () => HttpResponse.json({ repacks: [] })),
    );
    renderWithProviders(<StorageLayoutPanel datasetId="ds1" />, { permissions: ["datasets.read"] });
    expect(await screen.findByText(/Target pack size: 64\.0 MiB/)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Repack in path order" })).not.toBeInTheDocument();
  });
});

describe("RunChecks", () => {
  it("shows failed reads and the checkpoint to roll back to", async () => {
    server.use(
      http.get(`${API}/training-runs/run1/verifications`, () =>
        HttpResponse.json({ verifications: [{ id: "x", host: "gpu-1", sample_rate: 0.1, checked: 70, failed: 1,
          failures: [{ path: "a", reason: "sha256 mismatch" }], created_at: "2026-10-30T10:00:00Z" }] }),
      ),
      http.get(`${API}/training-runs/run1/checkpoints`, () =>
        HttpResponse.json({ checkpoints: [{ id: "c2", step: 200 }, { id: "c1", step: 100 }],
          last_verified: { id: "c1", step: 100, verified_by: "gpu-2" } }),
      ),
    );
    renderWithProviders(<RunChecks runId="run1" />, { permissions: ["datasets.read"] });
    expect(await screen.findByText("1 failed")).toBeInTheDocument();
    expect(screen.getByText("Roll back to step 100, verified on gpu-2")).toBeInTheDocument();
  });
});
