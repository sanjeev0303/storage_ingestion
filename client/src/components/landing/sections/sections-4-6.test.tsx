import { render, screen, within } from "@testing-library/react";
import { axe } from "vitest-axe";
import { afterEach, describe, expect, it, vi } from "vitest";
import { copy } from "../copy";
import { Ingest } from "./s06-ingest";
import { Upload } from "./s04-upload";
import { Verify } from "./s05-verify";

afterEach(() => vi.unstubAllGlobals());

// Pretend the reader has no motion preference so the GSAP timelines are built.
function allowMotion() {
  vi.stubGlobal("matchMedia", (query: string) => ({
    matches: query.includes("no-preference"),
    media: query,
    addEventListener: () => undefined,
    removeEventListener: () => undefined,
    addListener: () => undefined,
    removeListener: () => undefined,
    dispatchEvent: () => false,
  }));
}

describe("Upload", () => {
  it("shows the heading, the honest note and the diagram label", () => {
    render(<Upload />);
    expect(screen.getByRole("heading", { level: 2, name: copy.upload.title })).toBeInTheDocument();
    expect(screen.getByText(copy.upload.note)).toBeInTheDocument();
    expect(screen.getByRole("img", { name: copy.upload.diagramLabel })).toBeInTheDocument();
  });

  it("has no accessibility violations", async () => {
    const { container } = render(<Upload />);
    expect(await axe(container)).toHaveNoViolations();
  });
});

describe("Verify", () => {
  it("renders the horizontal and vertical pipelines with every stage", () => {
    render(<Verify />);
    expect(screen.getByRole("heading", { level: 2, name: copy.verify.title })).toBeInTheDocument();
    const figures = screen.getAllByRole("img", { name: copy.verify.diagramLabel });
    expect(figures).toHaveLength(2);
    for (const figure of figures) {
      for (const stage of copy.verify.stages) {
        expect(within(figure).getByText(stage)).toBeInTheDocument();
      }
      for (const state of copy.verify.states) {
        expect(within(figure).getByText(state)).toBeInTheDocument();
      }
    }
  });

  it("builds both timelines without throwing", () => {
    allowMotion();
    expect(() => render(<Verify />)).not.toThrow();
  });

  it("has no accessibility violations", async () => {
    const { container } = render(<Verify />);
    expect(await axe(container)).toHaveNoViolations();
  });
});

describe("Ingest", () => {
  it("shows the same entry in four named states", () => {
    render(<Ingest />);
    expect(screen.getByRole("heading", { level: 2, name: copy.ingest.title })).toBeInTheDocument();
    const strip = screen.getByRole("img", { name: copy.ingest.diagramLabel });
    for (const state of copy.ingest.states) {
      expect(within(strip).getByRole("heading", { name: state })).toBeInTheDocument();
    }
    expect(within(strip).getAllByText(copy.ingest.entry.path)).toHaveLength(4);
    expect(within(strip).getByText("invalid_checksum")).toBeInTheDocument();
    expect(within(strip).getByText("AVAILABLE")).toBeInTheDocument();
  });

  it("builds its timeline without throwing", () => {
    allowMotion();
    expect(() => render(<Ingest />)).not.toThrow();
  });

  it("has no accessibility violations", async () => {
    const { container } = render(<Ingest />);
    expect(await axe(container)).toHaveNoViolations();
  });
});
