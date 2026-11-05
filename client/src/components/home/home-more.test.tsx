import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { axe } from "vitest-axe";
import { describe, expect, it } from "vitest";
import { FeatureIndex } from "./feature-index";
import { FixChapters } from "./fix-chapters";
import { PlatformMap } from "./platform-map";

describe("homepage features", () => {
  it("the map explains the chosen stage and links to its fix", async () => {
    render(<PlatformMap />);
    expect(screen.getByRole("heading", { name: "Quarantine gate" })).toBeInTheDocument();
    await userEvent.click(screen.getByRole("radio", { name: "Second copy" }));
    expect(screen.getByRole("heading", { name: "Second copy" })).toBeInTheDocument();
    expect(screen.getByText("Replication to a second provider")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Read how it fixes this" })).toHaveAttribute("href", "#fix-copies");
  });

  it("the index lists every feature and narrows to one stage", async () => {
    render(<FeatureIndex />);
    expect(screen.getByText("29 of 29 features")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("radio", { name: "Evidence" }));
    expect(screen.getByText("3 of 29 features")).toBeInTheDocument();
    expect(screen.getByRole("rowheader", { name: "Audit chain" })).toBeInTheDocument();
    expect(screen.queryByRole("rowheader", { name: "Multipart uploads" })).not.toBeInTheDocument();
  });

  it("each of the six problems has its own section and diagram", () => {
    render(<FixChapters />);
    for (const title of [
      "Bad files pass the virus scan",
      "Uploading what you already hold",
      "Training reads unverified files",
      "A delete outruns a legal hold",
      "One copy, one bad day",
      "History nobody can prove",
    ]) {
      expect(screen.getByRole("heading", { name: title })).toBeInTheDocument();
    }
    expect(screen.getAllByRole("img").length).toBeGreaterThanOrEqual(6);
  });

  it("has no axe violations", async () => {
    for (const Part of [PlatformMap, FeatureIndex]) {
      const { container, unmount } = render(<Part />);
      expect(await axe(container)).toHaveNoViolations();
      unmount();
    }
  });
});
