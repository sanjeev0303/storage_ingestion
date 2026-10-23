import type { Outcome } from "@/components/ui/status";
import type { CheckStatus, Coverage, DimensionCoverage, DimensionStatus, JobState, Mode, ReportStatus } from "./types";

// Three vocabularies come from the API and must never be mixed:
//   report status     READY, READY_WITH_WARNINGS, FAILED       (overall verdict from the profile thresholds)
//   check status      PASSED, FAILED, PARTIAL, NOT_ASSESSED    (one rule)
//   dimension status  PASS, WARN, FAIL, NOT_ASSESSED           (one readiness dimension)
// Each maps to the shared Outcome shapes through its own function.
export type Mapped = { outcome: Outcome; label: string };

export function reportStatusOutcome(status: ReportStatus): Mapped {
  if (status === "READY") return { outcome: "passed", label: "Ready" };
  if (status === "READY_WITH_WARNINGS") return { outcome: "warning", label: "Ready with warnings" };
  return { outcome: "failed", label: "Failed" };
}

export function checkStatusOutcome(status: CheckStatus, coverage?: Coverage): Mapped {
  if (status === "PASSED") return { outcome: "passed", label: "Passed" };
  if (status === "FAILED") return { outcome: "failed", label: "Failed" };
  if (status === "PARTIAL") {
    return { outcome: "partial", label: coverage === "SAMPLED" ? "Partial (sampled)" : "Partial" };
  }
  return { outcome: "not_assessed", label: "Not checked" };
}

export function dimensionStatusOutcome(status: DimensionStatus): Mapped {
  if (status === "PASS") return { outcome: "passed", label: "Pass" };
  if (status === "WARN") return { outcome: "warning", label: "Warn" };
  if (status === "FAIL") return { outcome: "failed", label: "Fail" };
  return { outcome: "not_assessed", label: "Not checked" };
}

export const COVERAGE_LABELS: Record<Coverage | DimensionCoverage, string> = {
  NONE: "No content examined",
  SAMPLED: "Sampled",
  FULL: "Full",
  PARTIAL: "Partial",
};

export const MODE_LABELS: Record<Mode, string> = {
  METADATA: "Metadata only",
  FULL: "Full integrity",
  SAMPLED: "Sampled integrity",
};

export const MODE_HELP: Record<Mode, string> = {
  METADATA:
    "Reads catalog records only (names, sizes, labels, splits, recorded checksums) and never reads object content. Fast, and it cannot tell you the stored bytes are intact.",
  FULL: "Checks the integrity of every member's content, from processing evidence or by reading the bytes. Slowest, and the only mode that covers every member.",
  SAMPLED:
    "Checks the content of a deterministic sample of members (every Nth in manifest order). Not a full integrity scan: a passing content check counts as partial.",
};

const TERMINAL: JobState[] = ["COMPLETED", "COMPLETED_WITH_WARNINGS", "FAILED", "CANCELLED"];

export function isTerminal(state: JobState) {
  return TERMINAL.includes(state);
}

export const JOB_STATE_LABELS: Record<JobState, string> = {
  QUEUED: "Queued",
  RUNNING: "Running",
  COMPLETED: "Completed",
  COMPLETED_WITH_WARNINGS: "Completed with warnings",
  FAILED: "Failed",
  CANCELLED: "Cancelled",
};

// FAILED with a report means the version failed its checks. FAILED without
// one means the job itself could not run.
export function failedKind(job: { state: JobState; report_id?: string }): "version_failed" | "could_not_run" | null {
  if (job.state !== "FAILED") return null;
  return job.report_id ? "version_failed" : "could_not_run";
}
