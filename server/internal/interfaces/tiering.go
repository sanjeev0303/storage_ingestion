package interfaces

import (
	"context"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// TierStorage moves objects between a provider's storage classes and
// restores archived ones (docs/tiering.md).
type TierStorage interface {
	// SetTier rewrites the object in place in tier, from the bytes with
	// etag only: an object changed meanwhile fails instead of moving. It
	// returns the object as stored now; a provider that copies to change
	// class makes a new version, and the caller removes the old one.
	SetTier(ctx context.Context, bucket, key, etag string, size int64, tier model.StorageTier) (ObjectMetadata, error)
	// TierState is the version's tier and, for an archived one, whether a
	// restore is under way and until when it is readable.
	TierState(ctx context.Context, bucket, key, versionID string) (TierState, error)
	// Restore asks for an archived version to be readable for days.
	Restore(ctx context.Context, bucket, key, versionID string, days int, speed model.RestoreSpeed) error
}

type TierState struct {
	Tier          model.StorageTier
	Restoring     bool
	ReadableUntil *time.Time
}
