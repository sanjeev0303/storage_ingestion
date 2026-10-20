package model

import (
	"errors"
	"slices"
	"strings"
	"time"
)

// Identity, tenancy and authorization. A user signs in and gets a session;
// a request names an organization (the tenant) and is allowed what the
// user's role in it permits. Platform roles are separate: they manage
// organizations and accounts, not tenant data.

var (
	ErrForbidden          = errors.New("permission denied")
	ErrInvalidCredentials = errors.New("invalid email or password")
	ErrAccountInactive    = errors.New("the account is not active")
	ErrSessionInvalid     = errors.New("the session is expired or revoked")
	ErrRefreshReplayed    = errors.New("refresh token reuse detected; the session is revoked")
	ErrCredentialToken    = errors.New("the token is invalid, used or expired")
	ErrWeakPassword       = errors.New("password does not meet the requirements")
	ErrEmailTaken         = errors.New("an account with this email exists")
	ErrUserNotFound       = errors.New("user not found")
	ErrOrgNotFound        = errors.New("organization not found")
	ErrOrgInactive        = errors.New("the organization is not active")
	ErrOrgNotEmpty        = errors.New("the organization still owns data that must be kept")
	ErrOrgState           = errors.New("the organization's status does not allow this")
	ErrOrgExists          = errors.New("an organization with this slug exists")
	ErrRoleExists         = errors.New("a role with this name exists")
	ErrMembershipNotFound = errors.New("membership not found")
	ErrMembershipExists   = errors.New("the user is already a member")
	ErrInvitationNotFound = errors.New("invitation not found")
	ErrRoleNotFound       = errors.New("role not found")
	ErrRoleInUse          = errors.New("the role is assigned to members")
	ErrLastOwner          = errors.New("an organization must keep at least one active owner")
	ErrEscalation         = errors.New("cannot grant or change permissions beyond your own")
	ErrTenantRequired     = errors.New("select an organization for this request")
)

type UserStatus string

const (
	// UserPending: created by an administrator or an invitation, waiting
	// for its owner to set a password.
	UserPending     UserStatus = "PENDING"
	UserActive      UserStatus = "ACTIVE"
	UserDeactivated UserStatus = "DEACTIVATED"
)

// PlatformRole is a role across all organizations.
type PlatformRole string

const (
	PlatformAdmin   PlatformRole = "ADMIN"
	PlatformAuditor PlatformRole = "AUDITOR"
)

// User is an account. Its password hash never leaves the repository and
// service layers.
type User struct {
	ID                string
	Email             string
	DisplayName       string
	PasswordHash      string
	Status            UserStatus
	PlatformRole      PlatformRole
	PasswordChangedAt *time.Time
	LastLoginAt       *time.Time
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// Session is one sign-in. Its tokens are stored only as hashes.
type Session struct {
	ID              string
	UserID          string
	AccessExpiresAt time.Time
	ExpiresAt       time.Time
	SourceIP        string
	UserAgent       string
	CreatedAt       time.Time
	LastUsedAt      time.Time
	RevokedAt       *time.Time
	RevokeReason    string
}

// Credential token purposes.
const (
	CredentialActivate = "ACTIVATE"
	CredentialReset    = "RESET"
)

type OrgStatus string

const (
	OrgActive      OrgStatus = "ACTIVE"
	OrgSuspended   OrgStatus = "SUSPENDED"
	OrgDeactivated OrgStatus = "DEACTIVATED"
	OrgDeleted     OrgStatus = "DELETED"
)

// OrgSettings are an organization's own settings.
type OrgSettings struct {
	// AllowedEmailDomains, when set, limits invitations to these domains.
	AllowedEmailDomains []string `json:"allowed_email_domains,omitempty"`
	// InvitationTTLHours overrides the configured invitation lifetime.
	InvitationTTLHours int `json:"invitation_ttl_hours,omitempty"`
}

// EmailAllowed reports whether an invitation to email is allowed.
func (s OrgSettings) EmailAllowed(email string) bool {
	if len(s.AllowedEmailDomains) == 0 {
		return true
	}
	_, domain, ok := strings.Cut(strings.ToLower(email), "@")
	return ok && slices.Contains(s.AllowedEmailDomains, domain)
}

// Organization is a tenant: its ID is the tenant ID of everything it owns.
type Organization struct {
	ID            string
	Name          string
	Slug          string
	Status        OrgStatus
	Settings      OrgSettings
	CreatedBy     string
	CreatedAt     time.Time
	UpdatedAt     time.Time
	DeactivatedAt *time.Time
}

type MembershipStatus string

const (
	MembershipActive    MembershipStatus = "ACTIVE"
	MembershipSuspended MembershipStatus = "SUSPENDED"
)

// Membership is a user's role in an organization.
type Membership struct {
	OrgID     string
	UserID    string
	RoleID    string
	Status    MembershipStatus
	CreatedBy string
	CreatedAt time.Time
	UpdatedAt time.Time
	// Email and DisplayName are the member's, for listings.
	Email       string
	DisplayName string
}

type InvitationStatus string

const (
	InvitationPending  InvitationStatus = "PENDING"
	InvitationAccepted InvitationStatus = "ACCEPTED"
	InvitationRevoked  InvitationStatus = "REVOKED"
)

// Invitation offers a role in an organization to an email address. Its
// token is stored only as a hash.
type Invitation struct {
	ID         string
	OrgID      string
	Email      string
	RoleID     string
	Status     InvitationStatus
	CreatedBy  string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	AcceptedAt *time.Time
	AcceptedBy string
}

// ─── Permissions ──────────────────────────────────────────────────────

// Permission is a set of tenant-scoped capabilities, one bit each. A role
// is a Permission; checks ask for permissions, never for role names.
type Permission uint64

const (
	PermOrgRead Permission = 1 << iota
	PermOrgManage
	PermMembersRead
	PermMembersManage
	PermRolesManage
	PermDatasetsRead
	PermDatasetsWrite
	PermIngestionRead
	PermIngestionWrite
	PermIngestionManage
	PermUploadsWrite
	PermAssetsRead
	PermAssetsDownload
	PermAssetsDelete
	PermQuarantineRead
	PermQuarantineRelease
	PermRetentionManage
	PermQuotaRead
	PermAuditRead
	// PermCatalogWrite changes catalog objects: user metadata, names,
	// versioning and the current version.
	PermCatalogWrite
	// PermDatasetsManage archives datasets and manages validation
	// profiles. Reading, building, snapshotting and validating datasets
	// need datasets.read and datasets.write.
	PermDatasetsManage
	// PermOwnership grants or takes the owner role and deactivates the
	// organization. Only owners have it; custom roles cannot.
	PermOwnership
)

// permissionNames are the stable names used by the API and the database.
var permissionNames = []struct {
	perm Permission
	name string
}{
	{PermOrgRead, "org.read"},
	{PermOrgManage, "org.manage"},
	{PermMembersRead, "members.read"},
	{PermMembersManage, "members.manage"},
	{PermRolesManage, "roles.manage"},
	{PermDatasetsRead, "datasets.read"},
	{PermDatasetsWrite, "datasets.write"},
	{PermIngestionRead, "ingestion.read"},
	{PermIngestionWrite, "ingestion.write"},
	{PermIngestionManage, "ingestion.manage"},
	{PermUploadsWrite, "uploads.write"},
	{PermAssetsRead, "assets.read"},
	{PermAssetsDownload, "assets.download"},
	{PermAssetsDelete, "assets.delete"},
	{PermQuarantineRead, "quarantine.read"},
	{PermQuarantineRelease, "quarantine.release"},
	{PermRetentionManage, "retention.manage"},
	{PermQuotaRead, "quota.read"},
	{PermAuditRead, "audit.read"},
	{PermCatalogWrite, "catalog.write"},
	{PermDatasetsManage, "datasets.manage"},
	{PermOwnership, "org.ownership"},
}

// AllPermissions is every tenant permission.
var AllPermissions = func() Permission {
	var all Permission
	for _, p := range permissionNames {
		all |= p.perm
	}
	return all
}()

// Has reports whether every permission of want is in p.
func (p Permission) Has(want Permission) bool { return want != 0 && p&want == want }

// Names lists p's permissions by name.
func (p Permission) Names() []string {
	names := []string{}
	for _, n := range permissionNames {
		if p&n.perm != 0 {
			names = append(names, n.name)
		}
	}
	return names
}

// ParsePermissions reads permission names. Unknown names are an error.
func ParsePermissions(names []string) (Permission, error) {
	var p Permission
	for _, name := range names {
		found := false
		for _, n := range permissionNames {
			if n.name == name {
				p |= n.perm
				found = true
				break
			}
		}
		if !found {
			return 0, errors.New("unknown permission " + name)
		}
	}
	return p, nil
}

// Role is a named set of permissions: one of the built-in roles, or an
// organization's own.
type Role struct {
	ID          string
	OrgID       string
	Name        string
	Description string
	Permissions Permission
	BuiltIn     bool
	CreatedBy   string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Built-in role IDs.
const (
	RoleOwner     = "owner"
	RoleAdmin     = "admin"
	RoleDeveloper = "developer"
	RoleAuditor   = "auditor"
)

var builtInRoles = []Role{
	{ID: RoleOwner, Name: "Owner", BuiltIn: true, Permissions: AllPermissions,
		Description: "Everything, including granting ownership and deactivating the organization"},
	{ID: RoleAdmin, Name: "Administrator", BuiltIn: true, Permissions: AllPermissions &^ PermOwnership,
		Description: "Manages members, roles, settings and all data; cannot grant ownership"},
	{ID: RoleDeveloper, Name: "Developer", BuiltIn: true,
		Permissions: PermOrgRead | PermMembersRead | PermDatasetsRead | PermDatasetsWrite | PermIngestionRead |
			PermIngestionWrite | PermIngestionManage | PermUploadsWrite | PermAssetsRead | PermAssetsDownload |
			PermAssetsDelete | PermQuarantineRead | PermQuotaRead | PermCatalogWrite,
		Description: "Uploads, ingests, reads and deletes data"},
	{ID: RoleAuditor, Name: "Auditor", BuiltIn: true,
		Permissions: PermOrgRead | PermMembersRead | PermDatasetsRead | PermIngestionRead | PermAssetsRead |
			PermQuarantineRead | PermQuotaRead | PermAuditRead,
		Description: "Reads metadata, usage and the audit log; cannot download or change anything"},
}

// BuiltInRoles lists the built-in roles.
func BuiltInRoles() []Role { return slices.Clone(builtInRoles) }

// BuiltInRole returns the built-in role with this ID.
func BuiltInRole(id string) (Role, bool) {
	for _, r := range builtInRoles {
		if r.ID == id {
			return r, true
		}
	}
	return Role{}, false
}

// Platform permissions are separate from tenant permissions.
type PlatformPermission int

const (
	// PlatformManage: organizations, accounts, quotas, dead letters and
	// cross-tenant reconciliation.
	PlatformManage PlatformPermission = iota + 1
	// PlatformAuditRead: the audit log of any organization and the
	// platform's own.
	PlatformAuditRead
)

// Can reports whether the platform role has the permission.
func (r PlatformRole) Can(p PlatformPermission) bool {
	switch r {
	case PlatformAdmin:
		return true
	case PlatformAuditor:
		return p == PlatformAuditRead
	}
	return false
}

// ─── Audit ────────────────────────────────────────────────────────────

// Audit outcomes and actor types.
const (
	AuditSuccess = "SUCCESS"
	AuditDenied  = "DENIED"
	AuditFailed  = "FAILED"

	ActorUser      = "USER"
	ActorService   = "SERVICE"
	ActorAnonymous = "ANONYMOUS"
)

// AuditEvent is one entry of the security audit log. Details never hold
// credentials, tokens, signed URLs or file content.
type AuditEvent struct {
	ID            string
	TenantID      string
	ActorType     string
	ActorID       string
	Action        string
	ResourceType  string
	ResourceID    string
	Outcome       string
	Reason        string
	CorrelationID string
	SourceIP      string
	Details       map[string]any
	CreatedAt     time.Time
	// Chain, Seq, PrevHash and Hash place the event in its hash chain.
	Chain    string
	Seq      int64
	PrevHash []byte
	Hash     []byte
}

// PlatformChain is the audit chain of events that belong to no tenant.
const PlatformChain = "_platform"

// ErrRateLimited is matched by every RateLimitError.
var ErrRateLimited = errors.New("too many requests")

// RateLimitError is a refused request and when to try again.
type RateLimitError struct {
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string        { return ErrRateLimited.Error() }
func (e *RateLimitError) Is(target error) bool { return target == ErrRateLimited }
