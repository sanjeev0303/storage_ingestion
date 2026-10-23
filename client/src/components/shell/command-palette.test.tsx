import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import { renderWithProviders } from "@/test/render";
import { CommandPalette } from "./command-palette";

const push = vi.fn();
vi.mock("next/navigation", () => ({ useRouter: () => ({ push }), usePathname: () => "/app/overview" }));

describe("CommandPalette", () => {
  it("filters pages as you type and opens the chosen one with Enter", async () => {
    const user = userEvent.setup();
    renderWithProviders(<CommandPalette />, { permissions: ["datasets.read", "audit.read"] });

    await user.keyboard("{Control>}k{/Control}");
    await user.type(screen.getByRole("combobox"), "aud");

    expect(screen.getAllByRole("option")).toHaveLength(1);
    await user.keyboard("{Enter}");
    expect(push).toHaveBeenCalledWith("/app/audit");
  });

  it("hides pages the role cannot open", async () => {
    const user = userEvent.setup();
    renderWithProviders(<CommandPalette />, { permissions: ["datasets.read"] });
    await user.keyboard("{Control>}k{/Control}");
    expect(screen.queryByRole("option", { name: "Audit" })).not.toBeInTheDocument();
  });
});
