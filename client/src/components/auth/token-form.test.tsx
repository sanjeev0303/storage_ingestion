import { render, screen, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { TokenForm } from "./token-form";

vi.mock("next/navigation", () => ({ useSearchParams: () => new URLSearchParams("token=sit_from_link") }));

const flow = {
  title: "Accept the invitation",
  intro: "Paste the token.",
  path: "/invitations/accept",
  button: "Accept invitation",
  done: "Done",
  passwordOptional: true,
};

describe("TokenForm", () => {
  it("validates a token taken from the link, so nothing else has to be touched to submit", async () => {
    render(<TokenForm flow={flow} />);
    expect(screen.getByLabelText(/token/i)).toHaveValue("sit_from_link");
    await waitFor(() => expect(screen.getByRole("button", { name: "Accept invitation" })).toBeEnabled());
  });
});
