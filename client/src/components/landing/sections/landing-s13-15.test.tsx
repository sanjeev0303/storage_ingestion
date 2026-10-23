import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { axe } from "vitest-axe";
import { describe, expect, it, vi } from "vitest";
import { Access } from "./s14-access";
import { Begin } from "./s15-begin";
import { Retain } from "./s13-retain";

const push = vi.fn();
vi.mock("next/navigation", () => ({ useRouter: () => ({ push }) }));

describe("retain demo", () => {
  it("Restore returns the scheduled row to Live", async () => {
    render(<Retain />);
    expect(screen.getByRole("heading", { name: "Scheduled for removal" })).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Restore" }));
    expect(await screen.findByRole("button", { name: "Delete again" })).toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: "Scheduled for removal" })).not.toBeInTheDocument();
  });

  it("says held rows are kept by a rule", () => {
    render(<Retain />);
    expect(screen.getByText("Kept by a hold or retention rule")).toBeInTheDocument();
  });
});

describe("access roles", () => {
  const panel = () => screen.getByRole("tabpanel");
  const canCount = () => within(panel()).getAllByText("Can").length;

  async function openAllBands() {
    for (const band of within(panel()).getAllByRole("button", { expanded: false })) {
      await userEvent.click(band);
    }
  }

  it("switches with arrow keys and counts what each role can do", async () => {
    render(<Access />);
    await openAllBands();
    expect(canCount()).toBe(25);
    screen.getByRole("tab", { name: /Owner/ }).focus();
    await userEvent.keyboard("{ArrowDown}");
    expect(screen.getByRole("tab", { name: /Administrator/ })).toHaveAttribute("aria-selected", "true");
    expect(canCount()).toBe(24);
    await userEvent.keyboard("{ArrowDown}");
    expect(canCount()).toBe(16);
    await userEvent.keyboard("{ArrowDown}");
    expect(canCount()).toBe(9);
    await userEvent.keyboard("{ArrowDown}");
    expect(screen.getByRole("tab", { name: /Owner/ })).toHaveAttribute("aria-selected", "true");
  });

  it("puts the broken chain in the markup", () => {
    render(<Access />);
    expect(screen.getByText("intact: false, broken at event 4")).toBeInTheDocument();
  });

  it("passes axe", async () => {
    const { container } = render(<Access />);
    expect(await axe(container)).toHaveNoViolations();
  });
});

describe("begin dialog", () => {
  it("enables Create connection once name and bucket are filled, then goes to sign in", async () => {
    render(<Begin />);
    const create = screen.getByRole("button", { name: "Create connection" });
    expect(create).toBeDisabled();
    await userEvent.type(screen.getByLabelText(/^name/i), "Prod");
    expect(create).toBeDisabled();
    await userEvent.type(screen.getByLabelText(/^bucket/i), "data");
    expect(create).toBeEnabled();
    await userEvent.click(create);
    expect(push).toHaveBeenCalledWith("/login");
  });

  it("shows counts and an inline error after leaving a bad bucket", async () => {
    render(<Begin />);
    await userEvent.type(screen.getByLabelText(/^bucket/i), "a");
    expect(screen.getByText("1 / 222")).toBeInTheDocument();
    expect(screen.queryByText(/bucket names are 3 to 222/i)).toBeNull();
    await userEvent.tab();
    expect(screen.getByText(/bucket names are 3 to 222/i)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Create connection" })).toBeDisabled();
  });

  it("offers the five creatable providers and passes axe", async () => {
    const { container } = render(<Begin />);
    expect(within(screen.getByLabelText("Provider")).getAllByRole("option")).toHaveLength(5);
    expect(await axe(container)).toHaveNoViolations();
  });
});
