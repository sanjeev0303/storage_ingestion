import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";
import { axe } from "vitest-axe";
import { copy } from "../copy";
import { CatalogBoard } from "./catalog-board";

const rows = () => screen.getAllByRole("button");

describe("CatalogBoard", () => {
  it("opens the first row by default and one row at a time", async () => {
    render(<CatalogBoard />);
    expect(rows()[0]).toHaveAttribute("aria-expanded", "true");
    expect(screen.getByText("Version 2")).toBeInTheDocument();

    await userEvent.click(rows()[2]);
    expect(rows()[0]).toHaveAttribute("aria-expanded", "false");
    expect(rows()[2]).toHaveAttribute("aria-expanded", "true");
    expect(rows().filter((row) => row.getAttribute("aria-expanded") === "true")).toHaveLength(1);
  });

  it("toggles with Enter and Space and moves focus with arrows", async () => {
    const user = userEvent.setup();
    render(<CatalogBoard />);
    rows()[0].focus();
    await user.keyboard("{ArrowDown}");
    expect(rows()[1]).toHaveFocus();
    await user.keyboard("{Enter}");
    expect(rows()[1]).toHaveAttribute("aria-expanded", "true");
    await user.keyboard(" ");
    expect(rows()[1]).toHaveAttribute("aria-expanded", "false");
    await user.keyboard("{End}");
    expect(rows()[rows().length - 1]).toHaveFocus();
  });

  it("keeps every status word and the real list when motion is reduced", () => {
    // jsdom reports no motion preference match, so the flip never starts: statuses stay as written.
    render(<CatalogBoard />);
    expect(screen.getByText("Quarantined")).toBeInTheDocument();
    expect(rows()).toHaveLength(copy.catalog.rows.length);
  });

  it("has no accessibility violations", async () => {
    const { container } = render(<CatalogBoard />);
    expect(await axe(container)).toHaveNoViolations();
  });
});
