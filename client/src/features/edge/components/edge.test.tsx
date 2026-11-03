import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { describe, expect, it } from "vitest";
import { renderWithProviders } from "@/test/render";
import { API, server } from "@/test/server";
import { EdgeScreen } from "./edge-screen";

const device = (extra: object = {}) => ({
  id: "d1", name: "novaseq-1", dataset_id: "ds1", namespace: "instruments", path_prefix: "devices/novaseq-1/", pending_files: 3, pending_bytes: 2048,
  failed_files: 1, agent_version: "0.1.0", note: "", created_at: "2026-11-02T00:00:00Z", last_seen: new Date().toISOString(), ...extra,
});

function routes(devices: object[], recordings: object[] = []) {
  server.use(
    http.get(`${API}/edge-devices`, () => HttpResponse.json({ devices })),
    http.get(`${API}/edge-recordings`, () => HttpResponse.json({ recordings })),
    // The real API refuses a status it does not know, as ACTIVE once was.
    http.get(`${API}/datasets`, ({ request }) =>
      ["", "DRAFT", "VALIDATING", "READY", "READY_WITH_WARNINGS", "FAILED", "ARCHIVED"].includes(new URL(request.url).searchParams.get("status") ?? "")
        ? HttpResponse.json({ datasets: [{ id: "ds1", name: "instruments" }], next_cursor: "" })
        : HttpResponse.json({ error: "invalid status", code: "invalid_request" }, { status: 400 }),
    ),
  );
}

describe("EdgeScreen", () => {
  it("shows what waits on each device, and marks a silent one", async () => {
    routes(
      [device(), device({ id: "d2", name: "old-rig", last_seen: "2026-01-01T00:00:00Z" })],
      [
        { id: "r1", device_id: "d1", name: "run-7", kind: "RUN", state: "INCOMPLETE", missing: ["run-7/L001.fastq: not uploaded"], chunks: 2, bytes: 100, created_at: "2026-11-02T00:00:00Z" },
        { id: "r2", device_id: "d1", name: "run-6", kind: "RUN", state: "COMPLETE", missing: [], version_id: "v9", chunks: 4, bytes: 100, created_at: "2026-11-01T00:00:00Z" },
      ],
    );
    renderWithProviders(<EdgeScreen />, { permissions: ["org.read"] });
    expect((await screen.findAllByText(/3 files/)).length).toBe(2);
    expect(screen.getByText("Online")).toBeInTheDocument();
    expect(screen.getByText("Silent")).toBeInTheDocument();
    expect(await screen.findByText("run-7/L001.fastq: not uploaded")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Its snapshot" })).toHaveAttribute("href", "/app/datasets/ds1/versions/v9");
    expect(screen.queryByRole("button", { name: "Register device" })).not.toBeInTheDocument();
  });

  it("registers a device and shows its token once", async () => {
    let body: unknown;
    routes([]);
    server.use(
      http.post(`${API}/edge-devices`, async ({ request }) => {
        body = await request.json();
        return HttpResponse.json({ device: device({ name: "rig" }), token: "osp_pat_example" }, { status: 201 });
      }),
    );
    renderWithProviders(<EdgeScreen />, { permissions: ["org.read", "org.manage"] });
    await screen.findByRole("option", { name: "instruments" });
    await userEvent.type(screen.getByLabelText(/^Name/), "rig");
    await userEvent.selectOptions(screen.getByLabelText(/^Dataset/), "ds1");
    await userEvent.click(screen.getByRole("button", { name: "Register device" }));
    expect(await screen.findByText("osp_pat_example")).toBeInTheDocument();
    await waitFor(() => expect(body).toEqual({ name: "rig", dataset_id: "ds1", path_prefix: "" }));
    await userEvent.click(screen.getByRole("button", { name: "Done" }));
    expect(screen.queryByText("osp_pat_example")).not.toBeInTheDocument();
  });
});
