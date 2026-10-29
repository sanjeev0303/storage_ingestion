// Package identity authenticates users and authorizes them in their
// organizations.
//
// Authentication is token-based: signing in creates a session with a
// short-lived access token and a refresh token. Both are opaque random
// strings; only their SHA-256 is stored, so a database leak gives no
// usable token. Refreshing spends the refresh token and issues a new pair;
// presenting a spent refresh token again revokes the session, since one
// of its two holders is not its owner.
//
// A request names an organization; the caller's membership in it, checked
// on every request, gives the permissions. Removing a member or changing
// a role therefore takes effect on the member's next request.
package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sanjeev0303/object-storage-platform/internal/dto/event"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/config"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/password"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/ratelimit"
	"github.com/sanjeev0303/object-storage-platform/internal/service/audit"
)

var ErrInvalidInput = errors.New("invalid request")

// Dependencies of the identity service.
type Dependencies struct {
	Users   interfaces.IdentityRepository
	Tenancy interfaces.TenancyRepository
	Audit   interfaces.AuditRecorder
	Limiter *ratelimit.Limiter
	Log     *slog.Logger
}

type Service struct {
	deps   Dependencies
	cfg    config.Auth
	rates  map[string]config.RateRule
	params password.Params
	// dummyHash is verified against when an email is unknown, so a failed
	// sign-in takes as long whether or not the account exists.
	dummyHash string
	now       func() time.Time
	// sso is nil while single sign-on is off (EnableSSO).
	sso *sso
}

func NewService(deps Dependencies, sec config.Security) (*Service, error) {
	if deps.Users == nil || deps.Tenancy == nil || deps.Audit == nil || deps.Limiter == nil || deps.Log == nil {
		return nil, errors.New("identity service dependencies are required")
	}
	if err := sec.Validate(); err != nil {
		return nil, err
	}
	params := password.Params{MemoryKiB: sec.Auth.Argon2Memory, Time: sec.Auth.Argon2Time, Threads: sec.Auth.Argon2Threads}
	dummy, err := password.Hash("not a password anyone has", params)
	if err != nil {
		return nil, err
	}
	return &Service{
		deps: deps, cfg: sec.Auth, rates: sec.Rates, params: params, dummyHash: dummy,
		now: func() time.Time { return time.Now().UTC() },
	}, nil
}

// ─── Tokens ───────────────────────────────────────────────────────────

// Token prefixes tell what a token is for, so one kind can never be used
// as another.
const (
	accessPrefix     = "sat_"
	refreshPrefix    = "srt_"
	credentialPrefix = "sct_"
	invitationPrefix = "sit_"
)

// newToken returns a random token with prefix and the hash to store.
func newToken(prefix string) (string, []byte, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", nil, fmt.Errorf("generate token: %w", err)
	}
	token := prefix + base64.RawURLEncoding.EncodeToString(raw[:])
	return token, hashToken(token), nil
}

func hashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// tokenHash checks a presented token's shape and hashes it. A token of
// the wrong kind or shape is simply invalid.
func tokenHash(token, prefix string) ([]byte, bool) {
	if !strings.HasPrefix(token, prefix) || len(token) != len(prefix)+43 {
		return nil, false
	}
	return hashToken(token), true
}

// Tokens are what signing in or refreshing returns. They are shown to the
// client once and never logged or recorded.
type Tokens struct {
	AccessToken      string
	AccessExpiresAt  time.Time
	RefreshToken     string
	RefreshExpiresAt time.Time
	SessionID        string
	User             model.User
}

// ─── Requests ─────────────────────────────────────────────────────────

// Identity is an authenticated user and the session they used.
type Identity struct {
	User    model.User
	Session model.Session
}

// Authenticate checks an access token: it must be a live session's
// current access token, and its account active.
func (s *Service) Authenticate(ctx context.Context, accessToken string) (Identity, error) {
	hash, ok := tokenHash(accessToken, accessPrefix)
	if !ok {
		return Identity{}, model.ErrSessionInvalid
	}
	session, user, err := s.deps.Users.SessionByAccess(ctx, hash, s.now())
	if err != nil {
		return Identity{}, err
	}
	if user.Status != model.UserActive {
		return Identity{}, model.ErrAccountInactive
	}
	return Identity{User: user, Session: session}, nil
}

// Principal resolves the caller of a request. With orgID, the caller must
// be an active member of that active organization, and gets its role's
// permissions. Without one, a caller with exactly one active membership
// works in that organization; anyone else gets no tenant, which only
// account and platform routes accept.
func (s *Service) Principal(ctx context.Context, id Identity, orgID string) (model.Principal, error) {
	p := model.Principal{UserID: id.User.ID, SessionID: id.Session.ID, PlatformRole: id.User.PlatformRole}
	if orgID == "" {
		members, orgs, err := s.deps.Tenancy.UserMemberships(ctx, id.User.ID)
		if err != nil {
			return p, err
		}
		var usable []string
		for i, m := range members {
			if m.Status == model.MembershipActive && orgs[i].Status == model.OrgActive {
				usable = append(usable, m.OrgID)
			}
		}
		if len(usable) != 1 {
			return p, nil
		}
		orgID = usable[0]
	}

	org, member, role, err := s.deps.Tenancy.Access(ctx, orgID, id.User.ID)
	if errors.Is(err, model.ErrMembershipNotFound) {
		// The same answer whether the organization exists or not.
		return p, model.ErrForbidden
	}
	if err != nil {
		return p, err
	}
	if member.Status != model.MembershipActive {
		return p, model.ErrForbidden
	}
	if org.Status != model.OrgActive {
		return p, model.ErrOrgInactive
	}
	p.TenantID, p.RoleID, p.Permissions = org.ID, role.ID, role.Permissions
	return p, nil
}

// ─── Helpers ──────────────────────────────────────────────────────────

func (s *Service) rule(name string) ratelimit.Rule {
	r := s.rates[name]
	return ratelimit.Rule{Requests: r.Requests, Window: r.Window}
}

// throttle counts one request against a rate rule.
func (s *Service) throttle(name, key string) error {
	if ok, wait := s.deps.Limiter.Allow(name+":"+key, s.rule(name)); !ok {
		return &model.RateLimitError{RetryAfter: wait}
	}
	return nil
}

// checkPassword applies the password policy: a minimum length, a maximum
// that keeps hashing bounded, and not the account's email.
func (s *Service) checkPassword(pw, email string) error {
	n := utf8.RuneCountInString(pw)
	switch {
	case n < s.cfg.PasswordMinLength:
		return fmt.Errorf("%w: at least %d characters", model.ErrWeakPassword, s.cfg.PasswordMinLength)
	case len(pw) > 256:
		return fmt.Errorf("%w: at most 256 bytes", model.ErrWeakPassword)
	case email != "" && strings.EqualFold(pw, email):
		return fmt.Errorf("%w: must not be the email address", model.ErrWeakPassword)
	}
	return nil
}

// normalizeEmail checks an email's shape: one @, something on both sides,
// no spaces or control characters.
func normalizeEmail(email string) (string, error) {
	email = strings.TrimSpace(email)
	local, domain, ok := strings.Cut(email, "@")
	if !ok || local == "" || !strings.Contains(domain, ".") || strings.Contains(domain, "@") || len(email) > 254 {
		return "", fmt.Errorf("%w: a valid email address is required", ErrInvalidInput)
	}
	for _, r := range email {
		if r <= ' ' || r == 0x7f {
			return "", fmt.Errorf("%w: a valid email address is required", ErrInvalidInput)
		}
	}
	return email, nil
}

func newID() string { return event.NewID() }

// event is an audit event by p about a resource.
func (s *Service) event(ctx context.Context, p model.Principal, action, resourceType, resourceID string, details map[string]any) model.AuditEvent {
	e := audit.From(ctx, p, action, resourceType, resourceID)
	e.Details = details
	return e
}

// deny records a refused attempt and returns err.
func (s *Service) deny(ctx context.Context, p model.Principal, action, resourceType, resourceID string, err error) error {
	s.deps.Audit.Record(ctx, audit.Denied(ctx, p, action, resourceType, resourceID, err.Error()))
	return err
}

// require checks a tenant permission, recording a refusal.
func (s *Service) require(ctx context.Context, p model.Principal, perm model.Permission, action, resourceType, resourceID string) error {
	return audit.Require(ctx, s.deps.Audit, p, perm, action, resourceType, resourceID)
}

// requirePlatform checks a platform permission, recording a refusal.
func (s *Service) requirePlatform(ctx context.Context, p model.Principal, perm model.PlatformPermission, action, resourceType, resourceID string) error {
	if err := p.RequirePlatform(perm); err != nil {
		e := audit.Denied(ctx, p, action, resourceType, resourceID, err.Error())
		e.TenantID = ""
		s.deps.Audit.Record(ctx, e)
		return err
	}
	return nil
}

// platformEvent is an audit event in the platform's chain.
func (s *Service) platformEvent(ctx context.Context, p model.Principal, action, resourceType, resourceID string, details map[string]any) model.AuditEvent {
	e := s.event(ctx, p, action, resourceType, resourceID, details)
	e.TenantID = ""
	return e
}

// PrincipalFor rebuilds a user's current principal in an organization,
// for credentials that outlive a request (download links): the account
// must still be active and the membership still allow it.
func (s *Service) PrincipalFor(ctx context.Context, tenantID, userID string) (model.Principal, error) {
	user, err := s.deps.Users.GetUser(ctx, userID)
	if err != nil {
		return model.Principal{}, err
	}
	if user.Status != model.UserActive {
		return model.Principal{}, model.ErrAccountInactive
	}
	if tenantID == "" {
		return model.Principal{}, model.ErrTenantRequired
	}
	return s.Principal(ctx, Identity{User: user}, tenantID)
}
