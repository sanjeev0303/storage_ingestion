// Mirrors server/internal/dto/response/identity.go. Keys are snake_case as sent.

export type UserStatus = "PENDING" | "ACTIVE" | "DEACTIVATED";
export type PlatformRole = "ADMIN" | "AUDITOR";
export type MembershipStatus = "ACTIVE" | "SUSPENDED";
export type OrganizationStatus = "ACTIVE" | "SUSPENDED" | "DEACTIVATED" | "DELETED";

export type User = {
  id: string;
  email: string;
  display_name?: string;
  status: UserStatus;
  platform_role?: PlatformRole;
  last_login_at?: string;
  created_at: string;
};

export type Membership = {
  organization_id: string;
  organization_name?: string;
  organization_slug?: string;
  user_id: string;
  email?: string;
  display_name?: string;
  role_id: string;
  status: MembershipStatus;
  created_at: string;
};

export type Me = { user: User; memberships: Membership[] };

export type Session = {
  id: string;
  source_ip?: string;
  user_agent?: string;
  created_at: string;
  last_used_at: string;
  expires_at: string;
  access_expires_at: string;
};

export type Organization = {
  id: string;
  name: string;
  slug: string;
  status: OrganizationStatus;
  allowed_email_domains?: string[];
  invitation_ttl_hours?: number;
  created_at: string;
  deactivated_at?: string;
};

export type Invitation = {
  id: string;
  email: string;
  role_id: string;
  status: "PENDING" | "ACCEPTED" | "REVOKED";
  created_by: string;
  created_at: string;
  expires_at: string;
  accepted_at?: string;
  // Only present on the create response, shown once.
  token?: string;
};

export type Role = {
  id: string;
  name: string;
  description?: string;
  permissions: string[];
  built_in: boolean;
};
