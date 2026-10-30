package gcs

import (
	"context"
	"fmt"
	"strconv"

	"cloud.google.com/go/storage"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

var _ interfaces.TierStorage = (*Storage)(nil)

// GCS storage classes for each tier. Every class reads directly, so an
// archived object needs no restore (it costs a retrieval fee instead).
var gcsClasses = map[model.StorageTier]string{
	model.TierStandard: "STANDARD", model.TierInfrequent: "NEARLINE", model.TierArchive: "COLDLINE", model.TierDeepArchive: "ARCHIVE",
}

// SetTier rewrites the object onto itself in the tier's class, from the
// generation etag names only.
func (s *Storage) SetTier(ctx context.Context, bucket, key, etag string, size int64, tier model.StorageTier) (interfaces.ObjectMetadata, error) {
	class, ok := gcsClasses[tier]
	if !ok {
		return interfaces.ObjectMetadata{}, fmt.Errorf("%w: no GCS storage class for %s", model.ErrTierUnsupported, tier)
	}
	gen, err := generation(etag)
	if err != nil {
		return interfaces.ObjectMetadata{}, err
	}
	src := s.object(bucket, key).If(storage.Conditions{GenerationMatch: gen})
	copier := s.object(bucket, key).CopierFrom(src)
	copier.StorageClass = class
	copier.DestinationKMSKeyName = s.kmsKey
	attrs, err := copier.Run(ctx)
	if err != nil {
		return interfaces.ObjectMetadata{}, classify("change storage class", err)
	}
	return metadataOf(attrs), nil
}

func (s *Storage) TierState(ctx context.Context, bucket, key, versionID string) (interfaces.TierState, error) {
	obj := s.object(bucket, key)
	if versionID != "" {
		gen, err := strconv.ParseInt(versionID, 10, 64)
		if err != nil {
			return interfaces.TierState{}, err
		}
		obj = obj.Generation(gen)
	}
	attrs, err := obj.Attrs(ctx)
	if err != nil {
		return interfaces.TierState{}, classify("read storage class", err)
	}
	for tier, class := range gcsClasses {
		if class == attrs.StorageClass {
			return interfaces.TierState{Tier: tier}, nil
		}
	}
	return interfaces.TierState{Tier: model.TierStandard}, nil
}

// Restore has nothing to do: every GCS class reads directly.
func (s *Storage) Restore(context.Context, string, string, string, int, model.RestoreSpeed) error {
	return nil
}
