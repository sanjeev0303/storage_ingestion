package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// DownloadTokenRepository stores revocable download links by token hash.
type DownloadTokenRepository struct {
	db *DB
}

var _ interfaces.DownloadTokenRepository = (*DownloadTokenRepository)(nil)

func NewDownloadTokenRepository(db *DB) *DownloadTokenRepository {
	return &DownloadTokenRepository{db: db}
}

func (r *DownloadTokenRepository) Create(ctx context.Context, token model.DownloadToken) error {
	_, err := r.db.Pool.Exec(ctx, `
		INSERT INTO download_tokens
			(id, token_hash, tenant_id, asset_id, principal_id, filename, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''), $7, $8)
	`, token.ID, token.TokenHash, token.TenantID, token.AssetID, token.PrincipalID,
		token.Filename, token.CreatedAt, token.ExpiresAt)
	if err != nil {
		return fmt.Errorf("insert download token: %w", err)
	}
	return nil
}

func (r *DownloadTokenRepository) GetByHash(ctx context.Context, tokenHash string) (model.DownloadToken, error) {
	var token model.DownloadToken
	err := r.db.Pool.QueryRow(ctx, `
		SELECT id, token_hash, tenant_id, asset_id, principal_id, COALESCE(filename, ''),
		       created_at, expires_at, revoked_at
		FROM download_tokens
		WHERE token_hash = $1
	`, tokenHash).Scan(&token.ID, &token.TokenHash, &token.TenantID, &token.AssetID, &token.PrincipalID,
		&token.Filename, &token.CreatedAt, &token.ExpiresAt, &token.RevokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.DownloadToken{}, model.ErrDownloadTokenNotFound
	}
	if err != nil {
		return model.DownloadToken{}, fmt.Errorf("get download token: %w", err)
	}
	return token, nil
}

func (r *DownloadTokenRepository) RevokeForAsset(ctx context.Context, tenantID, assetID string, at time.Time) (int, error) {
	tag, err := r.db.Pool.Exec(ctx, `
		UPDATE download_tokens
		SET revoked_at = $3
		WHERE tenant_id = $1 AND asset_id = $2 AND revoked_at IS NULL
	`, tenantID, assetID, at)
	if err != nil {
		return 0, fmt.Errorf("revoke download tokens: %w", err)
	}
	return int(tag.RowsAffected()), nil
}
