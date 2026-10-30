package identity

import (
	"context"
	"encoding/hex"
	"strings"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/correlation"
)

// APITokenPrefix starts every API token, so it is told from a session's
// access token at a glance (and by secret scanners).
const APITokenPrefix = "osp_pat_"

// TokenStore finds API tokens (postgres.AccessRepository).
type TokenStore interface {
	TokenByHash(ctx context.Context, hash string) (model.APIToken, error)
	TouchToken(ctx context.Context, id, ip string, at time.Time) error
}

// WithTokens lets API tokens authenticate.
func (s *Service) WithTokens(store TokenStore) *Service {
	s.tokens = store
	return s
}

// HashAPIToken is what is stored for a token.
func HashAPIToken(raw string) string {
	return hex.EncodeToString(hashToken(raw))
}

// authenticateToken checks an API token: live, unexpired, used from one
// of its networks, and its member's account active.
func (s *Service) authenticateToken(ctx context.Context, raw string) (Identity, error) {
	if s.tokens == nil || len(raw) != len(APITokenPrefix)+43 {
		return Identity{}, model.ErrSessionInvalid
	}
	token, err := s.tokens.TokenByHash(ctx, HashAPIToken(raw))
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

// tokenPrincipal confines a token's principal: its own organization only,
// its permissions within the member's current role, its datasets.
func tokenPrincipal(p model.Principal, token *model.APIToken) model.Principal {
	p.Permissions &= token.Permissions
	p.Scope = model.Scope{TokenID: token.ID, Datasets: token.Datasets, NoLinks: !token.AllowLinks, Restricted: len(token.Datasets) > 0}
	return p
}

func isAPIToken(raw string) bool { return strings.HasPrefix(raw, APITokenPrefix) }
