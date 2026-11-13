package identity

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/correlation"
)

// APITokenPrefix starts every API token, so it is told from a session's
// access token at a glance (and by secret scanners).
const APITokenPrefix = "orb_pat_"

// legacyAPITokenPrefix started tokens issued before the Orbidune rename.
// They still authenticate; only the prefix changed, and it is the same length.
const legacyAPITokenPrefix = "osp_pat_"

// TokenStore finds API tokens (postgres.AccessRepository).
type TokenStore interface {
	TokenByHash(ctx context.Context, hash string) (model.APIToken, error)
	TouchToken(ctx context.Context, id, ip string, at time.Time) error
	// RehashToken replaces a token's stored hash, if it is still oldHash.
	RehashToken(ctx context.Context, id, oldHash, newHash string) error
}

// WithTokens lets API tokens authenticate.
func (s *Service) WithTokens(store TokenStore) *Service {
	s.tokens = store
	return s
}

// HashAPIToken is what is stored for a token. With a pepper it is an
// HMAC-SHA-256 keyed by it: whoever can read or write the token table but
// not the server's configuration can neither check a token against it nor
// plant one. Without a pepper it is the plain SHA-256 tokens were first
// stored as.
func HashAPIToken(raw string, pepper []byte) string {
	if len(pepper) == 0 {
		return hex.EncodeToString(hashToken(raw))
	}
	mac := hmac.New(sha256.New, pepper)
	mac.Write([]byte(raw))
	return hex.EncodeToString(mac.Sum(nil))
}

// authenticateToken checks an API token: live, unexpired, used from one
// of its networks, and its member's account active.
func (s *Service) authenticateToken(ctx context.Context, raw string) (Identity, error) {
	if s.tokens == nil || len(raw) != len(APITokenPrefix)+43 {
		return Identity{}, model.ErrSessionInvalid
	}
	token, err := s.lookupToken(ctx, raw)
	if err != nil {
		return Identity{}, model.ErrSessionInvalid
	}
	now, ip := s.now(), correlation.Source(ctx)
	if !token.Usable(now, ip) {
		return Identity{}, model.ErrSessionInvalid
	}
	user, err := s.deps.Users.GetUser(ctx, token.UserID)
	if err != nil || user.Status != model.UserActive {
		return Identity{}, model.ErrAccountInactive
	}
	// Recorded at most once a minute, so a busy script does not write on
	// every request.
	if token.LastUsedAt == nil || now.Sub(*token.LastUsedAt) > time.Minute {
		_ = s.tokens.TouchToken(ctx, token.ID, ip, now)
	}
	return Identity{User: user, Token: &token}, nil
}

// lookupToken finds a token by its peppered hash, or, for a token issued
// before the pepper was set, by its plain SHA-256. That older hash is
// replaced by the peppered one on first use, so it does not stay stored.
// Both lookups are by an index on the hash; no token value is compared
// here, so there is nothing to time.
func (s *Service) lookupToken(ctx context.Context, raw string) (model.APIToken, error) {
	pepper := s.cfg.APITokenPepper
	token, err := s.tokens.TokenByHash(ctx, HashAPIToken(raw, pepper))
	if err == nil || len(pepper) == 0 {
		return token, err
	}
	legacy := HashAPIToken(raw, nil)
	token, err = s.tokens.TokenByHash(ctx, legacy)
	if err != nil {
		return token, err
	}
	if err := s.tokens.RehashToken(ctx, token.ID, legacy, HashAPIToken(raw, pepper)); err != nil {
		s.deps.Log.Warn("API token hash not upgraded", "token_id", token.ID, "error", err)
	}
	return token, nil
}

// tokenPrincipal confines a token's principal: its own organization only,
// its permissions within the member's current role, its datasets.
func tokenPrincipal(p model.Principal, token *model.APIToken) model.Principal {
	p.Permissions &= token.Permissions
	p.Scope = model.Scope{TokenID: token.ID, Datasets: token.Datasets, NoLinks: !token.AllowLinks, Restricted: len(token.Datasets) > 0, PathPrefix: token.PathPrefix}
	return p
}

func isAPIToken(raw string) bool {
	return strings.HasPrefix(raw, APITokenPrefix) || strings.HasPrefix(raw, legacyAPITokenPrefix)
}
