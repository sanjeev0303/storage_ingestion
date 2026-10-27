package upload

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/dto/request"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

const defaultMaxUploadSize int64 = 5 * 1024 * 1024 * 1024 // 5 GiB
const defaultUploadURLTTL = 15 * time.Minute

// Service coordinates the upload-session creation workflow.
type Service struct {
	repository interfaces.UploadRepository
	maxSize    int64
	ttl        time.Duration
	storage    interfaces.ObjectStorage
	bucket     string
	now        func() time.Time
	// quotas, when set, reserves each new session's bytes against its
	// tenant's quota; audit records uploads started and completed.
	quotas Reserver
	audit  interfaces.AuditRecorder
	// chooser and router place uploads on tenants' storage connections
	// (routing.go); nil keeps every upload on the platform's bucket.
	chooser interfaces.StorageChooser
	router  interfaces.StorageRouter
	// assets, when set, lets AssetOf report what an upload became.
	assets AssetReader
}

// AssetReader finds the asset made from an upload.
type AssetReader interface {
	GetByUpload(ctx context.Context, tenantID, uploadID string) (*model.Asset, error)
}

// ReportAssets makes AssetOf answer from assets.
func (s *Service) ReportAssets(assets AssetReader) *Service {
	s.assets = assets
	return s
}

// Reserver stores an upload session with its quota reservation.
type Reserver interface {
	CreateReservedUpload(ctx context.Context, upload *model.UploadSession, idempotencyKey string, multipart bool) error
}

// Guard makes s reserve quota for new sessions and audit its
// principal-facing operations. The API and the workers set both.
func (s *Service) Guard(quotas Reserver, audit interfaces.AuditRecorder) *Service {
	s.quotas, s.audit = quotas, audit
	return s
}

// NewService receives its devendencies insted of constructing them.
// This nakes the service easy to test with different repository adapters.
func NewService(
	repository interfaces.UploadRepository,
	storage interfaces.ObjectStorage,
	bucket string,
	maxSize int64,
	ttl time.Duration,

) *Service {
	if maxSize <= 0 {
		maxSize = defaultMaxUploadSize
	}

	if ttl <= 0 {
		ttl = 30 * time.Minute
	}

	return &Service{
		repository: repository,
		storage:    storage,
		bucket:     bucket,
		maxSize:    maxSize,
		ttl:        ttl,
		now:        func() time.Time { return time.Now().UTC() },
	}
}

// CreateInput contains the request data and trusted indentity supplied
// by the HTTP authentication layer or another trusted caller.
type CreateInput struct {
	TenantID string
	OwnerID  string
	// Dataset optionally labels the upload and its asset; set only by a
	// trusted caller (bulk ingestion), never from the request body.
	Dataset string
	Request request.CreateUploadRequest
	// Principal, when set, is who asked: choosing a storage connection
	// other than the default needs its storage.use permission.
	Principal model.Principal
}

// CreateResult tessl the caller wheterh this is a newly created session.
type CreateResult struct {
	Upload *model.UploadSession
	Plan   interfaces.UploadPlan
	Reused bool
}

func (s *Service) createResult(
	ctx context.Context,
	session *model.UploadSession,
	reused bool,
) (CreateResult, error) {
	urlExpiresAt := s.now().Add(defaultUploadURLTTL)

	if session.ExpiresAt.Before(urlExpiresAt) {
		urlExpiresAt = session.ExpiresAt
	}

	storage, bucket, err := s.staging(ctx, session)
	if err != nil {
		return CreateResult{}, err
	}
	plan, err := storage.CreateUploadPlan(
		ctx,
		interfaces.UploadTarget{
			Bucket:      bucket,
			ObjectKey:   session.ObjectKey,
			ContentType: session.ContentType,
			SizeBytes:   session.ExpectedSize,
			ExpiresAt:   urlExpiresAt,
		},
	)

	if err != nil {
		return CreateResult{}, fmt.Errorf("create upload plan: %w", err)
	}

	return CreateResult{
		Upload: session,
		Plan:   plan,
		Reused: reused,
	}, nil
}

// Create validates the request and ccretes or reuses an upoload session.
func (s *Service) Create(
	ctx context.Context,
	input CreateInput,
) (CreateResult, error) {
	if err := ctx.Err(); err != nil {
		return CreateResult{}, err
	}

	req := input.Request

	if strings.TrimSpace(input.TenantID) == "" ||
		strings.TrimSpace(input.OwnerID) == "" {
		return CreateResult{}, fmt.Errorf("%w: trusted tenant and owner are requeire", ErrInvalidRequest)
	}

	if strings.TrimSpace(req.IdempotencyKey) == "" {
		return CreateResult{}, fmt.Errorf("%w: idempotency_key is required", ErrInvalidRequest)
	}

	if strings.TrimSpace(req.FileName) == "" {
		return CreateResult{}, fmt.Errorf("%w: file_name is required", ErrInvalidRequest)
	}

	if strings.TrimSpace(req.ContentType) == "" {
		return CreateResult{}, fmt.Errorf("%w: content_type is required", ErrInvalidRequest)
	}

	if req.SizeBytes <= 0 {
		return CreateResult{}, fmt.Errorf("%w: size_bytes must be positive", ErrInvalidRequest)
	}

	if req.SizeBytes > s.maxSize {
		return CreateResult{}, ErrUploadTooLarge
	}

	if input.Dataset == "" {
		input.Dataset = req.Dataset
	}
	if input.Dataset != "" && !model.ValidDataset(input.Dataset) {
		return CreateResult{}, fmt.Errorf("%w: invalid dataset", ErrInvalidRequest)
	}
	path, err := catalogPath(req.Path, req.FileName, req.Metadata)
	if err != nil {
		return CreateResult{}, err
	}

	// We only validate the checksum's shape here. The actual checksum
	// must be verified against the uploaded bytes later.
	if req.ChecksumSHA256 != "" {
		if len(req.ChecksumSHA256) != 64 {
			return CreateResult{}, fmt.Errorf("%w: checksum_sha256 must contain 64 hexadecimal character", ErrInvalidRequest)
		}

		if _, err := hex.DecodeString(req.ChecksumSHA256); err != nil {
			return CreateResult{}, fmt.Errorf("%w: checksum_sha256 is not valid hexadecimal", ErrInvalidRequest)
		}
	}

	// An identical retry returns the original session. Reusing the key
	// with different file metadata is a conflict.
	existing, err := s.repository.GetByIdempotencyKey(
		ctx,
		input.TenantID,
		input.OwnerID,
		req.IdempotencyKey,
	)

	if err == nil {
		if !sameUploadRequest(existing, input) {
			return CreateResult{}, ErrIdempotencyConflict
		}

		return s.createResult(ctx, existing, true)
	}

	if err != model.ErrUploadNotFound {
		return CreateResult{}, fmt.Errorf("find previous upload request: %w", err)
	}

	id, err := newUploadID()
	if err != nil {
		return CreateResult{}, fmt.Errorf("generate upload ID: %w", err)
	}

	principal := input.Principal
	if principal.TenantID == "" {
		principal = model.Principal{TenantID: input.TenantID, UserID: input.OwnerID}
	}
	bound, err := chooseBinding(ctx, s.chooser, principal, req.StorageConnectionID, input.Dataset, id)
	if err != nil {
		return CreateResult{}, err
	}
	if bound.staging != nil {
		caps := bound.staging.Capabilities
		if !caps.PresignedUpload {
			return CreateResult{}, fmt.Errorf("%w: the storage connection cannot take direct uploads", model.ErrUnsupportedProvider)
		}
		if caps.MaxSingleUploadBytes > 0 && req.SizeBytes > caps.MaxSingleUploadBytes {
			return CreateResult{}, fmt.Errorf("%w: use a multipart upload on this storage connection", ErrUploadTooLarge)
		}
	}

	now := s.now()

	session := &model.UploadSession{
		ID:             id,
		TenantID:       input.TenantID,
		OwnerID:        input.OwnerID,
		ObjectKey:      bound.key,
		FileName:       strings.TrimSpace(req.FileName),
		ContentType:    strings.TrimSpace(req.ContentType),
		ExpectedSize:   req.SizeBytes,
		ChecksumSHA256: strings.ToLower(req.ChecksumSHA256),
		Dataset:        input.Dataset,
		Status:         model.UploadStatusPending,
		CreatedAt:      now,
		UpdatedAt:      now,
		ExpiresAt:      now.Add(s.ttl),
		Path:           path,
		Metadata:       req.Metadata,
		CreateOnly:     req.CreateOnly,

		StorageConnectionID: bound.finalID,
		StagingConnectionID: bound.stagingID,
	}

	if err := session.Validate(); err != nil {
		return CreateResult{}, fmt.Errorf("validate upload session: %w", err)
	}

	if err := s.save(ctx, session, req.IdempotencyKey); err != nil {
		var quotaErr *model.QuotaError
		if errors.As(err, &quotaErr) || errors.Is(err, model.ErrPathExists) || errors.Is(err, model.ErrPathBusy) ||
			errors.Is(err, model.ErrConnectionNotActive) || errors.Is(err, model.ErrConnectionNotFound) {
			return CreateResult{}, err
		}
		// Another request may have insterted that same idempotency key
		// after our initial lookup. Replay its result if the request
		// metadata matches.
		if errors.Is(err, model.ErrIdempotencyConflict) {
			existing, lookupErr := s.repository.GetByIdempotencyKey(
				ctx,
				input.TenantID,
				input.OwnerID,
				req.IdempotencyKey,
			)

			if lookupErr == nil && sameUploadRequest(existing, input) {
				return s.createResult(ctx, existing, true)
			}

			if lookupErr == nil {
				return CreateResult{}, ErrIdempotencyConflict
			}

			return CreateResult{}, fmt.Errorf(
				"resolve concurrent idempotency conflict: %w",
				lookupErr,
			)
		}

		if !errors.Is(err, model.ErrUploadNotFound) {
			return CreateResult{}, fmt.Errorf("find previous upload request: %w", err)
		}

		return CreateResult{}, fmt.Errorf("save upload session: %w", err)
	}

	return s.createResult(ctx, session, false)
}

func sameUploadRequest(
	existing *model.UploadSession,
	input CreateInput,
) bool {
	req := input.Request
	path, _ := catalogPath(req.Path, req.FileName, nil)
	return existing != nil &&
		existing.FileName == strings.TrimSpace(req.FileName) &&
		existing.ContentType == strings.TrimSpace(req.ContentType) &&
		existing.ExpectedSize == req.SizeBytes &&
		existing.ChecksumSHA256 == strings.ToLower(req.ChecksumSHA256) &&
		existing.Dataset == input.Dataset &&
		existing.Path == path &&
		existing.CreateOnly == req.CreateOnly &&
		maps.Equal(existing.Metadata, req.Metadata) &&
		requestedMatches(req.StorageConnectionID, existing.StorageConnectionID)
}

// catalogPath validates the catalog part of an upload: its logical path
// (the file name when none is given), returned normalized, and its user
// metadata.
func catalogPath(path, fileName string, metadata map[string]string) (string, error) {
	if path == "" {
		path = strings.TrimSpace(fileName)
	}
	normalized, _, err := model.NormalizePath(path, model.DefaultPathPolicy)
	if err != nil {
		return "", fmt.Errorf("%w: path: %v", ErrInvalidRequest, err)
	}
	if err := model.ValidateUserMetadata(metadata, model.DefaultMetadataLimits); err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	return normalized, nil
}

// newUploadID generates a rendom identifier using the standard library.
func newUploadID() (string, error) {
	var bytes [16]byte

	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}

	// Mark the identifier as UUID v4-shaped.
	bytes[6] = (bytes[6] & 0x0f) | 0x40
	bytes[8] = (bytes[8] & 0x0f) | 0x80

	encoded := hex.EncodeToString(bytes[:])

	return encoded[0:8] + "-" +
		encoded[8:12] + "-" +
		encoded[12:16] + "-" +
		encoded[16:20] + "-" +
		encoded[20:32], nil
}

// save stores a new session, with its quota reservation when quotas are
// enforced.
func (s *Service) save(ctx context.Context, session *model.UploadSession, idempotencyKey string) error {
	if s.quotas != nil {
		return s.quotas.CreateReservedUpload(ctx, session, idempotencyKey, false)
	}
	return s.repository.CreateWithIdempotency(ctx, session, idempotencyKey)
}
