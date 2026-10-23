import { describe, expect, it } from "vitest";
import { checkStatusOutcome, dimensionStatusOutcome, failedKind, isTerminal, reportStatusOutcome } from "./status";

describe("the three status vocabularies", () => {
  it.each([
    ["READY", "passed", "Ready"],
    ["READY_WITH_WARNINGS", "warning", "Ready with warnings"],
    ["FAILED", "failed", "Failed"],
  ] as const)("report status %s", (status, outcome, label) => {
    expect(reportStatusOutcome(status)).toEqual({ outcome, label });
  });

  it.each([
    ["PASSED", "NONE", "passed", "Passed"],
    ["FAILED", "FULL", "failed", "Failed"],
    ["PARTIAL", "SAMPLED", "partial", "Partial (sampled)"],
    ["PARTIAL", "FULL", "partial", "Partial"],
    ["NOT_ASSESSED", "NONE", "not_assessed", "Not checked"],
  ] as const)("check status %s with %s coverage", (status, coverage, outcome, label) => {
    expect(checkStatusOutcome(status, coverage)).toEqual({ outcome, label });
  });

  it.each([
    ["PASS", "passed", "Pass"],
    ["WARN", "warning", "Warn"],
    ["FAIL", "failed", "Fail"],
    ["NOT_ASSESSED", "not_assessed", "Not checked"],
  ] as const)("dimension status %s", (status, outcome, label) => {
    expect(dimensionStatusOutcome(status)).toEqual({ outcome, label });
  });

  it("never maps NOT_ASSESSED to passed, in either vocabulary", () => {
    expect(checkStatusOutcome("NOT_ASSESSED").outcome).not.toBe("passed");
    expect(dimensionStatusOutcome("NOT_ASSESSED").outcome).not.toBe("passed");
  });
});

describe("job states", () => {
  it("treats only queued and running as live", () => {
    expect(isTerminal("QUEUED")).toBe(false);
    expect(isTerminal("RUNNING")).toBe(false);
    for (const state of ["COMPLETED", "COMPLETED_WITH_WARNINGS", "FAILED", "CANCELLED"] as const) {
      expect(isTerminal(state)).toBe(true);
    }
  });

  it("tells a failed version from a job that could not run", () => {
    expect(failedKind({ state: "FAILED", report_id: "r1" })).toBe("version_failed");
    expect(failedKind({ state: "FAILED" })).toBe("could_not_run");
    expect(failedKind({ state: "COMPLETED", report_id: "r1" })).toBeNull();
  });
});
