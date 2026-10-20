package memory

import (
	"context"
	"sync"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// DownloadTokenRepository keeps download tokens in memory. For tests only.
type DownloadTokenRepository struct {
	mu     sync.Mutex
	tokens map[string]model.DownloadToken
}

var _ interfaces.DownloadTokenRepository = (*DownloadTokenRepository)(nil)

func NewDownloadTokenRepository() *DownloadTokenRepository {
	return &DownloadTokenRepository{tokens: make(map[string]model.DownloadToken)}
}

func (r *DownloadTokenRepository) Create(ctx context.Context, token model.DownloadToken) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tokens[token.TokenHash] = token
	return nil
}

func (r *DownloadTokenRepository) GetByHash(ctx context.Context, tokenHash string) (model.DownloadToken, error) {
	if err := ctx.Err(); err != nil {
		return model.DownloadToken{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	token, ok := r.tokens[tokenHash]
	if !ok {
		return model.DownloadToken{}, model.ErrDownloadTokenNotFound
	}
	return token, nil
}

func (r *DownloadTokenRepository) RevokeForAsset(ctx context.Context, tenantID, assetID string, at time.Time) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	revoked := 0
	for hash, token := range r.tokens {
		if token.TenantID == tenantID && token.AssetID == assetID && token.RevokedAt == nil {
			revokedAt := at
			token.RevokedAt = &revokedAt
			r.tokens[hash] = token
			revoked++
		}
	}
	return revoked, nil
}

// Tokens returns a snapshot, for assertions.
func (r *DownloadTokenRepository) Tokens() []model.DownloadToken {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]model.DownloadToken, 0, len(r.tokens))
	for _, token := range r.tokens {
		out = append(out, token)
	}
	return out
}
