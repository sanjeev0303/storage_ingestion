package identity

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/password"
)

// ErrBootstrapDone refuses a second bootstrap: once a platform
// administrator exists, accounts are made through it.
var ErrBootstrapDone = errors.New("a platform administrator already exists")

// Bootstrap creates the first platform administrator. With an empty pw a
// random 24-character password is generated and returned, to be shown
// once; nothing in the code base or the configuration holds a default.
func (s *Service) Bootstrap(ctx context.Context, email, displayName, pw string) (model.User, string, error) {
	email, err := normalizeEmail(email)
	if err != nil {
		return model.User{}, "", err
	}
	admins, err := s.deps.Users.CountPlatformAdmins(ctx)
	if err != nil {
		return model.User{}, "", err
	}
	if admins > 0 {
		return model.User{}, "", ErrBootstrapDone
	}
	if pw == "" {
		if pw, err = password.Generate(24); err != nil {
			return model.User{}, "", err
		}
	}
	if err := s.checkPassword(pw, email); err != nil {
		return model.User{}, "", err
	}
	hash, err := password.Hash(pw, s.params)
	if err != nil {
		return model.User{}, "", err
	}
	now := s.now()
	user := model.User{
		ID: newID(), Email: email, DisplayName: strings.TrimSpace(displayName), PasswordHash: hash,
		Status: model.UserActive, PlatformRole: model.PlatformAdmin, PasswordChangedAt: &now, CreatedAt: now, UpdatedAt: now,
	}
	operator := model.Principal{UserID: "bootstrap", Service: true}
	e := s.platformEvent(ctx, operator, "platform.bootstrap", "user", user.ID, map[string]any{"platform_role": string(model.PlatformAdmin)})
	if err := s.deps.Users.CreateUser(ctx, user, e); err != nil {
		return model.User{}, "", err
	}
	return user, pw, nil
}

// CredentialToken is a single-use activation or reset token: shown once
// to the administrator who asked for it, to pass on to the account's
// owner.
type CredentialToken struct {
	Token     string
	UserID    string
	ExpiresAt time.Time
}

// CreateUser provisions an account for a platform administrator. It
// starts PENDING; its owner activates it with the returned token.
func (s *Service) CreateUser(ctx context.Context, p model.Principal, email, displayName string, role model.PlatformRole) (model.User, CredentialToken, error) {
	if err := s.requirePlatform(ctx, p, model.PlatformManage, "user.create", "user", ""); err != nil {
		return model.User{}, CredentialToken{}, err
	}
	if err := s.throttle("admin", p.UserID); err != nil {
		return model.User{}, CredentialToken{}, err
	}
	email, err := normalizeEmail(email)
	if err != nil {
		return model.User{}, CredentialToken{}, err
	}
	if role != "" && role != model.PlatformAdmin && role != model.PlatformAuditor {
		return model.User{}, CredentialToken{}, fmt.Errorf("%w: platform_role must be ADMIN, AUDITOR or empty", ErrInvalidInput)
	}
	now := s.now()
	user := model.User{
		ID: newID(), Email: email, DisplayName: strings.TrimSpace(displayName), Status: model.UserPending,
		PlatformRole: role, CreatedAt: now, UpdatedAt: now,
	}
	e := s.platformEvent(ctx, p, "user.create", "user", user.ID, map[string]any{"platform_role": string(role)})
	if err := s.deps.Users.CreateUser(ctx, user, e); err != nil {
		return model.User{}, CredentialToken{}, err
	}
	token, err := s.issueCredential(ctx, p, user.ID, model.CredentialActivate)
	return user, token, err
}

// IssueReset gives a platform administrator a password-reset token for
// an account: the local recovery flow, since there is no email delivery.
func (s *Service) IssueReset(ctx context.Context, p model.Principal, userID string) (CredentialToken, error) {
	if err := s.requirePlatform(ctx, p, model.PlatformManage, "user.password_reset_issue", "user", userID); err != nil {
		return CredentialToken{}, err
	}
	user, err := s.deps.Users.GetUser(ctx, userID)
	if err != nil {
		return CredentialToken{}, err
	}
	purpose := model.CredentialReset
	if user.Status == model.UserPending {
		purpose = model.CredentialActivate
	}
	if user.Status == model.UserDeactivated {
		return CredentialToken{}, model.ErrAccountInactive
	}
	return s.issueCredential(ctx, p, userID, purpose)
}

// IssueResetByEmail is IssueReset for the operator CLI, which acts with
// direct database access rather than as a signed-in administrator.
func (s *Service) IssueResetByEmail(ctx context.Context, email string) (CredentialToken, error) {
	user, err := s.deps.Users.GetUserByEmail(ctx, email)
	if err != nil {
		return CredentialToken{}, err
	}
	operator := model.Principal{UserID: "operator-cli", Service: true, PlatformRole: model.PlatformAdmin}
	return s.IssueReset(ctx, operator, user.ID)
}

func (s *Service) issueCredential(ctx context.Context, p model.Principal, userID, purpose string) (CredentialToken, error) {
	token, hash, err := newToken(credentialPrefix)
	if err != nil {
		return CredentialToken{}, err
	}
	now := s.now()
	ttl := s.cfg.ResetTTL
	if purpose == model.CredentialActivate {
		ttl = s.cfg.ActivationTTL
	}
	action := "user.password_reset_issue"
	if purpose == model.CredentialActivate {
		action = "user.activation_issue"
	}
	e := s.platformEvent(ctx, p, action, "user", userID, map[string]any{"expires_in_seconds": int64(ttl.Seconds())})
	if err := s.deps.Users.CreateCredentialToken(ctx, hash, userID, purpose, p.UserID, now.Add(ttl), now, e); err != nil {
		return CredentialToken{}, err
	}
	return CredentialToken{Token: token, UserID: userID, ExpiresAt: now.Add(ttl)}, nil
}

// SetUserStatus deactivates or reactivates an account. Deactivating
// revokes all its sessions at once; its organizations' data and jobs are
// untouched. A reactivated account without a password stays PENDING.
func (s *Service) SetUserStatus(ctx context.Context, p model.Principal, userID string, active bool) (model.User, error) {
	action := "user.deactivate"
	if active {
		action = "user.reactivate"
	}
	if err := s.requirePlatform(ctx, p, model.PlatformManage, action, "user", userID); err != nil {
		return model.User{}, err
	}
	user, err := s.deps.Users.GetUser(ctx, userID)
	if err != nil {
		return model.User{}, err
	}
	status := model.UserDeactivated
	if active {
		status = model.UserActive
		if user.PasswordHash == "" {
			status = model.UserPending
		}
	} else {
		if userID == p.UserID {
			return model.User{}, s.deny(ctx, p, action, "user", userID, fmt.Errorf("%w: you cannot deactivate yourself", model.ErrForbidden))
		}
		if err := s.keepAnAdmin(ctx, user); err != nil {
			return model.User{}, err
		}
	}
	e := s.platformEvent(ctx, p, action, "user", userID, map[string]any{"before": string(user.Status), "after": string(status)})
	return s.deps.Users.SetUserStatus(ctx, userID, status, s.now(), e)
}

// SetPlatformRole grants or removes a platform role.
func (s *Service) SetPlatformRole(ctx context.Context, p model.Principal, userID string, role model.PlatformRole) error {
	if err := s.requirePlatform(ctx, p, model.PlatformManage, "user.platform_role", "user", userID); err != nil {
		return err
	}
	if role != "" && role != model.PlatformAdmin && role != model.PlatformAuditor {
		return fmt.Errorf("%w: platform_role must be ADMIN, AUDITOR or empty", ErrInvalidInput)
	}
	user, err := s.deps.Users.GetUser(ctx, userID)
	if err != nil {
		return err
	}
	if role != model.PlatformAdmin {
		if err := s.keepAnAdmin(ctx, user); err != nil {
			return err
		}
	}
	e := s.platformEvent(ctx, p, "user.platform_role", "user", userID,
		map[string]any{"before": string(user.PlatformRole), "after": string(role)})
	return s.deps.Users.SetPlatformRole(ctx, userID, role, s.now(), e)
}

// keepAnAdmin refuses a change that would leave no active platform
// administrator.
func (s *Service) keepAnAdmin(ctx context.Context, user model.User) error {
	if user.PlatformRole != model.PlatformAdmin || user.Status != model.UserActive {
		return nil
	}
	admins, err := s.deps.Users.CountPlatformAdmins(ctx)
	if err != nil {
		return err
	}
	if admins <= 1 {
		return fmt.Errorf("%w: the platform must keep one active administrator", model.ErrForbidden)
	}
	return nil
}

// ListUsers pages accounts for a platform administrator.
func (s *Service) ListUsers(ctx context.Context, p model.Principal, after string, limit int) ([]model.User, error) {
	if err := s.requirePlatform(ctx, p, model.PlatformManage, "user.list", "user", ""); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	return s.deps.Users.ListUsers(ctx, after, limit)
}

// SetUserStatusByEmail is SetUserStatus for the operator CLI.
func (s *Service) SetUserStatusByEmail(ctx context.Context, email string, active bool) (model.User, error) {
	user, err := s.deps.Users.GetUserByEmail(ctx, email)
	if err != nil {
		return model.User{}, err
	}
	operator := model.Principal{UserID: "operator-cli", Service: true, PlatformRole: model.PlatformAdmin}
	return s.SetUserStatus(ctx, operator, user.ID, active)
}
