// Only the fields the overview reads. Full shapes live in docs/api.

export type JobState =
  | "PENDING_VALIDATION"
  | "VALIDATING"
  | "READY"
  | "RUNNING"
  | "PARTIALLY_COMPLETED"
  | "COMPLETED"
  | "FAILED"
  | "CANCELLING"
  | "CANCELLED";

export type JobSummary = {
  id: string;
  dataset_id: string;
  state: JobState;
  error?: string;
  progress: { total: number; published: number; failed: number; invalid: number };
  updated_at: string;
};

export type DatasetSummary = {
  id: string;
  name: string;
  lifecycle_status: string;
  updated_at: string;
};

export type ConnectionSummary = {
  id: string;
  name: string;
  provider: string;
  status: string;
  is_default: boolean;
  last_tested_at?: string;
  last_test_ok_at?: string;
  last_error_category?: string;
};

export type AuditEventSummary = {
  id: string;
  seq: number;
  actor_id: string;
  action: string;
  outcome: "SUCCESS" | "DENIED" | "FAILED";
  resource_type: string;
  created_at: string;
};

export type DeadLetterSummary = {
  id: string;
  event_type?: string;
  category?: string;
  last_error?: string;
  created_at: string;
};
