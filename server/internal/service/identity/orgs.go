package identity

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,62}[a-z0-9]$`)

// CreateOrganization creates an organization with owner as its first
// owner. Platform administrators create organizations; their owners run
// them from there.
func (s *Service) CreateOrganization(ctx context.Context, p model.Principal, name, slug, ownerUserID string) (model.Organization, error) {
	if err := s.requirePlatform(ctx, p, model.PlatformManage, "organization.create", "organization", slug); err != nil {
		return model.Organization{}, err
	}
	name, slug = strings.TrimSpace(name), strings.ToLower(strings.TrimSpace(slug))
	if name == "" || len(name) > 200 {
		return model.Organization{}, fmt.Errorf("%w: name is required, at most 200 characters", ErrInvalidInput)
	}
	if !slugPattern.MatchString(slug) {
		return model.Organization{}, fmt.Errorf("%w: slug must be 3-64 characters of a-z, 0-9 and '-'", ErrInvalidInput)
	}
	owner, err := s.deps.Users.GetUser(ctx, ownerUserID)
	if err != nil {
		return model.Organization{}, err
	}
	if owner.Status == model.UserDeactivated {
		return model.Organization{}, model.ErrAccountInactive
	}
	now := s.now()
	org := model.Organization{ID: newID(), Name: name, Slug: slug, Status: model.OrgActive, CreatedBy: p.UserID, CreatedAt: now, UpdatedAt: now}
	member := model.Membership{
		OrgID: org.ID, UserID: owner.ID, RoleID: model.RoleOwner, Status: model.MembershipActive,
		CreatedBy: p.UserID, CreatedAt: now, UpdatedAt: now,
	}
	created := s.event(ctx, p, "organization.create", "organization", org.ID, map[string]any{"slug": slug, "owner": owner.ID})
	created.TenantID = org.ID
	if err := s.deps.Tenancy.CreateOrganization(ctx, org, member, created); err != nil {
		return model.Organization{}, err
	}
	return org, nil
}

// GetOrganization returns the caller's organization.
func (s *Service) GetOrganization(ctx context.Context, p model.Principal) (model.Organization, error) {
	if err := s.require(ctx, p, model.PermOrgRead, "organization.read", "organization", p.TenantID); err != nil {
		return model.Organization{}, err
	}
	return s.deps.Tenancy.GetOrganization(ctx, p.TenantID)
}

// ListOrganizations pages every organization, for platform administrators.
func (s *Service) ListOrganizations(ctx context.Context, p model.Principal, after string, limit int) ([]model.Organization, error) {
	if err := s.requirePlatform(ctx, p, model.PlatformManage, "organization.list", "organization", ""); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	return s.deps.Tenancy.ListOrganizations(ctx, after, limit)
}

// UpdateOrganization changes the caller's organization's name and
// settings.
func (s *Service) UpdateOrganization(ctx context.Context, p model.Principal, name string, settings model.OrgSettings) (model.Organization, error) {
	if err := s.require(ctx, p, model.PermOrgManage, "organization.update", "organization", p.TenantID); err != nil {
		return model.Organization{}, err
	}
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 200 {
		return model.Organization{}, fmt.Errorf("%w: name is required, at most 200 characters", ErrInvalidInput)
	}
	if settings.InvitationTTLHours < 0 || settings.InvitationTTLHours > 720 {
		return model.Organization{}, fmt.Errorf("%w: invitation_ttl_hours must be between 0 and 720", ErrInvalidInput)
	}
	for i, d := range settings.AllowedEmailDomains {
		d = strings.ToLower(strings.TrimSpace(d))
		if d == "" || !strings.Contains(d, ".") || strings.ContainsAny(d, "@ ") {
			return model.Organization{}, fmt.Errorf("%w: %q is not a domain", ErrInvalidInput, d)
		}
		settings.AllowedEmailDomains[i] = d
	}
	before, err := s.deps.Tenancy.GetOrganization(ctx, p.TenantID)
	if err != nil {
		return model.Organization{}, err
	}
	org := before
	org.Name, org.Settings, org.UpdatedAt = name, settings, s.now()
	e := s.event(ctx, p, "organization.update", "organization", p.TenantID, map[string]any{
		"before": map[string]any{"name": before.Name, "allowed_email_domains": before.Settings.AllowedEmailDomains, "invitation_ttl_hours": before.Settings.InvitationTTLHours},
		"after":  map[string]any{"name": name, "allowed_email_domains": settings.AllowedEmailDomains, "invitation_ttl_hours": settings.InvitationTTLHours},
	})
	return s.deps.Tenancy.UpdateOrganization(ctx, org, e)
}

// DeactivateOrganization stops all access to the caller's organization.
// Its data, retention and legal holds are untouched; a platform
// administrator can reactivate it. Only an owner may.
func (s *Service) DeactivateOrganization(ctx context.Context, p model.Principal) (model.Organization, error) {
	if err := s.require(ctx, p, model.PermOwnership, "organization.deactivate", "organization", p.TenantID); err != nil {
		return model.Organization{}, err
	}
	e := s.event(ctx, p, "organization.deactivate", "organization", p.TenantID, nil)
	return s.deps.Tenancy.SetOrganizationStatus(ctx, p.TenantID,
		[]model.OrgStatus{model.OrgActive, model.OrgSuspended}, model.OrgDeactivated, s.now(), e)
}

// SetOrganizationStatus is the platform administrator's control:
// ACTIVE, SUSPENDED or DEACTIVATED. Suspension and deactivation refuse
// every member's requests until the organization is active again.
func (s *Service) SetOrganizationStatus(ctx context.Context, p model.Principal, orgID string, to model.OrgStatus) (model.Organization, error) {
	action := "organization." + strings.ToLower(string(to))
	if err := s.requirePlatform(ctx, p, model.PlatformManage, action, "organization", orgID); err != nil {
		return model.Organization{}, err
	}
	var from []model.OrgStatus
	switch to {
	case model.OrgActive, model.OrgSuspended, model.OrgDeactivated:
		from = []model.OrgStatus{model.OrgActive, model.OrgSuspended, model.OrgDeactivated}
	default:
		return model.Organization{}, fmt.Errorf("%w: status must be ACTIVE, SUSPENDED or DEACTIVATED", ErrInvalidInput)
	}
	e := s.event(ctx, p, action, "organization", orgID, nil)
	e.TenantID = orgID
	return s.deps.Tenancy.SetOrganizationStatus(ctx, orgID, from, to, s.now(), e)
}

// DeleteOrganization marks a deactivated organization DELETED and removes
// its members. It is refused while the organization owns anything that
// must be kept: assets (including soft-deleted ones awaiting their
// purge), uploads in progress, unfinished jobs, legal holds or pending
// deletions. The row itself is never removed: audit records and
// tombstones refer to it.
func (s *Service) DeleteOrganization(ctx context.Context, p model.Principal, orgID string) (model.Organization, error) {
	if err := s.requirePlatform(ctx, p, model.PlatformManage, "organization.delete", "organization", orgID); err != nil {
		return model.Organization{}, err
	}
	retained, err := s.deps.Tenancy.RetainedData(ctx, orgID)
	if err != nil {
		return model.Organization{}, err
	}
	if len(retained) > 0 {
		return model.Organization{}, fmt.Errorf("%w: %s", model.ErrOrgNotEmpty, strings.Join(retained, ", "))
	}
	e := s.event(ctx, p, "organization.delete", "organization", orgID, nil)
	e.TenantID = orgID
	org, err := s.deps.Tenancy.SetOrganizationStatus(ctx, orgID, []model.OrgStatus{model.OrgDeactivated}, model.OrgDeleted, s.now(), e)
	if errors.Is(err, model.ErrOrgState) {
		return org, fmt.Errorf("%w: deactivate the organization first", model.ErrOrgState)
	}
	return org, err
}
