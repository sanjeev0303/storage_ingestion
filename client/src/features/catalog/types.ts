export type AssetStatus = "QUARANTINED" | "PROCESSING" | "AVAILABLE" | "REJECTED" | "SOFT_DELETED" | "DELETED";

export type CurrentVersion = {
  version: number;
  asset_id: string;
  size_bytes: number;
  content_type: string;
  checksum_sha256?: string;
  published_at?: string;
  storage_connection_id?: string;
};

export type CatalogObject = {
  id: string;
  dataset: string;
  path: string;
  versioning: "ENABLED" | "DISABLED";
  revision: number;
  metadata: Record<string, string>;
  current_version?: CurrentVersion;
  versions: number;
  deleted_at?: string;
  created_at: string;
  updated_at: string;
};

export type ObjectVersion = {
  version: number;
  asset_id: string;
  status: AssetStatus;
  current: boolean;
  file_name: string;
  size_bytes: number;
  content_type: string;
  checksum_algorithm?: "sha256";
  checksum_sha256?: string;
  provider_etag?: string;
  provider_version_id?: string;
  metadata?: Record<string, string>;
  published_at?: string;
  created_at: string;
};

export type CatalogPage = { objects: CatalogObject[]; next_cursor?: string };
export type VersionsPage = { versions: ObjectVersion[]; next_before?: number };
export type DeleteObjectResponse = { object: CatalogObject; versions_in_processing: number };

export type CatalogCapabilities = {
  logical_versioning: boolean;
  provider_versioning: boolean;
  provider_versioning_status?: string;
};

export type DeletionState = "SCHEDULED" | "IN_PROGRESS" | "RETRY_WAIT" | "STORAGE_DELETED" | "FAILED" | "COMPLETED" | "CANCELLED";

export type AssetLifecycle = {
  asset_id: string;
  status: AssetStatus;
  dataset?: string;
  retain_until: string;
  expires_at?: string;
  deletion_grace_seconds: number;
  deletion?: { id: string; state: DeletionState; purge_after: string; restorable: boolean };
};

export type DownloadMode = "storage" | "token";

export type DownloadUrl = {
  url: string;
  expires_at: string;
  delivery: "storage" | "token" | "cdn";
  revocable: boolean;
};

export type ConnectionName = { id: string; name: string };
