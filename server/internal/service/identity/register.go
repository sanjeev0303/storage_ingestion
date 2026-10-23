package identity

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/password"
)

// ErrRegistrationClosed answers a sign-up when AUTH_SELF_REGISTRATION is
// off, which is the default: accounts then come from an administrator or
// an invitation.
var ErrRegistrationClosed = errors.New("self-registration is not enabled")

// Registration is what someone gives to create their own account and the
// organization they will own.
type Registration struct {
	Email            string
	Password         string
	DisplayName      string
	OrganizationName string
}

// Register creates an active account and an organization it owns, then
// signs the new owner in.
//
// There is no email delivery in the platform, so the address is not
// verified: whoever registers an address owns that account. That is why
// registration is off unless an operator turns it on, and why it is
// throttled per source address.
func (s *Service) Register(ctx context.Context, r Registration, client Client) (Tokens, error) {
	if !s.cfg.SelfRegistration {
		return Tokens{}, ErrRegistrationClosed
	}
	if err := s.throttle("register", client.IP); err != nil {
		return Tokens{}, err
	}
	email, err := normalizeEmail(r.Email)
	if err != nil {
		return Tokens{}, err
	}
	email = strings.ToLower(email)
	orgName := strings.TrimSpace(r.OrganizationName)
	if orgName == "" || len(orgName) > 200 {
		return Tokens{}, fmt.Errorf("%w: organization name is required, at most 200 characters", ErrInvalidInput)
	}
	displayName := strings.TrimSpace(r.DisplayName)
	if len(displayName) > 200 {
		return Tokens{}, fmt.Errorf("%w: name is at most 200 characters", ErrInvalidInput)
	}
	if err := s.checkPassword(r.Password, email); err != nil {
		return Tokens{}, err
	}
	slug, err := newSlug(orgName)
	if err != nil {
		return Tokens{}, err
	}
	hash, err := password.Hash(r.Password, s.params)
	if err != nil {
		return Tokens{}, err
	}

	now := s.now()
	user := model.User{
		ID: newID(), Email: email, DisplayName: displayName, PasswordHash: hash,
		Status: model.UserActive, PasswordChangedAt: &now, CreatedAt: now, UpdatedAt: now,
	}
	self := model.Principal{UserID: user.ID}
	created := s.platformEvent(ctx, self, "user.create", "user", user.ID, map[string]any{"via": "self_registration"})
	if err := s.deps.Users.CreateUser(ctx, user, created); err != nil {
		return Tokens{}, err
	}

	org := model.Organization{ID: newID(), Name: orgName, Slug: slug, Status: model.OrgActive, CreatedBy: user.ID, CreatedAt: now, UpdatedAt: now}
	owner := model.Membership{
		OrgID: org.ID, UserID: user.ID, RoleID: model.RoleOwner, Status: model.MembershipActive,
		CreatedBy: user.ID, CreatedAt: now, UpdatedAt: now,
	}
	orgEvent := s.event(ctx, self, "organization.create", "organization", org.ID, map[string]any{"slug": slug, "owner": user.ID, "via": "self_registration"})
	orgEvent.TenantID = org.ID
	if err := s.deps.Tenancy.CreateOrganization(ctx, org, owner, orgEvent); err != nil {
		// The account exists but owns nothing. An administrator can finish
		// it; the person can sign in and ask for an invitation.
		s.deps.Log.Error("self-registration created an account but not its organization", "user_id", user.ID, "error", err)
		return Tokens{}, err
	}
	return s.startSession(ctx, user, client)
}

// newSlug makes an organization slug from its name, with a random tail so
// two people naming their organization alike never collide.
func newSlug(name string) (string, error) {
	var b strings.Builder
	dash := true
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		case !dash:
			b.WriteByte('-')
			dash = true
		}
	}
	base := strings.Trim(b.String(), "-")
	if len(base) > 40 {
		base = strings.Trim(base[:40], "-")
	}
	if base == "" {
		base = "org"
	}
	var tail [4]byte
	if _, err := rand.Read(tail[:]); err != nil {
		return "", fmt.Errorf("generate slug: %w", err)
	}
	return base + "-" + hex.EncodeToString(tail[:]), nil
}
