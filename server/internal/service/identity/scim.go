package identity

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// SCIM 2.0 provisioning (docs/sso.md, "SCIM"): an identity provider keeps
// the organization's members in step with its directory. A SCIM user is
// a member of the token's organization; a SCIM group is a role.
//
// SCIM acts for the organization, but never on its owners: it cannot
// create, change, suspend or remove an owner, nor give the owner role.
// Ownership stays with people, in the console.

var (
	// ErrSCIMNotFound: no such user or group in the organization.
	ErrSCIMNotFound = errors.New("no such resource in this organization")
	// ErrSCIMConflict: the user is already a member.
	ErrSCIMConflict = errors.New("the user is already a member of this organization")
	// ErrSCIMOwner: SCIM may not change owners.
	ErrSCIMOwner = errors.New("SCIM does not change owners or give the owner role; do it in the console")
)

// SCIM tokens are prefixed so one is never mistaken for another kind.
const scimPrefix = "sst_"

// SCIMStore is the part of the SSO store SCIM uses.
type SCIMStore interface {
	PutSCIMToken(ctx context.Context, orgID string, hash []byte, actor string, now time.Time, audit ...model.AuditEvent) error
	DeleteSCIMToken(ctx context.Context, orgID string, audit ...model.AuditEvent) error
	SCIMToken(ctx context.Context, orgID string) (time.Time, *time.Time, bool, error)
	OrgBySCIMToken(ctx context.Context, hash []byte, now time.Time) (string, error)
}

func (s *Service) scimStore() (SCIMStore, error) {
	if s.sso == nil {
		return nil, ErrSSOOff
	}
	store, ok := s.sso.store.(SCIMStore)
	if !ok {
		return nil, ErrSSOOff
	}
	return store, nil
}

// SCIMTokenStatus is whether the organization has a SCIM token, and when it
// was made and last used.
type SCIMTokenStatus struct {
	Enabled  bool
	Created  time.Time
	LastUsed *time.Time
}

func (s *Service) SCIMTokenStatus(ctx context.Context, p model.Principal) (SCIMTokenStatus, error) {
	if err := s.require(ctx, p, model.PermOrgManage, "scim.read", "organization", p.TenantID); err != nil {
		return SCIMTokenStatus{}, err
	}
	store, err := s.scimStore()
	if err != nil {
		return SCIMTokenStatus{}, err
	}
	created, used, found, err := store.SCIMToken(ctx, p.TenantID)
	return SCIMTokenStatus{Enabled: found, Created: created, LastUsed: used}, err
}

// IssueSCIMToken makes the organization's SCIM token, replacing any, and
// returns it: shown this once.
func (s *Service) IssueSCIMToken(ctx context.Context, p model.Principal) (string, error) {
	if err := s.require(ctx, p, model.PermOrgManage, "scim.token_issue", "organization", p.TenantID); err != nil {
		return "", err
	}
	store, err := s.scimStore()
	if err != nil {
		return "", err
	}
	token, hash, err := newToken(scimPrefix)
	if err != nil {
		return "", err
	}
	e := s.event(ctx, p, "scim.token_issue", "organization", p.TenantID, nil)
	return token, store.PutSCIMToken(ctx, p.TenantID, hash, p.UserID, s.now(), e)
}

func (s *Service) RevokeSCIMToken(ctx context.Context, p model.Principal) error {
	if err := s.require(ctx, p, model.PermOrgManage, "scim.token_revoke", "organization", p.TenantID); err != nil {
		return err
	}
	store, err := s.scimStore()
	if err != nil {
		return err
	}
	return store.DeleteSCIMToken(ctx, p.TenantID, s.event(ctx, p, "scim.token_revoke", "organization", p.TenantID, nil))
}

// SCIMAuthenticate is the principal a SCIM token acts as: the
// organization's service principal named "scim".
func (s *Service) SCIMAuthenticate(ctx context.Context, token string, client Client) (model.Principal, error) {
	if err := s.throttle("sso", client.IP); err != nil {
		return model.Principal{}, err
	}
	store, err := s.scimStore()
	if err != nil {
		return model.Principal{}, err
	}
	hash, ok := tokenHash(token, scimPrefix)
	if !ok {
		return model.Principal{}, model.ErrUnauthenticated
	}
	orgID, err := store.OrgBySCIMToken(ctx, hash, s.now())
	if err != nil {
		return model.Principal{}, err
	}
	if orgID == "" {
		return model.Principal{}, model.ErrUnauthenticated
	}
	org, err := s.deps.Tenancy.GetOrganization(ctx, orgID)
	if err != nil || org.Status != model.OrgActive {
		return model.Principal{}, model.ErrOrgInactive
	}
	return model.ServicePrincipal(orgID, "scim"), nil
}

// SCIMUser is a member as SCIM sees them.
type SCIMUser struct {
	User   model.User
	Member model.Membership
}

// SCIMUsers lists the organization's members, all of them or the one whose
// email is userName (the filter identity providers send before creating).
func (s *Service) SCIMUsers(ctx context.Context, p model.Principal, userName string, start, count int) ([]SCIMUser, int, error) {
	var all []model.Membership
	after := ""
	for {
		page, err := s.deps.Tenancy.ListMembers(ctx, p.TenantID, after, 500)
		if err != nil {
			return nil, 0, err
		}
		all = append(all, page...)
		if len(page) < 500 {
			break
		}
		after = page[len(page)-1].UserID
	}
	var matched []model.Membership
	for _, m := range all {
		if userName == "" || strings.EqualFold(m.Email, userName) {
			matched = append(matched, m)
		}
	}
	total := len(matched)
	if start < 1 {
		start = 1
	}
	if count < 0 {
		count = 0
	}
	if count > 200 {
		count = 200
	}
	if start-1 >= len(matched) {
		return []SCIMUser{}, total, nil
	}
	matched = matched[start-1 : min(start-1+count, len(matched))]
	out := make([]SCIMUser, 0, len(matched))
	for _, m := range matched {
		user, err := s.deps.Users.GetUser(ctx, m.UserID)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, SCIMUser{User: user, Member: m})
	}
	return out, total, nil
}

func (s *Service) SCIMUser(ctx context.Context, p model.Principal, userID string) (SCIMUser, error) {
	_, member, _, err := s.deps.Tenancy.Access(ctx, p.TenantID, userID)
	if errors.Is(err, model.ErrMembershipNotFound) {
		return SCIMUser{}, ErrSCIMNotFound
	}
	if err != nil {
		return SCIMUser{}, err
	}
	user, err := s.deps.Users.GetUser(ctx, userID)
	return SCIMUser{User: user, Member: member}, err
}

// SCIMCreateUser makes the person a member: an existing account joins; a
// new one is made without a password, to sign in through the
// organization's identity provider. The role is the SSO connection's
// default role, or developer.
func (s *Service) SCIMCreateUser(ctx context.Context, p model.Principal, userName, displayName string, active bool) (SCIMUser, error) {
	email, err := normalizeEmail(userName)
	if err != nil {
		return SCIMUser{}, err
	}
	email = strings.ToLower(email)
	role := model.RoleDeveloper
	if conn, err := s.sso.store.GetSSO(ctx, p.TenantID); err == nil {
		_, domain, _ := strings.Cut(email, "@")
		if !slices.Contains(conn.AllowedDomains, domain) {
			return SCIMUser{}, fmt.Errorf("%w: the email domain %s is not one this organization's single sign-on allows", ErrInvalidInput, domain)
		}
		if conn.DefaultRole != "" {
			role = conn.DefaultRole
		}
	}
	if len(displayName) > 200 {
		displayName = displayName[:200]
	}
	now := s.now()
	provision := model.SSOProvision{}
	user, err := s.deps.Users.GetUserByEmail(ctx, email)
	switch {
	case errors.Is(err, model.ErrUserNotFound):
		user = model.User{ID: newID(), Email: email, DisplayName: strings.TrimSpace(displayName), Status: model.UserActive, CreatedAt: now, UpdatedAt: now}
		provision.NewUser = true
	case err != nil:
		return SCIMUser{}, err
	default:
		if _, _, _, err := s.deps.Tenancy.Access(ctx, p.TenantID, user.ID); err == nil {
			return SCIMUser{}, ErrSCIMConflict
		}
	}
	status := model.MembershipActive
	if !active {
		status = model.MembershipSuspended
	}
	provision.User = user
	provision.Member = &model.Membership{OrgID: p.TenantID, UserID: user.ID, RoleID: role, Status: status,
		CreatedBy: p.UserID, CreatedAt: now, UpdatedAt: now}
	events := []model.AuditEvent{s.event(ctx, p, "member.join", "user", user.ID, map[string]any{"via": "scim", "role": role})}
	if provision.NewUser {
		events = append(events, s.platformEvent(ctx, p, "user.create", "user", user.ID, map[string]any{"via": "scim"}))
	}
	if err := s.sso.store.Provision(ctx, provision, now, events...); err != nil {
		return SCIMUser{}, err
	}
	return s.SCIMUser(ctx, p, user.ID)
}

// SCIMSetActive suspends or reactivates a member.
func (s *Service) SCIMSetActive(ctx context.Context, p model.Principal, userID string, active bool) (SCIMUser, error) {
	current, err := s.SCIMUser(ctx, p, userID)
	if err != nil {
		return current, err
	}
	if current.Member.RoleID == model.RoleOwner {
		return current, ErrSCIMOwner
	}
	if (current.Member.Status == model.MembershipActive) != active {
		if active {
			_, err = s.ReactivateMember(ctx, p, userID)
		} else {
			_, err = s.SuspendMember(ctx, p, userID)
		}
		if err != nil {
			return current, err
		}
	}
	return s.SCIMUser(ctx, p, userID)
}

// SCIMDeleteUser removes the member; their account stays.
func (s *Service) SCIMDeleteUser(ctx context.Context, p model.Principal, userID string) error {
	current, err := s.SCIMUser(ctx, p, userID)
	if err != nil {
		return err
	}
	if current.Member.RoleID == model.RoleOwner {
		return ErrSCIMOwner
	}
	return s.RemoveMember(ctx, p, userID)
}

// SCIMGroup is a role and its members.
type SCIMGroup struct {
	Role    model.Role
	Members []model.Membership
}

func (s *Service) SCIMGroups(ctx context.Context, p model.Principal) ([]SCIMGroup, error) {
	roles, err := s.Roles(ctx, p)
	if err != nil {
		return nil, err
	}
	users, _, err := s.SCIMUsers(ctx, p, "", 1, 1<<30)
	if err != nil {
		return nil, err
	}
	var out []SCIMGroup
	for _, role := range roles {
		g := SCIMGroup{Role: role}
		for _, u := range users {
			if u.Member.RoleID == role.ID {
				g.Members = append(g.Members, u.Member)
			}
		}
		out = append(out, g)
	}
	return out, nil
}

func (s *Service) SCIMGroup(ctx context.Context, p model.Principal, roleID string) (SCIMGroup, error) {
	groups, err := s.SCIMGroups(ctx, p)
	if err != nil {
		return SCIMGroup{}, err
	}
	for _, g := range groups {
		if g.Role.ID == roleID {
			return g, nil
		}
	}
	return SCIMGroup{}, ErrSCIMNotFound
}

// SCIMPatchGroup gives the role to the added members, and takes it from
// the removed ones: they get the SSO connection's default role, or
// developer, unless that is the role being taken.
func (s *Service) SCIMPatchGroup(ctx context.Context, p model.Principal, roleID string, add, remove []string) (SCIMGroup, error) {
	if roleID == model.RoleOwner {
		return SCIMGroup{}, ErrSCIMOwner
	}
	if _, err := s.SCIMGroup(ctx, p, roleID); err != nil {
		return SCIMGroup{}, err
	}
	fallback := model.RoleDeveloper
	if conn, err := s.sso.store.GetSSO(ctx, p.TenantID); err == nil && conn.DefaultRole != "" {
		fallback = conn.DefaultRole
	}
	change := func(userID, to string) error {
		current, err := s.SCIMUser(ctx, p, userID)
		if err != nil {
			return err
		}
		if current.Member.RoleID == model.RoleOwner {
			return ErrSCIMOwner
		}
		if current.Member.RoleID == to {
			return nil
		}
		_, err = s.ChangeMemberRole(ctx, p, userID, to)
		return err
	}
	for _, userID := range add {
		if err := change(userID, roleID); err != nil {
			return SCIMGroup{}, err
		}
	}
	for _, userID := range remove {
		if fallback == roleID {
			return SCIMGroup{}, fmt.Errorf("%w: the members of the default role cannot be removed from it; give them another role", ErrInvalidInput)
		}
		current, err := s.SCIMUser(ctx, p, userID)
		if err != nil {
			return SCIMGroup{}, err
		}
		if current.Member.RoleID != roleID {
			continue
		}
		if err := change(userID, fallback); err != nil {
			return SCIMGroup{}, err
		}
	}
	return s.SCIMGroup(ctx, p, roleID)
}
