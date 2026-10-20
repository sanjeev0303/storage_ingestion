package integration

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/service/audit"
	"github.com/sanjeev0303/object-storage-platform/internal/service/identity"
)

func TestOrganizationMembershipIsValidated(t *testing.T) {
	e := openSecurityEnv(t, nil)
	ctx := context.Background()
	acme := e.team(t, "acme")
	stranger, strangerPW := e.account(t, "stranger")
	id, err := e.identity.Authenticate(ctx, e.login(t, stranger.Email, strangerPW).AccessToken)
	if err != nil {
		t.Fatal(err)
	}

	// Naming an organization does not make you a member of it, and a
	// missing organization answers the same.
	_, notMember := e.identity.Principal(ctx, id, acme.org.ID)
	_, missing := e.identity.Principal(ctx, id, "no-such-organization")
	if !errors.Is(notMember, model.ErrForbidden) || !errors.Is(missing, model.ErrForbidden) {
		t.Fatalf("not a member: %v, missing: %v", notMember, missing)
	}
	// Without memberships a caller has no tenant at all.
	if p, err := e.identity.Principal(ctx, id, ""); err != nil || p.TenantID != "" || p.Permissions != 0 {
		t.Fatalf("no memberships: %+v %v", p, err)
	}

	// Two organizations: the request must name one, and gets that role.
	other := e.organization(t, "other", stranger)
	e.join(t, e.as(t, other.ID, stranger), acme.devUser, model.RoleAuditor)
	devID, err := e.identity.Authenticate(ctx, e.login(t, acme.devUser.Email, acme.devPW).AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	if p, err := e.identity.Principal(ctx, devID, ""); err != nil || p.TenantID != "" {
		t.Fatalf("two organizations and none named: %+v %v", p, err)
	}
	if p, _ := e.identity.Principal(ctx, devID, acme.org.ID); p.RoleID != model.RoleDeveloper || !p.Can(model.PermUploadsWrite) {
		t.Fatalf("in acme: %+v", p)
	}
	if p, _ := e.identity.Principal(ctx, devID, other.ID); p.RoleID != model.RoleAuditor || p.Can(model.PermUploadsWrite) {
		t.Fatalf("in other: %+v", p)
	}

	// A suspended member is refused until reactivated.
	if _, err := e.identity.SuspendMember(ctx, acme.owner, acme.devUser.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.identity.Principal(ctx, devID, acme.org.ID); !errors.Is(err, model.ErrForbidden) {
		t.Fatalf("suspended member: %v", err)
	}
	if _, err := e.identity.ReactivateMember(ctx, acme.owner, acme.devUser.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.identity.Principal(ctx, devID, acme.org.ID); err != nil {
		t.Fatalf("reactivated member: %v", err)
	}

	// A suspended or deactivated organization refuses all its members.
	if _, err := e.identity.SetOrganizationStatus(ctx, acme.owner, acme.org.ID, model.OrgSuspended); !errors.Is(err, model.ErrForbidden) {
		t.Fatalf("an owner cannot use the platform's suspension: %v", err)
	}
	if _, err := e.identity.SetOrganizationStatus(ctx, operator, acme.org.ID, model.OrgSuspended); err != nil {
		t.Fatal(err)
	}
	if _, err := e.identity.PrincipalFor(ctx, acme.org.ID, acme.ownerUser.ID); !errors.Is(err, model.ErrOrgInactive) {
		t.Fatalf("suspended organization: %v", err)
	}
	if _, err := e.identity.SetOrganizationStatus(ctx, operator, acme.org.ID, model.OrgActive); err != nil {
		t.Fatal(err)
	}
	if _, err := e.identity.DeactivateOrganization(ctx, acme.admin); !errors.Is(err, model.ErrForbidden) {
		t.Fatalf("only owners deactivate: %v", err)
	}
	if _, err := e.identity.DeleteOrganization(ctx, operator, acme.org.ID); !errors.Is(err, model.ErrOrgState) {
		t.Fatalf("an active organization is not deleted: %v", err)
	}
	if _, err := e.identity.DeactivateOrganization(ctx, e.as(t, acme.org.ID, acme.ownerUser)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.identity.Principal(ctx, devID, acme.org.ID); !errors.Is(err, model.ErrOrgInactive) {
		t.Fatalf("deactivated organization: %v", err)
	}
	// It owns nothing that must be kept, so it can now be deleted; its
	// members go with it.
	deleted, err := e.identity.DeleteOrganization(ctx, operator, acme.org.ID)
	if err != nil || deleted.Status != model.OrgDeleted {
		t.Fatalf("delete: %+v %v", deleted, err)
	}
	if _, err := e.identity.Principal(ctx, devID, acme.org.ID); !errors.Is(err, model.ErrForbidden) {
		t.Fatalf("deleted organization: %v", err)
	}
}

func TestOrganizationWithDataIsNotDeleted(t *testing.T) {
	e := openSecurityEnv(t, nil)
	ctx := context.Background()
	acme := e.team(t, "keep")
	if _, err := e.uploads().CreateFor(ctx, acme.developer, uploadRequest("in-flight", 10)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.identity.SetOrganizationStatus(ctx, operator, acme.org.ID, model.OrgDeactivated); err != nil {
		t.Fatal(err)
	}
	if _, err := e.identity.DeleteOrganization(ctx, operator, acme.org.ID); !errors.Is(err, model.ErrOrgNotEmpty) {
		t.Fatalf("an organization with an upload in flight must be kept: %v", err)
	}
}

func TestRBACIsEnforcedInServices(t *testing.T) {
	e := openSecurityEnv(t, nil)
	ctx := context.Background()
	acme := e.team(t, "rbac")
	uploads := e.uploads()
	newcomer, _ := e.account(t, "newcomer")

	type check struct {
		name string
		call func(p model.Principal) error
	}
	checks := []check{
		{"list members", func(p model.Principal) error { _, err := e.identity.ListMembers(ctx, p, "", 10); return err }},
		{"invite", func(p model.Principal) error {
			_, err := e.identity.Invite(ctx, p, newcomer.Email, model.RoleAuditor)
			return err
		}},
		{"list invitations", func(p model.Principal) error { _, err := e.identity.ListInvitations(ctx, p, "", 10); return err }},
		{"create role", func(p model.Principal) error {
			_, err := e.identity.CreateRole(ctx, p, "reader-"+p.RoleID, "", []string{"assets.read"})
			return err
		}},
		{"update organization", func(p model.Principal) error {
			_, err := e.identity.UpdateOrganization(ctx, p, "Renamed", model.OrgSettings{})
			return err
		}},
		{"read quota", func(p model.Principal) error { _, err := e.quotas.Get(ctx, p); return err }},
		{"read audit log", func(p model.Principal) error { _, err := e.audit.List(ctx, p, audit.Filter{}); return err }},
		{"upload", func(p model.Principal) error {
			_, err := uploads.CreateFor(ctx, p, uploadRequest("rbac-"+p.RoleID, 10))
			return err
		}},
	}
	// What each built-in role may do, in the order of checks.
	allowed := map[string][]bool{
		model.RoleOwner:     {true, true, true, true, true, true, true, true},
		model.RoleAdmin:     {true, true, true, true, true, true, true, true},
		model.RoleDeveloper: {true, false, false, false, false, true, false, true},
		model.RoleAuditor:   {true, false, false, false, false, true, true, false},
	}
	for _, p := range []model.Principal{acme.auditor, acme.developer, acme.admin, acme.owner} {
		for i, c := range checks {
			if c.name == "invite" && allowed[p.RoleID][i] {
				// One pending invitation per address: revoke the last.
				revokePending(t, e, acme.owner)
			}
			err := c.call(p)
			switch {
			case allowed[p.RoleID][i] && err != nil:
				t.Fatalf("%s may %s: %v", p.RoleID, c.name, err)
			case !allowed[p.RoleID][i] && !errors.Is(err, model.ErrForbidden):
				t.Fatalf("%s may not %s, got %v", p.RoleID, c.name, err)
			}
		}
	}

	// Every refusal is in the organization's audit log, with its actor.
	denied := 0
	rows, err := e.db.Pool.Query(ctx, `SELECT actor_id, action FROM audit_events WHERE chain = $1 AND outcome = 'DENIED'`, acme.org.ID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var actor, action string
		if err := rows.Scan(&actor, &action); err != nil {
			t.Fatal(err)
		}
		if actor != acme.devUser.ID && actor != acme.auditorUser.ID {
			t.Fatalf("unexpected denial of %s (%s)", actor, action)
		}
		denied++
	}
	rows.Close()
	if denied != 10 {
		t.Fatalf("expected 10 recorded refusals, got %d", denied)
	}
}

func revokePending(t *testing.T, e *securityEnv, owner model.Principal) {
	t.Helper()
	ctx := context.Background()
	pending, err := e.identity.ListInvitations(ctx, owner, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, inv := range pending {
		if inv.Status == model.InvitationPending {
			if err := e.identity.RevokeInvitation(ctx, owner, inv.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestPrivilegeEscalationIsPrevented(t *testing.T) {
	e := openSecurityEnv(t, nil)
	ctx := context.Background()
	acme := e.team(t, "esc")
	target, _ := e.account(t, "target")

	// Administrators cannot hand out or take away ownership.
	if _, err := e.identity.Invite(ctx, acme.admin, target.Email, model.RoleOwner); !errors.Is(err, model.ErrEscalation) {
		t.Fatalf("admin invites an owner: %v", err)
	}
	if _, err := e.identity.ChangeMemberRole(ctx, acme.admin, acme.devUser.ID, model.RoleOwner); !errors.Is(err, model.ErrEscalation) {
		t.Fatalf("admin promotes to owner: %v", err)
	}
	if _, err := e.identity.ChangeMemberRole(ctx, acme.admin, acme.adminUser.ID, model.RoleOwner); !errors.Is(err, model.ErrEscalation) {
		t.Fatalf("admin promotes themself: %v", err)
	}
	if _, err := e.identity.ChangeMemberRole(ctx, acme.admin, acme.ownerUser.ID, model.RoleAuditor); !errors.Is(err, model.ErrEscalation) {
		t.Fatalf("admin demotes the owner: %v", err)
	}
	if _, err := e.identity.SuspendMember(ctx, acme.admin, acme.ownerUser.ID); !errors.Is(err, model.ErrEscalation) {
		t.Fatalf("admin suspends the owner: %v", err)
	}
	if err := e.identity.RemoveMember(ctx, acme.admin, acme.ownerUser.ID); !errors.Is(err, model.ErrEscalation) {
		t.Fatalf("admin removes the owner: %v", err)
	}
	// Nor can anyone change their own role without members.manage.
	if _, err := e.identity.ChangeMemberRole(ctx, acme.developer, acme.devUser.ID, model.RoleAdmin); !errors.Is(err, model.ErrForbidden) {
		t.Fatalf("developer promotes themself: %v", err)
	}

	// Custom roles never carry ownership, nor unknown permissions.
	for _, perms := range [][]string{{"org.ownership"}, {"assets.read", "everything"}, {}} {
		if _, err := e.identity.CreateRole(ctx, acme.owner, "bad", "", perms); !errors.Is(err, identity.ErrInvalidInput) {
			t.Fatalf("role with %v: %v", perms, err)
		}
	}

	// A member manager may grant only what they hold.
	manager, err := e.identity.CreateRole(ctx, acme.owner, "member manager", "", []string{"members.read", "members.manage", "uploads.write"})
	if err != nil {
		t.Fatal(err)
	}
	mgrUser, _ := e.account(t, "mgr")
	mgr := e.join(t, acme.owner, mgrUser, manager.ID)
	if _, err := e.identity.Invite(ctx, mgr, target.Email, model.RoleDeveloper); !errors.Is(err, model.ErrEscalation) {
		t.Fatalf("a manager grants the developer role they lack: %v", err)
	}
	if _, err := e.identity.Invite(ctx, mgr, target.Email, manager.ID); err != nil {
		t.Fatalf("a manager grants their own role: %v", err)
	}
	if _, err := e.identity.ChangeMemberRole(ctx, mgr, acme.devUser.ID, manager.ID); !errors.Is(err, model.ErrEscalation) {
		t.Fatalf("a manager changes a developer, whose permissions they lack: %v", err)
	}
	if _, err := e.identity.CreateRole(ctx, mgr, "sneaky", "", []string{"members.manage"}); !errors.Is(err, model.ErrForbidden) {
		t.Fatalf("a manager without roles.manage creates a role: %v", err)
	}
	if _, err := e.identity.UpdateRole(ctx, acme.admin, manager.ID, "member manager", "", []string{"members.manage", "org.ownership"}); !errors.Is(err, identity.ErrInvalidInput) {
		t.Fatalf("an admin adds ownership to a role: %v", err)
	}
	if _, err := e.identity.UpdateRole(ctx, acme.admin, model.RoleDeveloper, "dev", "", []string{"audit.read"}); !errors.Is(err, identity.ErrInvalidInput) {
		t.Fatalf("built-in roles are fixed: %v", err)
	}

	// Another organization's role does not exist here.
	other := e.organization(t, "esc-other", target)
	foreign, err := e.identity.CreateRole(ctx, e.as(t, other.ID, target), "foreign", "", []string{"assets.read"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.identity.ChangeMemberRole(ctx, acme.owner, acme.devUser.ID, foreign.ID); !errors.Is(err, identity.ErrInvalidInput) {
		t.Fatalf("a foreign role: %v", err)
	}

	// None of the refused changes happened.
	members, err := e.identity.ListMembers(ctx, acme.owner, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range members {
		want := map[string]string{
			acme.ownerUser.ID: model.RoleOwner, acme.adminUser.ID: model.RoleAdmin, acme.devUser.ID: model.RoleDeveloper,
			acme.auditorUser.ID: model.RoleAuditor, mgrUser.ID: manager.ID,
		}[m.UserID]
		if m.RoleID != want || m.Status != model.MembershipActive {
			t.Fatalf("member %s changed: %+v", m.UserID, m)
		}
	}
}

func TestLastOwnerIsProtected(t *testing.T) {
	e := openSecurityEnv(t, nil)
	ctx := context.Background()
	acme := e.team(t, "own")

	if _, err := e.identity.ChangeMemberRole(ctx, acme.owner, acme.ownerUser.ID, model.RoleAdmin); !errors.Is(err, model.ErrLastOwner) {
		t.Fatalf("the last owner steps down: %v", err)
	}
	if err := e.identity.RemoveMember(ctx, acme.owner, acme.ownerUser.ID); !errors.Is(err, model.ErrLastOwner) {
		t.Fatalf("the last owner leaves: %v", err)
	}

	// Two owners demoting each other at the same moment: one wins, one
	// owner always remains.
	secondUser, _ := e.account(t, "second")
	second := e.join(t, acme.owner, secondUser, model.RoleOwner)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, c := range []struct {
		by     model.Principal
		target string
	}{{acme.owner, secondUser.ID}, {second, acme.ownerUser.ID}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = e.identity.ChangeMemberRole(ctx, c.by, c.target, model.RoleAdmin)
		}()
	}
	wg.Wait()
	wins, refused := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, model.ErrLastOwner):
			refused++
		default:
			t.Fatalf("unexpected: %v", err)
		}
	}
	if wins != 1 || refused != 1 || activeOwners(t, e, acme.org.ID) != 1 {
		t.Fatalf("expected one demotion and one owner left: %v, %d owners", errs, activeOwners(t, e, acme.org.ID))
	}

	// An owner whose account is deactivated does not count: the other
	// cannot leave on the strength of it.
	thirdUser, _ := e.account(t, "third")
	remaining := ownerPrincipal(t, e, acme.org.ID, acme.ownerUser, secondUser)
	e.join(t, remaining, thirdUser, model.RoleOwner)
	if _, err := e.identity.SetUserStatus(ctx, operator, thirdUser.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := e.identity.RemoveMember(ctx, remaining, remaining.UserID); !errors.Is(err, model.ErrLastOwner) {
		t.Fatalf("leaving with only a deactivated co-owner: %v", err)
	}
}

func activeOwners(t *testing.T, e *securityEnv, orgID string) int {
	t.Helper()
	var n int
	if err := e.db.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM memberships WHERE org_id = $1 AND role_id = 'owner' AND status = 'ACTIVE'`, orgID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// ownerPrincipal is the principal of whichever of users is still an owner.
func ownerPrincipal(t *testing.T, e *securityEnv, orgID string, users ...model.User) model.Principal {
	t.Helper()
	for _, u := range users {
		if p := e.as(t, orgID, u); p.RoleID == model.RoleOwner {
			return p
		}
	}
	t.Fatal("no owner left")
	return model.Principal{}
}

func TestRoleAndMembershipChangesTakeEffectOnTheNextRequest(t *testing.T) {
	e := openSecurityEnv(t, nil)
	ctx := context.Background()
	acme := e.team(t, "rev")
	uploads := e.uploads()
	devID, err := e.identity.Authenticate(ctx, e.login(t, acme.devUser.Email, acme.devPW).AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	request := func() model.Principal {
		p, err := e.identity.Principal(ctx, devID, acme.org.ID)
		if err != nil {
			t.Fatalf("principal: %v", err)
		}
		return p
	}

	if _, err := uploads.CreateFor(ctx, request(), uploadRequest("before", 10)); err != nil {
		t.Fatal(err)
	}
	// A role change applies to the same session's next request.
	if _, err := e.identity.ChangeMemberRole(ctx, acme.admin, acme.devUser.ID, model.RoleAuditor); err != nil {
		t.Fatal(err)
	}
	if _, err := uploads.CreateFor(ctx, request(), uploadRequest("after", 10)); !errors.Is(err, model.ErrForbidden) {
		t.Fatalf("after demotion to auditor: %v", err)
	}

	// A custom role in use cannot be deleted; once unused it can.
	role, err := e.identity.CreateRole(ctx, acme.admin, "uploader", "", []string{"uploads.write"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.identity.ChangeMemberRole(ctx, acme.admin, acme.devUser.ID, role.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.identity.DeleteRole(ctx, acme.admin, role.ID); !errors.Is(err, model.ErrRoleInUse) {
		t.Fatalf("deleting a role in use: %v", err)
	}
	// Narrowing the role narrows its members at once.
	if _, err := e.identity.UpdateRole(ctx, acme.admin, role.ID, "uploader", "", []string{"assets.read"}); err != nil {
		t.Fatal(err)
	}
	if p := request(); p.Can(model.PermUploadsWrite) || !p.Can(model.PermAssetsRead) {
		t.Fatalf("after narrowing the role: %v", p.Permissions.Names())
	}

	// Removal ends tenant access; the account itself stays usable.
	if err := e.identity.RemoveMember(ctx, acme.admin, acme.devUser.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.identity.Principal(ctx, devID, acme.org.ID); !errors.Is(err, model.ErrForbidden) {
		t.Fatalf("after removal: %v", err)
	}
	if _, err := e.identity.Me(ctx, model.Principal{UserID: acme.devUser.ID}); err != nil {
		t.Fatalf("the account stays: %v", err)
	}
	if err := e.identity.DeleteRole(ctx, acme.admin, role.ID); err != nil {
		t.Fatalf("deleting the unused role: %v", err)
	}

	// Anyone may leave, and the change is audited with before and after.
	if err := e.identity.RemoveMember(ctx, acme.auditor, acme.auditorUser.ID); err != nil {
		t.Fatalf("leaving: %v", err)
	}
	changes := e.auditEvents(t, acme.org.ID, "member.role_change")
	if len(changes) != 2 || changes[0].Details["role_before"] != model.RoleDeveloper || changes[0].Details["role_after"] != model.RoleAuditor {
		t.Fatalf("role changes are audited: %+v", changes)
	}
	if removals := e.auditEvents(t, acme.org.ID, "member.remove"); len(removals) != 2 {
		t.Fatalf("removals are audited: %+v", removals)
	}
}

func TestInvitationsExpireAndAreSingleUse(t *testing.T) {
	e := openSecurityEnv(t, nil)
	ctx := context.Background()
	acme := e.team(t, "inv")
	invitee, _ := e.account(t, "invitee")
	bystander, _ := e.account(t, "bystander")

	// The token is for an address: another account cannot use it.
	inv, err := e.identity.Invite(ctx, acme.admin, invitee.Email, model.RoleDeveloper)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.identity.AcceptInvitation(ctx, inv.Token, &model.Principal{UserID: bystander.ID}, nil, e.client); !errors.Is(err, model.ErrForbidden) {
		t.Fatalf("someone else's invitation: %v", err)
	}
	// An expired invitation is no invitation.
	if _, err := e.db.Pool.Exec(ctx, `UPDATE invitations SET expires_at = now() - interval '1 second' WHERE id = $1`, inv.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.identity.AcceptInvitation(ctx, inv.Token, &model.Principal{UserID: invitee.ID}, nil, e.client); !errors.Is(err, model.ErrCredentialToken) {
		t.Fatalf("expired invitation: %v", err)
	}
	revokePending(t, e, acme.owner)

	// A revoked one neither.
	inv, err = e.identity.Invite(ctx, acme.admin, invitee.Email, model.RoleDeveloper)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.identity.RevokeInvitation(ctx, acme.admin, inv.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.identity.AcceptInvitation(ctx, inv.Token, &model.Principal{UserID: invitee.ID}, nil, e.client); !errors.Is(err, model.ErrCredentialToken) {
		t.Fatalf("revoked invitation: %v", err)
	}

	// A valid one works once.
	inv, err = e.identity.Invite(ctx, acme.admin, invitee.Email, model.RoleDeveloper)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.identity.AcceptInvitation(ctx, inv.Token, &model.Principal{UserID: invitee.ID}, nil, e.client); err != nil {
		t.Fatal(err)
	}
	if _, err := e.identity.AcceptInvitation(ctx, inv.Token, &model.Principal{UserID: invitee.ID}, nil, e.client); !errors.Is(err, model.ErrCredentialToken) {
		t.Fatalf("an invitation works once: %v", err)
	}
	if p := e.as(t, acme.org.ID, invitee); p.RoleID != model.RoleDeveloper {
		t.Fatalf("invitee: %+v", p)
	}

	// Someone without an account creates it by accepting.
	inv, err = e.identity.Invite(ctx, acme.admin, e.email("brand-new"), model.RoleAuditor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.identity.AcceptInvitation(ctx, inv.Token, nil, &identity.NewAccount{Password: "short"}, e.client); !errors.Is(err, model.ErrWeakPassword) {
		t.Fatalf("weak password: %v", err)
	}
	if _, err := e.identity.AcceptInvitation(ctx, inv.Token, nil, &identity.NewAccount{Password: "a brand new password"}, e.client); err != nil {
		t.Fatal(err)
	}
	e.login(t, e.email("brand-new"), "a brand new password")

	// The organization can restrict invitations to its domains.
	if _, err := e.identity.UpdateOrganization(ctx, acme.admin, acme.org.Name, model.OrgSettings{AllowedEmailDomains: []string{"Partner.example"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.identity.Invite(ctx, acme.admin, e.email("outsider"), model.RoleAuditor); !errors.Is(err, identity.ErrInvalidInput) {
		t.Fatalf("an address outside the allowed domains: %v", err)
	}
	if _, err := e.identity.Invite(ctx, acme.admin, "someone@partner.example", model.RoleAuditor); err != nil {
		t.Fatalf("an allowed domain: %v", err)
	}
}
