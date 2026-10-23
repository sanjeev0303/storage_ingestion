import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { axe } from "vitest-axe";
import { copy } from "../copy";
import { Connect } from "./s03-connect";

describe("connect section", () => {
  it("renders the three steps with an example connection row each", () => {
    render(<Connect />);
    expect(screen.getByRole("heading", { level: 2, name: copy.connect.title })).toBeInTheDocument();
    for (const step of copy.connect.steps) {
      expect(screen.getByText(step.order)).toBeInTheDocument();
    }
    expect(screen.getAllByRole("group", { name: /Example connection/ })).toHaveLength(3);
    expect(screen.getByRole("img", { name: copy.connect.diagramLabel })).toBeInTheDocument();
  });

  it("lists the providers", () => {
    render(<Connect />);
    expect(screen.getByText("Cloudflare R2")).toBeInTheDocument();
  });

  it("has no accessibility violations", async () => {
    const { container } = render(<Connect />);
    expect(await axe(container)).toHaveNoViolations();
  });
});
