import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { axe } from "vitest-axe";
import { copy } from "../copy";
import { readCells, ValidateGrid } from "../diagrams/validate-grid";
import { ValidateBody } from "./validate-body";

const expected = [
  { read: 0, coverage: "None", outcome: "Not checked" },
  { read: 8, coverage: "Sampled", outcome: "Partial" },
  { read: 48, coverage: "Full", outcome: "Passed" },
];

describe("ValidateGrid", () => {
  it.each(expected.map((item, mode) => ({ mode, ...item })))(
    "mode $mode reads $read cells with coverage $coverage",
    ({ mode, read, coverage, outcome }) => {
      render(<ValidateGrid mode={mode} />);
      expect(screen.getByText(`Content read: ${read} of 48`)).toBeInTheDocument();
      expect(screen.getByText(`Coverage: ${coverage}`)).toBeInTheDocument();
      expect(screen.getByText(outcome)).toBeInTheDocument();
      expect(readCells(mode)).toHaveLength(read);
    },
  );

  it("samples every sixth cell", () => {
    expect(readCells(1)).toEqual([0, 6, 12, 18, 24, 30, 36, 42]);
  });
});

describe("ValidateBody", () => {
  it("shows the heading and all three statements, and the stage with its counter", () => {
    render(<ValidateBody />);
    expect(screen.getByRole("heading", { level: 2, name: copy.validate.title })).toBeInTheDocument();
    copy.validate.statements.forEach((statement) => {
      expect(screen.getByText(statement.mode)).toBeInTheDocument();
    });
    // jsdom has no layout, so which mode is active is decided in the browser check.
    expect(screen.getByText(/Content read: \d+ of 48/)).toBeInTheDocument();
  });

  it("has no accessibility violations", async () => {
    const { container } = render(
      <main>
        <ValidateBody />
      </main>,
    );
    expect(await axe(container)).toHaveNoViolations();
  });
});
