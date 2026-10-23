import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { describe, expect, it, vi } from "vitest";
import { API, server } from "@/test/server";
import { renderWithProviders } from "@/test/render";
import { makeDataset } from "../fixtures";
import type { DatasetMember, DatasetVersion } from "../types";
import { MembersScreen } from "./members-screen";
import { VersionDetail } from "./version-detail";
import { VersionsScreen } from "./versions-screen";

vi.mock("next/navigation", () => ({
  useRouter: () => ({ push: vi.fn() }),
  usePathname: () => "/app/datasets/ds-1/members",
}));

function member(over: Partial<DatasetMember> = {}): DatasetMember {
  return {
    id: "m-1",
    object_id: "obj-1",
    namespace: "",
    path: "pets/cat-1.txt",
    metadata: {},
    source: "api",
    added_by: "user-1",
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
    ...over,
  };
}

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

function serveDataset(over = {}) {
  server.use(http.get(`${API}/datasets/ds-1`, () => HttpResponse.json(makeDataset(over))));
}

describe("members", () => {
  it("shows pinned and unpinned members and warns about unpinned ones", async () => {
    serveDataset();
    server.use(
      http.get(`${API}/datasets/ds-1/members`, () =>
        HttpResponse.json({
          members: [
            member({ version_id: "asset-9", label: "cat", split: "train", source: "import:abcdef123456" }),
            member({ id: "m-2", path: "pets/dog.txt" }),
          ],
        }),
      ),
    );
    renderWithProviders(<MembersScreen id="ds-1" />);
    expect(await screen.findByText("Pinned asset-9")).toBeInTheDocument();
    expect(screen.getByText("Follows current version")).toBeInTheDocument();
    expect(screen.getByText(/1 of the 2 loaded members are not pinned/)).toBeInTheDocument();
    expect(screen.getByText("Import abcdef12")).toBeInTheDocument();
  });

  it("pages members with the cursor", async () => {
    serveDataset();
    const cursors: (string | null)[] = [];
    server.use(
      http.get(`${API}/datasets/ds-1/members`, ({ request }) => {
        const cursor = new URL(request.url).searchParams.get("cursor");
        cursors.push(cursor);
        return cursor
          ? HttpResponse.json({ members: [member({ id: "m-2", path: "second.txt" })] })
          : HttpResponse.json({ members: [member()], next_cursor: "c1.next" });
      }),
    );
    renderWithProviders(<MembersScreen id="ds-1" />);
    await screen.findByText("pets/cat-1.txt");
    await userEvent.click(screen.getByRole("button", { name: "Load more" }));
    expect(await screen.findByText("second.txt")).toBeInTheDocument();
    expect(cursors).toEqual([null, "c1.next"]);
  });

  it("teaches when the draft is empty", async () => {
    serveDataset({ member_count: 0 });
    server.use(http.get(`${API}/datasets/ds-1/members`, () => HttpResponse.json({ members: [] })));
    renderWithProviders(<MembersScreen id="ds-1" />);
    expect(await screen.findByText("The draft has no members")).toBeInTheDocument();
  });

  it("renders every outcome of an add, with the rejected rows first", async () => {
    serveDataset();
    let body: { on_duplicate: string; members: unknown[] } = { on_duplicate: "", members: [] };
    server.use(
      http.get(`${API}/datasets/ds-1/members`, () => HttpResponse.json({ members: [] })),
      http.post(`${API}/datasets/ds-1/members`, async ({ request }) => {
        body = (await request.json()) as typeof body;
        return HttpResponse.json({
          added: 1,
          updated: 0,
          skipped: 1,
          rejected: 1,
          results: [
            { index: 0, outcome: "ADDED", object_id: "o1" },
            { index: 1, outcome: "SKIPPED", object_id: "o2" },
            { index: 2, outcome: "REJECTED", code: "object_not_found", error: "no such object" },
          ],
          dataset: makeDataset(),
        });
      }),
    );
    renderWithProviders(<MembersScreen id="ds-1" />);
    await userEvent.click(await screen.findByRole("button", { name: "Add members" }));
    const dialog = await screen.findByRole("dialog");
    await userEvent.click(within(dialog).getByRole("button", { name: "Paste JSON lines" }));
    await userEvent.type(
      within(dialog).getByLabelText(/Members as JSON lines/),
      '{{"path":"a.txt"}\n{{"path":"b.txt"}\n{{"path":"nope.txt"}',
    );
    await userEvent.selectOptions(within(dialog).getByLabelText("If the object is already a member"), "SKIP");
    await userEvent.click(within(dialog).getByRole("button", { name: "Add members" }));

    expect(await within(dialog).findByText("1 added, 0 updated, 1 skipped, 1 rejected.")).toBeInTheDocument();
    expect(body.on_duplicate).toBe("SKIP");
    expect(body.members).toHaveLength(3);
    const rows = within(dialog).getAllByRole("row").slice(1);
    expect(rows[0]).toHaveTextContent("nope.txt");
    expect(rows[0]).toHaveTextContent("REJECTED");
    expect(rows[0]).toHaveTextContent("object_not_found");
    expect(rows[0]).toHaveTextContent("no such object");
    expect(within(dialog).getByText("ADDED")).toBeInTheDocument();
    expect(within(dialog).getByText("SKIPPED")).toBeInTheDocument();
  });

  it("refuses a pasted line that is not JSON before sending", async () => {
    serveDataset();
    server.use(http.get(`${API}/datasets/ds-1/members`, () => HttpResponse.json({ members: [] })));
    renderWithProviders(<MembersScreen id="ds-1" />);
    await userEvent.click(await screen.findByRole("button", { name: "Add members" }));
    const dialog = await screen.findByRole("dialog");
    await userEvent.click(within(dialog).getByRole("button", { name: "Paste JSON lines" }));
    await userEvent.type(within(dialog).getByLabelText(/Members as JSON lines/), "not json");
    await userEvent.click(within(dialog).getByRole("button", { name: "Add members" }));
    expect(await within(dialog).findByText("Line 1 is not a JSON object.")).toBeInTheDocument();
  });

  it("removes a member and shows no write actions when archived", async () => {
    serveDataset();
    let removed = "";
    server.use(
      http.get(`${API}/datasets/ds-1/members`, () => HttpResponse.json({ members: [member()] })),
      http.delete(`${API}/datasets/ds-1/members/m-1`, () => {
        removed = "m-1";
        return HttpResponse.json(makeDataset());
      }),
    );
    const first = renderWithProviders(<MembersScreen id="ds-1" />);
    await userEvent.click(await screen.findByRole("button", { name: "Remove pets/cat-1.txt" }));
    const dialog = await screen.findByRole("alertdialog");
    await userEvent.click(within(dialog).getByRole("button", { name: "Remove member" }));
    await vi.waitFor(() => expect(removed).toBe("m-1"));
    first.unmount();

    serveDataset({ lifecycle_status: "ARCHIVED" });
    renderWithProviders(<MembersScreen id="ds-1" />);
    await screen.findByText("pets/cat-1.txt");
    expect(screen.queryByRole("button", { name: "Add members" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /Remove/ })).not.toBeInTheDocument();
  });

  it("picks catalog objects with a prefix search", async () => {
    serveDataset();
    let sent: { members: { object_id?: string; version_id?: string }[] } = { members: [] };
    const prefixes: (string | null)[] = [];
    server.use(
      http.get(`${API}/datasets/ds-1/members`, () => HttpResponse.json({ members: [] })),
      http.get(`${API}/catalog/objects`, ({ request }) => {
        prefixes.push(new URL(request.url).searchParams.get("prefix"));
        return HttpResponse.json({
          objects: [
            {
              id: "obj-7",
              dataset: "",
              path: "pets/a.txt",
              versions: 1,
              current_version: { version: 1, asset_id: "asset-7", size_bytes: 10, content_type: "text/plain" },
            },
          ],
        });
      }),
      http.post(`${API}/datasets/ds-1/members`, async ({ request }) => {
        sent = (await request.json()) as typeof sent;
        return HttpResponse.json({ added: 1, updated: 0, skipped: 0, rejected: 0, results: [{ index: 0, outcome: "ADDED" }], dataset: makeDataset() });
      }),
    );
    renderWithProviders(<MembersScreen id="ds-1" />);
    await userEvent.click(await screen.findByRole("button", { name: "Add members" }));
    const dialog = await screen.findByRole("dialog");
    await userEvent.click(within(dialog).getByRole("button", { name: "Pick from catalog" }));
    await userEvent.type(within(dialog).getByLabelText("Path starts with"), "pets/");
    await userEvent.click(within(dialog).getByRole("button", { name: "Search" }));
    await userEvent.click(await within(dialog).findByRole("checkbox", { name: "Select pets/a.txt" }));
    await userEvent.click(within(dialog).getByRole("button", { name: "Add 1 selected" }));
    await within(dialog).findByText(/1 added/);
    expect(prefixes).toContain("pets/");
    expect(sent.members).toEqual([{ object_id: "obj-7" }]);
  });
});

describe("versions", () => {
  it("shows an unvalidated version as not validated, never as passed", async () => {
    serveDataset({ current_version_id: "ver-1" });
    server.use(
      http.get(`${API}/datasets/ds-1/versions`, () =>
        HttpResponse.json({
          versions: [version(), version({ id: "ver-0", version: 0 + 2, validation_status: "READY_WITH_WARNINGS" })],
        }),
      ),
    );
    renderWithProviders(<VersionsScreen id="ds-1" />);
    expect(await screen.findByText("Not validated")).toBeInTheDocument();
    expect(screen.getByText("Ready with warnings")).toBeInTheDocument();
    expect(screen.getAllByText("Immutable")).toHaveLength(2);
    expect(screen.getByRole("link", { name: "Version 1" })).toHaveAttribute("href", "/app/datasets/ds-1/versions/ver-1");
  });

  it("pages versions with before", async () => {
    serveDataset();
    const seen: (string | null)[] = [];
    server.use(
      http.get(`${API}/datasets/ds-1/versions`, ({ request }) => {
        const before = new URL(request.url).searchParams.get("before");
        seen.push(before);
        return before
          ? HttpResponse.json({ versions: [version({ id: "ver-0", version: 1 })] })
          : HttpResponse.json({ versions: [version({ id: "ver-2", version: 2 })], next_before: 2 });
      }),
    );
    renderWithProviders(<VersionsScreen id="ds-1" />);
    await screen.findByText("Version 2");
    await userEvent.click(screen.getByRole("button", { name: "Load more" }));
    await screen.findByText("Version 1");
    expect(seen).toEqual([null, "2"]);
  });

  it("teaches when there are no versions", async () => {
    serveDataset();
    server.use(http.get(`${API}/datasets/ds-1/versions`, () => HttpResponse.json({ versions: [] })));
    renderWithProviders(<VersionsScreen id="ds-1" />);
    expect(await screen.findByText("No versions yet")).toBeInTheDocument();
  });

  it("shows a version as immutable with its members and links to validate and lineage", async () => {
    server.use(
      http.get(`${API}/datasets/ds-1/versions/ver-1`, () => HttpResponse.json(version({ latest_report_id: "rep-1" }))),
      http.get(`${API}/datasets/ds-1/versions/ver-1/members`, ({ request }) => {
        const after = new URL(request.url).searchParams.get("after");
        return after
          ? HttpResponse.json({ members: [{ ordinal: 2, object_id: "o2", version_id: "a2", object_version: 1, path: "two.txt", size_bytes: 5, content_type: "text/plain", metadata: {} }] })
          : HttpResponse.json({
              members: [{ ordinal: 1, object_id: "o1", version_id: "a1", object_version: 3, path: "one.txt", size_bytes: 5, content_type: "text/plain", checksum_sha256: "ff".repeat(32), label: "cat", split: "train", metadata: {} }],
              next_after: 1,
            });
      }),
    );
    renderWithProviders(<VersionDetail id="ds-1" versionId="ver-1" />);
    expect(await screen.findByText("Immutable")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Validate this version" })).toHaveAttribute("href", "/app/datasets/ds-1/validation?version=ver-1");
    expect(screen.getByRole("link", { name: "Lineage" })).toHaveAttribute("href", "/app/datasets/ds-1/lineage?version=ver-1");
    expect(screen.getByRole("link", { name: "rep-1" })).toHaveAttribute("href", "/app/validation/reports/rep-1");
    expect(await screen.findByText("one.txt")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Load more" }));
    expect(await screen.findByText("two.txt")).toBeInTheDocument();
  });
});
