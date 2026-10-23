import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { axe } from "vitest-axe";
import { copy } from "../copy";
import { SnapshotStage } from "./snapshot-stage";

describe("SnapshotStage", () => {
  it("maps slider values 0 to 3 to the stage name, clause and aria-valuetext", () => {
    render(<SnapshotStage />);
    const slider = screen.getByRole("slider", { name: copy.snapshot.sliderLabel });

    copy.snapshot.stages.forEach((stage, index) => {
      fireEvent.change(slider, { target: { value: String(index) } });
      expect(slider).toHaveAttribute("aria-valuetext", stage.name);
      expect(screen.getByText(stage.clause)).toBeInTheDocument();
    });
  });

  it("has no accessibility violations", async () => {
    const { container } = render(
      <main>
        <SnapshotStage />
      </main>,
    );
    expect(await axe(container)).toHaveNoViolations();
  });
});
