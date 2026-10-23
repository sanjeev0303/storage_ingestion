import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { copy } from "../copy";
import { Problem } from "./s02-problem";

describe("problem section", () => {
  it("shows the heading, the lead and the four run-in phrases", () => {
    render(<Problem />);
    expect(screen.getByRole("heading", { level: 2, name: copy.problem.title })).toBeInTheDocument();
    expect(screen.getByText(copy.problem.lead)).toBeInTheDocument();
    for (const column of copy.problem.columns) {
      expect(screen.getByText(column.run).tagName).toBe("STRONG");
    }
    expect(copy.problem.columns).toHaveLength(4);
  });
});
