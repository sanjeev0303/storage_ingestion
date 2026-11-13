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

// replay answers a start repeated with the same idempotency key: a fresh
// upload URL while the session still takes its bytes, the session alone
// once it completed (there is nothing left to upload), and ErrSessionEnded
// otherwise, rather than a URL that has expired or a session that is gone.
func (s *Service) replay(ctx context.Context, session *model.UploadSession) (CreateResult, error) {
	switch {
	case session.Status == model.UploadStatusCompleted || session.Status == model.UploadStatusVerified:
		return CreateResult{Upload: session, Reused: true}, nil
	case session.Status != model.UploadStatusPending || !session.ExpiresAt.After(s.now()):
		return CreateResult{}, ErrSessionEnded
	}
	return s.createResult(ctx, session, true)
}

// Create validates the request and ccretes or reuses an upoload session.
func (s *Service) Create(
	ctx context.Context,
	input CreateInput,
) (CreateResult, error) {
	if err := ctx.Err(); err != nil {
		return CreateResult{}, err
	}

	input, path, err := s.validCreate(input)
	if err != nil {
		return CreateResult{}, err
	}
	req := input.Request

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

		return s.replay(ctx, existing)
	}

	if err != model.ErrUploadNotFound {
		return CreateResult{}, fmt.Errorf("find previous upload request: %w", err)
	}

	session, err := s.newSession(ctx, input, path)
	if err != nil {
		return CreateResult{}, err
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
				return s.replay(ctx, existing)
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

// validCreate checks a create request and returns it with its dataset
// filled in, and its catalog path normalized.
func (s *Service) validCreate(input CreateInput) (CreateInput, string, error) {
	req := input.Request

	if strings.TrimSpace(input.TenantID) == "" ||
		strings.TrimSpace(input.OwnerID) == "" {
		return input, "", fmt.Errorf("%w: trusted tenant and owner are requeire", ErrInvalidRequest)
	}

	if strings.TrimSpace(req.IdempotencyKey) == "" {
		return input, "", fmt.Errorf("%w: idempotency_key is required", ErrInvalidRequest)
	}

	if strings.TrimSpace(req.FileName) == "" {
		return input, "", fmt.Errorf("%w: file_name is required", ErrInvalidRequest)
	}

	if strings.TrimSpace(req.ContentType) == "" {
		return input, "", fmt.Errorf("%w: content_type is required", ErrInvalidRequest)
	}

	if req.SizeBytes <= 0 {
		return input, "", fmt.Errorf("%w: size_bytes must be positive", ErrInvalidRequest)
	}

	if req.SizeBytes > s.maxSize {
		return input, "", ErrUploadTooLarge
	}

	if input.Dataset == "" {
		input.Dataset = req.Dataset
	}
	if input.Dataset != "" && !model.ValidDataset(input.Dataset) {
		return input, "", fmt.Errorf("%w: invalid dataset", ErrInvalidRequest)
	}
	path, err := catalogPath(req.Path, req.FileName, req.Metadata)
	if err != nil {
		return input, "", err
	}

	// We only validate the checksum's shape here. The actual checksum
	// must be verified against the uploaded bytes later.
	if req.ChecksumSHA256 != "" {
		if len(req.ChecksumSHA256) != 64 {
			return input, "", fmt.Errorf("%w: checksum_sha256 must contain 64 hexadecimal character", ErrInvalidRequest)
		}

		if _, err := hex.DecodeString(req.ChecksumSHA256); err != nil {
			return input, "", fmt.Errorf("%w: checksum_sha256 is not valid hexadecimal", ErrInvalidRequest)
		}
	}
	return input, path, nil
}

// newSession builds the PENDING session of a checked request: a new ID,
// its storage binding and its expiry.
func (s *Service) newSession(ctx context.Context, input CreateInput, path string) (*model.UploadSession, error) {
	req := input.Request

	id, err := newUploadID()
	if err != nil {
		return nil, fmt.Errorf("generate upload ID: %w", err)
	}

	principal := input.Principal
	if principal.TenantID == "" {
		principal = model.Principal{TenantID: input.TenantID, UserID: input.OwnerID}
	}
	bound, err := chooseBinding(ctx, s.chooser, principal, req.StorageConnectionID, input.Dataset, id)
	if err != nil {
		return nil, err
	}
	if bound.staging != nil {
		caps := bound.staging.Capabilities
		if !caps.PresignedUpload {
			return nil, fmt.Errorf("%w: the storage connection cannot take direct uploads", model.ErrUnsupportedProvider)
		}
		if caps.MaxSingleUploadBytes > 0 && req.SizeBytes > caps.MaxSingleUploadBytes {
			return nil, fmt.Errorf("%w: use a multipart upload on this storage connection", ErrUploadTooLarge)
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
		return nil, fmt.Errorf("validate upload session: %w", err)
	}
	return session, nil
}

// BatchReserver stores many sessions with their reservations at once; the
// quota service is one.
type BatchReserver interface {
	CreateReservedUploads(ctx context.Context, uploads []model.ReservedUpload) error
}

// Reserve stores the sessions of several create requests of one tenant, with
// their quota reservations, in one transaction: bulk ingestion calls it for
// a claim's grants, so the tenant's usage row is locked once per claim
// rather than once per file (docs/ingestion.md, "Where the database's time
// goes").
//
// It is a head start, not a replacement for Create. A Create with the same
// request afterwards finds its session by idempotency key and only presigns
// it. When the batch is not made (a request is invalid, a limit or a path
// refuses one, a key is already taken, or quotas are not enforced here),
// nothing is stored and Create does the work one request at a time, with
// every outcome reported as before.
func (s *Service) Reserve(ctx context.Context, inputs []CreateInput) error {
	batcher, ok := s.quotas.(BatchReserver)
	if !ok || len(inputs) < 2 {
		return nil
	}
	uploads := make([]model.ReservedUpload, 0, len(inputs))
	for _, input := range inputs {
		input, path, err := s.validCreate(input)
		if err != nil {
			return err
		}
		session, err := s.newSession(ctx, input, path)
		if err != nil {
			return err
		}
		uploads = append(uploads, model.ReservedUpload{Upload: session, IdempotencyKey: input.Request.IdempotencyKey})
	}
	return batcher.CreateReservedUploads(ctx, uploads)
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
