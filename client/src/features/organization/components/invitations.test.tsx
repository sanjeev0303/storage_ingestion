import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { describe, expect, it } from "vitest";
import { axe } from "vitest-axe";
import { renderWithProviders } from "@/test/render";
import { API, server } from "@/test/server";
import { Invitations } from "./invitations";

const roles = [{ id: "developer", name: "Developer", permissions: ["org.read"], built_in: true }];
const invitation = {
  id: "inv-1",
  email: "new@example.test",
  role_id: "developer",
  status: "PENDING",
  created_by: "user-1",
  created_at: "2026-01-01T00:00:00Z",
  expires_at: "2026-01-08T00:00:00Z",
};

function mockApi() {
  let created = false;
  const sent: { body?: unknown } = {};
  server.use(
    http.get(`${API}/organization/roles`, () => HttpResponse.json({ roles })),
    http.get(`${API}/organization/invitations`, () => HttpResponse.json({ invitations: created ? [invitation] : [] })),
    http.post(`${API}/organization/invitations`, async ({ request }) => {
      sent.body = await request.json();
      created = true;
      return HttpResponse.json({ ...invitation, token: "sit_secret_value" }, { status: 201 });
    }),
  );
  return sent;
}

async function invite() {
  await userEvent.type(await screen.findByLabelText(/Email address/), "new@example.test");
  await userEvent.click(await screen.findByRole("button", { name: "Create invitation" }));
}

describe("Invitations", () => {
  it("shows the token once, with the no-email note, and never again", async () => {
    const sent = mockApi();
    renderWithProviders(<Invitations />);
    await screen.findByRole("option", { name: "Developer" });
    await invite();

    const dialog = await screen.findByRole("dialog");
    expect(dialog).toHaveTextContent("sit_secret_value");
    expect(dialog).toHaveTextContent("No email is sent");
    expect(sent.body).toEqual({ email: "new@example.test", role_id: "developer" });

    await userEvent.click(screen.getByRole("button", { name: "I have copied it" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    // the refreshed list has the invitation, but the token is gone from the page
    await screen.findByRole("cell", { name: "new@example.test" });
    expect(document.body).not.toHaveTextContent("sit_secret_value");
  });

  it("keeps Create invitation disabled until the email is valid, and says why after blur", async () => {
    mockApi();
    renderWithProviders(<Invitations />);
    await screen.findByRole("option", { name: "Developer" });
    const button = screen.getByRole("button", { name: "Create invitation" });
    expect(button).toBeDisabled();

    await userEvent.type(screen.getByLabelText(/Email address/), "not-an-email");
    await userEvent.tab();
    expect(await screen.findByRole("alert")).toHaveTextContent(/Enter a valid email address/);
    expect(button).toBeDisabled();

    await userEvent.clear(screen.getByLabelText(/Email address/));
    await userEvent.type(screen.getByLabelText(/Email address/), " New@Example.test ");
    expect(button).toBeEnabled();
  });

  it("tells a member without members.manage that the tab is off limits", async () => {
    renderWithProviders(<Invitations />, { permissions: ["org.read", "members.read"] });
    expect(await screen.findByRole("alert")).toHaveTextContent(/members\.manage/);
  });

  it("has an accessible invitation form", async () => {
    mockApi();
    const { container } = renderWithProviders(<Invitations />);
    await screen.findByLabelText(/Email address/);
    await screen.findByRole("option", { name: "Developer" });
    expect(await axe(container)).toHaveNoViolations();
  });
});
