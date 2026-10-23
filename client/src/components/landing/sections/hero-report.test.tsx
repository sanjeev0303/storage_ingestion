import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";
import { axe } from "vitest-axe";
import { Hero } from "./s01-hero";
import { HeroReport } from "./hero-report";

function integrityRow() {
  const row = screen.getByText("Content integrity").closest("li");
  if (!row) throw new Error("integrity row missing");
  return within(row);
}

describe("hero report", () => {
  it("starts on Metadata only", () => {
    render(<HeroReport />);
    expect(screen.getByRole("radio", { name: "Metadata only" })).toBeChecked();
    expect(integrityRow().getByText("Not checked")).toBeInTheDocument();
    expect(integrityRow().getByText("Content was never read")).toBeInTheDocument();
    expect(screen.getByText("0 of 1,204")).toBeInTheDocument();
  });

  it("changes the integrity row and the live summary for each mode", async () => {
    const user = userEvent.setup();
    render(<HeroReport />);

    await user.click(screen.getByRole("radio", { name: "Sampled" }));
    expect(integrityRow().getByText("Partial")).toBeInTheDocument();
    expect(integrityRow().getByText("Read 100 of 1,204 objects")).toBeInTheDocument();
    expect(screen.getByText("100 of 1,204")).toBeInTheDocument();

    await user.click(screen.getByRole("radio", { name: "Full integrity" }));
    expect(integrityRow().getByText("Passed")).toBeInTheDocument();
    expect(integrityRow().getByText("Read 1,204 of 1,204 objects")).toBeInTheDocument();
    expect(screen.getByText("1,204 of 1,204")).toBeInTheDocument();
  });

  it("moves between modes with the arrow keys", async () => {
    const user = userEvent.setup();
    render(<HeroReport />);

    await user.click(screen.getByRole("radio", { name: "Metadata only" }));
    await user.keyboard("{ArrowDown}");
    expect(screen.getByRole("radio", { name: "Sampled" })).toBeChecked();
    expect(integrityRow().getByText("Partial")).toBeInTheDocument();
    await user.keyboard("{ArrowRight}");
    expect(screen.getByRole("radio", { name: "Full integrity" })).toBeChecked();
  });

  it("states the verdict in a polite live region", () => {
    render(<HeroReport />);
    const summary = screen.getByText(/There is no score/).closest("[aria-live]");
    expect(summary).toHaveAttribute("aria-live", "polite");
    expect(summary).toHaveTextContent("Verdict: Failed (1 error, 1 warning)");
  });

  it("has no accessibility violations", async () => {
    const { container } = render(<Hero />);
    expect(await axe(container)).toHaveNoViolations();
  });

  it("has the one h1 and the console link", () => {
    render(<Hero />);
    expect(screen.getByRole("heading", { level: 1, name: "Know what was checked." })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Open the console" })).toHaveAttribute("href", "/login");
    expect(screen.getByRole("link", { name: "Follow one dataset" })).toHaveAttribute("href", "#connect");
  });
});
