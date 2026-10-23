import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { axe } from "vitest-axe";
import { describe, expect, it } from "vitest";
import { Lineage } from "./s12-lineage";

function walkedEdges(container: HTMLElement) {
  return [...container.querySelectorAll('[data-edge][data-walked="true"]')].map((el) => el.getAttribute("data-edge"));
}

describe("Lineage section", () => {
  it("marks different edges for upstream and downstream", async () => {
    const { container } = render(<Lineage />);
    expect(screen.getByRole("button", { name: "Upstream" })).toHaveAttribute("aria-pressed", "true");
    expect(walkedEdges(container).sort()).toEqual(["c1", "c2", "der", "imp", "tr"]);

    await userEvent.click(screen.getByRole("button", { name: "Downstream" }));

    expect(screen.getByRole("button", { name: "Downstream" })).toHaveAttribute("aria-pressed", "true");
    expect(walkedEdges(container)).toEqual(["va2"]);
  });

  it("picks the start node from the keyboard", async () => {
    const { container } = render(<Lineage />);
    await userEvent.click(screen.getByRole("button", { name: "Downstream" }));

    const dataset = screen.getByRole("button", { name: "Dataset, start node" });
    dataset.focus();
    await userEvent.keyboard("{Enter}");

    expect(dataset).toHaveAttribute("aria-pressed", "true");
    expect(walkedEdges(container).sort()).toEqual(["c1", "c2", "der", "va1", "va2"]);

    const manifest = screen.getByRole("button", { name: "Manifest import, start node" });
    manifest.focus();
    await userEvent.keyboard(" ");
    expect(manifest).toHaveAttribute("aria-pressed", "true");
  });

  it("lists the relations as text", () => {
    render(<Lineage />);
    expect(screen.getByText(/Version 2 derived from Version 1/)).toBeInTheDocument();
  });

  it("has no accessibility violations", async () => {
    const { container } = render(<Lineage />);
    expect(await axe(container)).toHaveNoViolations();
  });
});
