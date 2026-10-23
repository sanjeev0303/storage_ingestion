import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { server } from "@/test/server";
import { RegisterForm } from "./register-form";

const replace = vi.fn();
vi.mock("next/navigation", () => ({ useRouter: () => ({ replace }) }));

const REGISTER = "http://localhost:3100/api/session/register";

async function fillIn() {
  await userEvent.type(screen.getByLabelText(/organization name/i), "Sam Labs");
  await userEvent.type(screen.getByLabelText(/email/i), "sam@example.test");
  await userEvent.type(screen.getByLabelText(/^password/i), "correct horse battery staple");
}

beforeEach(() => replace.mockReset());

describe("RegisterForm", () => {
  it("sends the details and opens the console", async () => {
    let body: unknown;
    server.use(
      http.post(REGISTER, async ({ request }) => {
        body = await request.json();
        return HttpResponse.json({ user: { id: "u1" }, session_id: "s1" });
      }),
    );
    render(<RegisterForm />);
    await fillIn();
    await userEvent.click(screen.getByRole("button", { name: "Create account" }));

    await waitFor(() => expect(replace).toHaveBeenCalledWith("/app/overview"));
    expect(body).toEqual({
      organization_name: "Sam Labs",
      display_name: "",
      email: "sam@example.test",
      password: "correct horse battery staple",
    });
  });

  it("keeps the button disabled until the form is valid and says why", async () => {
    render(<RegisterForm />);
    const button = screen.getByRole("button", { name: "Create account" });
    expect(button).toBeDisabled();
    expect(screen.getByText("Fill in the required fields to continue.")).toBeInTheDocument();

    await fillIn();
    await waitFor(() => expect(button).toBeEnabled());
  });

  it("checks off the password requirements as you type", async () => {
    render(<RegisterForm />);
    const rules = screen.getByRole("list", { name: "Requirements for your password" });
    expect(rules).toHaveTextContent("At least 12 characters, not met yet");

    await userEvent.type(screen.getByLabelText(/^password/i), "correct horse battery staple");
    expect(rules).toHaveTextContent("At least 12 characters, met");
  });

  it("shows and hides the password on request", async () => {
    render(<RegisterForm />);
    const input = screen.getByLabelText(/^password/i);
    expect(input).toHaveAttribute("type", "password");
    await userEvent.click(screen.getByRole("button", { name: "Show password" }));
    expect(input).toHaveAttribute("type", "text");
    await userEvent.click(screen.getByRole("button", { name: "Hide password" }));
    expect(input).toHaveAttribute("type", "password");
  });

  it("shows an inline error once a field is left, and counts characters", async () => {
    render(<RegisterForm />);
    const email = screen.getByLabelText(/email/i);
    await userEvent.type(email, "not-an-email");
    expect(screen.queryByText("Enter a valid email address.")).not.toBeInTheDocument();
    await userEvent.tab();
    expect(await screen.findByText("Enter a valid email address.")).toBeInTheDocument();

    await userEvent.type(screen.getByLabelText(/organization name/i), "Sam Labs");
    expect(screen.getByText("8 / 200")).toBeInTheDocument();
  });

  it("lowercases the email it sends", async () => {
    let body: { email?: string } = {};
    server.use(
      http.post(REGISTER, async ({ request }) => {
        body = (await request.json()) as { email?: string };
        return HttpResponse.json({ user: { id: "u1" }, session_id: "s1" });
      }),
    );
    render(<RegisterForm />);
    await userEvent.type(screen.getByLabelText(/organization name/i), "Sam Labs");
    await userEvent.type(screen.getByLabelText(/email/i), "  Sam@Example.TEST ");
    await userEvent.type(screen.getByLabelText(/^password/i), "correct horse battery staple");
    await userEvent.click(screen.getByRole("button", { name: "Create account" }));
    await waitFor(() => expect(body.email).toBe("sam@example.test"));
  });

  it.each([
    ["registration_closed", 403, /sign-up is not open/i],
    ["conflict", 409, /already exists/i],
  ])("explains a %s answer", async (code, status, message) => {
    server.use(http.post(REGISTER, () => HttpResponse.json({ error: "x", code }, { status })));
    render(<RegisterForm />);
    await fillIn();
    await userEvent.click(screen.getByRole("button", { name: "Create account" }));
    expect(await screen.findByRole("alert")).toHaveTextContent(message);
    expect(replace).not.toHaveBeenCalled();
  });

  it("shows the server's message for a refused password", async () => {
    server.use(
      http.post(REGISTER, () =>
        HttpResponse.json({ error: "weak password: at least 12 characters", code: "invalid_request" }, { status: 400 }),
      ),
    );
    render(<RegisterForm />);
    await fillIn();
    await userEvent.click(screen.getByRole("button", { name: "Create account" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("at least 12 characters");
  });
});
