package identity

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/password"
)

// Escalation rules, applied to every role grant and membership change:
//   - you can grant only a role whose permissions you hold yourself;
//   - you can change or remove only a member whose current role's
//     permissions you hold yourself, so an administrator cannot demote an
//     owner;
//   - the owner role (the only one with org.ownership) can be granted or
//     taken only by an owner;
//   - the last active owner cannot be demoted, suspended or removed.

// grantable checks that p may give role to someone.
func grantable(p model.Principal, role model.Role) error {
	if !p.Permissions.Has(role.Permissions) && role.Permissions != 0 {
		return model.ErrEscalation
	}
	if role.Permissions.Has(model.PermOwnership) && !p.Can(model.PermOwnership) {
		return model.ErrEscalation
	}
	return nil
}

// role resolves a role of the caller's organization: built-in or custom.
func (s *Service) role(ctx context.Context, orgID, roleID string) (model.Role, error) {
	role, err := s.deps.Tenancy.GetRole(ctx, orgID, roleID)
	if errors.Is(err, model.ErrRoleNotFound) {
		return role, fmt.Errorf("%w: unknown role %q", ErrInvalidInput, roleID)
	}
	return role, err
}

// ListMembers pages the caller's organization's members.
func (s *Service) ListMembers(ctx context.Context, p model.Principal, after string, limit int) ([]model.Membership, error) {
	if err := s.require(ctx, p, model.PermMembersRead, "member.list", "organization", p.TenantID); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	return s.deps.Tenancy.ListMembers(ctx, p.TenantID, after, limit)
}

// ChangeMemberRole gives a member another role.
func (s *Service) ChangeMemberRole(ctx context.Context, p model.Principal, userID, roleID string) (model.Membership, error) {
	if err := s.require(ctx, p, model.PermMembersManage, "member.role_change", "member", userID); err != nil {
		return model.Membership{}, err
	}
	role, err := s.role(ctx, p.TenantID, roleID)
	if err != nil {
		return model.Membership{}, err
	}
	if err := grantable(p, role); err != nil {
		return model.Membership{}, s.deny(ctx, p, "member.role_change", "member", userID, err)
	}
	return s.changeMember(ctx, p, "member.role_change", interfaces.MemberChange{
		OrgID: p.TenantID, UserID: userID, RoleID: role.ID, Now: s.now(),
		Allow: func(current model.Membership) error { return s.mayManage(ctx, p, current) },
	})
}

// SuspendMember stops a member's access without removing them;
// ReactivateMember restores it.
func (s *Service) SuspendMember(ctx context.Context, p model.Principal, userID string) (model.Membership, error) {
	return s.setMemberStatus(ctx, p, userID, model.MembershipSuspended, "member.suspend")
}

func (s *Service) ReactivateMember(ctx context.Context, p model.Principal, userID string) (model.Membership, error) {
	return s.setMemberStatus(ctx, p, userID, model.MembershipActive, "member.reactivate")
}

func (s *Service) setMemberStatus(ctx context.Context, p model.Principal, userID string, status model.MembershipStatus, action string) (model.Membership, error) {
	if err := s.require(ctx, p, model.PermMembersManage, action, "member", userID); err != nil {
		return model.Membership{}, err
	}
	return s.changeMember(ctx, p, action, interfaces.MemberChange{
		OrgID: p.TenantID, UserID: userID, Status: status, Now: s.now(),
		Allow: func(current model.Membership) error {
			if userID == p.UserID {
				return fmt.Errorf("%w: you cannot change your own membership status", model.ErrForbidden)
			}
			return s.mayManage(ctx, p, current)
		},
	})
}

// RemoveMember removes someone from the caller's organization. Their
// account stays; their uploads and assets belong to the organization and
// stay too.
func (s *Service) RemoveMember(ctx context.Context, p model.Principal, userID string) error {
	if userID != p.UserID {
		if err := s.require(ctx, p, model.PermMembersManage, "member.remove", "member", userID); err != nil {
			return err
		}
	}
	_, err := s.changeMember(ctx, p, "member.remove", interfaces.MemberChange{
		OrgID: p.TenantID, UserID: userID, Remove: true, Now: s.now(),
		Allow: func(current model.Membership) error {
			if userID == p.UserID {
				return nil // leaving is always allowed, but for the last owner
			}
			return s.mayManage(ctx, p, current)
		},
	})
	return err
}

// mayManage refuses changes to a member more privileged than the caller.
func (s *Service) mayManage(ctx context.Context, p model.Principal, current model.Membership) error {
	role, err := s.deps.Tenancy.GetRole(ctx, current.OrgID, current.RoleID)
	if errors.Is(err, model.ErrRoleNotFound) {
		return nil // a role that is gone grants nothing
	}
	if err != nil {
		return err
	}
	return grantable(p, role)
}

func (s *Service) changeMember(ctx context.Context, p model.Principal, action string, change interfaces.MemberChange) (model.Membership, error) {
	member, err := s.deps.Tenancy.ChangeMember(ctx, change, func(before model.Membership, after *model.Membership) model.AuditEvent {
		details := map[string]any{"role_before": before.RoleID, "status_before": string(before.Status)}
		if after == nil {
			details["removed"] = true
		} else {
			details["role_after"], details["status_after"] = after.RoleID, string(after.Status)
		}
		return s.event(ctx, p, action, "member", change.UserID, details)
	})
	switch {
	case errors.Is(err, model.ErrEscalation), errors.Is(err, model.ErrLastOwner), errors.Is(err, model.ErrForbidden):
		return member, s.deny(ctx, p, action, "member", change.UserID, err)
	}
	return member, err
}

// ─── Invitations ──────────────────────────────────────────────────────

// Invitation is a created invitation and its token, shown once.
type Invitation struct {
	model.Invitation
	Token string
}

// Invite offers a role in the caller's organization to an email address.
func (s *Service) Invite(ctx context.Context, p model.Principal, email, roleID string) (Invitation, error) {
	if err := s.require(ctx, p, model.PermMembersManage, "invitation.create", "invitation", ""); err != nil {
		return Invitation{}, err
	}
	if err := s.throttle("membership", p.TenantID); err != nil {
		return Invitation{}, err
	}
	email, err := normalizeEmail(email)
	if err != nil {
		return Invitation{}, err
	}
	role, err := s.role(ctx, p.TenantID, roleID)
	if err != nil {
		return Invitation{}, err
	}
	if err := grantable(p, role); err != nil {
		return Invitation{}, s.deny(ctx, p, "invitation.create", "invitation", "", err)
	}
	org, err := s.deps.Tenancy.GetOrganization(ctx, p.TenantID)
	if err != nil {
		return Invitation{}, err
	}
	if !org.Settings.EmailAllowed(email) {
		return Invitation{}, fmt.Errorf("%w: the organization accepts invitations only for %s", ErrInvalidInput,
			strings.Join(org.Settings.AllowedEmailDomains, ", "))
	}
	ttl := s.cfg.InvitationTTL
	if org.Settings.InvitationTTLHours > 0 {
		ttl = time.Duration(org.Settings.InvitationTTLHours) * time.Hour
	}
	token, hash, err := newToken(invitationPrefix)
	if err != nil {
		return Invitation{}, err
	}
	now := s.now()
	inv := model.Invitation{
		ID: newID(), OrgID: p.TenantID, Email: email, RoleID: role.ID, Status: model.InvitationPending,
		CreatedBy: p.UserID, CreatedAt: now, ExpiresAt: now.Add(ttl),
	}
	// The audit record names the role, not the address.
	e := s.event(ctx, p, "invitation.create", "invitation", inv.ID, map[string]any{"role": role.ID})
	if err := s.deps.Tenancy.CreateInvitation(ctx, inv, hash, e); err != nil {
		return Invitation{}, err
	}
	return Invitation{Invitation: inv, Token: token}, nil
}

func (s *Service) ListInvitations(ctx context.Context, p model.Principal, after string, limit int) ([]model.Invitation, error) {
	if err := s.require(ctx, p, model.PermMembersManage, "invitation.list", "organization", p.TenantID); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	return s.deps.Tenancy.ListInvitations(ctx, p.TenantID, after, limit)
}

func (s *Service) RevokeInvitation(ctx context.Context, p model.Principal, invitationID string) error {
	if err := s.require(ctx, p, model.PermMembersManage, "invitation.revoke", "invitation", invitationID); err != nil {
		return err
	}
	e := s.event(ctx, p, "invitation.revoke", "invitation", invitationID, nil)
	return s.deps.Tenancy.RevokeInvitation(ctx, p.TenantID, invitationID, s.now(), e)
}

// NewAccount is the account to create when an invitation is accepted by
// someone without one.
type NewAccount struct {
	Password    string
	DisplayName string
}

// AcceptInvitation spends an invitation token. A signed-in caller (p set)
// joins with their account, which must have the invited address. Anyone
// else creates their account here with account.
func (s *Service) AcceptInvitation(ctx context.Context, token string, p *model.Principal, account *NewAccount, client Client) (model.Membership, error) {
	if err := s.throttle("credentials", client.IP); err != nil {
		return model.Membership{}, err
	}
	hash, ok := tokenHash(token, invitationPrefix)
	if !ok {
		return model.Membership{}, model.ErrCredentialToken
	}
	accept := interfaces.InvitationAcceptance{TokenHash: hash, Now: s.now()}
	switch {
	case p != nil && p.UserID != "":
		accept.UserID = p.UserID
	case account != nil:
		if err := s.checkPassword(account.Password, ""); err != nil {
			return model.Membership{}, err
		}
		hashed, err := password.Hash(account.Password, s.params)
		if err != nil {
			return model.Membership{}, err
		}
		accept.NewUser = &model.User{ID: newID(), DisplayName: strings.TrimSpace(account.DisplayName), PasswordHash: hashed}
	default:
		return model.Membership{}, fmt.Errorf("%w: sign in, or give a password for a new account", ErrInvalidInput)
	}
	member, _, err := s.deps.Tenancy.AcceptInvitation(ctx, accept, func(inv model.Invitation, user model.User) []model.AuditEvent {
		actor := model.Principal{TenantID: inv.OrgID, UserID: user.ID}
		events := []model.AuditEvent{s.event(ctx, actor, "invitation.accept", "invitation", inv.ID, map[string]any{"role": inv.RoleID})}
		if accept.NewUser != nil {
			created := s.event(ctx, actor, "user.create", "user", user.ID, map[string]any{"via": "invitation"})
			created.TenantID = ""
			events = append(events, created)
		}
		return events
	})
	return member, err
}

// ─── Roles ────────────────────────────────────────────────────────────

// Roles lists the built-in roles and the organization's own.
func (s *Service) Roles(ctx context.Context, p model.Principal) ([]model.Role, error) {
	if err := s.require(ctx, p, model.PermMembersRead, "role.list", "organization", p.TenantID); err != nil {
		return nil, err
	}
	custom, err := s.deps.Tenancy.ListRoles(ctx, p.TenantID)
	if err != nil {
		return nil, err
	}
	return append(model.BuiltInRoles(), custom...), nil
}

// CreateRole adds a custom role. Its permissions must be ones the caller
// holds, and never include ownership.
func (s *Service) CreateRole(ctx context.Context, p model.Principal, name, description string, permissions []string) (model.Role, error) {
	if err := s.require(ctx, p, model.PermRolesManage, "role.create", "role", ""); err != nil {
		return model.Role{}, err
	}
	role, err := s.customRole(p, name, description, permissions)
	if err != nil {
		return model.Role{}, err
	}
	if err := grantable(p, role); err != nil {
		return model.Role{}, s.deny(ctx, p, "role.create", "role", "", err)
	}
	now := s.now()
	role.ID, role.OrgID, role.CreatedBy, role.CreatedAt, role.UpdatedAt = "role_"+newID(), p.TenantID, p.UserID, now, now
	e := s.event(ctx, p, "role.create", "role", role.ID, map[string]any{"name": role.Name, "permissions": role.Permissions.Names()})
	if err := s.deps.Tenancy.CreateRole(ctx, role, e); err != nil {
		return model.Role{}, err
	}
	return role, nil
}

// UpdateRole changes a custom role. The caller must hold both its old and
// its new permissions.
func (s *Service) UpdateRole(ctx context.Context, p model.Principal, roleID, name, description string, permissions []string) (model.Role, error) {
	if err := s.require(ctx, p, model.PermRolesManage, "role.update", "role", roleID); err != nil {
		return model.Role{}, err
	}
	if _, builtIn := model.BuiltInRole(roleID); builtIn {
		return model.Role{}, fmt.Errorf("%w: built-in roles cannot be changed", ErrInvalidInput)
	}
	before, err := s.deps.Tenancy.GetRole(ctx, p.TenantID, roleID)
	if err != nil {
		return model.Role{}, err
	}
	role, err := s.customRole(p, name, description, permissions)
	if err != nil {
		return model.Role{}, err
	}
	if err := grantable(p, before); err != nil {
		return model.Role{}, s.deny(ctx, p, "role.update", "role", roleID, err)
	}
	if err := grantable(p, role); err != nil {
		return model.Role{}, s.deny(ctx, p, "role.update", "role", roleID, err)
	}
	role.ID, role.OrgID, role.UpdatedAt = roleID, p.TenantID, s.now()
	e := s.event(ctx, p, "role.update", "role", roleID, map[string]any{
		"before": before.Permissions.Names(), "after": role.Permissions.Names(),
	})
	return s.deps.Tenancy.UpdateRole(ctx, role, e)
}

// DeleteRole removes a custom role no member or pending invitation uses.
func (s *Service) DeleteRole(ctx context.Context, p model.Principal, roleID string) error {
	if err := s.require(ctx, p, model.PermRolesManage, "role.delete", "role", roleID); err != nil {
		return err
	}
	if _, builtIn := model.BuiltInRole(roleID); builtIn {
		return fmt.Errorf("%w: built-in roles cannot be deleted", ErrInvalidInput)
	}
	e := s.event(ctx, p, "role.delete", "role", roleID, nil)
	return s.deps.Tenancy.DeleteRole(ctx, p.TenantID, roleID, s.now(), e)
}

func (s *Service) customRole(p model.Principal, name, description string, permissions []string) (model.Role, error) {
	name, description = strings.TrimSpace(name), strings.TrimSpace(description)
	if name == "" || len(name) > 64 || len(description) > 500 {
		return model.Role{}, fmt.Errorf("%w: name is required (at most 64 characters), description at most 500", ErrInvalidInput)
	}
	perms, err := model.ParsePermissions(permissions)
	if err != nil {
		return model.Role{}, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	if perms == 0 {
		return model.Role{}, fmt.Errorf("%w: a role needs at least one permission", ErrInvalidInput)
	}
	if perms.Has(model.PermOwnership) {
		return model.Role{}, fmt.Errorf("%w: org.ownership belongs to the owner role only", ErrInvalidInput)
	}
	return model.Role{Name: name, Description: description, Permissions: perms}, nil
}
