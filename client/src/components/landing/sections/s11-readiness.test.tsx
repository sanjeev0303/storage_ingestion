import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { axe } from "vitest-axe";
import { describe, expect, it } from "vitest";
import { Readiness } from "./s11-readiness";

const filter = () => screen.getByLabelText("Filter dimensions");

describe("Readiness section", () => {
  it("filters the list and updates the live count", async () => {
    render(<Readiness />);
    expect(screen.getByRole("status")).toHaveTextContent("9 of 9 dimensions shown");

    await userEvent.type(filter(), "INTEG");

    expect(screen.getAllByRole("button")).toHaveLength(1);
    expect(screen.getByRole("button", { name: "Integrity" })).toBeInTheDocument();
    expect(screen.getByRole("status")).toHaveTextContent("1 of 9 dimensions shown");
  });

  it("moves the selection with the arrow keys and clears with Escape", async () => {
    render(<Readiness />);
    const first = screen.getByRole("button", { name: "Completeness" });
    expect(first).toHaveAttribute("aria-expanded", "true");

    await userEvent.click(filter());
    await userEvent.keyboard("{ArrowDown}");
    expect(screen.getByRole("button", { name: "Integrity" })).toHaveAttribute("aria-expanded", "true");
    expect(first).toHaveAttribute("aria-expanded", "false");

    await userEvent.keyboard("{ArrowUp}");
    expect(first).toHaveAttribute("aria-expanded", "true");

    await userEvent.type(filter(), "sec");
    expect(screen.getByRole("status")).toHaveTextContent("1 of 9");
    await userEvent.keyboard("{Escape}");
    expect(filter()).toHaveValue("");
    expect(screen.getByRole("status")).toHaveTextContent("9 of 9");
  });

  it("selects a name on click and says there is no score", async () => {
    render(<Readiness />);
    await userEvent.click(screen.getByRole("button", { name: "Security" }));
    expect(screen.getByRole("button", { name: "Security" })).toHaveAttribute("aria-expanded", "true");
    expect(screen.getByText("There is no overall score.")).toBeInTheDocument();
    expect(screen.getByText("Not checked")).toBeInTheDocument();
  });

  it("has no accessibility violations", async () => {
    const { container } = render(<Readiness />);
    expect(await axe(container)).toHaveNoViolations();
  });
});
