import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { describe, expect, it, vi } from "vitest";
import { renderWithProviders } from "@/test/render";
import { API, server } from "@/test/server";
import type { Membership } from "../types";
import { Members } from "./members";

vi.mock("next/navigation", () => ({ useRouter: () => ({ push: vi.fn() }) }));

const roles = [
  { id: "developer", name: "Developer", permissions: ["org.read", "members.read"], built_in: true },
  { id: "auditor", name: "Auditor", permissions: ["org.read", "audit.read"], built_in: true },
];

function member(n: number, overrides: Partial<Membership> = {}): Membership {
  const id = `u-${String(n).padStart(2, "0")}`;
  return {
    organization_id: "org-1",
    user_id: id,
    email: `${id}@example.test`,
    role_id: "developer",
    status: "ACTIVE",
    created_at: "2026-01-01T00:00:00Z",
    ...overrides,
  };
}

function mockApi(members: (after: string | null) => Membership[]) {
  server.use(
    http.get(`${API}/organization/roles`, () => HttpResponse.json({ roles })),
    http.get(`${API}/organization/members`, ({ request }) =>
      HttpResponse.json({ members: members(new URL(request.url).searchParams.get("after")) }),
    ),
  );
}

describe("Members", () => {
  it("hides member actions from someone without members.manage", async () => {
    mockApi(() => [member(1), member(2)]);
    renderWithProviders(<Members />, { permissions: ["org.read", "members.read"] });

    await screen.findByText("u-02@example.test");
    expect(screen.queryByRole("button", { name: "Suspend" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Remove" })).not.toBeInTheDocument();
    expect(screen.getByRole("combobox", { name: "Role for u-02@example.test" })).toBeDisabled();
  });

  it("shows actions to members.manage and never lets you suspend yourself", async () => {
    mockApi(() => [member(1, { user_id: "user-1", email: "me@example.test" }), member(2)]);
    renderWithProviders(<Members />, { permissions: ["org.read", "members.read", "members.manage"] });

    const other = (await screen.findByText("u-02@example.test")).closest("tr")!;
    expect(within(other).getByRole("button", { name: "Suspend" })).toBeInTheDocument();
    const self = screen.getByText("me@example.test").closest("tr")!;
    expect(within(self).queryByRole("button", { name: "Suspend" })).not.toBeInTheDocument();
    expect(within(self).getByRole("button", { name: "Leave organization" })).toBeInTheDocument();
  });

  it("does not offer members whose role holds permissions the signed-in user lacks", async () => {
    mockApi(() => [member(2, { role_id: "auditor" })]);
    renderWithProviders(<Members />, { permissions: ["org.read", "members.read", "members.manage"] });

    await screen.findByText("u-02@example.test");
    expect(screen.queryByRole("button", { name: "Suspend" })).not.toBeInTheDocument();
  });

  it("pages with the last user id as the cursor", async () => {
    const seen: (string | null)[] = [];
    mockApi((after) => {
      seen.push(after);
      return after ? [member(51)] : Array.from({ length: 50 }, (_, i) => member(i + 1));
    });
    renderWithProviders(<Members />);

    await userEvent.click(await screen.findByRole("button", { name: "Load more" }));
    await screen.findByText("u-51@example.test");
    expect(seen).toEqual([null, "u-50"]);
    await waitFor(() => expect(screen.queryByRole("button", { name: "Load more" })).not.toBeInTheDocument());
  });

  it("explains a last-owner refusal in plain words", async () => {
    mockApi(() => [member(2)]);
    server.use(
      http.post(`${API}/organization/members/u-02/suspend`, () =>
        HttpResponse.json({ error: "last owner", code: "last_owner" }, { status: 409 }),
      ),
    );
    renderWithProviders(<Members />);

    await userEvent.click(await screen.findByRole("button", { name: "Suspend" }));
    expect(await screen.findByRole("alert")).toHaveTextContent(/without an active owner/);
  });
});
