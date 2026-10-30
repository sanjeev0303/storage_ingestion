import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { describe, expect, it } from "vitest";
import { API, server } from "@/test/server";
import { renderWithProviders } from "@/test/render";
import { ContentPolicyScreen } from "./content-policy-screen";
import { BlastRadiusPanel, ContentFindingsPanel } from "./findings-panel";

const FAMILIES = ["active_content", "model", "secret", "format", "external", "malware"];

const finding = {
  id: "f-1",
  asset_id: "a-1",
  source: "secrets",
  code: "secret.aws_access_key",
  action: "flag",
  detail: "an AWS access key ID at byte 17 (1 found)",
  created_at: "2026-10-30T10:00:00Z",
};

describe("ContentFindingsPanel", () => {
  it("shows each finding with its action, and a blocked file plainly", async () => {
    server.use(http.get(`${API}/assets/a-1/findings`, () => HttpResponse.json({ findings: [finding] })));
    renderWithProviders(<ContentFindingsPanel assetId="a-1" blockedReason="malware_detected" />);
    expect(await screen.findByText("Credentials in text")).toBeInTheDocument();
    expect(screen.getByText("Flagged")).toBeInTheDocument();
    expect(screen.getByText(finding.detail)).toBeInTheDocument();
    expect(screen.getByRole("alert")).toHaveTextContent("No download is served");
  });

  it("says when nothing was found", async () => {
    server.use(http.get(`${API}/assets/a-2/findings`, () => HttpResponse.json({ findings: [] })));
    renderWithProviders(<ContentFindingsPanel assetId="a-2" />);
    expect(await screen.findByText(/Nothing found/)).toBeInTheDocument();
  });
});

describe("BlastRadiusPanel", () => {
  it("loads only when asked, then lists copies, snapshots and downloads", async () => {
    let calls = 0;
    server.use(
      http.get(`${API}/assets/a-1/blast-radius`, () => {
        calls++;
        return HttpResponse.json({
          checksum_sha256: "ab".repeat(32),
          size_bytes: 2048,
          assets: [{ asset_id: "a-1", object_id: "o", version: 1, file_name: "keys.env", status: "AVAILABLE", blocked_reason: "malware_detected", created_at: "2026-10-30T10:00:00Z" }],
          snapshots: [{ dataset_id: "d-1", dataset_name: "pets", version: 3, members: 1 }],
          downloads: [{ actor_type: "USER", actor_id: "user-7", links: 2, first: "2026-10-29T10:00:00Z", last: "2026-10-30T09:00:00Z" }],
        });
      }),
    );
    renderWithProviders(<BlastRadiusPanel assetId="a-1" />, { permissions: ["audit.read"] });
    expect(calls).toBe(0);
    await userEvent.click(screen.getByRole("button", { name: "Trace these bytes" }));
    expect(await screen.findByText("keys.env")).toBeInTheDocument();
    expect(screen.getByText("Blocked")).toBeInTheDocument();
    expect(screen.getByText(/pets version 3/)).toBeInTheDocument();
    expect(screen.getByText("user-7")).toBeInTheDocument();
    expect(calls).toBe(1);
  });

  it("needs audit.read", () => {
    renderWithProviders(<BlastRadiusPanel assetId="a-1" />, { permissions: ["assets.read"] });
    expect(screen.getByText(/needs the audit.read permission/)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Trace these bytes" })).not.toBeInTheDocument();
  });
});

describe("ContentPolicyScreen", () => {
  const empty = { allowed_types: [], denied_types: [], max_size_bytes: 0, unknown_type_action: "", finding_actions: {} };

  it("saves family actions and a code exception, and keeps malware fixed", async () => {
    let sent: unknown;
    server.use(
      http.get(`${API}/content-policy`, () => HttpResponse.json({ policy: empty, finding_families: FAMILIES })),
      http.put(`${API}/content-policy`, async ({ request }) => {
        sent = await request.json();
        return HttpResponse.json({ policy: sent });
      }),
    );
    renderWithProviders(<ContentPolicyScreen />, { permissions: ["org.manage", "org.read"] });

    await userEvent.selectOptions(await screen.findByLabelText("Credentials in text"), "reject");
    expect(screen.getByLabelText("Malware")).toBeDisabled();
    await userEvent.type(screen.getByLabelText("Finding code"), "model.pickle_imports");
    await userEvent.selectOptions(screen.getByLabelText("Action"), "allow");
    await userEvent.click(screen.getByRole("button", { name: "Add exception" }));
    await userEvent.type(screen.getByLabelText("Refused types"), "application/x-tar");
    await userEvent.click(screen.getByRole("button", { name: "Save content policy" }));

    await waitFor(() => expect(sent).toBeDefined());
    expect(sent).toEqual({
      finding_actions: { secret: "reject", "model.pickle_imports": "allow" },
      allowed_types: [],
      denied_types: ["application/x-tar"],
      max_size_bytes: 0,
      unknown_type_action: "",
    });
  });

  it("refuses a malware exception and is read-only without org.manage", async () => {
    server.use(http.get(`${API}/content-policy`, () => HttpResponse.json({ policy: empty, finding_families: FAMILIES })));
    const { unmount } = renderWithProviders(<ContentPolicyScreen />, { permissions: ["org.manage"] });
    await userEvent.type(await screen.findByLabelText("Finding code"), "malware.x");
    await userEvent.tab(); // errors show once the field is left
    expect(screen.getByText("Malware findings always reject.")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Add exception" })).toBeDisabled();
    unmount();

    renderWithProviders(<ContentPolicyScreen />, { permissions: ["org.read"] });
    expect(await screen.findByText(/needs the org.manage permission/)).toBeInTheDocument();
    expect(screen.getByLabelText("Credentials in text")).toBeDisabled();
  });
});
