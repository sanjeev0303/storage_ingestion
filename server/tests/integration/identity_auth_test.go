package integration

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/service/identity"
)

func TestLoginIssuesWorkingTokens(t *testing.T) {
	e := openSecurityEnv(t, nil)
	ctx := context.Background()
	user, pw := e.account(t, "ada")
	org := e.organization(t, "ada-org", user)

	// Emails match without regard to case or surrounding space.
	tokens := e.login(t, "  "+strings.ToUpper(user.Email)+" ", pw)
	if !strings.HasPrefix(tokens.AccessToken, "sat_") || !strings.HasPrefix(tokens.RefreshToken, "srt_") ||
		tokens.SessionID == "" || tokens.User.ID != user.ID || !tokens.AccessExpiresAt.Before(tokens.RefreshExpiresAt) {
		t.Fatalf("tokens: %+v", tokens)
	}
	id, err := e.identity.Authenticate(ctx, tokens.AccessToken)
	if err != nil || id.User.ID != user.ID || id.Session.ID != tokens.SessionID {
		t.Fatalf("authenticate: %+v %v", id, err)
	}
	// One organization: it is used without naming it.
	p, err := e.identity.Principal(ctx, id, "")
	if err != nil || p.TenantID != org.ID || p.RoleID != model.RoleOwner || !p.Can(model.PermOwnership) {
		t.Fatalf("principal: %+v %v", p, err)
	}

	// Only the SHA-256 of each token is stored.
	var stored int
	err = e.db.Pool.QueryRow(ctx, `
		SELECT count(*) FROM auth_sessions AS s JOIN auth_refresh_tokens AS r ON r.session_id = s.id
		WHERE s.id = $1 AND s.access_hash = sha256($2::bytea) AND r.token_hash = sha256($3::bytea)
	`, tokens.SessionID, []byte(tokens.AccessToken), []byte(tokens.RefreshToken)).Scan(&stored)
	if err != nil || stored != 1 {
		t.Fatalf("tokens must be stored as their SHA-256: %d %v", stored, err)
	}

	// A token of the wrong kind, or of the wrong shape, is no token.
	for _, bad := range []string{tokens.RefreshToken, "sat_" + strings.Repeat("A", 43), tokens.AccessToken + "x", "", "Bearer " + tokens.AccessToken} {
		if _, err := e.identity.Authenticate(ctx, bad); !errors.Is(err, model.ErrSessionInvalid) {
			t.Fatalf("%.12q...: expected ErrSessionInvalid, got %v", bad, err)
		}
	}
	if got := e.auditEvents(t, model.PlatformChain, "auth.login"); len(got) == 0 {
		t.Fatal("sign-in is audited")
	}
}

func TestInvalidCredentialsAreRefusedAlikeAndThrottled(t *testing.T) {
	e := openSecurityEnv(t, map[string]string{"AUTH_LOGIN_ATTEMPTS": "3"})
	ctx := context.Background()
	user, pw := e.account(t, "bob")

	_, wrong := e.identity.Login(ctx, user.Email, pw+"!", e.client)
	_, unknown := e.identity.Login(ctx, e.email("nobody"), pw, e.client)
	if !errors.Is(wrong, model.ErrInvalidCredentials) || !errors.Is(unknown, model.ErrInvalidCredentials) || wrong.Error() != unknown.Error() {
		t.Fatalf("a wrong password and an unknown account must answer alike: %v / %v", wrong, unknown)
	}

	// The third failure for this email and address locks it for the
	// window, even with the right password.
	if _, err := e.identity.Login(ctx, user.Email, "not it either", e.client); !errors.Is(err, model.ErrInvalidCredentials) {
		t.Fatal(err)
	}
	if _, err := e.identity.Login(ctx, user.Email, "again wrong", e.client); !errors.Is(err, model.ErrInvalidCredentials) {
		t.Fatal(err)
	}
	_, err := e.identity.Login(ctx, user.Email, pw, e.client)
	var limited *model.RateLimitError
	if !errors.As(err, &limited) || limited.RetryAfter <= 0 {
		t.Fatalf("expected throttling, got %v", err)
	}
	// Another address is not locked out by these failures.
	other := identity.Client{IP: "192.0.2.99"}
	if _, err := e.identity.Login(ctx, user.Email, pw, other); err != nil {
		t.Fatalf("another address: %v", err)
	}

	failed := e.auditEvents(t, model.PlatformChain, "auth.login")
	var failures, throttled int
	for _, ev := range failed {
		if ev.ResourceID == user.ID && ev.Outcome == model.AuditFailed {
			failures++
		}
		if ev.Outcome == model.AuditDenied && ev.Reason == "throttled" {
			throttled++
		}
	}
	if failures < 3 || throttled == 0 {
		t.Fatalf("failed and throttled sign-ins are audited: %d failures, %d throttled", failures, throttled)
	}
}

func TestExpiredAndRevokedTokensAreRefused(t *testing.T) {
	e := openSecurityEnv(t, nil)
	ctx := context.Background()
	user, pw := e.account(t, "cyd")
	tokens := e.login(t, user.Email, pw)

	// An expired access token is refused; the session can still refresh.
	if _, err := e.db.Pool.Exec(ctx, `UPDATE auth_sessions SET access_expires_at = now() - interval '1 second' WHERE id = $1`, tokens.SessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.identity.Authenticate(ctx, tokens.AccessToken); !errors.Is(err, model.ErrSessionInvalid) {
		t.Fatalf("expired access token: %v", err)
	}
	fresh, err := e.identity.Refresh(ctx, tokens.RefreshToken, e.client)
	if err != nil {
		t.Fatalf("refresh after access expiry: %v", err)
	}
	if _, err := e.identity.Authenticate(ctx, fresh.AccessToken); err != nil {
		t.Fatalf("refreshed token: %v", err)
	}

	// An expired session is over: neither token works.
	if _, err := e.db.Pool.Exec(ctx, `UPDATE auth_sessions SET expires_at = now() - interval '1 second' WHERE id = $1`, tokens.SessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.identity.Authenticate(ctx, fresh.AccessToken); !errors.Is(err, model.ErrSessionInvalid) {
		t.Fatalf("access token of an expired session: %v", err)
	}
	if _, err := e.identity.Refresh(ctx, fresh.RefreshToken, e.client); !errors.Is(err, model.ErrSessionInvalid) {
		t.Fatalf("refresh of an expired session: %v", err)
	}

	// Logging out revokes the session at once.
	second := e.login(t, user.Email, pw)
	third := e.login(t, user.Email, pw)
	p := model.Principal{UserID: user.ID, SessionID: second.SessionID}
	if err := e.identity.Logout(ctx, p); err != nil {
		t.Fatal(err)
	}
	if _, err := e.identity.Authenticate(ctx, second.AccessToken); !errors.Is(err, model.ErrSessionInvalid) {
		t.Fatalf("after logout: %v", err)
	}
	if _, err := e.identity.Refresh(ctx, second.RefreshToken, e.client); err == nil {
		t.Fatal("a logged-out session must not refresh")
	}

	// A user revokes their own other sessions, never someone else's.
	intruder, intruderPW := e.account(t, "eve")
	eve := e.login(t, intruder.Email, intruderPW)
	if err := e.identity.RevokeSession(ctx, model.Principal{UserID: intruder.ID, SessionID: eve.SessionID}, third.SessionID); !errors.Is(err, model.ErrSessionInvalid) {
		t.Fatalf("revoking another user's session: %v", err)
	}
	if _, err := e.identity.Authenticate(ctx, third.AccessToken); err != nil {
		t.Fatalf("a refused revocation must change nothing: %v", err)
	}
	if err := e.identity.RevokeSession(ctx, model.Principal{UserID: user.ID}, third.SessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.identity.Authenticate(ctx, third.AccessToken); !errors.Is(err, model.ErrSessionInvalid) {
		t.Fatalf("after revocation: %v", err)
	}
}

func TestRefreshRotatesAndAReplayRevokesTheSession(t *testing.T) {
	e := openSecurityEnv(t, nil)
	ctx := context.Background()
	user, pw := e.account(t, "dot")
	first := e.login(t, user.Email, pw)

	second, err := e.identity.Refresh(ctx, first.RefreshToken, e.client)
	if err != nil {
		t.Fatal(err)
	}
	if second.SessionID != first.SessionID || second.AccessToken == first.AccessToken || second.RefreshToken == first.RefreshToken {
		t.Fatalf("refresh must rotate both tokens in the same session: %+v", second)
	}
	if _, err := e.identity.Authenticate(ctx, first.AccessToken); !errors.Is(err, model.ErrSessionInvalid) {
		t.Fatalf("the old access token stops working: %v", err)
	}
	if _, err := e.identity.Authenticate(ctx, second.AccessToken); err != nil {
		t.Fatal(err)
	}

	// The spent refresh token presented again: someone else has a copy.
	// The whole session ends, including the legitimate new tokens.
	if _, err := e.identity.Refresh(ctx, first.RefreshToken, e.client); !errors.Is(err, model.ErrRefreshReplayed) {
		t.Fatalf("replay: expected ErrRefreshReplayed, got %v", err)
	}
	if _, err := e.identity.Authenticate(ctx, second.AccessToken); !errors.Is(err, model.ErrSessionInvalid) {
		t.Fatalf("after a replay the session is revoked: %v", err)
	}
	if _, err := e.identity.Refresh(ctx, second.RefreshToken, e.client); err == nil {
		t.Fatal("after a replay no refresh token of the session works")
	}
	var reason string
	if err := e.db.Pool.QueryRow(ctx, `SELECT COALESCE(revoke_reason, '') FROM auth_sessions WHERE id = $1`, first.SessionID).Scan(&reason); err != nil || reason == "" {
		t.Fatalf("the revocation is recorded: %q %v", reason, err)
	}
	denied := 0
	for _, ev := range e.auditEvents(t, model.PlatformChain, "auth.refresh") {
		if ev.ResourceID == first.SessionID && ev.Outcome == model.AuditDenied {
			denied++
		}
	}
	if denied != 1 {
		t.Fatalf("the replay is audited once, got %d", denied)
	}
}

func TestInactiveAccountsAreRejected(t *testing.T) {
	e := openSecurityEnv(t, nil)
	ctx := context.Background()
	owner, _ := e.account(t, "own")
	org := e.organization(t, "inactive-org", owner)
	user, pw := e.account(t, "fay")
	e.join(t, e.as(t, org.ID, owner), user, model.RoleDeveloper)
	tokens := e.login(t, user.Email, pw)

	if _, err := e.identity.SetUserStatus(ctx, operator, user.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.identity.Authenticate(ctx, tokens.AccessToken); err == nil {
		t.Fatal("deactivation ends existing sessions")
	}
	if _, err := e.identity.Refresh(ctx, tokens.RefreshToken, e.client); err == nil {
		t.Fatal("deactivation ends refresh too")
	}
	if _, err := e.identity.Login(ctx, user.Email, pw, e.client); !errors.Is(err, model.ErrAccountInactive) {
		t.Fatalf("a deactivated account cannot sign in: %v", err)
	}
	// Credentials outliving a request (download links) are checked again.
	if _, err := e.identity.PrincipalFor(ctx, org.ID, user.ID); !errors.Is(err, model.ErrAccountInactive) {
		t.Fatalf("principal of a deactivated account: %v", err)
	}
	if _, err := e.identity.IssueReset(ctx, operator, user.ID); !errors.Is(err, model.ErrAccountInactive) {
		t.Fatalf("no reset for a deactivated account: %v", err)
	}

	if _, err := e.identity.SetUserStatus(ctx, operator, user.ID, true); err != nil {
		t.Fatal(err)
	}
	back := e.login(t, user.Email, pw)
	if _, err := e.identity.Authenticate(ctx, back.AccessToken); err != nil {
		t.Fatalf("reactivated: %v", err)
	}

	// A provisioned account cannot sign in until it is activated.
	pending, activation, err := e.identity.CreateUser(ctx, operator, e.email("pending"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.identity.Login(ctx, pending.Email, "any password at all", e.client); !errors.Is(err, model.ErrInvalidCredentials) {
		t.Fatalf("pending account: %v", err)
	}
	if _, err := e.identity.Activate(ctx, activation.Token, "short", e.client); !errors.Is(err, model.ErrWeakPassword) {
		t.Fatalf("weak password: %v", err)
	}
	if _, err := e.identity.Activate(ctx, activation.Token, "a long enough password", e.client); err != nil {
		t.Fatal(err)
	}
	if _, err := e.identity.Activate(ctx, activation.Token, "a different password", e.client); !errors.Is(err, model.ErrCredentialToken) {
		t.Fatalf("an activation token works once: %v", err)
	}
}

func TestPasswordResetIsSingleUseAndEndsSessions(t *testing.T) {
	e := openSecurityEnv(t, nil)
	ctx := context.Background()
	user, pw := e.account(t, "gus")
	before := e.login(t, user.Email, pw)

	// Ordinary users cannot issue resets; administrators can.
	if _, err := e.identity.IssueReset(ctx, model.Principal{UserID: user.ID}, user.ID); !errors.Is(err, model.ErrForbidden) {
		t.Fatalf("self-issued reset: %v", err)
	}
	reset, err := e.identity.IssueReset(ctx, operator, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.identity.Activate(ctx, reset.Token, "another long password", e.client); !errors.Is(err, model.ErrCredentialToken) {
		t.Fatalf("a reset token does not activate: %v", err)
	}
	if _, err := e.identity.ResetPassword(ctx, reset.Token, "another long password", e.client); err != nil {
		t.Fatal(err)
	}
	if _, err := e.identity.ResetPassword(ctx, reset.Token, "yet another password", e.client); !errors.Is(err, model.ErrCredentialToken) {
		t.Fatalf("a reset token works once: %v", err)
	}
	if _, err := e.identity.Authenticate(ctx, before.AccessToken); err == nil {
		t.Fatal("a reset ends every session")
	}
	if _, err := e.identity.Login(ctx, user.Email, pw, e.client); !errors.Is(err, model.ErrInvalidCredentials) {
		t.Fatalf("the old password: %v", err)
	}
	e.login(t, user.Email, "another long password")

	// Changing the password needs the current one and keeps only this
	// session.
	now := e.login(t, user.Email, "another long password")
	other := e.login(t, user.Email, "another long password")
	p := model.Principal{UserID: user.ID, SessionID: now.SessionID}
	if err := e.identity.ChangePassword(ctx, p, "wrong current password", "a third long password"); !errors.Is(err, model.ErrInvalidCredentials) {
		t.Fatalf("wrong current password: %v", err)
	}
	if err := e.identity.ChangePassword(ctx, p, "another long password", "a third long password"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.identity.Authenticate(ctx, now.AccessToken); err != nil {
		t.Fatalf("the changing session stays: %v", err)
	}
	if _, err := e.identity.Authenticate(ctx, other.AccessToken); err == nil {
		t.Fatal("other sessions end on a password change")
	}
}

func TestBootstrapCreatesTheFirstAdministratorWithoutADefaultPassword(t *testing.T) {
	e := openSecurityEnv(t, nil)
	ctx := context.Background()

	// Bootstrap runs only on a platform without an administrator. Park the
	// test database's own administrators and put them back afterwards.
	parked := parkAdministrators(t, e)
	t.Cleanup(func() {
		if _, err := e.db.Pool.Exec(context.Background(), `UPDATE users SET platform_role = 'ADMIN' WHERE id = ANY($1)`, parked); err != nil {
			t.Errorf("restore administrators: %v", err)
		}
	})

	if _, _, err := e.identity.Bootstrap(ctx, e.email("root"), "Root", "password"); !errors.Is(err, model.ErrWeakPassword) {
		t.Fatalf("a weak bootstrap password: %v", err)
	}
	admin, generated, err := e.identity.Bootstrap(ctx, e.email("root"), "Root", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(generated) < 24 || admin.PlatformRole != model.PlatformAdmin || admin.Status != model.UserActive {
		t.Fatalf("bootstrap: %+v, %d-character password", admin, len(generated))
	}
	var hash string
	if err := e.db.Pool.QueryRow(ctx, `SELECT password_hash FROM users WHERE id = $1`, admin.ID).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(hash, "$argon2id$") || strings.Contains(hash, generated) {
		t.Fatalf("the password is stored only as an Argon2id hash: %.20s", hash)
	}

	// The generated password signs in, as a platform administrator.
	tokens := e.login(t, admin.Email, generated)
	id, err := e.identity.Authenticate(ctx, tokens.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	p, err := e.identity.Principal(ctx, id, "")
	if err != nil || p.PlatformRole != model.PlatformAdmin || p.TenantID != "" {
		t.Fatalf("administrator principal: %+v %v", p, err)
	}
	if _, err := e.identity.CreateOrganization(ctx, p, "Boot Org", "boot-"+e.run, admin.ID); err != nil {
		t.Fatalf("the administrator can create an organization: %v", err)
	}

	// Once is enough.
	if _, _, err := e.identity.Bootstrap(ctx, e.email("root2"), "", ""); !errors.Is(err, identity.ErrBootstrapDone) {
		t.Fatalf("second bootstrap: %v", err)
	}

	// Nothing is fixed: another bootstrap generates another password.
	parkAdministrators(t, e)
	_, again, err := e.identity.Bootstrap(ctx, e.email("root3"), "", "")
	if err != nil || again == generated {
		t.Fatalf("a second platform's password must differ: %v", err)
	}

	events := e.auditEvents(t, model.PlatformChain, "platform.bootstrap")
	found := false
	for _, ev := range events {
		if ev.ResourceID == admin.ID {
			found = true
			if strings.Contains(ev.Reason, generated) || ev.Details["password"] != nil {
				t.Fatal("the audit record must not hold the password")
			}
		}
	}
	if !found {
		t.Fatal("bootstrap is audited")
	}
}

// parkAdministrators removes the platform role from every active
// administrator and returns their IDs. Test administrators are deleted
// with the test; the others the caller restores.
func parkAdministrators(t *testing.T, e *securityEnv) []string {
	t.Helper()
	var ids []string
	rows, err := e.db.Pool.Query(context.Background(), `
		UPDATE users SET platform_role = NULL
		WHERE platform_role = 'ADMIN' AND status = 'ACTIVE' AND email NOT LIKE $1
		RETURNING id
	`, "%@"+e.run+".example.test")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if _, err := e.db.Pool.Exec(context.Background(), `
		UPDATE users SET platform_role = NULL WHERE platform_role = 'ADMIN' AND email LIKE $1
	`, "%@"+e.run+".example.test"); err != nil {
		t.Fatal(err)
	}
	return ids
}
