package upload

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
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
	Request  request.CreateUploadRequest
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

	plan, err := s.storage.CreateUploadPlan(
		ctx,
		interfaces.UploadTarget{
			Bucket:      s.bucket,
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
		if !sameUploadRequest(existing, req) {
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

	now := s.now()

	session := &model.UploadSession{
		ID:             id,
		TenantID:       input.TenantID,
		OwnerID:        input.OwnerID,
		ObjectKey:      "quarantine/" + input.TenantID + "/" + id,
		FileName:       strings.TrimSpace(req.FileName),
		ContentType:    strings.TrimSpace(req.ContentType),
		ExpectedSize:   req.SizeBytes,
		ChecksumSHA256: strings.ToLower(req.ChecksumSHA256),
		Status:         model.UploadStatusPending,
		CreatedAt:      now,
		UpdatedAt:      now,
		ExpiresAt:      now.Add(s.ttl),
	}

	if err := session.Validate(); err != nil {
		return CreateResult{}, fmt.Errorf("validate upload session: %w", err)
	}

	if err := s.repository.CreateWithIdempotency(
		ctx,
		session,
		req.IdempotencyKey,
	); err != nil {
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

			if lookupErr == nil && sameUploadRequest(existing, req) {
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
	req request.CreateUploadRequest,
) bool {
	return existing != nil &&
		existing.FileName == strings.TrimSpace(req.FileName) &&
		existing.ContentType == strings.TrimSpace(req.ContentType) &&
		existing.ExpectedSize == req.SizeBytes &&
		existing.ChecksumSHA256 == strings.ToLower(req.ChecksumSHA256)
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
