export type ProviderType =
  | "PLATFORM_MANAGED"
  | "AWS_S3"
  | "S3_COMPATIBLE"
  | "CLOUDFLARE_R2"
  | "GOOGLE_CLOUD_STORAGE"
  | "AZURE_BLOB";

export type AuthMethod = "PLATFORM" | "ACCESS_KEY" | "ASSUME_ROLE" | "SERVICE_ACCOUNT_KEY" | "ACCOUNT_KEY" | "SAS_TOKEN";
export type ConnectionStatus = "PENDING" | "ACTIVE" | "DISABLED" | "DELETED";
export type StagingMode = "CONNECTION" | "PLATFORM";

export type TestCategory =
  | "ACCESS_DENIED"
  | "NOT_FOUND"
  | "THROTTLED"
  | "NETWORK"
  | "ENDPOINT_REJECTED"
  | "TLS"
  | "INVALID_CREDENTIALS"
  | "UNSUPPORTED"
  | "PROVIDER_ERROR";

export type Capabilities = {
  presigned_upload: boolean;
  max_single_upload_bytes: number;
  multipart: boolean;
  multipart_kind?: "S3" | "GCS_COMPOSE" | "AZURE_BLOCKS";
  max_parts?: number;
  min_part_bytes?: number;
  part_validator?: "CONTENT" | "BLOCK_ID";
  list_incomplete_uploads: boolean;
  server_side_copy: boolean;
  range_reads: boolean;
  conditional_reads: boolean;
  presigned_download: boolean;
  list_objects: boolean;
  provider_versioning: boolean;
  versioning_status?: string;
  version_id_kind?: "S3_VERSION_ID" | "GCS_GENERATION" | "AZURE_VERSION_ID";
  checksum_algorithms: string[];
};

export type StorageConnection = {
  id: string;
  name: string;
  provider: ProviderType;
  status: ConnectionStatus;
  auth_method: AuthMethod;
  bucket?: string;
  region?: string;
  endpoint?: string;
  account_id?: string;
  force_path_style: boolean;
  role_arn?: string;
  external_id?: string;
  staging_prefix?: string;
  ready_prefix?: string;
  staging_mode?: StagingMode;
  is_default: boolean;
  has_credentials: boolean;
  credential_rotation: number;
  credential_rotated_at?: string;
  capabilities?: Capabilities;
  capabilities_checked_at?: string;
  last_tested_at?: string;
  last_test_ok_at?: string;
  last_error_category?: TestCategory;
  // How request checksums are sent, and the account or project number the
  // bucket must belong to (docs/storage-posture.md).
  checksum_mode?: ChecksumMode;
  expected_owner?: string;
  // The customer-managed key every write is encrypted with (docs/residency.md).
  encryption_key?: string;
  staged_credential?: { rotation: number; staged_at?: string; staged_by?: string };
  previous_credential_until?: string;
  revision: number;
  created_by?: string;
  updated_by?: string;
  created_at: string;
  updated_at: string;
  disabled_at?: string;
  deleted_at?: string;
};

export type ChecksumMode = "AUTO" | "WHEN_SUPPORTED" | "WHEN_REQUIRED";

export type PostureStatus = "PASS" | "WARN" | "FAIL" | "NOT_CHECKED";

export type PostureCheck = { name: string; status: PostureStatus; detail: string };

export type PostureReport = {
  connection_id: string;
  checked_at: string;
  worst: "PASS" | "WARN" | "FAIL";
  phi_safe: boolean;
  checks: PostureCheck[];
  owner_account?: string;
  location?: string;
  location_verified?: boolean;
  encryption_key?: string;
};

export type DriftKind = "OVERWRITTEN" | "DELETED" | "NEW_VERSION" | "UNREADABLE" | "UNKNOWN_OBJECT";

export type DriftFinding = {
  id: string;
  connection_id: string;
  asset_id?: string;
  object_key: string;
  kind: DriftKind;
  detail: string;
  detected_at: string;
  resolved_at?: string;
  resolved_by?: string;
};

export type ConnectionList = {
  connections: StorageConnection[];
  default_connection_id: string;
};

export type ProviderInfo = {
  provider: ProviderType;
  name: string;
  auth_methods: { method: AuthMethod; secret_fields: string[] }[];
  required_fields: string[];
  notes: string[];
};

export type ProviderCatalog = {
  providers: ProviderInfo[];
  byos_enabled: boolean;
};

export type CapabilityMatrix = {
  providers: { provider: ProviderType; capabilities: Capabilities }[] | null;
};

export type TestCheckName = "endpoint" | "credentials" | "bucket" | "list" | "existing_object" | "write_probe";

export type TestResponse = {
  ok: boolean;
  category?: TestCategory;
  message?: string;
  checks: { name: TestCheckName; ok: boolean; category?: TestCategory }[];
  capabilities?: Capabilities;
  tested_at: string;
  connection?: StorageConnection;
  posture?: PostureReport;
};

export type StorageCredentials = {
  access_key_id?: string;
  secret_access_key?: string;
  session_token?: string;
  service_account_json?: string;
  account_key?: string;
  sas_token?: string;
};

export type CreateConnectionRequest = {
  name: string;
  provider: ProviderType;
  auth_method: AuthMethod;
  bucket: string;
  region?: string;
  endpoint?: string;
  account_id?: string;
  force_path_style?: boolean;
  role_arn?: string;
  staging_prefix?: string;
  ready_prefix?: string;
  staging_mode?: StagingMode;
  credentials?: StorageCredentials;
};

export type UpdateConnectionRequest = {
  revision: number;
  name?: string;
  region?: string;
  endpoint?: string;
  force_path_style?: boolean;
  staging_mode?: StagingMode;
  checksum_mode?: ChecksumMode;
  expected_owner?: string;
  encryption_key?: string;
};
