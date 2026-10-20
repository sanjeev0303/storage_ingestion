package model

import (
	"errors"
	"testing"
)

func TestPermissionNamesRoundTrip(t *testing.T) {
	names := AllPermissions.Names()
	if len(names) != 25 {
		t.Fatalf("expected 25 permissions, got %d: %v", len(names), names)
	}
	parsed, err := ParsePermissions(names)
	if err != nil || parsed != AllPermissions {
		t.Fatalf("round trip: %v %v", parsed, err)
	}
	if _, err := ParsePermissions([]string{"uploads.write", "root"}); err == nil {
		t.Fatal("an unknown permission must be refused")
	}
	if Permission(0).Has(0) {
		t.Fatal("an empty set grants nothing")
	}
}

// The built-in roles as the permission matrix in docs/security.md
// describes them.
func TestBuiltInRoleMatrix(t *testing.T) {
	role := func(id string) Permission {
		r, ok := BuiltInRole(id)
		if !ok {
			t.Fatalf("no built-in role %s", id)
		}
		return r.Permissions
	}
	owner, admin, developer, auditor := role(RoleOwner), role(RoleAdmin), role(RoleDeveloper), role(RoleAuditor)

	if owner != AllPermissions || !owner.Has(PermOwnership) {
		t.Fatal("owners hold every permission")
	}
	if admin.Has(PermOwnership) || !admin.Has(PermMembersManage|PermRolesManage|PermQuarantineRelease|PermAuditRead) {
		t.Fatalf("admin: %v", admin.Names())
	}
	if !developer.Has(PermUploadsWrite|PermAssetsDownload|PermIngestionWrite|PermCatalogWrite) ||
		developer.Has(PermMembersManage) || developer.Has(PermAuditRead) || developer.Has(PermQuarantineRelease) ||
		developer.Has(PermDatasetsManage) || !admin.Has(PermDatasetsManage) {
		t.Fatalf("developer: %v", developer.Names())
	}
	if !auditor.Has(PermAuditRead|PermAssetsRead) || auditor.Has(PermAssetsDownload) || auditor.Has(PermUploadsWrite) ||
		auditor.Has(PermCatalogWrite) ||
		auditor.Has(PermIngestionWrite) || auditor.Has(PermMembersManage) {
		t.Fatalf("auditor: %v", auditor.Names())
	}
	// Storage connections: everyone reads them, developers may choose one
	// for an upload, only owners and admins manage them.
	if !auditor.Has(PermStorageRead) || auditor.Has(PermStorageUse) || !developer.Has(PermStorageRead|PermStorageUse) ||
		developer.Has(PermStorageManage) || !admin.Has(PermStorageManage) || !owner.Has(PermStorageManage) {
		t.Fatal("storage permissions do not match the matrix")
	}
	if _, ok := BuiltInRole("superuser"); ok {
		t.Fatal("unknown built-in role")
	}
	// The list is a copy: changing it changes no role.
	roles := BuiltInRoles()
	roles[0].Permissions = 0
	if role(roles[0].ID) == 0 {
		t.Fatal("BuiltInRoles must return a copy")
	}
}

func TestPrincipalChecks(t *testing.T) {
	developer, _ := BuiltInRole(RoleDeveloper)
	p := Principal{TenantID: "t1", UserID: "u1", Permissions: developer.Permissions}
	if err := p.Require(PermUploadsWrite); err != nil {
		t.Fatal(err)
	}
	if err := p.Require(PermUploadsWrite | PermAuditRead); !errors.Is(err, ErrForbidden) {
		t.Fatalf("every permission asked for is needed: %v", err)
	}
	noTenant := Principal{UserID: "u1", Permissions: AllPermissions}
	if err := noTenant.Require(PermAssetsRead); !errors.Is(err, ErrTenantRequired) || noTenant.Can(PermAssetsRead) {
		t.Fatalf("no organization, no tenant permission: %v", err)
	}
	if (Principal{TenantID: "t1"}).Valid() {
		t.Fatal("a principal needs a user")
	}

	// Platform permissions are separate from tenant ones.
	if err := p.RequirePlatform(PlatformManage); !errors.Is(err, ErrForbidden) {
		t.Fatal("tenant permissions grant no platform permission")
	}
	auditor := Principal{UserID: "u2", PlatformRole: PlatformAuditor}
	if auditor.RequirePlatform(PlatformAuditRead) != nil || auditor.RequirePlatform(PlatformManage) == nil {
		t.Fatal("platform auditors read the audit log and manage nothing")
	}
	if (Principal{UserID: "u3", PlatformRole: PlatformAdmin}).Can(PermAssetsRead) {
		t.Fatal("a platform administrator gets no tenant data without a membership")
	}
}

func TestQuotaErrorReportsWhatIsLeft(t *testing.T) {
	err := error(&QuotaError{Limit: "storage_bytes", Max: 100, Used: 70, Requested: 50})
	var qe *QuotaError
	if !errors.Is(err, ErrQuotaExceeded) || !errors.As(err, &qe) || qe.Remaining() != 30 {
		t.Fatalf("quota error: %v", err)
	}
	if over := (&QuotaError{Max: 10, Used: 12}); over.Remaining() != 0 {
		t.Fatalf("remaining never goes below zero: %d", over.Remaining())
	}
}

func TestOrgEmailDomains(t *testing.T) {
	s := OrgSettings{AllowedEmailDomains: []string{"example.com"}}
	for email, want := range map[string]bool{
		"a@example.com": true, "a@EXAMPLE.com": true, "a@evil-example.com": false, "a@example.com.evil": false,
	} {
		if s.EmailAllowed(email) != want {
			t.Fatalf("%s: want %v", email, want)
		}
	}
	if !(OrgSettings{}).EmailAllowed("x@y.z") {
		t.Fatal("no domains means any address")
	}
}
