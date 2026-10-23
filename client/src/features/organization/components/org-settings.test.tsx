import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { describe, expect, it } from "vitest";
import { renderWithProviders } from "@/test/render";
import { API, server } from "@/test/server";
import { OrgSettings } from "./org-settings";

const org = {
  id: "org-1",
  name: "Acme",
  slug: "acme",
  status: "ACTIVE",
  allowed_email_domains: ["acme.test", "acme.example"],
  invitation_ttl_hours: 72,
  created_at: "2026-01-01T00:00:00Z",
};

function mockReads() {
  server.use(
    http.get(`${API}/organization`, () => HttpResponse.json(org)),
    http.get(`${API}/organization/roles`, () => HttpResponse.json({ roles: [] })),
  );
}

describe("OrgSettings", () => {
  it("sends current domains and lifetime when only the name changed", async () => {
    mockReads();
    const sent: { body?: unknown } = {};
    server.use(
      http.patch(`${API}/organization`, async ({ request }) => {
        sent.body = await request.json();
        return HttpResponse.json({ ...org, name: "Acme Labs" });
      }),
    );
    renderWithProviders(<OrgSettings />);

    const name = await screen.findByLabelText(/^Name/);
    await userEvent.clear(name);
    await userEvent.type(name, "Acme Labs");
    await userEvent.click(screen.getByRole("button", { name: "Save settings" }));

    await screen.findByDisplayValue("Acme Labs");
    expect(sent.body).toEqual({
      name: "Acme Labs",
      allowed_email_domains: ["acme.test", "acme.example"],
      invitation_ttl_hours: 72,
    });
  });

  it("says plainly that saving replaces every setting", async () => {
    mockReads();
    renderWithProviders(<OrgSettings />);
    expect(await screen.findByText(/replaces all three settings at once/)).toBeInTheDocument();
  });

  it("offers deactivation to owners only", async () => {
    mockReads();
    renderWithProviders(<OrgSettings />, { permissions: ["org.read", "org.manage"] });
    await screen.findByLabelText(/^Name/);
    expect(screen.queryByRole("button", { name: "Deactivate organization" })).not.toBeInTheDocument();
  });
});
