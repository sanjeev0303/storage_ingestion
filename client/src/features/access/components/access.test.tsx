import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { describe, expect, it } from "vitest";
import { API, server } from "@/test/server";
import { renderWithProviders } from "@/test/render";
import { AccessScreen } from "./access-screen";

const policy = {
  links_enabled: true,
  max_link_ttl: "0s",
  egress_monthly_bytes: 0,
  egress_daily_member_bytes: 0,
  egress_action: "WARN",
  mass_link_threshold: 300,
  anomaly_action: "ALERT",
};

const link = {
  id: "l1",
  asset_id: "a1",
  file_name: "scan-001.dcm",
  principal_id: "user-1",
  created_at: "2026-10-30T09:00:00Z",
  expires_at: "2026-10-30T10:00:00Z",
  uses: 3,
};

function base(extra: { tokens?: unknown[]; links?: unknown[]; alerts?: unknown[] } = {}) {
  server.use(
    http.get(`${API}/api-tokens`, () => HttpResponse.json({ tokens: extra.tokens ?? [] })),
    http.get(`${API}/links`, () => HttpResponse.json({ links: extra.links ?? [] })),
    http.get(`${API}/security/alerts`, () => HttpResponse.json({ alerts: extra.alerts ?? [] })),
    http.get(`${API}/access-policy`, () => HttpResponse.json(policy)),
    http.get(`${API}/egress`, () =>
      HttpResponse.json({ month_bytes: 2048, month_budget: 0, daily_member_budget: 0, action: "WARN", members: [] }),
    ),
  );
}

describe("AccessScreen", () => {
  it("creates a vendor token confined to a dataset and shows the secret once", async () => {
    let sent: Record<string, unknown> | undefined;
    base();
    server.use(
      http.post(`${API}/api-tokens`, async ({ request }) => {
        sent = (await request.json()) as Record<string, unknown>;
        return HttpResponse.json({ id: "t1", name: "vendor-acme", token: "osp_pat_abc123", hint: "osp_pat_abc…", permissions: [],
          datasets: ["scans"], networks: [], allow_links: false, expires_at: "2027-01-28T00:00:00Z", created_at: "2026-10-30T00:00:00Z" },
          { status: 201 });
      }),
    );
    renderWithProviders(<AccessScreen />, { permissions: ["assets.read", "assets.download", "org.read"] });

    await userEvent.click(await screen.findByRole("button", { name: "New token" }));
    const dialog = await screen.findByRole("dialog");
    await userEvent.type(within(dialog).getByLabelText(/^Name/), "vendor-acme");
    await userEvent.click(within(dialog).getByLabelText("assets.download"));
    await userEvent.type(within(dialog).getByLabelText("Only these datasets"), "Scans");
    expect(within(dialog).getByLabelText(/Share links are never allowed/)).toBeDisabled();
    await userEvent.click(within(dialog).getByRole("button", { name: "Create token" }));

    expect(await screen.findByText("osp_pat_abc123")).toBeInTheDocument();
    expect(sent).toEqual({ name: "vendor-acme", permissions: ["assets.read", "assets.download"], datasets: ["scans"], networks: [],
      expires_in: "2160h", allow_links: false });
  });

  it("revokes a share link and acknowledges an alert", async () => {
    const calls: string[] = [];
    base({
      links: [link],
      alerts: [{ id: "al-1", principal_id: "user-2", kind: "MASS_LINKS", detail: "400 share links in five minutes", metrics: {},
        state: "OPEN", created_at: "2026-10-30T09:05:00Z" }],
    });
    server.use(
      http.post(`${API}/links/revoke`, async ({ request }) => {
        calls.push(JSON.stringify(await request.json()));
        return HttpResponse.json({ revoked: 1 });
      }),
      http.post(`${API}/security/alerts/al-1/acknowledge`, () => {
        calls.push("ack");
        return HttpResponse.json({});
      }),
    );
    renderWithProviders(<AccessScreen />, { permissions: ["assets.read", "audit.read", "org.read"] });

    expect(await screen.findByText("scan-001.dcm")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Revoke" }));
    await userEvent.click(within(await screen.findByRole("alertdialog")).getByRole("button", { name: "Revoke link" }));
    await userEvent.click(await screen.findByRole("button", { name: "Acknowledge" }));
    await waitFor(() => expect(calls).toEqual(['{"link_ids":["l1"]}', "ack"]));
  });

  it("shows the policy read-only without org.manage", async () => {
    base();
    renderWithProviders(<AccessScreen />, { permissions: ["assets.read", "org.read"] });
    expect(await screen.findByText("Changing it needs the org.manage permission.")).toBeInTheDocument();
    expect(screen.getByLabelText("Members may create share links")).toBeDisabled();
  });
});
