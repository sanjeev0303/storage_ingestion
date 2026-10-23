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

export type EntryState =
  | "INVALID"
  | "PENDING"
  | "SCHEDULED"
  | "UPLOADING"
  | "UPLOADED"
  | "PROCESSING"
  | "PUBLISHED"
  | "FAILED_RETRYABLE"
  | "FAILED"
  | "CANCELLED";

export type LimitedBy =
  | "global_slots"
  | "tenant_slots"
  | "job_slots"
  | "multipart_sessions"
  | "storage_quota"
  | "processing_backlog"
  | "no_pending_entries"
  | "job_not_running";

export type Progress = {
  total: number;
  valid: number;
  invalid: number;
  pending: number;
  scheduled: number;
  uploading: number;
  uploaded: number;
  processing: number;
  published: number;
  failed_retryable: number;
  failed: number;
  cancelled: number;
  required_failed: number;
  expected_bytes: number;
  stored_bytes: number;
  percent_published: number;
};

export type IngestionJob = {
  id: string;
  dataset_id: string;
  state: JobState;
  max_concurrency: number;
  manifest_source?: "INLINE" | "STAGED";
  manifest_bytes: number;
  manifest_sha256?: string;
  manifest_complete: boolean;
  parsed_lines: number;
  progress: Progress;
  error?: string;
  created_at: string;
  updated_at: string;
  finalized_at?: string;
  completed_at?: string;
  cancelled_at?: string;
};

export type IngestionEntry = {
  position: number;
  path?: string;
  size_bytes: number;
  checksum_sha256?: string;
  content_type?: string;
  metadata?: Record<string, string>;
  required: boolean;
  strategy?: "SINGLE" | "MULTIPART";
  state: EntryState;
  upload_id?: string;
  asset_id?: string;
  attempts: number;
  error_code?: string;
  error_detail?: string;
  lease_expires_at?: string;
  next_attempt_at?: string;
  updated_at: string;
};

export type Grant = { entry: IngestionEntry; upload_url?: string; url_expires_at?: string };

export type ClaimResponse = {
  grants: Grant[] | null;
  limited_by?: LimitedBy;
  retry_after_seconds?: number;
  failed?: number;
};

export type PartUrl = { part_number: number; url: string; expires_at: string };
export type PartRecord = { part_number: number; etag: string; size_bytes: number };

export type ManifestReport = {
  lines: number;
  accepted: number;
  invalid: number;
  duplicates: number;
  replayed: number;
  complete: boolean;
  sha256?: string;
  invalid_samples?: IngestionEntry[];
};

export type Dataset = { id: string; name: string; completion_policy?: string; status?: string; storage_connection_id?: string };

export type JobsPage = { jobs: IngestionJob[]; next_after?: string };
export type EntriesPage = { entries: IngestionEntry[]; next_after?: number };
