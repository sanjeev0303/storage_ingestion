package request

// Authentication, organization and administration requests.

type LoginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type RefreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

type ChangePasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

// CredentialTokenRequest activates an account or resets its password.
type CredentialTokenRequest struct {
	Token    string `json:"token"`
	Password string `json:"password"`
}

// AcceptInvitationRequest accepts an invitation. Signed-in callers send
// only the token; anyone else also chooses a password for their new
// account.
type AcceptInvitationRequest struct {
	Token       string `json:"token"`
	Password    string `json:"password,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
}

type UpdateOrganizationRequest struct {
	Name                string   `json:"name"`
	AllowedEmailDomains []string `json:"allowed_email_domains,omitempty"`
	InvitationTTLHours  int      `json:"invitation_ttl_hours,omitempty"`
}

type InviteRequest struct {
	Email  string `json:"email"`
	RoleID string `json:"role_id"`
}

type ChangeRoleRequest struct {
	RoleID string `json:"role_id"`
}

type RoleRequest struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Permissions []string `json:"permissions"`
}

type CreateUserRequest struct {
	Email        string `json:"email"`
	DisplayName  string `json:"display_name,omitempty"`
	PlatformRole string `json:"platform_role,omitempty"`
}

type PlatformRoleRequest struct {
	PlatformRole string `json:"platform_role"`
}

type CreateOrganizationRequest struct {
	Name        string `json:"name"`
	Slug        string `json:"slug"`
	OwnerUserID string `json:"owner_user_id"`
}

type OrganizationStatusRequest struct {
	Status string `json:"status"`
}

// QuotaRequest replaces a tenant's quota overrides; omitted or zero
// fields fall back to the defaults.
type QuotaRequest struct {
	MaxStorageBytes      int64 `json:"max_storage_bytes,omitempty"`
	MaxAssets            int64 `json:"max_assets,omitempty"`
	MaxObjectBytes       int64 `json:"max_object_bytes,omitempty"`
	MaxActiveUploads     int   `json:"max_active_uploads,omitempty"`
	MaxActiveMultipart   int   `json:"max_active_multipart,omitempty"`
	MaxActiveJobs        int   `json:"max_active_jobs,omitempty"`
	MaxPendingProcessing int   `json:"max_pending_processing,omitempty"`
	MaxPendingEntries    int64 `json:"max_pending_entries,omitempty"`
	MaxManifestEntries   int64 `json:"max_manifest_entries,omitempty"`
	MaxManifestBytes     int64 `json:"max_manifest_bytes,omitempty"`
	MaxRequestsPerMinute int   `json:"max_requests_per_minute,omitempty"`
}

type ReasonRequest struct {
	Reason string `json:"reason"`
}

// RetentionPolicyRequest sets a retention policy of the caller's
// organization. Durations are Go durations such as 720h.
type RetentionPolicyRequest struct {
	Scope               string `json:"scope"`
	ScopeKey            string `json:"scope_key,omitempty"`
	MinRetention        string `json:"min_retention,omitempty"`
	MaxAge              string `json:"max_age,omitempty"`
	DeletionGrace       string `json:"deletion_grace,omitempty"`
	QuarantineRetention string `json:"quarantine_retention,omitempty"`
	Reason              string `json:"reason"`
}

type LegalHoldRequest struct {
	Scope    string `json:"scope"`
	ScopeKey string `json:"scope_key,omitempty"`
	Reason   string `json:"reason"`
}
