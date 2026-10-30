package azure

import (
	"context"
	"fmt"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

var _ interfaces.TierStorage = (*Storage)(nil)

// Azure access tiers for each tier. Only Archive needs rehydrating
// before a read.
var azureTiers = map[model.StorageTier]blob.AccessTier{
	model.TierStandard: blob.AccessTierHot, model.TierInfrequent: blob.AccessTierCool,
	model.TierArchive: blob.AccessTierCold, model.TierDeepArchive: blob.AccessTierArchive,
}

// SetTier changes the blob's access tier in place; the bytes, ETag and
// version stay. Azure's Set Blob Tier takes no ETag condition, so the
// ETag is checked just before.
func (s *Storage) SetTier(ctx context.Context, bucket, key, etag string, size int64, tier model.StorageTier) (interfaces.ObjectMetadata, error) {
	access, ok := azureTiers[tier]
	if !ok {
		return interfaces.ObjectMetadata{}, fmt.Errorf("%w: no Azure access tier for %s", model.ErrTierUnsupported, tier)
	}
	b := s.blob(bucket, key)
	before, err := s.HeadObject(ctx, bucket, key)
	if err != nil {
		return interfaces.ObjectMetadata{}, err
	}
	if before.ETag != etag || before.SizeBytes != size {
		return interfaces.ObjectMetadata{}, fmt.Errorf("change access tier: %w", interfaces.ErrPreconditionFailed)
	}
	if _, err := b.SetTier(ctx, access, nil); err != nil {
		return interfaces.ObjectMetadata{}, classify("change access tier", err)
	}
	return s.HeadObject(ctx, bucket, key)
}

func (s *Storage) TierState(ctx context.Context, bucket, key, versionID string) (interfaces.TierState, error) {
	b := s.blob(bucket, key)
	if versionID != "" {
		v, err := s.version(bucket, key, versionID)
		if err != nil {
			return interfaces.TierState{}, err
		}
		b = v
	}
	p, err := b.GetProperties(ctx, nil)
	if err != nil {
		return interfaces.TierState{}, classify("read access tier", err)
	}
	state := interfaces.TierState{Tier: model.TierStandard}
	if p.AccessTier != nil {
		for tier, access := range azureTiers {
			if strings.EqualFold(string(access), *p.AccessTier) {
				state.Tier = tier
			}
		}
	}
	state.Restoring = p.ArchiveStatus != nil && strings.HasPrefix(*p.ArchiveStatus, "rehydrate-pending")
	return state, nil
}

// Restore rehydrates an archived blob to Hot. Unlike S3 it stays there:
// days does not apply, and a tier rule moves it again later.
func (s *Storage) Restore(ctx context.Context, bucket, key, versionID string, _ int, speed model.RestoreSpeed) error {
	b := s.blob(bucket, key)
	if versionID != "" {
		v, err := s.version(bucket, key, versionID)
		if err != nil {
			return err
		}
		b = v
	}
	priority := blob.RehydratePriorityStandard
	if speed == model.RestoreFast {
		priority = blob.RehydratePriorityHigh
	}
	if _, err := b.SetTier(ctx, blob.AccessTierHot, &blob.SetTierOptions{RehydratePriority: &priority}); err != nil {
		return classify("rehydrate archived blob", err)
	}
	return nil
}
