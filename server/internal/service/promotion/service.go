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
//
// The source is the asset's staging connection and the destination its
// storage connection (Phase 11). On one connection the copy stays inside
// the provider; between two (an upload staged on the platform for a
// tenant's bucket) the bytes are streamed, one part in memory at a time.
type Service struct {
	assets  interfaces.AssetRepository
	storage interfaces.MultipartStorage
	cfg     Config
	now     func() time.Time
	// router reaches tenants' storage connections; nil keeps promotion on
	// the platform's buckets.
	router interfaces.StorageRouter
}

// Route lets promotion reach tenants' storage connections.
func (s *Service) Route(router interfaces.StorageRouter) *Service {
	s.router = router
	return s
}

// side is one of an asset's connections: where it is and how to reach it.
type side struct {
	interfaces.StoragePlace
	storage interfaces.MultipartStorage
	// writer streams into a tenant connection; nil on the platform.
	writer interfaces.StorageProvider
}

func (s *Service) place(ctx context.Context, tenantID, connectionID string) (side, error) {
	if model.IsPlatformConnection(connectionID) {
		return side{storage: s.storage, StoragePlace: interfaces.StoragePlace{
			ConnectionID:  model.PlatformConnectionID,
			StagingBucket: s.cfg.SourceBucket, StagingPrefix: "quarantine",
			ReadyBucket: s.cfg.DestinationBucket, ReadyPrefix: s.cfg.Prefix,
		}}, nil
	}
	if s.router == nil {
		return side{}, model.ErrStorageNotConfigured
	}
	loc, err := s.router.Locate(ctx, tenantID, connectionID)
	if err != nil {
		return side{}, fmt.Errorf("resolve storage connection: %w", err)
	}
	return side{StoragePlace: loc.StoragePlace, storage: loc.Storage, writer: loc.Storage}, nil
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

// DestinationKey is where an asset is promoted to, on its storage
// connection.
func (s *Service) DestinationKey(ctx context.Context, asset model.Asset) (string, error) {
	dst, err := s.place(ctx, asset.TenantID, asset.StorageConnectionID)
	if err != nil {
		return "", err
	}
	return readyKey(dst, asset), nil
}

func readyKey(dst side, asset model.Asset) string {
	return fmt.Sprintf("%s/%s/%s", dst.ReadyPrefix, asset.TenantID, asset.ID)
}

// Promote copies the quarantine object to destinationKey, verifies the copy
// against the asset's verified size and SHA-256, and only then moves the
// asset PROCESSING -> AVAILABLE with the new key. It returns the asset's
// resulting status. A REJECTED or DELETED result means someone else
// finished the asset first; the caller then removes the copy.
func (s *Service) Promote(ctx context.Context, asset model.Asset, destinationKey string) (model.AssetStatus, error) {
	src, err := s.place(ctx, asset.TenantID, asset.StagingConnectionID)
	if err != nil {
		return "", err
	}
	dst, err := s.place(ctx, asset.TenantID, asset.StorageConnectionID)
	if err != nil {
		return "", err
	}
	if destinationKey != readyKey(dst, asset) {
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

	if asset.ObjectKey != src.StagingKey(asset.TenantID, asset.UploadID) {
		return "", retry.Permanent(errors.New("asset source is not its verified quarantine key"))
	}

	// A crash after an earlier copy leaves a destination object behind. A
	// matching one is reused; anything else is overwritten by the copy.
	stored, matches, err := s.copyMatches(ctx, dst, asset, destinationKey)
	if err != nil {
		return "", err
	}
	if !matches {
		err := s.transfer(ctx, src, dst, asset, destinationKey)
		if errors.Is(err, interfaces.ErrObjectNotFound) {
			// The verified source is gone and the asset was never promoted, so
			// there is nothing that could be delivered.
			return s.reject(ctx, asset, model.RejectSourceMissing)
		}
		if err != nil {
			return "", fmt.Errorf("copy to delivery key: %w", err)
		}

		stored, matches, err = s.copyMatches(ctx, dst, asset, destinationKey)
		if err != nil {
			return "", err
		}
		if !matches {
			// A bad copy is never promoted.
			if err := s.RemoveDestination(ctx, asset, destinationKey); err != nil {
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

// transfer copies the verified source to the destination: inside the
// provider when both are one connection, otherwise streamed from one to
// the other. A failed stream leaves no object (the writer aborts it) or a
// partial one that copyMatches refuses, so promotion just runs again.
func (s *Service) transfer(ctx context.Context, src, dst side, asset model.Asset, destinationKey string) error {
	if src.ConnectionID == dst.ConnectionID {
		return src.storage.CopyObject(ctx, src.StagingBucket, asset.ObjectKey, dst.ReadyBucket, destinationKey)
	}
	if dst.writer == nil {
		return retry.Permanent(errors.New("an upload staged on a tenant connection cannot be published on the platform"))
	}
	body, err := src.storage.OpenObject(ctx, src.StagingBucket, asset.ObjectKey)
	if err != nil {
		return err
	}
	defer body.Close()
	_, err = dst.writer.WriteObject(ctx, dst.ReadyBucket, destinationKey, body, asset.SizeBytes, asset.ContentType)
	return err
}

// copyMatches reports whether the destination holds exactly the verified
// bytes: same size, then the same SHA-256, streamed. A missing destination
// does not match. It returns the destination's validators.
func (s *Service) copyMatches(ctx context.Context, dst side, asset model.Asset, destinationKey string) (interfaces.ObjectMetadata, bool, error) {
	metadata, err := dst.storage.HeadObject(ctx, dst.ReadyBucket, destinationKey)
	if errors.Is(err, interfaces.ErrObjectNotFound) {
		return metadata, false, nil
	}
	if err != nil {
		return metadata, false, fmt.Errorf("inspect promoted copy: %w", err)
	}
	if metadata.SizeBytes != asset.SizeBytes {
		return metadata, false, nil
	}

	body, err := dst.storage.OpenObject(ctx, dst.ReadyBucket, destinationKey)
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
	src, err := s.place(ctx, asset.TenantID, asset.StagingConnectionID)
	if err != nil {
		return err
	}
	err = src.storage.DeleteObject(ctx, src.StagingBucket, src.StagingKey(asset.TenantID, asset.UploadID))
	if err != nil && !errors.Is(err, interfaces.ErrObjectNotFound) {
		return fmt.Errorf("remove quarantine source: %w", err)
	}
	return nil
}

// RemoveDestination deletes a copy of the asset that must not be
// delivered.
func (s *Service) RemoveDestination(ctx context.Context, asset model.Asset, destinationKey string) error {
	dst, err := s.place(ctx, asset.TenantID, asset.StorageConnectionID)
	if err != nil {
		return err
	}
	err = dst.storage.DeleteObject(ctx, dst.ReadyBucket, destinationKey)
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
