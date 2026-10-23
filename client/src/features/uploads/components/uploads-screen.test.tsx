import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { axe } from "vitest-axe";
import { describe, expect, it } from "vitest";
import { API, server } from "@/test/server";
import { renderWithProviders } from "@/test/render";
import { parseMetadata, validateDataset, validatePath } from "../validation";
import { Dropzone } from "./dropzone";
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
  });
});

describe("dropzone", () => {
  it("has no accessibility violations", async () => {
    const { container } = renderWithProviders(<Dropzone onFiles={() => undefined} />);
    expect(await axe(container)).toHaveNoViolations();
  });
});
