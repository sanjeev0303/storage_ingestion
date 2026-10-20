package model

import "errors"

// Principal is the authenticated caller. It comes only from the
// authentication layer, never from request bodies or query parameters:
// the tenant is one whose membership the server has checked, and the
// permissions are those of the member's role in it.
type Principal struct {
	TenantID     string
	UserID       string
	SessionID    string
	RoleID       string
	Permissions  Permission
	PlatformRole PlatformRole
	// Service marks a trusted internal caller (a worker or an operator
	// CLI) acting with every permission of one tenant.
	Service bool
}

// Valid reports whether the principal identifies a caller.
func (p Principal) Valid() bool {
	return p.UserID != ""
}

// Can reports whether the principal holds every permission of perm in its
// tenant.
func (p Principal) Can(perm Permission) bool {
	return p.TenantID != "" && p.Permissions.Has(perm)
}

// Require is Can as an error: ErrTenantRequired without a tenant,
// ErrForbidden without the permission.
func (p Principal) Require(perm Permission) error {
	if p.TenantID == "" {
		return ErrTenantRequired
	}
	if !p.Permissions.Has(perm) {
		return ErrForbidden
	}
	return nil
}

// RequirePlatform checks a platform permission.
func (p Principal) RequirePlatform(perm PlatformPermission) error {
	if !p.PlatformRole.Can(perm) {
		return ErrForbidden
	}
	return nil
}

// ServicePrincipal is a trusted internal caller with every permission of
// one tenant. Workers and operator tools use it; HTTP requests never get
// one.
func ServicePrincipal(tenantID, name string) Principal {
	return Principal{TenantID: tenantID, UserID: name, Permissions: AllPermissions, Service: true}
}

var ErrUnauthenticated = errors.New("authentication required")
