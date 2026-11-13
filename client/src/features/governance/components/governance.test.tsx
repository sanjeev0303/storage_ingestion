import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { describe, expect, it } from "vitest";
import { API, server } from "@/test/server";
import { renderWithProviders } from "@/test/render";
import { GovernanceScreen } from "./governance-screen";

const settings = { profile: "STANDARD", two_person: true, approve_deletes: false, retention_floor: "0s" };

const pending = (requestedBy: string) => ({
  id: "ap-1",
  kind: "HOLD_RELEASE",
  summary: "release legal hold h-1",
  payload: { hold_id: "h-1" },
  state: "PENDING",
  reason: "matter settled",
  requested_by: requestedBy,
  requested_at: "2026-10-30T09:00:00Z",
  expires_at: "2026-11-06T09:00:00Z",
});

function base(approvals: unknown[]) {
  server.use(
    http.get(`${API}/governance`, () => HttpResponse.json(settings)),
    http.get(`${API}/approvals`, () => HttpResponse.json({ approvals })),
  );
}

describe("GovernanceScreen", () => {
  it("approves another member's change and records the note", async () => {
    let decided: unknown;
    base([pending("user:someone-else")]);
    server.use(
      http.post(`${API}/approvals/ap-1/approve`, async ({ request }) => {
        decided = await request.json();
        return HttpResponse.json({ ...pending("user:someone-else"), state: "EXECUTED", result: "hold h-1 released" });
      }),
    );
    renderWithProviders(<GovernanceScreen />, { permissions: ["retention.manage", "assets.delete"] });

    await userEvent.click(await screen.findByRole("button", { name: "Review" }));
    const dialog = await screen.findByRole("dialog");
    await userEvent.type(within(dialog).getByLabelText(/Note/), "checked with counsel");
    await userEvent.click(within(dialog).getByRole("button", { name: "Approve and carry out" }));
    await waitFor(() => expect(decided).toEqual({ note: "checked with counsel" }));
  });

  it("does not offer a member their own change", async () => {
    base([pending("user:user-1")]);
    renderWithProviders(<GovernanceScreen />, { permissions: ["retention.manage"] });
    expect(await screen.findByRole("button", { name: "Yours: another member decides" })).toBeDisabled();
  });

  it("previews a bulk delete before anything is deleted", async () => {
    const bodies: Record<string, unknown>[] = [];
    base([]);
    server.use(
      http.post(`${API}/assets/bulk-delete`, async ({ request }) => {
        const body = (await request.json()) as Record<string, unknown>;
        bodies.push(body);
        return HttpResponse.json(
          body.dry_run
            ? { matched: 3, bytes: 3072, blocked: { legal_hold: 1 }, checked: 3, scheduled: 0, sample: [] }
            : { matched: 3, bytes: 3072, blocked: {}, checked: 0, scheduled: 0, approval_id: "ap-9", sample: [] },
          { status: body.dry_run ? 200 : 202 },
        );
      }),
    );
    renderWithProviders(<GovernanceScreen />, { permissions: ["retention.manage", "assets.delete"] });

    await userEvent.type(await screen.findByLabelText("Dataset"), "old-scans");
    await userEvent.type(screen.getAllByLabelText(/^Reason/)[0], "past retention");
    await userEvent.click(screen.getByRole("button", { name: "Preview deletion" }));
    expect(await screen.findByText(/1 by legal hold/)).toBeInTheDocument();
    expect(bodies).toEqual([{ dataset: "old-scans", reason: "past retention", dry_run: true }]);

    await userEvent.click(screen.getByRole("button", { name: "Delete 3 assets" }));
    const dialog = await screen.findByRole("alertdialog");
    await userEvent.type(within(dialog).getByRole("textbox"), "DELETE");
    await userEvent.click(within(dialog).getByRole("button", { name: "Delete assets" }));
    await waitFor(() => expect(bodies[1]).toEqual({ dataset: "old-scans", reason: "past retention", dry_run: false }));
  });

  it("drops the previewed plan when a field changes, so only a previewed request can be deleted", async () => {
    const bodies: Record<string, unknown>[] = [];
    base([]);
    server.use(
      http.post(`${API}/assets/bulk-delete`, async ({ request }) => {
        bodies.push((await request.json()) as Record<string, unknown>);
        return HttpResponse.json({ matched: 3, bytes: 3072, blocked: {}, checked: 3, scheduled: 0, sample: [] });
      }),
    );
    renderWithProviders(<GovernanceScreen />, { permissions: ["retention.manage", "assets.delete"] });

    await userEvent.type(await screen.findByLabelText("Dataset"), "old-scans");
    await userEvent.type(screen.getAllByLabelText(/^Reason/)[0], "past retention");
    await userEvent.click(screen.getByRole("button", { name: "Preview deletion" }));
    expect(await screen.findByRole("button", { name: "Delete 3 assets" })).toBeInTheDocument();

    await userEvent.type(screen.getByLabelText("Path starts with"), "everything/");
    expect(screen.queryByRole("button", { name: "Delete 3 assets" })).not.toBeInTheDocument();
    expect(bodies).toHaveLength(1);
  });

  it("sends the switches a stricter profile turns on, as they are shown", async () => {
    let sent: Record<string, unknown> | undefined;
    base([]);
    server.use(
      http.get(`${API}/governance`, () => HttpResponse.json({ ...settings, two_person: false })),
      http.put(`${API}/governance`, async ({ request }) => {
        sent = (await request.json()) as Record<string, unknown>;
        return HttpResponse.json({ ...settings, profile: "GXP_VAULT" });
      }),
    );
    renderWithProviders(<GovernanceScreen />, { permissions: ["retention.manage"] });

    await userEvent.selectOptions(await screen.findByLabelText("Profile"), "GXP_VAULT");
    expect(screen.getByRole("checkbox", { name: /Two-person rule/ })).toBeChecked();
    expect(screen.getByRole("checkbox", { name: /Every deletion needs approval/ })).toBeChecked();
    await userEvent.click(screen.getByRole("button", { name: "Save governance" }));
    await waitFor(() => expect(sent).toMatchObject({ profile: "GXP_VAULT", two_person: true, approve_deletes: true }));
  });

  it("keeps unsaved governance edits through a refetch and says someone else changed it", async () => {
    let current = { ...settings, updated_at: "2026-10-30T09:00:00Z" };
    base([]);
    server.use(http.get(`${API}/governance`, () => HttpResponse.json(current)));
    const { client } = renderWithProviders(<GovernanceScreen />, { permissions: ["retention.manage"] });
    await userEvent.type(await screen.findByLabelText(/^Reason/), "audit asked for it");

    current = { ...current, approve_deletes: true, updated_at: "2026-10-30T10:00:00Z" };
    await client.invalidateQueries();
    expect(await screen.findByText(/Someone else changed the governance settings after you started editing/)).toBeInTheDocument();
    expect(screen.getByLabelText(/^Reason/)).toHaveValue("audit asked for it");
  });

  it("does not act on an approval another member already decided", async () => {
    let lists = 0;
    let decided = false;
    server.use(
      http.get(`${API}/governance`, () => HttpResponse.json(settings)),
      http.get(`${API}/approvals`, () => {
        lists += 1;
        return HttpResponse.json({ approvals: lists === 1 ? [pending("user:someone-else")] : [] });
      }),
      http.post(`${API}/approvals/ap-1/approve`, () => {
        decided = true;
        return HttpResponse.json({ ...pending("user:someone-else"), state: "EXECUTED" });
      }),
    );
    renderWithProviders(<GovernanceScreen />, { permissions: ["retention.manage"] });

    await userEvent.click(await screen.findByRole("button", { name: "Review" }));
    const dialog = await screen.findByRole("dialog");
    await userEvent.click(within(dialog).getByRole("button", { name: "Approve and carry out" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(decided).toBe(false);
  });

  it("checks a certificate's hash in the browser", async () => {
    const signed = '{"type":"disposition-certificate/v1","file_name":"old.txt"}';
    const digest = new Uint8Array(await crypto.subtle.digest("SHA-256", new TextEncoder().encode(signed)));
    const hex = Array.from(digest, (b) => b.toString(16).padStart(2, "0")).join("");
    base([]);
    server.use(
      http.get(`${API}/deletions/del-1/certificate`, () =>
        HttpResponse.json({ id: "c1", deletion_id: "del-1", asset_id: "a1", body: JSON.parse(signed), signed_body: signed,
          body_sha256: hex, key_id: "", signature: "", created_at: "2026-10-30T10:00:00Z" }),
      ),
      http.get(`${API}/certificates/public-key`, () => HttpResponse.json({ error: "unsigned" }, { status: 404 })),
    );
    renderWithProviders(<GovernanceScreen />, { permissions: ["retention.manage"] });
    await userEvent.type(await screen.findByLabelText("Deletion id"), "del-1");
    await userEvent.click(screen.getByRole("button", { name: "Check certificate" }));
    expect(await screen.findByText(/only its SHA-256 is checked/)).toBeInTheDocument();
    expect(screen.getByText("old.txt")).toBeInTheDocument();
  });
});
