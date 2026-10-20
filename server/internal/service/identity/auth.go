package identity

import (
	"context"
	"errors"
	"strings"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/password"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/ratelimit"
	"github.com/sanjeev0303/object-storage-platform/internal/service/audit"
)

// Client describes where a request came from, as the server determined
// it.
type Client struct {
	IP        string
	UserAgent string
}

// Login checks an email and password and starts a session.
//
// Failures are throttled per email and address, and per address alone, in
// memory. They all answer model.ErrInvalidCredentials, and an unknown
// email costs the same hashing as a wrong password, so the answer does not
// tell whether an account exists. Only someone with the right password
// learns that the account is inactive.
func (s *Service) Login(ctx context.Context, email, pw string, client Client) (Tokens, error) {
	email = strings.TrimSpace(email)
	emailKey := "login:" + strings.ToLower(email) + "|" + client.IP
	ipKey := "login-ip:" + client.IP
	emailRule := ratelimit.Rule{Requests: s.cfg.LoginAttempts, Window: s.cfg.LoginWindow}
	ipRule := ratelimit.Rule{Requests: s.cfg.LoginIPAttempts, Window: s.cfg.LoginWindow}
	for _, check := range []struct {
		key  string
		rule ratelimit.Rule
	}{{emailKey, emailRule}, {ipKey, ipRule}} {
		if over, wait := s.deps.Limiter.Exceeded(check.key, check.rule); over {
			e := audit.Anonymous(ctx, "auth.login", "session", "")
			e.Outcome, e.Reason = model.AuditDenied, "throttled"
			s.deps.Audit.Record(ctx, e)
			return Tokens{}, &model.RateLimitError{RetryAfter: wait}
		}
	}

	fail := func(userID, reason string) error {
		s.deps.Limiter.Add(emailKey, emailRule)
		s.deps.Limiter.Add(ipKey, ipRule)
		e := audit.Anonymous(ctx, "auth.login", "user", userID)
		e.Outcome, e.Reason = model.AuditFailed, reason
		s.deps.Audit.Record(ctx, e)
		return model.ErrInvalidCredentials
	}

	user, err := s.deps.Users.GetUserByEmail(ctx, email)
	if errors.Is(err, model.ErrUserNotFound) || (err == nil && user.PasswordHash == "") {
		_, _ = password.Verify(pw, s.dummyHash)
		return Tokens{}, fail(user.ID, "unknown account or no password")
	}
	if err != nil {
		return Tokens{}, err
	}
	ok, err := password.Verify(pw, user.PasswordHash)
	if err != nil || !ok {
		return Tokens{}, fail(user.ID, "wrong password")
	}
	if user.Status != model.UserActive {
		e := audit.Anonymous(ctx, "auth.login", "user", user.ID)
		e.Outcome, e.Reason = model.AuditDenied, "account "+strings.ToLower(string(user.Status))
		s.deps.Audit.Record(ctx, e)
		return Tokens{}, model.ErrAccountInactive
	}

	s.deps.Limiter.Reset(emailKey)
	if password.NeedsRehash(user.PasswordHash, s.params) {
		if stronger, err := password.Hash(pw, s.params); err == nil {
			_ = s.deps.Users.RehashPassword(ctx, user.ID, stronger)
		}
	}
	return s.startSession(ctx, user, client)
}

func (s *Service) startSession(ctx context.Context, user model.User, client Client) (Tokens, error) {
	access, accessHash, err := newToken(accessPrefix)
	if err != nil {
		return Tokens{}, err
	}
	refresh, refreshHash, err := newToken(refreshPrefix)
	if err != nil {
		return Tokens{}, err
	}
	now := s.now()
	session := model.Session{
		ID: newID(), UserID: user.ID, AccessExpiresAt: now.Add(s.cfg.AccessTokenTTL), ExpiresAt: now.Add(s.cfg.RefreshTokenTTL),
		SourceIP: client.IP, UserAgent: client.UserAgent, CreatedAt: now, LastUsedAt: now,
	}
	p := model.Principal{UserID: user.ID, SessionID: session.ID}
	e := s.event(ctx, p, "auth.login", "session", session.ID, nil)
	if err := s.deps.Users.CreateSession(ctx, session, accessHash, refreshHash, e); err != nil {
		return Tokens{}, err
	}
	_ = s.deps.Users.RecordLogin(ctx, user.ID, now)
	return Tokens{
		AccessToken: access, AccessExpiresAt: session.AccessExpiresAt,
		RefreshToken: refresh, RefreshExpiresAt: session.ExpiresAt, SessionID: session.ID, User: user,
	}, nil
}

// Refresh spends a refresh token for a new token pair. The old access
// token stops working at once. A spent token presented again revokes the
// session.
func (s *Service) Refresh(ctx context.Context, refreshToken string, client Client) (Tokens, error) {
	if err := s.throttle("refresh", client.IP); err != nil {
		return Tokens{}, err
	}
	hash, ok := tokenHash(refreshToken, refreshPrefix)
	if !ok {
		return Tokens{}, model.ErrSessionInvalid
	}
	access, accessHash, err := newToken(accessPrefix)
	if err != nil {
		return Tokens{}, err
	}
	refresh, refreshHash, err := newToken(refreshPrefix)
	if err != nil {
		return Tokens{}, err
	}
	now := s.now()
	session, user, err := s.deps.Users.RotateRefresh(ctx, hash, accessHash, refreshHash, now.Add(s.cfg.AccessTokenTTL), now)
	if errors.Is(err, model.ErrRefreshReplayed) {
		p := model.Principal{UserID: session.UserID, SessionID: session.ID}
		e := audit.Denied(ctx, p, "auth.refresh", "session", session.ID, "refresh token reused; session revoked")
		s.deps.Audit.Record(ctx, e)
		return Tokens{}, err
	}
	if err != nil {
		return Tokens{}, err
	}
	if user.Status != model.UserActive {
		return Tokens{}, model.ErrAccountInactive
	}
	return Tokens{
		AccessToken: access, AccessExpiresAt: session.AccessExpiresAt,
		RefreshToken: refresh, RefreshExpiresAt: session.ExpiresAt, SessionID: session.ID, User: user,
	}, nil
}

// Logout ends the caller's session.
func (s *Service) Logout(ctx context.Context, p model.Principal) error {
	e := s.event(ctx, p, "auth.logout", "session", p.SessionID, nil)
	e.TenantID = ""
	return s.deps.Users.RevokeSession(ctx, p.UserID, p.SessionID, "logout", s.now(), e)
}

// RevokeSession ends one of the caller's own sessions.
func (s *Service) RevokeSession(ctx context.Context, p model.Principal, sessionID string) error {
	e := s.event(ctx, p, "auth.session_revoke", "session", sessionID, nil)
	e.TenantID = ""
	// model.ErrSessionInvalid when it is not one of the caller's live
	// sessions.
	return s.deps.Users.RevokeSession(ctx, p.UserID, sessionID, "revoked by its user", s.now(), e)
}

// Sessions lists the caller's live sessions.
func (s *Service) Sessions(ctx context.Context, p model.Principal) ([]model.Session, error) {
	return s.deps.Users.ListSessions(ctx, p.UserID, s.now())
}

// ChangePassword replaces the caller's password after checking the
// current one. Every other session of the account ends.
func (s *Service) ChangePassword(ctx context.Context, p model.Principal, current, next string) error {
	if err := s.throttle("credentials", "user:"+p.UserID); err != nil {
		return err
	}
	user, err := s.deps.Users.GetUser(ctx, p.UserID)
	if err != nil {
		return err
	}
	if ok, err := password.Verify(current, user.PasswordHash); err != nil || !ok {
		return s.deny(ctx, p, "auth.password_change", "user", p.UserID, model.ErrInvalidCredentials)
	}
	if err := s.checkPassword(next, user.Email); err != nil {
		return err
	}
	if next == current {
		return model.ErrWeakPassword
	}
	hash, err := password.Hash(next, s.params)
	if err != nil {
		return err
	}
	e := s.event(ctx, p, "auth.password_change", "user", p.UserID, nil)
	e.TenantID = ""
	return s.deps.Users.ChangePassword(ctx, p.UserID, hash, p.SessionID, s.now(), e)
}

// Activate sets the first password of an account from its activation
// token.
func (s *Service) Activate(ctx context.Context, token, pw string, client Client) (model.User, error) {
	return s.consume(ctx, token, pw, model.CredentialActivate, "auth.activate", client)
}

// ResetPassword sets a new password from a reset token an administrator
// issued. Every session of the account ends.
func (s *Service) ResetPassword(ctx context.Context, token, pw string, client Client) (model.User, error) {
	return s.consume(ctx, token, pw, model.CredentialReset, "auth.password_reset", client)
}

func (s *Service) consume(ctx context.Context, token, pw, purpose, action string, client Client) (model.User, error) {
	if err := s.throttle("credentials", client.IP); err != nil {
		return model.User{}, err
	}
	hash, ok := tokenHash(token, credentialPrefix)
	if !ok {
		return model.User{}, model.ErrCredentialToken
	}
	if err := s.checkPassword(pw, ""); err != nil {
		return model.User{}, err
	}
	pwHash, err := password.Hash(pw, s.params)
	if err != nil {
		return model.User{}, err
	}
	user, err := s.deps.Users.ConsumeCredentialToken(ctx, hash, purpose, pwHash, s.now(), func(u model.User) model.AuditEvent {
		e := s.event(ctx, model.Principal{UserID: u.ID}, action, "user", u.ID, nil)
		e.TenantID = ""
		return e
	})
	if err != nil {
		e := audit.Anonymous(ctx, action, "user", "")
		e.Outcome, e.Reason = model.AuditFailed, err.Error()
		s.deps.Audit.Record(ctx, e)
	}
	return user, err
}

// Me is the caller's account and memberships.
type Me struct {
	User          model.User
	Memberships   []model.Membership
	Organizations []model.Organization
}

func (s *Service) Me(ctx context.Context, p model.Principal) (Me, error) {
	user, err := s.deps.Users.GetUser(ctx, p.UserID)
	if err != nil {
		return Me{}, err
	}
	members, orgs, err := s.deps.Tenancy.UserMemberships(ctx, p.UserID)
	return Me{User: user, Memberships: members, Organizations: orgs}, err
}
