import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { axe } from "vitest-axe";
import { describe, expect, it, vi } from "vitest";
import { API, server } from "@/test/server";
import { renderWithProviders } from "@/test/render";
import { parseMetadata, validateDataset, validatePath } from "../validation";
import { Dropzone } from "./dropzone";

// jsdom cannot run the WebAssembly hasher; the checksum's value does not matter here.
vi.mock("@/lib/upload/sha256", async (original) => ({
  ...(await original<typeof import("@/lib/upload/sha256")>()),
  sha256Blob: async () => "0".repeat(64),
}));
import { UploadsScreen } from "./uploads-screen";

const platform = {
  id: "platform",
  name: "Platform-managed storage",
  provider: "PLATFORM_MANAGED",
  status: "ACTIVE",
  auth_method: "PLATFORM",
  force_path_style: false,
  is_default: true,
  has_credentials: false,
  credential_rotation: 0,
  revision: 1,
  created_at: "2026-01-01T00:00:00Z",
  updated_at: "2026-01-01T00:00:00Z",
};

function mockPage() {
  server.use(
    http.get(`${API}/storage-connections`, () => HttpResponse.json({ connections: [platform], default_connection_id: "platform" })),
    http.get(`${API}/organization/quota`, () =>
      HttpResponse.json({
        limits: { max_object_bytes: 1000, max_active_uploads: 200, max_active_multipart: 50 },
        usage: { active_uploads: 1, active_multipart: 0 },
      }),
    ),
  );
}

describe("validation rules", () => {
  it("accepts and rejects dataset names", () => {
    expect(validateDataset("")).toBeNull();
    expect(validateDataset("train-v1.2_a")).toBeNull();
    expect(validateDataset("Train")).not.toBeNull();
    expect(validateDataset("-bad")).not.toBeNull();
    expect(validateDataset("a".repeat(65))).not.toBeNull();
  });

  it("applies the catalog path rules", () => {
    expect(validatePath("images/cat.png")).toBeNull();
    expect(validatePath("/abs")).not.toBeNull();
    expect(validatePath("a/../b")).not.toBeNull();
    expect(validatePath("a\\b")).not.toBeNull();
    expect(validatePath("a/b?.png")).not.toBeNull();
    expect(validatePath("_system/x")).not.toBeNull();
    expect(validatePath("a//b")).not.toBeNull();
  });

  it("parses key=value metadata and reports the first problem", () => {
    expect(parseMetadata("owner=ml\nsplit=train").value).toEqual({ owner: "ml", split: "train" });
    expect(parseMetadata("novalue").error).toMatch(/key=value/);
    expect(parseMetadata("system.x=1").error).toMatch(/reserved/);
  });
});

describe("uploads screen", () => {
  it("blocks start and explains a bad dataset name or path, then allows a valid file", async () => {
    mockPage();
    const user = userEvent.setup();
    renderWithProviders(<UploadsScreen />);

    await user.upload(await screen.findByTestId("file-input"), new File(["hello"], "notes.txt", { type: "text/plain" }));

    const table = await screen.findByRole("table", { name: "Upload queue" });
    expect(within(table).getByText("notes.txt")).toBeInTheDocument();
    const start = screen.getByRole("button", { name: "Start upload" });
    expect(start).toBeEnabled();

    await user.click(within(table).getByRole("button", { name: "Edit details" }));
    await user.click(screen.getByLabelText("Dataset name"));
    await user.paste("Bad Name");
    expect(screen.getByText(/lowercase letters, digits/)).toBeInTheDocument();
    expect(start).toBeDisabled();

    await user.clear(screen.getByLabelText("Dataset name"));
    await user.click(screen.getByLabelText("Catalog path"));
    await user.paste("a/../x");
    expect(screen.getByText(/cannot contain ".."/)).toBeInTheDocument();
    expect(start).toBeDisabled();

    await user.clear(screen.getByLabelText("Catalog path"));
    await user.paste("docs/notes.txt");
    expect(start).toBeEnabled();
  }, 20_000);

  it("flags a file over the organization's object limit before upload", async () => {
    mockPage();
    const user = userEvent.setup();
    renderWithProviders(<UploadsScreen />);
    await screen.findByText("1000 B");

    await user.upload(await screen.findByTestId("file-input"), new File([new Uint8Array(2000)], "huge.bin"));

    expect(await screen.findByText(/larger than your organization/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Start upload" })).toBeDisabled();
    expect(screen.getByText("Needs a fix")).toBeInTheDocument();
    expect(screen.getByText(/0 ready to start, 0 waiting for a slot, 1 needs a fix/)).toBeInTheDocument();
  });

  it("says which files wait for Start and which wait for a slot, and never uploads one removed while waiting", async () => {
    mockPage();
    const created: string[] = [];
    let freeSlots!: () => void;
    const slotsFreed = new Promise<void>((resolve) => (freeSlots = resolve));
    // Uploads that never finish keep every slot busy, until a.txt and b.txt
    // fail and free two of them.
    server.use(
      http.get(`${API}/catalog/lookup`, () => HttpResponse.json({ error: "not found" }, { status: 404 })),
      http.post(`${API}/uploads`, async ({ request }) => {
        const name = ((await request.json()) as { file_name: string }).file_name;
        created.push(name);
        if (name === "a.txt" || name === "b.txt") {
          await slotsFreed;
          return HttpResponse.json({ error: "bad request" }, { status: 400 });
        }
        return new Promise<never>(() => undefined);
      }),
    );
    const user = userEvent.setup();
    renderWithProviders(<UploadsScreen />);
    const files = ["a", "b", "c", "d", "e", "f"].map((name) => new File(["x"], `${name}.txt`, { type: "text/plain" }));
    await user.upload(await screen.findByTestId("file-input"), files);

    expect(await screen.findByText(/6 ready to start/)).toBeInTheDocument();
    expect(screen.getAllByText("Ready")).toHaveLength(6);

    await user.click(screen.getByRole("button", { name: "Start 6 uploads" }));
    // Four files go at once; the last two wait their turn, and say so.
    expect(await screen.findByText(/2 waiting for a slot/)).toBeInTheDocument();
    expect(screen.getByText("Next in line for a free upload slot")).toBeInTheDocument();
    expect(screen.getByText("Waiting for a free upload slot, 1 file ahead")).toBeInTheDocument();

    const table = screen.getByRole("table", { name: "Upload queue" });
    const lastRow = within(table).getByText("f.txt").closest("tr")!;
    await user.click(within(lastRow).getByRole("button", { name: "Remove" }));
    expect(await screen.findByText(/1 waiting for a slot/)).toBeInTheDocument();

    freeSlots();
    // e.txt takes the first freed slot; the second has nothing left to start.
    await vi.waitFor(() => expect(created).toContain("e.txt"));
    await new Promise((resolve) => setTimeout(resolve, 300));
    expect(created).not.toContain("f.txt");
  }, 20_000);
});

describe("dropzone", () => {
  it("has no accessibility violations", async () => {
    const { container } = renderWithProviders(<Dropzone onFiles={() => undefined} />);
    expect(await axe(container)).toHaveNoViolations();
  });
});
