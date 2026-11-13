import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { describe, expect, it, vi } from "vitest";
import { renderWithProviders } from "@/test/render";
import { API, server } from "@/test/server";
import { PlatformAdmin } from "./platform";

const org = { id: "org-9", name: "Acme", slug: "acme", status: "PENDING_REVIEW", created_at: "2026-01-01T00:00:00Z" };

describe("PlatformAdmin", () => {
  it("shows an organization's real status and changes nothing until a new one is chosen", async () => {
    let sent: unknown;
    server.use(
      http.get(`${API}/admin/organizations`, () => HttpResponse.json({ organizations: [org] })),
      http.get(`${API}/admin/users`, () => HttpResponse.json({ users: [] })),
      http.put(`${API}/admin/organizations/org-9/status`, async ({ request }) => {
        sent = await request.json();
        return HttpResponse.json({ ...org, status: "SUSPENDED" });
      }),
    );
    renderWithProviders(<PlatformAdmin />, { user: { platform_role: "ADMIN" } });

    await userEvent.click(await screen.findByRole("button", { name: "Set status" }));
    const dialog = await screen.findByRole("alertdialog");
    expect(within(dialog).getByLabelText("New status")).toHaveValue("PENDING_REVIEW");
    expect(within(dialog).getByRole("button", { name: "Set status" })).toBeDisabled();

    await userEvent.selectOptions(within(dialog).getByLabelText("New status"), "SUSPENDED");
    await userEvent.click(within(dialog).getByRole("button", { name: "Set status" }));
    await vi.waitFor(() => expect(sent).toEqual({ status: "SUSPENDED" }));
  });
});
