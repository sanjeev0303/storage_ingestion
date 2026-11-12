import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";
import { integrations, problem } from "./copy";
import { Integrations } from "./integrations";
import { Problem } from "./problem";

describe("homepage figures", () => {
  it("shows every failure beside its fix, with no switch to find", () => {
    render(<Problem />);
    expect(screen.queryByRole("radio")).not.toBeInTheDocument();
    const rows = screen.getAllByRole("listitem");
    expect(rows).toHaveLength(problem.items.length);
    problem.items.forEach((item, i) => {
      const row = within(rows[i]);
      expect(row.getByText(item.title)).toBeVisible();
      expect(row.getByText(item.before)).toBeVisible();
      expect(row.getByText(item.after)).toBeVisible();
    });
  });

  it("switches the training snippet by framework, with arrow keys too", async () => {
    render(<Integrations />);
    expect(screen.getByRole("tabpanel")).toHaveTextContent("osp.torch");

    await userEvent.click(screen.getByRole("tab", { name: "JAX" }));
    expect(screen.getByRole("tabpanel")).toHaveTextContent("osp.jax");

    await userEvent.keyboard("{ArrowRight}");
    expect(screen.getByRole("tab", { name: integrations.frameworks[4].name })).toHaveAttribute("aria-selected", "true");
  });
});
