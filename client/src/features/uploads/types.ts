export type UploadStatus =
  | "PENDING"
  | "UPLOADING"
  | "COMPLETING"
  | "COMPLETED"
  | "VERIFIED"
  | "ABORTING"
  | "ABORTED"
  | "FAILED"
  | "EXPIRED";

export type UploadRequest = {
  file_name: string;
  content_type: string;
  dataset?: string;
  path?: string;
  metadata?: Record<string, string>;
  create_only?: boolean;
  storage_connection_id?: string;
};

export type SingleUploadResponse = {
  upload_id: string;
  status: UploadStatus;
  upload_url: string;
  expires_at: string;
  upload_headers?: Record<string, string>;
  storage_connection_id: string;
};

export type MultipartStartResponse = {
  upload_id: string;
  status: UploadStatus;
  expires_at: string;
  reused: boolean;
  storage_connection_id: string;
};

export type PartUrlsResponse = {
  upload_id: string;
  parts: { part_number: number; url: string; expires_at: string }[];
};

export type UploadInfo = {
  upload_id: string;
  status: UploadStatus;
  file_name: string;
  size_bytes: number;
  expires_at: string;
  multipart: boolean;
  // What processing made of the upload, once there is an asset. A rejected
  // asset never reaches the catalog, so this is how the screen learns of it.
  asset?: { asset_id: string; status: AssetStatus; rejection_reason?: string; object_id?: string; version?: number };
};

export type AssetStatus = "QUARANTINED" | "PROCESSING" | "AVAILABLE" | "REJECTED" | "SOFT_DELETED" | "DELETED";

export type CatalogObject = {
  id: string;
  dataset: string;
  path: string;
  versions: number;
};

export type ObjectVersion = {
  version: number;
  asset_id: string;
  status: AssetStatus;
  checksum_sha256?: string;
  size_bytes: number;
};

export type QuotaResponse = {
  limits: Record<string, number>;
  usage: Record<string, number>;
};

export type StoredPart = { number: number; etag: string; size: number };

// What is kept in IndexedDB so a multipart upload can continue after a reload.
// It holds ids, sizes and ETags. It never holds a presigned URL or any secret.
export type ResumeRecord = {
  upload_id: string;
  organization_id: string;
  idempotency_key: string;
  request: UploadRequest;
  file_size: number;
  file_last_modified: number;
  sha256: string;
  part_size: number;
  parts: StoredPart[];
  expires_at: string;
  saved_at: string;
};

export type UploadState =
  | "queued"
  | "hashing"
  | "uploading"
  | "finalizing"
  | "verifying"
  | "processing"
  | "available"
  | "rejected"
  | "failed"
  | "cancelled";

export type FailureKind =
  | "session_expired"
  | "too_large"
  | "quota"
  | "size_mismatch"
  | "cors"
  | "forbidden"
  | "other";

export type UploadSnapshot = {
  id: string;
  fileName: string;
  fileSize: number;
  request: UploadRequest;
  state: UploadState;
  mode?: "single" | "multipart";
  hashedBytes: number;
  sentBytes: number;
  speedBps: number;
  partsDone: number;
  partsTotal: number;
  uploadId?: string;
  storageConnectionId?: string;
  note?: string;
  failure?: { kind: FailureKind; message: string };
  asset?: { objectId?: string; status?: AssetStatus; rejectionReason?: string };
  // Status polling gave up after its limit; "Check again" resumes it.
  pollingStopped: boolean;
  resumed: boolean;
};
