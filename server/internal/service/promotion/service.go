package promotion

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	checksum "github.com/sanjeev0303/object-storage-platform/internal/infrastructure/checksum/sha256"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/retry"
)

type Config struct {
	// SourceBucket holds quarantine objects; it is never a delivery bucket.
	SourceBucket string
	// DestinationBucket and Prefix decide where promoted objects live. The
	// key is derived here, never chosen by a client.
	DestinationBucket string
	Prefix            string
}

// Service moves verified, scanned assets out of quarantine. Copy and
// delete are not atomic, so promotion is split: Promote copies, verifies
// the copy and records AVAILABLE; RemoveSource deletes the quarantine
// object only afterwards. Every step is safe to repeat.
type Service struct {
	assets  interfaces.AssetRepository
	storage interfaces.MultipartStorage
	cfg     Config
	now     func() time.Time
}

func NewService(assets interfaces.AssetRepository, storage interfaces.MultipartStorage, cfg Config) (*Service, error) {
	if assets == nil || storage == nil {
		return nil, errors.New("promotion dependencies are required")
	}
	if cfg.SourceBucket == "" || cfg.DestinationBucket == "" || strings.Trim(cfg.Prefix, "/") == "" {
		return nil, errors.New("promotion buckets and prefix are required")
	}
	if strings.Trim(cfg.Prefix, "/") == "quarantine" {
		return nil, errors.New("promotion prefix must not be the quarantine prefix")
	}
	cfg.Prefix = strings.Trim(cfg.Prefix, "/")

	return &Service{assets: assets, storage: storage, cfg: cfg, now: func() time.Time { return time.Now().UTC() }}, nil
}

// DestinationKey is where an asset is promoted to.
func (s *Service) DestinationKey(asset model.Asset) string {
	return fmt.Sprintf("%s/%s/%s", s.cfg.Prefix, asset.TenantID, asset.ID)
}

// Promote copies the quarantine object to destinationKey, verifies the copy
// against the asset's verified size and SHA-256, and only then moves the
// asset PROCESSING -> AVAILABLE with the new key. It returns the asset's
// resulting status. A REJECTED or DELETED result means someone else
// finished the asset first; the caller then removes the copy.
func (s *Service) Promote(ctx context.Context, asset model.Asset, destinationKey string) (model.AssetStatus, error) {
	if destinationKey != s.DestinationKey(asset) {
		return "", retry.Permanent(errors.New("promotion destination does not match the derived key"))
	}

	switch asset.Status {
	case model.AssetStatusAvailable:
		if asset.ObjectKey == destinationKey {
			// Promoted earlier; only the record of it was lost.
			return model.AssetStatusAvailable, nil
		}
		return "", retry.Permanent(errors.New("asset is available under an unexpected key"))
	case model.AssetStatusProcessing:
	default:
		return asset.Status, nil
	}

	if asset.ObjectKey != quarantineKey(asset.TenantID, asset.UploadID) {
		return "", retry.Permanent(errors.New("asset source is not its verified quarantine key"))
	}

	// A crash after an earlier copy leaves a destination object behind. A
	// matching one is reused; anything else is overwritten by the copy.
	stored, matches, err := s.copyMatches(ctx, asset, destinationKey)
	if err != nil {
		return "", err
	}
	if !matches {
		err := s.storage.CopyObject(ctx, s.cfg.SourceBucket, asset.ObjectKey, s.cfg.DestinationBucket, destinationKey)
		if errors.Is(err, interfaces.ErrObjectNotFound) {
			// The verified source is gone and the asset was never promoted, so
			// there is nothing that could be delivered.
			return s.reject(ctx, asset, model.RejectSourceMissing)
		}
		if err != nil {
			return "", fmt.Errorf("copy to delivery key: %w", err)
		}

		stored, matches, err = s.copyMatches(ctx, asset, destinationKey)
		if err != nil {
			return "", err
		}
		if !matches {
			// A bad copy is never promoted.
			if err := s.RemoveDestination(ctx, destinationKey); err != nil {
				return "", fmt.Errorf("promoted copy failed verification and could not be removed: %w", err)
			}
			return "", errors.New("promoted copy failed verification; it was removed and will be copied again")
		}
	}

	// Delivery reads only this exact stored object (If-Match / version), so
	// a later out-of-band overwrite can never be served under this asset.
	err = s.assets.RecordStorageVersion(ctx, asset.TenantID, asset.ID, stored.ETag, stored.VersionID, s.now())
	if err == nil {
		err = s.assets.TransitionStatus(ctx, asset.TenantID, asset.ID,
			[]model.AssetStatus{model.AssetStatusProcessing}, model.AssetStatusAvailable, "", destinationKey, s.now())
	}
	if errors.Is(err, model.ErrInvalidAssetState) {
		current, getErr := s.assets.GetByID(ctx, asset.TenantID, asset.ID)
		if getErr != nil {
			return "", getErr
		}
		return current.Status, nil
	}
	if err != nil {
		return "", fmt.Errorf("record promotion: %w", err)
	}

	return model.AssetStatusAvailable, nil
}

// copyMatches reports whether the destination holds exactly the verified
// bytes: same size, then the same SHA-256, streamed. A missing destination
// does not match. It returns the destination's validators.
func (s *Service) copyMatches(ctx context.Context, asset model.Asset, destinationKey string) (interfaces.ObjectMetadata, bool, error) {
	metadata, err := s.storage.HeadObject(ctx, s.cfg.DestinationBucket, destinationKey)
	if errors.Is(err, interfaces.ErrObjectNotFound) {
		return metadata, false, nil
	}
	if err != nil {
		return metadata, false, fmt.Errorf("inspect promoted copy: %w", err)
	}
	if metadata.SizeBytes != asset.SizeBytes {
		return metadata, false, nil
	}

	body, err := s.storage.OpenObject(ctx, s.cfg.DestinationBucket, destinationKey)
	if err != nil {
		return metadata, false, fmt.Errorf("open promoted copy: %w", err)
	}
	defer body.Close()
	sum, err := checksum.Sum(ctx, body)
	if err != nil {
		return metadata, false, fmt.Errorf("hash promoted copy: %w", err)
	}
	return metadata, sum == asset.ChecksumSHA256, nil
}

// RemoveSource deletes the quarantine object once the asset is AVAILABLE.
// An object that is already gone counts as removed.
func (s *Service) RemoveSource(ctx context.Context, asset model.Asset) error {
	if asset.Status != model.AssetStatusAvailable {
		return retry.Permanent(errors.New("the quarantine source is only removed after promotion"))
	}
	err := s.storage.DeleteObject(ctx, s.cfg.SourceBucket, quarantineKey(asset.TenantID, asset.UploadID))
	if err != nil && !errors.Is(err, interfaces.ErrObjectNotFound) {
		return fmt.Errorf("remove quarantine source: %w", err)
	}
	return nil
}

// RemoveDestination deletes a copy that must not be delivered.
func (s *Service) RemoveDestination(ctx context.Context, destinationKey string) error {
	err := s.storage.DeleteObject(ctx, s.cfg.DestinationBucket, destinationKey)
	if err != nil && !errors.Is(err, interfaces.ErrObjectNotFound) {
		return fmt.Errorf("remove promoted copy: %w", err)
	}
	return nil
}

func (s *Service) reject(ctx context.Context, asset model.Asset, reason string) (model.AssetStatus, error) {
	err := s.assets.TransitionStatus(ctx, asset.TenantID, asset.ID,
		[]model.AssetStatus{model.AssetStatusQuarantined, model.AssetStatusProcessing},
		model.AssetStatusRejected, reason, "", s.now())
	if errors.Is(err, model.ErrInvalidAssetState) {
		current, getErr := s.assets.GetByID(ctx, asset.TenantID, asset.ID)
		if getErr != nil {
			return "", getErr
		}
		return current.Status, nil
	}
	if err != nil {
		return "", fmt.Errorf("reject asset: %w", err)
	}
	return model.AssetStatusRejected, nil
}

// quarantineKey matches the key the upload service gives every session.
func quarantineKey(tenantID, uploadID string) string {
	return fmt.Sprintf("quarantine/%s/%s", tenantID, uploadID)
}
