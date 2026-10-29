import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { describe, expect, it, vi } from "vitest";
import { API, server } from "@/test/server";
import { renderWithProviders } from "@/test/render";
import { makeDataset } from "../fixtures";
import type { DatasetVersion, MergeResult } from "../types";
import { BranchesScreen } from "./branches-screen";

const push = vi.fn();
vi.mock("next/navigation", () => ({
  useRouter: () => ({ push }),
  usePathname: () => "/app/datasets/ds-1/branches",
}));

function version(over: Partial<DatasetVersion> = {}): DatasetVersion {
  return {
    id: "ver-1",
    dataset_id: "ds-1",
    version: 1,
    status: "FINALIZED",
    validation_status: "NOT_VALIDATED",
    member_count: 6,
    total_bytes: 2048,
    manifest_sha256: "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789",
    manifest_format: "dataset-manifest/v1",
    config: {},
    created_by: "user-1",
    created_at: "2026-01-03T00:00:00Z",
    ...over,
  };
}

const parent = makeDataset({ id: "ds-1", name: "pets", current_version_id: "ver-1" });
const branch = makeDataset({
  id: "ds-2",
  name: "pets@relabel",
  current_version_id: "ver-9",
  branch: { parent_id: "ds-1", name: "relabel", fork_version_id: "ver-1", merge_base_version_id: "ver-1" },
});

const conflicted: MergeResult = {
  target_dataset_id: "ds-1",
  source_dataset_id: "ds-2",
  source_version_id: "ver-9",
  base_version_id: "ver-1",
  strategy: "FAIL",
  applied: false,
  added: 2,
  updated: 1,
  removed: 0,
  already_same: 0,
  conflicts: 1,
  conflict_list: [
    {
      object_id: "obj-1",
      namespace: "pets",
      path: "cat-1.jpg",
      base: { present: true, version_id: "aaaaaaaaaaaa1111", label: "cat" },
      source: { present: true, version_id: "aaaaaaaaaaaa1111", label: "kitten" },
      target: { present: false },
    },
  ],
};

function serveFamily() {
  server.use(
    http.get(`${API}/datasets/ds-1`, () => HttpResponse.json(parent)),
    http.get(`${API}/datasets/ds-2`, () => HttpResponse.json(branch)),
    http.get(`${API}/datasets/ds-1/branches`, () => HttpResponse.json({ branches: [branch] })),
    http.get(`${API}/datasets/ds-1/versions`, () => HttpResponse.json({ versions: [version()] })),
    http.get(`${API}/datasets/ds-2/versions`, () => HttpResponse.json({ versions: [version({ id: "ver-9", dataset_id: "ds-2", version: 1 })] })),
    http.get(`${API}/datasets/ds-1/versions/ver-1`, () => HttpResponse.json(version())),
    http.get(`${API}/datasets/:id/merges`, () => HttpResponse.json({ merges: [] })),
  );
}

describe("branches", () => {
  it("previews a merge from a branch and shows its conflicts without changing anything", async () => {
    serveFamily();
    let sent: Record<string, unknown> | undefined;
    server.use(
      http.post(`${API}/datasets/ds-1/merges`, async ({ request }) => {
        sent = (await request.json()) as Record<string, unknown>;
        // A dry run answers its conflicts; a real merge stopped by them is a 409.
        if (sent.dry_run) return HttpResponse.json(conflicted);
        return HttpResponse.json({ ...conflicted, error: "merge conflict", code: "merge_conflict" }, { status: 409 });
      }),
    );
    renderWithProviders(<BranchesScreen id="ds-1" />);

    expect(await screen.findByRole("link", { name: "relabel" })).toHaveAttribute("href", "/app/datasets/ds-2");
    expect(await screen.findByRole("link", { name: "Version 1" })).toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: "Preview the merge" }));
    expect(await screen.findByText(/Preview: 1 object was changed differently/)).toBeInTheDocument();
    expect(sent).toEqual({ source_dataset_id: "ds-2", strategy: "FAIL", dry_run: true });

    const row = screen.getByRole("row", { name: /pets\/cat-1.jpg/ });
    expect(within(row).getByText("Not a member")).toBeInTheDocument();
    expect(within(row).getByText(/label kitten/)).toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: "Merge into the draft" }));
    expect(await screen.findByText(/Stopped: 1 object was changed differently/)).toBeInTheDocument();
    expect(sent).toMatchObject({ dry_run: false });
  });

  it("applies a merge that takes the incoming side and offers a snapshot", async () => {
    serveFamily();
    let sent: Record<string, unknown> | undefined;
    server.use(
      http.post(`${API}/datasets/ds-1/merges`, async ({ request }) => {
        sent = (await request.json()) as Record<string, unknown>;
        return HttpResponse.json({ ...conflicted, strategy: "SOURCE", applied: true, conflict_list: [], id: "mrg-1" });
      }),
    );
    renderWithProviders(<BranchesScreen id="ds-1" />);

    await userEvent.click(await screen.findByRole("radio", { name: /Take the incoming side/ }));
    await userEvent.click(screen.getByRole("button", { name: "Merge into the draft" }));

    expect(await screen.findByText(/Merged into the draft/)).toBeInTheDocument();
    expect(sent).toMatchObject({ source_dataset_id: "ds-2", strategy: "SOURCE", dry_run: false });
    expect(typeof sent?.idempotency_key).toBe("string");
    expect(screen.getByRole("button", { name: "Create snapshot" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Merge into the draft" })).toBeDisabled();
  });

  it("creates a branch from a chosen version and opens it", async () => {
    serveFamily();
    let sent: Record<string, unknown> | undefined;
    server.use(
      http.post(`${API}/datasets/ds-1/branches`, async ({ request }) => {
        sent = (await request.json()) as Record<string, unknown>;
        return HttpResponse.json(branch, { status: 201 });
      }),
    );
    renderWithProviders(<BranchesScreen id="ds-1" />);

    await userEvent.click(await screen.findByRole("button", { name: "New branch" }));
    await userEvent.type(screen.getByLabelText(/Branch name/), "Bad Name");
    await userEvent.click(screen.getByRole("button", { name: "Create branch" }));
    expect(sent).toBeUndefined();

    await userEvent.clear(screen.getByLabelText(/Branch name/));
    await userEvent.type(screen.getByLabelText(/Branch name/), "relabel");
    await userEvent.selectOptions(screen.getByLabelText("Start from"), "ver-1");
    await userEvent.click(screen.getByRole("button", { name: "Create branch" }));

    await vi.waitFor(() => expect(push).toHaveBeenCalledWith("/app/datasets/ds-2"));
    expect(sent).toEqual({ name: "relabel", from_version_id: "ver-1" });
  });

  it("on a branch, merges from its parent", async () => {
    serveFamily();
    renderWithProviders(<BranchesScreen id="ds-2" />);

    expect(await screen.findByRole("heading", { name: "Branch relabel" })).toBeInTheDocument();
    expect(await screen.findByRole("heading", { name: "Merge into relabel" })).toBeInTheDocument();
    expect(screen.getByLabelText("Bring changes from")).toHaveDisplayValue("pets");
  });
});
