package interfaces

import (
	"context"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// DeliveryAuditRepository appends delivery decisions. It is append-only.
type DeliveryAuditRepository interface {
	Record(ctx context.Context, event model.DeliveryAuditEvent) error
}

// DownloadTokenRepository stores revocable download links by the SHA-256
// of the token, never the token itself.
type DownloadTokenRepository interface {
	Create(ctx context.Context, token model.DownloadToken) error
	// GetByHash returns the token with this hash, revoked or expired ones
	// included, so the caller can tell why it is refused.
	GetByHash(ctx context.Context, tokenHash string) (model.DownloadToken, error)
	// RevokeForAsset revokes every live token for the asset and returns
	// how many it revoked.
	RevokeForAsset(ctx context.Context, tenantID, assetID string, at time.Time) (int, error)
}
