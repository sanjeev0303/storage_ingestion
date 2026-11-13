package identity

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/config"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/password"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/ratelimit"
)

// Fakes holding just enough for these sign-in paths; anything else panics.

type oneUser struct {
	interfaces.IdentityRepository
	user model.User
}

func (u oneUser) GetUserByEmail(_ context.Context, email string) (model.User, error) {
	if email == u.user.Email {
		return u.user, nil
	}
	return model.User{}, model.ErrUserNotFound
}

type memberships struct {
	interfaces.TenancyRepository
	orgs []string // the organizations the user belongs to
}

func (m memberships) UserMemberships(_ context.Context, userID string) ([]model.Membership, []model.Organization, error) {
	var out []model.Membership
	for _, org := range m.orgs {
		out = append(out, model.Membership{OrgID: org, UserID: userID, Status: model.MembershipActive})
	}
	return out, nil, nil
}

func (m memberships) Access(_ context.Context, orgID, _ string) (model.Organization, model.Membership, model.Role, error) {
	for _, org := range m.orgs {
		if org == orgID {
			return model.Organization{ID: orgID}, model.Membership{OrgID: orgID, Status: model.MembershipActive}, model.Role{}, nil
		}
	}
	return model.Organization{}, model.Membership{}, model.Role{}, model.ErrMembershipNotFound
}

type noLinks struct{ SSOStore }

func (noLinks) LinkedUser(context.Context, string, string) (string, error) { return "", nil }

func (noLinks) GetSSO(context.Context, string) (model.SSOConnection, error) {
	return model.SSOConnection{}, model.ErrSSONotConfigured
}

type noAudit struct{}

func (noAudit) Record(context.Context, model.AuditEvent) {}

var alice = model.User{ID: "u-alice", Email: "alice@victim.example", Status: model.UserActive}

// Another organization's identity provider, naming the victim's domain,
// must not sign in as an existing account: that was account takeover.
func TestSSODoesNotTakeOverAnExistingAccount(t *testing.T) {
	withPassword := alice
	withPassword.PasswordHash = "argon2id$stored"
	s := &Service{deps: Dependencies{Users: oneUser{user: withPassword}, Tenancy: memberships{orgs: []string{"org-victim"}}},
		sso: &sso{store: noLinks{}}}
	attacker := model.SSOConnection{OrgID: "org-attacker", Enabled: true, AllowedDomains: []string{"victim.example"}, DefaultRole: model.RoleAuditor}
	claims := federatedClaims{Subject: "s1", Email: alice.Email, EmailVerified: true}

	_, err := s.signInFederated(context.Background(), attacker, "https://idp.attacker.example", claims, Client{}, time.Now())
	if !errors.Is(err, model.ErrSSOFailed) {
		t.Fatalf("err = %v, want ErrSSOFailed", err)
	}
}

// An organization's identity provider may link only an account that
// belongs to that organization alone.
func TestSSOLinksOnlyAccountsOfThisOrganizationAlone(t *testing.T) {
	cases := []struct {
		name string
		user model.User
		orgs []string
		want bool
	}{
		{"made by this directory", alice, []string{"org-a"}, true},
		{"a member here with a password", model.User{ID: "u2", PasswordHash: "argon2id$x"}, []string{"org-a"}, true},
		{"also in another organization", alice, []string{"org-a", "org-b"}, false},
		{"in another organization only", alice, []string{"org-b"}, false},
		{"in no organization", alice, nil, false},
		{"platform admin", model.User{ID: "u3", PlatformRole: model.PlatformAdmin}, []string{"org-a"}, false},
	}
	for _, c := range cases {
		s := &Service{deps: Dependencies{Tenancy: memberships{orgs: c.orgs}}}
		got, err := s.belongsOnlyTo(context.Background(), c.user, "org-a")
		if err != nil || got != c.want {
			t.Errorf("%s: got %v, %v; want %v", c.name, got, err, c.want)
		}
	}
}

// SCIM never adds an existing account to an organization: its holder joins
// by accepting an invitation.
func TestSCIMDoesNotAttachAnExistingAccount(t *testing.T) {
	s := &Service{deps: Dependencies{Users: oneUser{user: alice}, Tenancy: memberships{}}, sso: &sso{store: noLinks{}},
		now: time.Now}
	_, err := s.SCIMCreateUser(context.Background(), model.ServicePrincipal("org-a", "scim"), alice.Email, "Alice", true)
	if !errors.Is(err, ErrSCIMAccountExists) {
		t.Fatalf("err = %v, want ErrSCIMAccountExists", err)
	}
}

// One account has a cap on failed sign-ins whatever address they come
// from, so a forged client address does not give unlimited guesses.
func TestLoginCapsOneAccountAcrossAddresses(t *testing.T) {
	dummy, err := password.Hash("dummy", password.Params{MemoryKiB: 64, Time: 1, Threads: 1})
	if err != nil {
		t.Fatal(err)
	}
	s := &Service{
		deps:      Dependencies{Users: oneUser{}, Audit: noAudit{}, Limiter: ratelimit.New()},
		cfg:       config.Auth{LoginAttempts: 5, LoginIPAttempts: 50, LoginWindow: time.Minute},
		dummyHash: dummy,
	}
	for i := 1; i <= 4*5+1; i++ {
		_, err := s.Login(context.Background(), "alice@victim.example", "guess", Client{IP: fmt.Sprintf("198.51.100.%d", i)})
		var limited *model.RateLimitError
		if errors.As(err, &limited) {
			if i <= 4*5 {
				t.Fatalf("throttled after %d attempts, before the account cap", i-1)
			}
			return
		}
	}
	t.Fatal("a new address on every attempt was never throttled")
}
