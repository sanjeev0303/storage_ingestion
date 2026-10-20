package interfaces

import (
	"context"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// IdentityRepository stores accounts, sessions and single-use credential
// tokens. Tokens arrive as SHA-256 hashes; nothing here ever sees one in
// clear. Every method that changes security state takes the audit events
// to write in the same transaction.
type IdentityRepository interface {
	CreateUser(ctx context.Context, user model.User, audit ...model.AuditEvent) error
	GetUser(ctx context.Context, userID string) (model.User, error)
	GetUserByEmail(ctx context.Context, email string) (model.User, error)
	ListUsers(ctx context.Context, after string, limit int) ([]model.User, error)
	CountPlatformAdmins(ctx context.Context) (int, error)
	// SetUserStatus changes an account's status. Deactivating it revokes
	// all its sessions in the same transaction.
	SetUserStatus(ctx context.Context, userID string, status model.UserStatus, now time.Time, audit ...model.AuditEvent) (model.User, error)
	SetPlatformRole(ctx context.Context, userID string, role model.PlatformRole, now time.Time, audit ...model.AuditEvent) error
	// ChangePassword stores a new hash and revokes every other session of
	// the account.
	ChangePassword(ctx context.Context, userID, passwordHash, keepSessionID string, now time.Time, audit ...model.AuditEvent) error
	// RehashPassword replaces a hash with a stronger one of the same
	// password; nothing else changes.
	RehashPassword(ctx context.Context, userID, passwordHash string) error
	RecordLogin(ctx context.Context, userID string, now time.Time) error

	CreateSession(ctx context.Context, session model.Session, accessHash, refreshHash []byte, audit ...model.AuditEvent) error
	// SessionByAccess returns the live session of an access token and its
	// account. model.ErrSessionInvalid when the token is unknown, expired
	// or revoked.
	SessionByAccess(ctx context.Context, accessHash []byte, now time.Time) (model.Session, model.User, error)
	// RotateRefresh spends a refresh token and gives the session new
	// tokens. A token spent before revokes its session:
	// model.ErrRefreshReplayed.
	RotateRefresh(ctx context.Context, refreshHash, newAccessHash, newRefreshHash []byte, accessExpiresAt, now time.Time) (model.Session, model.User, error)
	RevokeSession(ctx context.Context, userID, sessionID, reason string, now time.Time, audit ...model.AuditEvent) error
	ListSessions(ctx context.Context, userID string, now time.Time) ([]model.Session, error)

	CreateCredentialToken(ctx context.Context, tokenHash []byte, userID, purpose, createdBy string, expiresAt, now time.Time, audit ...model.AuditEvent) error
	// ConsumeCredentialToken spends an activation or reset token: the
	// account gets the new password hash (and becomes ACTIVE when it was
	// PENDING), and all its sessions are revoked.
	ConsumeCredentialToken(ctx context.Context, tokenHash []byte, purpose, passwordHash string, now time.Time, audit func(model.User) model.AuditEvent) (model.User, error)
}

// TenancyRepository stores organizations, memberships, custom roles and
// invitations.
type TenancyRepository interface {
	// CreateOrganization stores org with its first owner.
	CreateOrganization(ctx context.Context, org model.Organization, owner model.Membership, audit ...model.AuditEvent) error
	GetOrganization(ctx context.Context, orgID string) (model.Organization, error)
	ListOrganizations(ctx context.Context, after string, limit int) ([]model.Organization, error)
	UpdateOrganization(ctx context.Context, org model.Organization, audit ...model.AuditEvent) (model.Organization, error)
	SetOrganizationStatus(ctx context.Context, orgID string, from []model.OrgStatus, to model.OrgStatus, now time.Time, audit ...model.AuditEvent) (model.Organization, error)
	// RetainedData names what an organization still owns that must be
	// kept: live assets, uploads in progress, unfinished jobs, legal holds.
	RetainedData(ctx context.Context, orgID string) ([]string, error)

	// Access returns what a request needs to authorize a user in an
	// organization: the organization, the membership and its role.
	Access(ctx context.Context, orgID, userID string) (model.Organization, model.Membership, model.Role, error)
	UserMemberships(ctx context.Context, userID string) ([]model.Membership, []model.Organization, error)

	ListMembers(ctx context.Context, orgID, after string, limit int) ([]model.Membership, error)
	// ChangeMember applies change under the organization's lock, refusing
	// it with model.ErrLastOwner when it would leave no active owner. The
	// audit event is built from the membership before and after (nil when
	// removed) and written in the same transaction.
	ChangeMember(ctx context.Context, change MemberChange, audit func(before model.Membership, after *model.Membership) model.AuditEvent) (model.Membership, error)

	CreateRole(ctx context.Context, role model.Role, audit ...model.AuditEvent) error
	GetRole(ctx context.Context, orgID, roleID string) (model.Role, error)
	ListRoles(ctx context.Context, orgID string) ([]model.Role, error)
	UpdateRole(ctx context.Context, role model.Role, audit ...model.AuditEvent) (model.Role, error)
	// DeleteRole removes a custom role no member holds: model.ErrRoleInUse
	// otherwise.
	DeleteRole(ctx context.Context, orgID, roleID string, now time.Time, audit ...model.AuditEvent) error

	CreateInvitation(ctx context.Context, inv model.Invitation, tokenHash []byte, audit ...model.AuditEvent) error
	ListInvitations(ctx context.Context, orgID, after string, limit int) ([]model.Invitation, error)
	RevokeInvitation(ctx context.Context, orgID, invitationID string, now time.Time, audit ...model.AuditEvent) error
	// AcceptInvitation spends a pending, unexpired invitation: the account
	// (created if newUser is set) becomes a member with the invited role.
	AcceptInvitation(ctx context.Context, accept InvitationAcceptance, audit func(model.Invitation, model.User) []model.AuditEvent) (model.Membership, model.User, error)
}

// MemberChange is a change to one membership. Remove deletes it; otherwise
// RoleID and Status, when set, replace the current ones.
type MemberChange struct {
	OrgID  string
	UserID string
	RoleID string
	Status model.MembershipStatus
	Remove bool
	Now    time.Time
	// Allow, when set, sees the membership as it is before the change and
	// may refuse it (the caller's escalation rules), inside the
	// transaction that makes it.
	Allow func(current model.Membership) error
}

// InvitationAcceptance spends an invitation token. Either UserID (an
// existing, signed-in account whose email matches) or NewUser is set.
type InvitationAcceptance struct {
	TokenHash []byte
	UserID    string
	NewUser   *model.User
	Now       time.Time
}

// AuditRepository stores the append-only audit log.
type AuditRepository interface {
	// Append adds e to its chain in its own transaction.
	Append(ctx context.Context, e model.AuditEvent) (model.AuditEvent, error)
	// List pages a chain newest first, before beforeSeq (0: from the end).
	List(ctx context.Context, filter AuditFilter) ([]model.AuditEvent, error)
	// Verify recomputes a chain's hashes from its last checkpoint.
	Verify(ctx context.Context, chain string) (AuditVerification, error)
	// Purge removes up to limit events older than before, recording a
	// checkpoint per chain first.
	Purge(ctx context.Context, before time.Time, limit int, now time.Time) (int, error)
	Chains(ctx context.Context) ([]string, error)
}

type AuditFilter struct {
	Chain     string
	BeforeSeq int64
	Limit     int
	// Action matches exactly, or as a prefix when it ends in ".".
	Action  string
	Outcome string
	ActorID string
}

// AuditVerification is what verifying a chain found.
type AuditVerification struct {
	Chain    string
	Checked  int64
	FirstSeq int64
	LastSeq  int64
	// BrokenAt is the first seq whose hash does not match; 0 when the
	// chain is intact.
	BrokenAt int64
	Problem  string
}

// QuotaRepository stores tenant quota policies and storage usage, and
// creates upload sessions together with their reservation.
type QuotaRepository interface {
	// Policy returns the tenant's overrides; zero fields use defaults.
	Policy(ctx context.Context, tenantID string) (model.TenantQuota, error)
	SetPolicy(ctx context.Context, tenantID string, q model.TenantQuota, now time.Time, audit ...model.AuditEvent) error
	// CreateReservedUpload stores an upload session and reserves its
	// declared bytes in one transaction, under the tenant's usage lock.
	// A *model.QuotaError when a limit would be passed.
	CreateReservedUpload(ctx context.Context, upload *model.UploadSession, idempotencyKey string, limits model.TenantQuota, multipart bool) error
	Usage(ctx context.Context, tenantID string) (model.StorageUsage, error)
	// Reconcile settles stale reservations and recounts the tenant's usage
	// from its sessions and assets.
	Reconcile(ctx context.Context, tenantID string, now time.Time) (before, after model.StorageUsage, settled int, err error)
	TenantsWithUsage(ctx context.Context, after string, limit int) ([]string, error)
}

// AuditRecorder records an audit event without failing the operation it
// describes: a failure to record is logged and counted. Changes that must
// not happen unrecorded write their events in their own transaction
// instead (the repositories' audit arguments).
type AuditRecorder interface {
	Record(ctx context.Context, e model.AuditEvent)
}
