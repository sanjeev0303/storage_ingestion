package upload

import (
	"context"
	"fmt"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// Uploads go to storage connections (Phase 11). A new session is bound to
// the connection its bytes will be published on and the one its upload
// lands on; every later step reaches the bytes through that binding. The
// platform's own bucket is the service's storage and bucket, as before; a
// tenant's connection is resolved through the router. Without a chooser
// and router everything stays on the platform.

// binding is where a new session goes.
type binding struct {
	finalID   string
	stagingID string
	// key is where the upload's bytes land.
	key string
	// staging is the staging connection when it is a tenant's.
	staging *interfaces.StorageLocation
}

// chooseBinding picks the connections of a new upload: the one asked for,
// the dataset's, or the organization's default. Asking for a connection
// other than the default needs storage.use, which the chooser checks.
func chooseBinding(ctx context.Context, chooser interfaces.StorageChooser, p model.Principal, requested, dataset, uploadID string) (binding, error) {
	b := binding{finalID: model.PlatformConnectionID, stagingID: model.PlatformConnectionID, key: quarantineKey(p.TenantID, uploadID)}
	if chooser == nil {
		if requested != "" && !model.IsPlatformConnection(requested) {
			return binding{}, fmt.Errorf("%w: %w", ErrInvalidRequest, model.ErrStorageNotConfigured)
		}
		return b, nil
	}
	choice, err := chooser.ChooseForUpload(ctx, p, requested, dataset)
	if err != nil {
		return binding{}, err
	}
	b.finalID, b.stagingID = choice.Final.ConnectionID, choice.Staging.ConnectionID
	if !model.IsPlatformConnection(b.stagingID) {
		staging := choice.Staging
		b.staging = &staging
		b.key = staging.StagingKey(p.TenantID, uploadID)
	}
	return b, nil
}

// requestedMatches tells whether a replayed request asked for the
// connection the existing session is bound to ("" asks for none).
func requestedMatches(requested, boundID string) bool {
	if requested == "" {
		return true
	}
	if model.IsPlatformConnection(requested) {
		return model.IsPlatformConnection(boundID)
	}
	return requested == boundID
}

// partRules are what a provider's multipart parts must satisfy.
type partRules struct {
	minPart int64
	// checkETag: the part validator is derived from the bytes (S3, GCS).
	// Azure blocks have none; their sizes are checked, and the SHA-256
	// verification after upload proves the bytes.
	checkETag bool
}

var s3PartRules = partRules{minPart: minNonFinalPartSize, checkETag: true}

func rulesFor(caps model.ProviderCapabilities) partRules {
	return partRules{minPart: caps.MinPartBytes, checkETag: caps.PartValidator != "BLOCK_ID"}
}

// sessionStorage is where a multipart session's bytes are.
type sessionStorage struct {
	storage interfaces.MultipartStorage
	bucket  string
	// key is where the session's bytes must be.
	key   string
	rules partRules
}

// staging resolves a multipart session's staging connection.
func (s *MultipartService) staging(ctx context.Context, session model.UploadSession) (sessionStorage, error) {
	if model.IsPlatformConnection(session.StagingConnectionID) {
		return sessionStorage{storage: s.storage, bucket: s.bucket, key: quarantineKey(session.TenantID, session.ID), rules: s3PartRules}, nil
	}
	if s.router == nil {
		return sessionStorage{}, model.ErrStorageNotConfigured
	}
	loc, err := s.router.Locate(ctx, session.TenantID, session.StagingConnectionID)
	if err != nil {
		return sessionStorage{}, fmt.Errorf("resolve storage connection: %w", err)
	}
	return sessionStorage{
		storage: loc.Storage, bucket: loc.StagingBucket,
		key: loc.StagingKey(session.TenantID, session.ID), rules: rulesFor(loc.Capabilities),
	}, nil
}

// Route lets the multipart service place uploads on tenants' storage
// connections.
func (s *MultipartService) Route(chooser interfaces.StorageChooser, router interfaces.StorageRouter) *MultipartService {
	s.chooser, s.router = chooser, router
	return s
}

// Route lets the service place uploads on tenants' storage connections.
func (s *Service) Route(chooser interfaces.StorageChooser, router interfaces.StorageRouter) *Service {
	s.chooser, s.router = chooser, router
	return s
}

// staging resolves a single-request session's staging connection.
func (s *Service) staging(ctx context.Context, session *model.UploadSession) (interfaces.ObjectStorage, string, error) {
	if model.IsPlatformConnection(session.StagingConnectionID) {
		return s.storage, s.bucket, nil
	}
	if s.router == nil {
		return nil, "", model.ErrStorageNotConfigured
	}
	loc, err := s.router.Locate(ctx, session.TenantID, session.StagingConnectionID)
	if err != nil {
		return nil, "", fmt.Errorf("resolve storage connection: %w", err)
	}
	return loc.Storage, loc.StagingBucket, nil
}
