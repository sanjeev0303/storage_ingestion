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

export function describeConnectionTest(c: ConnectionSummary) {
  if (c.id === "platform") return "Managed by the platform, not tested here";
  if (!c.last_tested_at) return "Never tested";
  const when = formatRelative(c.last_tested_at);
  const failed = c.last_error_category && (!c.last_test_ok_at || Date.parse(c.last_tested_at) > Date.parse(c.last_test_ok_at));
  return failed ? `Last test failed (${c.last_error_category}), ${when}` : `Last test passed, ${when}`;
}
