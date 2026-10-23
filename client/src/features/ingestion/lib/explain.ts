import type { EntryState, JobState } from "../types";

export const ERROR_EXPLANATIONS: Record<string, { text: string; retryable: boolean }> = {
  invalid_json: { text: "The manifest line is not valid JSON. Fix the line and submit a new job.", retryable: false },
  line_too_long: { text: "The manifest line is longer than 16,384 bytes.", retryable: false },
  invalid_path: { text: "The path is empty, absolute, or uses .., backslashes or reserved characters.", retryable: false },
  duplicate_path: { text: "Another live entry in this dataset already uses this path (compared without case).", retryable: false },
  invalid_size: { text: 'The "size" is missing or not a whole number of bytes in the allowed range.', retryable: false },
  too_large: { text: "The file is larger than the per-object limit.", retryable: false },
  invalid_checksum: { text: 'The checksum is not "sha256:" followed by 64 hex characters.', retryable: false },
  checksum_required: { text: "The line has no checksum, so the file could never be verified and published.", retryable: false },
  invalid_content_type: { text: "The content type is missing or is not a valid media type.", retryable: false },
  invalid_metadata: { text: "Metadata must be a flat object of short string values.", retryable: false },
  upload_abandoned: { text: "The upload stopped before it finished and its lease ran out. Retry to upload it again.", retryable: true },
  object_missing: { text: "The storage provider has no object for this entry. Retry to upload it again.", retryable: true },
  size_mismatch: { text: "The stored bytes differ from the size in the manifest. Check the local file, then retry.", retryable: true },
  upload_failed: { text: "The upload failed. Retry to upload it again.", retryable: true },
  storage_unavailable: { text: "The storage provider could not be reached. Retry later.", retryable: true },
  content_rejected: { text: "Content checks rejected the file (for example the checksum or a scan). Retrying will not help.", retryable: false },
  processing_exhausted: { text: "Processing ran out of attempts, for example during a scanner outage. Retry requeues processing.", retryable: true },
  attempts_exhausted: { text: "The entry used all its automatic attempts. A manual retry is allowed.", retryable: true },
  cancelled: { text: "The job was cancelled before this entry was uploaded.", retryable: false },
  quota_exceeded: { text: "The organization or dataset quota had no room for this file. Free space or raise the quota, then retry.", retryable: true },
};

export function explainError(code: string | undefined) {
  if (!code) return "No reason was recorded.";
  return ERROR_EXPLANATIONS[code]?.text ?? "An error code this console does not know yet.";
}

const JOB_LABELS: Record<JobState, string> = {
  PENDING_VALIDATION: "Waiting for manifest",
  VALIDATING: "Validating",
  READY: "Ready to upload",
  RUNNING: "Running",
  PARTIALLY_COMPLETED: "Partly completed",
  COMPLETED: "Completed",
  FAILED: "Failed",
  CANCELLING: "Cancelling",
  CANCELLED: "Cancelled",
};

export const jobLabel = (state: JobState) => JOB_LABELS[state] ?? state;

export const TERMINAL_JOB_STATES: JobState[] = ["COMPLETED", "PARTIALLY_COMPLETED", "FAILED", "CANCELLED"];
export const isTerminal = (state: JobState) => TERMINAL_JOB_STATES.includes(state);

export function jobTone(state: JobState) {
  if (state === "COMPLETED") return "done" as const;
  if (state === "FAILED" || state === "PARTIALLY_COMPLETED") return "attention" as const;
  if (state === "RUNNING" || state === "CANCELLING" || state === "READY" || state === "VALIDATING") return "active" as const;
  return "neutral" as const;
}

export function entryTone(state: EntryState) {
  if (state === "PUBLISHED") return "done" as const;
  if (state === "FAILED" || state === "FAILED_RETRYABLE" || state === "INVALID") return "attention" as const;
  if (state === "PENDING" || state === "CANCELLED") return "neutral" as const;
  return "active" as const;
}

export const entryLabel = (state: EntryState) => state.charAt(0) + state.slice(1).toLowerCase().replace(/_/g, " ");

export const ENTRY_STATES: EntryState[] = [
  "PENDING",
  "SCHEDULED",
  "UPLOADING",
  "UPLOADED",
  "PROCESSING",
  "PUBLISHED",
  "FAILED_RETRYABLE",
  "FAILED",
  "INVALID",
  "CANCELLED",
];

export const LIMITED_BY_TEXT: Record<string, string> = {
  global_slots: "All upload slots on the platform are in use.",
  tenant_slots: "Your organization is using all its upload slots.",
  job_slots: "This job is using all its upload slots.",
  multipart_sessions: "Your organization has the maximum number of large uploads open.",
  storage_quota: "The storage quota has no room for the next file.",
  processing_backlog: "Uploaded files are waiting to be checked. Uploads resume as the backlog clears.",
  no_pending_entries: "No entries are waiting to be uploaded.",
  job_not_running: "The job is not accepting uploads.",
};
