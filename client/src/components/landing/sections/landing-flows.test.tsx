import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { axe } from "vitest-axe";
import { describe, expect, it } from "vitest";
import { Adopt } from "./s06b-adopt";
import { Copies } from "./s13c-copies";
import { Inspect } from "./s05b-inspect";
import { Lock } from "./s13b-lock";
import { Prove } from "./s14b-prove";
import { Stream } from "./s11b-stream";

// The step lists are what phones and screen readers get, so the tests read them.
const steps = () => within(screen.getByRole("list")).getAllByRole("listitem").map((li) => li.textContent ?? "");
const last = () => steps().at(-1) ?? "";

describe("homepage flows", () => {
  it("inspect: a pickle is rejected, a clean FASTQ is published", async () => {
    render(<Inspect />);
    expect(last()).toContain("Rejected");
    expect(screen.getByText("model.unsafe_pickle")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("radio", { name: "reads.fastq.gz" }));
    expect(last()).toContain("Published");
    expect(steps().some((s) => s.includes("Malware scan") && s.includes("not reached"))).toBe(false);
  });

  it("adopt: a changed object is refused, and STAC lands through an upload", async () => {
    render(<Adopt />);
    expect(last()).toContain("Published in place");
    await userEvent.click(screen.getByRole("radio", { name: "STAC" }));
    expect(last()).toContain("Uploaded, then checked");
    await userEvent.click(screen.getByRole("checkbox", { name: "One object's bytes changed" }));
    expect(last()).toContain("Refused: digest mismatch");
  });

  it("stream: the code follows the framework", async () => {
    render(<Stream />);
    expect(screen.getByText(/SnapshotDataset\(client/)).toBeInTheDocument();
    await userEvent.click(screen.getByRole("radio", { name: "Ray" }));
    expect(screen.getByText(/read_snapshot\(client/)).toBeInTheDocument();
    expect(last()).toContain("Ray");
  });

  it("lock: any one of the three keeps the bytes; none purges with a certificate", async () => {
    render(<Lock />);
    expect(last()).toContain("Kept, checked again later");
    await userEvent.click(screen.getByRole("checkbox", { name: "Legal hold" }));
    expect(last()).toContain("Signed certificate");
    await userEvent.click(screen.getByRole("checkbox", { name: "Object Lock" }));
    expect(last()).toContain("Kept, checked again later");
  });

  it("copies: failover is served from the replica", async () => {
    render(<Copies />);
    await userEvent.click(screen.getByRole("radio", { name: "The primary goes down" }));
    expect(last()).toContain("serves the read");
    expect(steps()[1]).toContain("not reached");
  });

  it("prove: changing event 3 breaks the chain from there on", async () => {
    render(<Prove />);
    expect(screen.getByText(/Chain verified/)).toBeInTheDocument();
    await userEvent.click(screen.getByRole("checkbox", { name: "Change event 3" }));
    expect(screen.getByText(/Chain broken at event 3/)).toBeInTheDocument();
    expect(steps()[3]).toContain("not reached");
    expect(steps()[1]).not.toContain("not reached");
  });

  it("has no axe violations", async () => {
    for (const Section of [Inspect, Adopt, Stream, Lock, Copies, Prove]) {
      const { container, unmount } = render(<Section />);
      expect(await axe(container)).toHaveNoViolations();
      unmount();
    }
  });
});
