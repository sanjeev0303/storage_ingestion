import { render, screen } from "@testing-library/react";
import { axe } from "vitest-axe";
import { describe, expect, it } from "vitest";
import { Report } from "./s10-report";

describe("Report section", () => {
  it("shows all five outcomes and the verdict", () => {
    render(<Report />);
    for (const word of ["Passed", "Partial", "Warning", "Not checked"]) {
      expect(screen.getByText(word)).toBeInTheDocument();
    }
    expect(screen.getByText("Failed")).toBeInTheDocument();
    expect(screen.getByText("FAILED")).toBeInTheDocument();
    expect(screen.getByText("Sampled 100 of 1,204")).toBeInTheDocument();
  });

  it("has no accessibility violations", async () => {
    const { container } = render(<Report />);
    expect(await axe(container)).toHaveNoViolations();
  });
});
