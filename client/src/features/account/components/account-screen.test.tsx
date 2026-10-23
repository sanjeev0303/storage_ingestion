import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { describe, expect, it } from "vitest";
import { renderWithProviders } from "@/test/render";
import { API, server } from "@/test/server";
import { AccountScreen } from "./account-screen";

function mockSessions() {
  server.use(http.get(`${API}/auth/sessions`, () => HttpResponse.json({ sessions: [] })));
}

async function fill(current: string, next: string, confirm = next) {
  await userEvent.type(await screen.findByLabelText(/^Current password/), current);
  await userEvent.type(screen.getByLabelText(/^New password/), next);
  await userEvent.type(screen.getByLabelText(/^Repeat new password/), confirm);
  await userEvent.click(screen.getByRole("button", { name: "Change password" }));
}

describe("Change password", () => {
  it("keeps the button disabled and explains the problem inline", async () => {
    mockSessions();
    const calls: number[] = [];
    server.use(
      http.post(`${API}/auth/password`, () => {
        calls.push(1);
        return new HttpResponse(null, { status: 204 });
      }),
    );
    renderWithProviders(<AccountScreen />);

    await fill("old-password-1", "short", "different");
    await userEvent.tab();
    expect(screen.getByRole("button", { name: "Change password" })).toBeDisabled();
    expect(screen.getByRole("list", { name: "Requirements for your password" })).toHaveTextContent("At least 12 characters, not met yet");
    expect(await screen.findByText("The two passwords do not match.")).toBeInTheDocument();
    expect(calls).toHaveLength(0);
  });

  it("maps a wrong current password to that field", async () => {
    mockSessions();
    server.use(
      http.post(`${API}/auth/password`, () =>
        HttpResponse.json({ error: "invalid credentials", code: "invalid_credentials" }, { status: 401 }),
      ),
    );
    renderWithProviders(<AccountScreen />);

    await fill("old-password-1", "a-new-password-12");
    expect(await screen.findByText("That is not your current password.")).toBeInTheDocument();
  });

  it("shows the server message for a rejected new password", async () => {
    mockSessions();
    server.use(
      http.post(`${API}/auth/password`, () =>
        HttpResponse.json({ error: "password must not equal your email", code: "invalid_request" }, { status: 400 }),
      ),
    );
    renderWithProviders(<AccountScreen />);

    await fill("old-password-1", "a-new-password-12");
    expect(await screen.findByText("password must not equal your email")).toBeInTheDocument();
  });

  it("clears the fields and says other sessions ended on success", async () => {
    mockSessions();
    server.use(http.post(`${API}/auth/password`, () => new HttpResponse(null, { status: 204 })));
    renderWithProviders(<AccountScreen />);

    await fill("old-password-1", "a-new-password-12");
    expect(await screen.findByText(/Other sessions were signed out/)).toBeInTheDocument();
    expect(screen.getByLabelText(/^Current password/)).toHaveValue("");
    expect(screen.getByLabelText(/^New password/)).toHaveValue("");
  });
});
