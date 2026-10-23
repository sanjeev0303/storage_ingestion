import { configure, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { describe, expect, it } from "vitest";
import { renderWithProviders } from "@/test/render";
import { API, server } from "@/test/server";
import { profileList } from "../fixtures";
import { ProfileForm } from "./profile-form";

configure({ asyncUtilTimeout: 5000 });

describe("ProfileForm", () => {
  it("keeps Create disabled until valid, shows the name error after blur, and counts characters", async () => {
    server.use(
      http.get(`${API}/validation-profiles`, () =>
        HttpResponse.json(profileList),
      ),
    );
    renderWithProviders(<ProfileForm />);
    const name = await screen.findByLabelText(/^Name/);
    const create = screen.getByRole("button", { name: "Create profile" });
    expect(create).toBeDisabled();
    expect(screen.getByText("0 / 128")).toBeInTheDocument();
    expect(screen.queryByText(/Name is required/)).not.toBeInTheDocument();

    await userEvent.click(name);
    await userEvent.tab();
    expect(await screen.findByText(/Name is required/)).toBeInTheDocument();

    await userEvent.type(name, "My profile");
    expect(screen.getByText("10 / 128")).toBeInTheDocument();
    expect(screen.queryByText(/Name is required/)).not.toBeInTheDocument();
  });
});
