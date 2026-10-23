import type { Invitation, Membership, Organization, Role, User } from "@/lib/api/types/identity";

export type { Invitation, Membership, Organization, Role, User };

export const LIMIT_KEYS = [
  "max_storage_bytes",
  "max_assets",
  "max_object_bytes",
  "max_active_uploads",
  "max_active_multipart",
  "max_active_jobs",
  "max_pending_processing",
  "max_pending_entries",
  "max_manifest_entries",
  "max_manifest_bytes",
  "max_requests_per_minute",
] as const;

export type QuotaLimits = Record<(typeof LIMIT_KEYS)[number], number>;

export type QuotaUsage = {
  reserved_bytes: number;
  reserved_uploads: number;
  stored_bytes: number;
  asset_count: number;
  pending_deletion_bytes: number;
  quarantined_bytes: number;
  incomplete_multipart_bytes: number;
  active_uploads: number;
  active_multipart: number;
  pending_processing: number;
  remaining_storage_bytes: number;
};

export type Quota = { limits: QuotaLimits; usage: QuotaUsage };

export type OrganizationUpdate = {
  name: string;
  allowed_email_domains?: string[];
  invitation_ttl_hours?: number;
};

export type CredentialToken = { user_id: string; token: string; expires_at: string };
