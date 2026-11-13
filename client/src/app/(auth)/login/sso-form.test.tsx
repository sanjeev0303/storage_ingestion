import { act, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { describe, expect, it, vi } from "vitest";
import { server } from "@/test/server";
import { SsoForm } from "./sso-form";

describe("SsoForm", () => {
  it("is usable again when the page comes back from the back-forward cache", async () => {
    const assign = vi.fn();
    vi.stubGlobal("location", { ...window.location, assign });
    server.use(
      http.post("http://localhost:3100/api/session/sso/start", () =>
        HttpResponse.json({ authorization_url: "https://idp.example.test/auth" }),
      ),
    );
    try {
      render(<SsoForm />);
      await userEvent.type(screen.getByLabelText(/organization short name/i), "acme");
      const button = screen.getByRole("button", { name: "Continue with single sign-on" });
      await userEvent.click(button);
      await waitFor(() => expect(assign).toHaveBeenCalledWith("https://idp.example.test/auth"));
      expect(button).toBeDisabled();

      act(() => {
        window.dispatchEvent(new PageTransitionEvent("pageshow", { persisted: true }));
      });
      expect(button).toBeEnabled();
    } finally {
      vi.unstubAllGlobals();
    }
  });
});
