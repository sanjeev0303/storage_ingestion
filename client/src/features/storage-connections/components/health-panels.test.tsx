import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { describe, expect, it } from "vitest";
import { renderWithProviders } from "@/test/render";
import { API, server } from "@/test/server";
import type { PostureReport, StorageConnection } from "../types";
import { DriftPanel, IntegritySettings, PosturePanel, SecondCredential } from "./health-panels";

const connection: StorageConnection = {
  id: "sc_1",
  name: "Lab bucket",
  provider: "AWS_S3",
  status: "ACTIVE",
  auth_method: "ACCESS_KEY",
  bucket: "lab-scans",
  force_path_style: false,
  is_default: false,
  has_credentials: true,
  credential_rotation: 1,
  checksum_mode: "AUTO",
  revision: 4,
  created_at: "2026-10-30T09:00:00Z",
  updated_at: "2026-10-30T09:00:00Z",
};

const report: PostureReport = {
  connection_id: "sc_1",
  checked_at: "2026-10-30T10:00:00Z",
  worst: "FAIL",
  phi_safe: false,
  owner_account: "123456789012",
  checks: [
    { name: "versioning", status: "PASS", detail: "versioning is on" },
    { name: "sse_c_denied", status: "FAIL", detail: "the bucket accepted a write encrypted with a customer-provided key" },
    { name: "object_lock", status: "WARN", detail: "Object Lock is off" },
  ],
};

describe("storage health panels", () => {
  it("lists failing checks first and checks again with the probe", async () => {
    let asked: unknown;
    server.use(
      http.get(`${API}/storage-connections/sc_1/posture`, () => HttpResponse.json(report)),
      http.post(`${API}/storage-connections/sc_1/posture`, async ({ request }) => {
        asked = await request.json();
        return HttpResponse.json({ ...report, worst: "PASS", phi_safe: true });
      }),
    );
    renderWithProviders(<PosturePanel connection={connection} />, { permissions: ["storage.read", "storage.manage"] });

    const items = await screen.findAllByRole("listitem");
    expect(items[0]).toHaveTextContent("Customer-provided keys (SSE-C) refused");
    expect(items[0]).toHaveTextContent("Fail");
    expect(screen.getByText("Not safe for PHI")).toBeInTheDocument();
    await userEvent.click(screen.getByLabelText(/customer-provided key, which must be refused/));
    await userEvent.click(screen.getByRole("button", { name: "Check now" }));
    await waitFor(() => expect(asked).toEqual({ probe: true }));
  });

  it("says when the posture was never checked", async () => {
    server.use(
      http.get(`${API}/storage-connections/sc_1/posture`, () =>
        HttpResponse.json({ error: "not checked", code: "not_checked" }, { status: 404 }),
      ),
    );
    renderWithProviders(<PosturePanel connection={connection} />, { permissions: ["storage.read"] });
    expect(await screen.findByText("Not checked yet")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Check now" })).not.toBeInTheDocument();
  });

  it("pins the bucket owner with the connection's revision", async () => {
    let sent: unknown;
    server.use(
      http.patch(`${API}/storage-connections/sc_1`, async ({ request }) => {
        sent = await request.json();
        return HttpResponse.json({ ...connection, expected_owner: "123456789012", revision: 5 });
      }),
    );
    renderWithProviders(<IntegritySettings connection={connection} />, { permissions: ["storage.manage"] });
    await userEvent.type(screen.getByLabelText(/Bucket owner account/), "123456789012");
    await userEvent.click(screen.getByRole("button", { name: "Save settings" }));
    await waitFor(() => expect(sent).toEqual({ revision: 4, expected_owner: "123456789012" }));
  });

  it("sets a customer-managed key, which the backend tests with a write first", async () => {
    let sent: unknown;
    server.use(
      http.patch(`${API}/storage-connections/sc_1`, async ({ request }) => {
        sent = await request.json();
        return HttpResponse.json(
          { ok: false, category: "ACCESS_DENIED", message: "not allowed", checks: [{ name: "write_probe", ok: false, category: "ACCESS_DENIED" }], tested_at: "2026-10-30T10:00:00Z" },
          { status: 422 },
        );
      }),
    );
    renderWithProviders(<IntegritySettings connection={connection} />, { permissions: ["storage.manage"] });
    await userEvent.type(screen.getByLabelText(/KMS key ARN, ID or alias/), "alias/aws/s3");
    await userEvent.click(screen.getByRole("button", { name: "Save settings" }));
    await waitFor(() => expect(sent).toEqual({ revision: 4, encryption_key: "alias/aws/s3" }));
    expect(await screen.findByText(/Not saved: the connection failed its test/)).toBeInTheDocument();
  });

  it("promotes staged credentials only after confirming", async () => {
    let promoted = false;
    server.use(
      http.post(`${API}/storage-connections/sc_1/credentials/promote`, () => {
        promoted = true;
        return HttpResponse.json({ ok: true, checks: [], tested_at: "2026-10-30T10:00:00Z" });
      }),
    );
    renderWithProviders(
      <SecondCredential connection={{ ...connection, staged_credential: { rotation: 2, staged_at: "2026-10-30T09:30:00Z" } }} />,
      { permissions: ["storage.manage"] },
    );
    await userEvent.click(screen.getByRole("button", { name: "Promote" }));
    expect(promoted).toBe(false);
    await userEvent.click(within(await screen.findByRole("alertdialog")).getByRole("button", { name: "Test and promote" }));
    await waitFor(() => expect(promoted).toBe(true));
    expect(await screen.findByText(/Revoke the old keys/)).toBeInTheDocument();
  });

  it("resolves a change found outside the platform", async () => {
    let resolved = "";
    server.use(
      http.get(`${API}/storage-connections/sc_1/drift`, () =>
        HttpResponse.json({
          findings: [{ id: "df_1", connection_id: "sc_1", object_key: "ready/t1/a", kind: "OVERWRITTEN", detail: "replaced", detected_at: "2026-10-30T10:00:00Z" }],
        }),
      ),
      http.post(`${API}/storage-drift/df_1/resolve`, () => {
        resolved = "df_1";
        return HttpResponse.json({});
      }),
    );
    renderWithProviders(<DriftPanel connection={connection} />, { permissions: ["storage.read", "storage.manage"] });
    expect(await screen.findByText("Replaced")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Mark resolved" }));
    await waitFor(() => expect(resolved).toBe("df_1"));
  });
});
