import { formatRelative } from "@/lib/utils/format";
import type { ConnectionSummary, JobState, JobSummary } from "./types";

const STALL_AFTER_MS = 60 * 60 * 1000;
// Jobs the browser or server should be moving along. READY waits for a person, so it is not stalled.
const IN_MOTION: JobState[] = ["PENDING_VALIDATION", "VALIDATING", "RUNNING", "CANCELLING"];

export function findAttentionJobs(jobs: JobSummary[], now = Date.now()) {
  const found: { job: JobSummary; reason: string }[] = [];
  for (const job of jobs) {
    if (job.state === "FAILED") {
      found.push({ job, reason: job.error ?? "Nothing was published" });
    } else if (job.state === "PARTIALLY_COMPLETED") {
      found.push({ job, reason: `${job.progress.published} of ${job.progress.total} published` });
    } else if (IN_MOTION.includes(job.state) && now - Date.parse(job.updated_at) > STALL_AFTER_MS) {
      found.push({ job, reason: "No update for over an hour" });
    }
  }
  return found;
}

// The strip board's racks for one page of jobs. A stalled job sits in attention only, never in motion too.
export function rackJobs(jobs: JobSummary[], now = Date.now()) {
  const attention = findAttentionJobs(jobs, now);
  const flagged = new Set(attention.map((a) => a.job.id));
  return {
    attention,
    moving: jobs.filter((j) => IN_MOTION.includes(j.state) && !flagged.has(j.id)),
    ready: jobs.filter((j) => j.state === "READY"),
    completed: jobs.filter((j) => j.state === "COMPLETED"),
  };
}

export function lastTestFailed(c: ConnectionSummary) {
  return Boolean(
    c.last_tested_at &&
    c.last_error_category &&
    (!c.last_test_ok_at || Date.parse(c.last_tested_at) > Date.parse(c.last_test_ok_at)),
  );
}

export function describeConnectionTest(c: ConnectionSummary) {
  if (c.id === "platform") return "Managed by the platform, not tested here";
  if (!c.last_tested_at) return "Never tested";
  const when = formatRelative(c.last_tested_at);
  return lastTestFailed(c) ? `Last test failed (${c.last_error_category}), ${when}` : `Last test passed, ${when}`;
}
