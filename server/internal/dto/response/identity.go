package response

import "time"

// Authentication, organization and administration responses. None of
// them carries a password hash or a token hash; tokens appear only in the
// responses that issue them.

type TokensResponse struct {
	TokenType        string       `json:"token_type"`
	AccessToken      string       `json:"access_token"`
	AccessExpiresAt  time.Time    `json:"access_expires_at"`
	RefreshToken     string       `json:"refresh_token"`
	RefreshExpiresAt time.Time    `json:"refresh_expires_at"`
	SessionID        string       `json:"session_id"`
	User             UserResponse `json:"user"`
}

type UserResponse struct {
	ID           string     `json:"id"`
	Email        string     `json:"email"`
	DisplayName  string     `json:"display_name,omitempty"`
	Status       string     `json:"status"`
	PlatformRole string     `json:"platform_role,omitempty"`
	LastLoginAt  *time.Time `json:"last_login_at,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
}

type MembershipResponse struct {
	OrganizationID   string    `json:"organization_id"`
	OrganizationName string    `json:"organization_name,omitempty"`
	OrganizationSlug string    `json:"organization_slug,omitempty"`
	UserID           string    `json:"user_id"`
	Email            string    `json:"email,omitempty"`
	DisplayName      string    `json:"display_name,omitempty"`
	RoleID           string    `json:"role_id"`
	Status           string    `json:"status"`
	CreatedAt        time.Time `json:"created_at"`
}

type MeResponse struct {
	User        UserResponse         `json:"user"`
	Memberships []MembershipResponse `json:"memberships"`
}

type SessionResponse struct {
	ID              string    `json:"id"`
	SourceIP        string    `json:"source_ip,omitempty"`
	UserAgent       string    `json:"user_agent,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	LastUsedAt      time.Time `json:"last_used_at"`
	ExpiresAt       time.Time `json:"expires_at"`
	AccessExpiresAt time.Time `json:"access_expires_at"`
}

type OrganizationResponse struct {
	ID                  string     `json:"id"`
	Name                string     `json:"name"`
	Slug                string     `json:"slug"`
	Status              string     `json:"status"`
	AllowedEmailDomains []string   `json:"allowed_email_domains,omitempty"`
	InvitationTTLHours  int        `json:"invitation_ttl_hours,omitempty"`
	CreatedAt           time.Time  `json:"created_at"`
	DeactivatedAt       *time.Time `json:"deactivated_at,omitempty"`
}

type InvitationResponse struct {
	ID        string     `json:"id"`
	Email     string     `json:"email"`
	RoleID    string     `json:"role_id"`
	Status    string     `json:"status"`
	CreatedBy string     `json:"created_by"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt time.Time  `json:"expires_at"`
	Accepted  *time.Time `json:"accepted_at,omitempty"`
	// Token is present only in the response that creates the invitation.
	Token string `json:"token,omitempty"`
}

type RoleResponse struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Permissions []string `json:"permissions"`
	BuiltIn     bool     `json:"built_in"`
}

// CredentialTokenResponse carries an activation or reset token, once.
type CredentialTokenResponse struct {
	UserID    string    `json:"user_id"`
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

type CreatedUserResponse struct {
	User       UserResponse            `json:"user"`
	Activation CredentialTokenResponse `json:"activation"`
}

type QuotaResponse struct {
	Limits map[string]int64 `json:"limits"`
	Usage  map[string]int64 `json:"usage"`
}

type AuditEventResponse struct {
	ID            string         `json:"id"`
	Seq           int64          `json:"seq"`
	TenantID      string         `json:"tenant_id,omitempty"`
	ActorType     string         `json:"actor_type"`
	ActorID       string         `json:"actor_id"`
	Action        string         `json:"action"`
	ResourceType  string         `json:"resource_type"`
	ResourceID    string         `json:"resource_id,omitempty"`
	Outcome       string         `json:"outcome"`
	Reason        string         `json:"reason,omitempty"`
	CorrelationID string         `json:"correlation_id,omitempty"`
	SourceIP      string         `json:"source_ip,omitempty"`
	Details       map[string]any `json:"details,omitempty"`
	CreatedAt     time.Time      `json:"created_at"`
	Hash          string         `json:"hash"`
}

type AuditPageResponse struct {
	Events     []AuditEventResponse `json:"events"`
	NextBefore int64                `json:"next_before,omitempty"`
}

type AuditVerificationResponse struct {
	Chain    string `json:"chain"`
	Checked  int64  `json:"checked"`
	FirstSeq int64  `json:"first_seq"`
	LastSeq  int64  `json:"last_seq"`
	Intact   bool   `json:"intact"`
	BrokenAt int64  `json:"broken_at,omitempty"`
	Problem  string `json:"problem,omitempty"`
}

type DeadLetterResponse struct {
	ID         string    `json:"id"`
	EventID    string    `json:"event_id"`
	EventType  string    `json:"event_type,omitempty"`
	TenantID   string    `json:"tenant_id,omitempty"`
	Consumer   string    `json:"consumer,omitempty"`
	Status     string    `json:"status"`
	Category   string    `json:"category,omitempty"`
	LastError  string    `json:"last_error,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	ReplayedAs string    `json:"replay_event_id,omitempty"`
	Stale      string    `json:"stale,omitempty"`
}

type ProcessingRecordResponse struct {
	AssetID     string           `json:"asset_id"`
	AssetStatus string           `json:"asset_status"`
	JobState    string           `json:"job_state"`
	JobStage    string           `json:"job_stage"`
	Attempts    int              `json:"attempts"`
	LastError   string           `json:"last_error,omitempty"`
	Quarantine  []map[string]any `json:"quarantine"`
}
