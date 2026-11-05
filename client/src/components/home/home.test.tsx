import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { axe } from "vitest-axe";
import { describe, expect, it } from "vitest";
import { AccessionSlip } from "./accession-slip";
import { DepthSwitch } from "./depth-switch";
import { ReadinessDeck } from "./readiness-deck";

describe("homepage", () => {
  it("the accession slip follows the chosen upload", async () => {
    render(<AccessionSlip />);
    expect(screen.getByText("ACC-0412")).toBeInTheDocument();
    expect(screen.getByText("Rejected")).toBeInTheDocument();
    expect(screen.getByText("Not reached")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("radio", { name: "reads.fastq.gz" }));
    expect(screen.getByText("ACC-0415")).toBeInTheDocument();
    expect(screen.getByText("Released")).toBeInTheDocument();
    expect(screen.getByText("Clean")).toBeInTheDocument();
    // The one row no upload check can pass is always shown as not tested.
    expect(screen.getByText("Not tested")).toBeInTheDocument();
  });

  it("the depth switch changes what the heading says was read", async () => {
    render(<DepthSwitch />);
    expect(screen.getByRole("heading", { name: "Read 8 of 48" })).toBeInTheDocument();
    await userEvent.click(screen.getByRole("radio", { name: "Full integrity" }));
    expect(screen.getByRole("heading", { name: "Read 48 of 48" })).toBeInTheDocument();
    await userEvent.click(screen.getByRole("radio", { name: "Metadata only" }));
    expect(screen.getByRole("heading", { name: "Read 0 of 48" })).toBeInTheDocument();
  });

  it("a spine opens its dimension", async () => {
    render(<ReadinessDeck />);
    const deck = screen.getAllByRole("list")[0];
    await userEvent.click(within(deck).getByRole("button", { name: "Show Governance, Fail" }));
    expect(within(deck).getByRole("heading", { name: "Governance" })).toBeInTheDocument();
  });

  it("has no axe violations", async () => {
    for (const Part of [AccessionSlip, DepthSwitch, ReadinessDeck]) {
      const { container, unmount } = render(<Part />);
      expect(await axe(container)).toHaveNoViolations();
      unmount();
    }
  });
});
