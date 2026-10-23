import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { describe, expect, it, vi } from "vitest";
import { API, server } from "@/test/server";
import { renderWithProviders } from "@/test/render";
import { makeDataset } from "../fixtures";
import { DatasetOverview } from "./dataset-overview";
import { DatasetShell } from "./dataset-shell";
import { SnapshotControl } from "./snapshot-control";

vi.mock("next/navigation", () => ({
  useRouter: () => ({ push: vi.fn() }),
  usePathname: () => "/app/datasets/ds-1",
}));

function serveDataset(over = {}) {
  server.use(http.get(`${API}/datasets/ds-1`, () => HttpResponse.json(makeDataset(over))));
}

describe("dataset overview", () => {
  it("explains that the draft changes and versions never do", async () => {
    serveDataset({ lifecycle_status: "READY" });
    renderWithProviders(<DatasetOverview id="ds-1" />);
    expect(await screen.findByText("The dataset is a draft")).toBeInTheDocument();
    expect(screen.getByText("A snapshot is a version, and it never changes")).toBeInTheDocument();
  });

  it("marks the shell as a mutable draft with its lifecycle state", async () => {
    serveDataset({ lifecycle_status: "DRAFT" });
    renderWithProviders(
      <DatasetShell id="ds-1">
        <p>child</p>
      </DatasetShell>,
    );
    expect(await screen.findByText("Mutable draft")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Lineage" })).toHaveAttribute("href", "/app/datasets/ds-1/lineage");
    expect(screen.getByRole("link", { name: "Validation" })).toHaveAttribute("href", "/app/datasets/ds-1/validation");
    expect(screen.getByRole("link", { name: "Imports" })).toHaveAttribute("href", "/app/datasets/ds-1/imports");
  });

  it("is read-only when archived", async () => {
    serveDataset({ lifecycle_status: "ARCHIVED", archived_at: "2026-02-01T00:00:00Z" });
    renderWithProviders(
      <DatasetShell id="ds-1">
        <DatasetOverview id="ds-1" />
      </DatasetShell>,
    );
    expect(await screen.findByText("This dataset is archived")).toBeInTheDocument();
    expect(screen.queryByText("Mutable draft")).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Edit" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Archive dataset" })).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Create snapshot" })).toBeDisabled();
  });

  it("hides edit and archive without write and manage permissions", async () => {
    serveDataset();
    renderWithProviders(<DatasetOverview id="ds-1" />, { permissions: ["datasets.read"] });
    await screen.findByText("The dataset is a draft");
    expect(screen.queryByRole("button", { name: "Edit" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Archive dataset" })).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Create snapshot" })).toBeDisabled();
  });

  it("reloads the latest values and tells the user on a 412", async () => {
    let revision = 3;
    let description = "old";
    server.use(
      http.get(`${API}/datasets/ds-1`, () => HttpResponse.json(makeDataset({ revision, description }))),
      http.patch(`${API}/datasets/ds-1`, () => {
        revision = 4;
        description = "changed by someone else";
        return HttpResponse.json({ error: "stale", code: "revision_conflict" }, { status: 412 });
      }),
      http.get(`${API}/validation-profiles`, () => HttpResponse.json({ profiles: [] })),
    );
    renderWithProviders(<DatasetOverview id="ds-1" />);
    await userEvent.click(await screen.findByRole("button", { name: "Edit" }));
    const dialog = await screen.findByRole("dialog");
    const box = within(dialog).getByLabelText("Description");
    await userEvent.clear(box);
    await userEvent.type(box, "mine");
    await userEvent.click(within(dialog).getByRole("button", { name: "Save changes" }));

    expect(await within(dialog).findByText(/your edits were not saved/)).toBeInTheDocument();
    await waitFor(() => expect(within(dialog).getByLabelText("Description")).toHaveValue("changed by someone else"));
  });

  it("sends the revision and merges metadata with null for removed keys", async () => {
    let body: Record<string, unknown> = {};
    server.use(
      http.get(`${API}/datasets/ds-1`, () => HttpResponse.json(makeDataset({ metadata: { a: "1", gone: "x" } }))),
      http.patch(`${API}/datasets/ds-1`, async ({ request }) => {
        body = (await request.json()) as Record<string, unknown>;
        return HttpResponse.json(makeDataset());
      }),
      http.get(`${API}/validation-profiles`, () => HttpResponse.json({ profiles: [] })),
    );
    renderWithProviders(<DatasetOverview id="ds-1" />);
    await userEvent.click(await screen.findByRole("button", { name: "Edit" }));
    const dialog = await screen.findByRole("dialog");
    const meta = within(dialog).getByLabelText("Metadata");
    await userEvent.clear(meta);
    await userEvent.type(meta, "a=2");
    await userEvent.click(within(dialog).getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(body.revision).toBe(3));
    expect(body.metadata).toEqual({ a: "2", gone: null });
  });

  it("asks for the dataset name before archiving", async () => {
    let archived = false;
    server.use(
      http.get(`${API}/datasets/ds-1`, () => HttpResponse.json(makeDataset())),
      http.post(`${API}/datasets/ds-1/archive`, () => {
        archived = true;
        return HttpResponse.json(makeDataset({ lifecycle_status: "ARCHIVED" }));
      }),
    );
    renderWithProviders(<DatasetOverview id="ds-1" />);
    await userEvent.click(await screen.findByRole("button", { name: "Archive dataset" }));
    const dialog = await screen.findByRole("alertdialog");
    const confirm = within(dialog).getByRole("button", { name: "Archive dataset" });
    expect(confirm).toBeDisabled();
    await userEvent.type(within(dialog).getByRole("textbox"), "pets");
    await userEvent.click(confirm);
    await waitFor(() => expect(archived).toBe(true));
  });

  it("explains archiving while validating", async () => {
    server.use(
      http.get(`${API}/datasets/ds-1`, () => HttpResponse.json(makeDataset({ lifecycle_status: "VALIDATING" }))),
      http.post(`${API}/datasets/ds-1/archive`, () =>
        HttpResponse.json({ error: "bad", code: "invalid_transition" }, { status: 409 }),
      ),
    );
    renderWithProviders(<DatasetOverview id="ds-1" />);
    await userEvent.click(await screen.findByRole("button", { name: "Archive dataset" }));
    const dialog = await screen.findByRole("alertdialog");
    await userEvent.type(within(dialog).getByRole("textbox"), "pets");
    await userEvent.click(within(dialog).getByRole("button", { name: "Archive dataset" }));
    expect(await within(dialog).findByText(/A validation is running/)).toBeInTheDocument();
  });
});

describe("snapshot", () => {
  async function open() {
    renderWithProviders(<SnapshotControl dataset={makeDataset()} />);
    await userEvent.click(screen.getByRole("button", { name: "Create snapshot" }));
    return await screen.findByRole("dialog");
  }

  it.each([
    ["no_members", /no members/],
    ["unresolved_members", /no pinned version/],
    ["dataset_archived", /archived/],
  ])("maps the %s error", async (code, message) => {
    server.use(http.post(`${API}/datasets/ds-1/snapshots`, () => HttpResponse.json({ error: "x", code }, { status: 409 })));
    const dialog = await open();
    await userEvent.click(within(dialog).getByRole("button", { name: "Create snapshot" }));
    expect(await within(dialog).findByRole("alert")).toHaveTextContent(message);
  });

  it("maps snapshot_too_large", async () => {
    server.use(
      http.post(`${API}/datasets/ds-1/snapshots`, () => HttpResponse.json({ error: "x", code: "snapshot_too_large" }, { status: 422 })),
    );
    const dialog = await open();
    await userEvent.click(within(dialog).getByRole("button", { name: "Create snapshot" }));
    expect(await within(dialog).findByRole("alert")).toHaveTextContent(/more members than one snapshot/);
  });

  it("reuses the idempotency key on retry and links to the new version on success", async () => {
    const keys: string[] = [];
    server.use(
      http.post(`${API}/datasets/ds-1/snapshots`, async ({ request }) => {
        const body = (await request.json()) as { idempotency_key: string };
        keys.push(body.idempotency_key);
        if (keys.length === 1) return HttpResponse.json({ error: "x", code: "no_members" }, { status: 409 });
        return HttpResponse.json(
          { id: "ver-1", dataset_id: "ds-1", version: 2, status: "FINALIZED", member_count: 5 },
          { status: 201 },
        );
      }),
    );
    const dialog = await open();
    await userEvent.click(within(dialog).getByRole("button", { name: "Create snapshot" }));
    await userEvent.click(await within(dialog).findByRole("button", { name: "Try again" }));
    const link = await within(dialog).findByRole("link", { name: "Open version 2" });
    expect(link).toHaveAttribute("href", "/app/datasets/ds-1/versions/ver-1");
    expect(keys[0]).toBe(keys[1]);
    expect(within(dialog).getByText(/members pinned/)).toBeInTheDocument();
  });

  it("says it can take minutes while running", async () => {
    server.use(
      http.post(`${API}/datasets/ds-1/snapshots`, async () => {
        await new Promise((r) => setTimeout(r, 200));
        return HttpResponse.json({ id: "v", version: 1, member_count: 1 }, { status: 201 });
      }),
    );
    const dialog = await open();
    await userEvent.click(within(dialog).getByRole("button", { name: "Create snapshot" }));
    expect(await within(dialog).findByText(/several minutes/)).toBeInTheDocument();
    await within(dialog).findByRole("link", { name: /Open version/ });
  });
});
