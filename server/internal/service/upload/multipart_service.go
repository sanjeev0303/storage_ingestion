package upload

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

const (
	minNonFinalPartSize int64 = 5 * 1024 * 1024 // 5 MiB
	maxMultipartParts         = 10_000
)

type MultipartService struct {
	uploads    interfaces.UploadRepository
	parts      interfaces.MultipartUploadRepository
	storage    interfaces.MultipartStorage
	bucket     string
	maxSize    int64
	sessionTTL time.Duration
	partURLTTL time.Duration
	now        func() time.Time
	newID      func() (string, error)
}

type MultipartServiceConfig struct {
	Bucket     string
	MaxSize    int64
	SessionTTL time.Duration
	PartURLTTL time.Duration
}

// NewMultipartService creates the service using application-owned interfaces.
func NewMultipartService(
	uploads interfaces.UploadRepository,
	parts interfaces.MultipartUploadRepository,
	storage interfaces.MultipartStorage,
	cfg MultipartServiceConfig,
) (*MultipartService, error) {
	if uploads == nil || parts == nil || storage == nil {
		return nil, errors.New("multipart service dependencies are required")
	}
	if strings.TrimSpace(cfg.Bucket) == "" {
		return nil, errors.New("multipart storage bucket is required")
	}
	if cfg.MaxSize <= 0 {
		return nil, errors.New("maximum upload size must be positive")
	}
	if cfg.SessionTTL <= 0 {
		return nil, errors.New("upload session TTL must be positive")
	}
	if cfg.PartURLTTL <= 0 || cfg.PartURLTTL > 7*24*time.Hour {
		return nil, errors.New("part URL TTL must be greater than zero and at most 7 days")
	}

	return &MultipartService{
		uploads:    uploads,
		parts:      parts,
		storage:    storage,
		bucket:     cfg.Bucket,
		maxSize:    cfg.MaxSize,
		sessionTTL: cfg.SessionTTL,
		partURLTTL: cfg.PartURLTTL,
		now:        func() time.Time { return time.Now().UTC() },
		newID:      generateMultipartUploadID,
	}, nil
}

// Start creates a database session, initiates provider multipart upload,
// and then persists the provider upload ID.
func (s *MultipartService) Start(
	ctx context.Context,
	input StartMultipartInput,
) (MultipartResult, error) {
	if err := validateStartInput(input, s.maxSize); err != nil {
		return MultipartResult{}, err
	}

	// An idempotent retry should reuse the original session.
	existing, err := s.uploads.GetByIdempotencyKey(
		ctx,
		input.TenantID,
		input.OwnerID,
		input.IdempotencyKey,
	)
	if err == nil {
		return replayStart(*existing, input)
	}
	if !errors.Is(err, model.ErrUploadNotFound) {
		return MultipartResult{}, fmt.Errorf("find idempotent upload: %w", err)
	}

	id, err := s.newID()
	if err != nil {
		return MultipartResult{}, fmt.Errorf("generate upload ID: %w", err)
	}

	now := s.now()
	session := model.UploadSession{
		ID:             id,
		TenantID:       input.TenantID,
		OwnerID:        input.OwnerID,
		ObjectKey:      quarantineKey(input.TenantID, id),
		FileName:       input.FileName,
		ContentType:    input.ContentType,
		ExpectedSize:   input.SizeBytes,
		ChecksumSHA256: strings.ToLower(input.ChecksumSHA256),
		Status:         model.UploadStatusPending,
		ExpiresAt:      now.Add(s.sessionTTL),
		CreatedAt:      now,
		UpdatedAt:      now,
	}

	if err := session.Validate(); err != nil {
		return MultipartResult{}, fmt.Errorf("validate upload session: %w", err)
	}

	if err := s.uploads.CreateWithIdempotency(
		ctx,
		&session,
		input.IdempotencyKey,
	); err != nil {
		// Another request may have won the same idempotency race.
		// Read it back and reuse it only if the payload matches.
		winner, lookupErr := s.uploads.GetByIdempotencyKey(
			ctx,
			input.TenantID,
			input.OwnerID,
			input.IdempotencyKey,
		)
		if lookupErr == nil {
			return replayStart(*winner, input)
		}
		return MultipartResult{}, fmt.Errorf("create upload session: %w", err)
	}

	// The PENDING session without a provider ID is now the durable record
	// of this initiation. If the call below times out, or saving its ID
	// fails, reconciliation lists the provider's uploads for this exact
	// object key and adopts or cleans up what it finds. Nothing here ever
	// initiates a second upload for the same session.
	storageUploadID, err := s.storage.BeginMultipart(
		ctx,
		s.bucket,
		session.ObjectKey,
		session.ContentType,
	)
	if err != nil {
		return MultipartResult{}, fmt.Errorf("%w: begin provider multipart upload: %w", ErrMultipartStartPending, err)
	}

	err = s.uploads.SetStorageUploadID(ctx, session.TenantID, session.ID, storageUploadID, s.now())
	if errors.Is(err, model.ErrInvalidUploadState) {
		// Reconciliation already closed this session while the provider
		// call was in flight, so the new provider upload has no owner.
		if abortErr := s.storage.AbortMultipart(ctx, s.bucket, session.ObjectKey, storageUploadID); abortErr != nil {
			return MultipartResult{}, fmt.Errorf("%w: orphaned provider upload not aborted: %w", ErrMultipartNotActive, abortErr)
		}
		return MultipartResult{}, ErrMultipartNotActive
	}
	if err != nil {
		return MultipartResult{}, fmt.Errorf("%w: persist storage upload ID: %w", ErrMultipartStartPending, err)
	}

	session.StorageUploadID = storageUploadID
	return MultipartResult{Session: session}, nil
}

// replayStart answers a retry that carries an existing idempotency key.
func replayStart(existing model.UploadSession, input StartMultipartInput) (MultipartResult, error) {
	if !sameMultipartRequest(existing, input) {
		return MultipartResult{}, model.ErrIdempotencyConflict
	}
	if existing.StorageUploadID == "" {
		if existing.Status == model.UploadStatusPending {
			return MultipartResult{}, ErrMultipartStartPending
		}
		// Reconciliation found the initiation never took effect.
		return MultipartResult{}, ErrMultipartNotActive
	}
	return MultipartResult{Session: existing, Reused: true}, nil
}

// PresignParts generates URLs for requested part numbers.
// Only active, tenant-owned sessions with a persisted provider ID are eligible.
func (s *MultipartService) PresignParts(
	ctx context.Context,
	tenantID string,
	ownerID string,
	uploadID string,
	partNumbers []int32,
) ([]PartURL, error) {
	session, err := s.getOwnedSession(ctx, tenantID, ownerID, uploadID)
	if err != nil {
		return nil, err
	}
	if err := s.requireActive(session); err != nil {
		return nil, err
	}
	if len(partNumbers) == 0 || len(partNumbers) > maxMultipartParts {
		return nil, ErrInvalidPart
	}

	seen := make(map[int32]struct{}, len(partNumbers))
	for _, number := range partNumbers {
		if number < 1 || number > maxMultipartParts {
			return nil, ErrInvalidPart
		}
		if _, exists := seen[number]; exists {
			return nil, fmt.Errorf("%w: duplicate part number %d", ErrInvalidPart, number)
		}
		seen[number] = struct{}{}
	}

	// Keep URL generation deterministic for easier debugging.
	numbers := append([]int32(nil), partNumbers...)
	sort.Slice(numbers, func(i, j int) bool {
		return numbers[i] < numbers[j]
	})

	urls := make([]PartURL, 0, len(numbers))
	expiresAt := s.now().Add(s.partURLTTL)
	if expiresAt.After(session.ExpiresAt) {
		expiresAt = session.ExpiresAt
	}
	ttl := expiresAt.Sub(s.now())
	if ttl <= 0 {
		return nil, ErrMultipartNotActive
	}

	for _, number := range numbers {
		url, err := s.storage.PresignPart(
			ctx,
			s.bucket,
			session.ObjectKey,
			session.StorageUploadID,
			number,
			ttl,
		)
		if err != nil {
			return nil, fmt.Errorf("presign part %d: %w", number, err)
		}

		urls = append(urls, PartURL{
			PartNumber: number,
			URL:        url,
			ExpiresAt:  expiresAt,
		})
	}

	return urls, nil
}

// RecordParts records part metadata a client reports after uploading.
// A client's report is only a claim: each part must match what the
// provider itself lists for this upload, so a retried or repeated
// confirmation is idempotent and a conflicting one is refused.
func (s *MultipartService) RecordParts(
	ctx context.Context,
	tenantID string,
	ownerID string,
	uploadID string,
	inputs []RecordPartInput,
) error {
	session, err := s.getOwnedSession(ctx, tenantID, ownerID, uploadID)
	if err != nil {
		return err
	}
	if err := s.requireActive(session); err != nil {
		return err
	}
	if len(inputs) == 0 || len(inputs) > maxMultipartParts {
		return ErrInvalidPart
	}

	stored, err := s.storage.ListParts(ctx, s.bucket, session.ObjectKey, session.StorageUploadID)
	if errors.Is(err, interfaces.ErrStorageUploadNotFound) {
		return ErrMultipartNotActive
	}
	if err != nil {
		return fmt.Errorf("inspect provider parts: %w", err)
	}
	onProvider := make(map[int32]interfaces.StoredPart, len(stored))
	for _, part := range stored {
		onProvider[part.PartNumber] = part
	}

	// Check every part before writing any, so a bad batch records nothing.
	verified := make([]model.UploadPart, 0, len(inputs))
	seen := make(map[int32]struct{}, len(inputs))
	for _, input := range inputs {
		if input.PartNumber < 1 || input.PartNumber > maxMultipartParts ||
			strings.TrimSpace(input.ETag) == "" ||
			input.SizeBytes <= 0 {
			return ErrInvalidPart
		}
		if _, exists := seen[input.PartNumber]; exists {
			return fmt.Errorf("%w: duplicate part number %d", ErrInvalidPart, input.PartNumber)
		}
		seen[input.PartNumber] = struct{}{}

		actual, exists := onProvider[input.PartNumber]
		if !exists {
			return fmt.Errorf("%w: part %d is not stored at the provider", ErrInvalidPart, input.PartNumber)
		}
		if !sameETag(actual.ETag, input.ETag) || actual.SizeBytes != input.SizeBytes {
			return fmt.Errorf("%w: part %d does not match the provider's record", ErrInvalidPart, input.PartNumber)
		}

		part := providerPart(session, actual, s.now())
		part.ChecksumSHA256 = strings.ToLower(input.ChecksumSHA256)
		if err := part.Validate(); err != nil {
			return err
		}
		verified = append(verified, part)
	}

	for _, part := range verified {
		if err := s.parts.UpsertPart(ctx, tenantID, part); err != nil {
			return fmt.Errorf("record part %d: %w", part.PartNumber, err)
		}
	}

	if session.Status == model.UploadStatusPending {
		err := s.uploads.TransitionStatus(ctx, tenantID, uploadID,
			[]model.UploadStatus{model.UploadStatusPending}, model.UploadStatusUploading, "", s.now())
		// Already UPLOADING (a concurrent confirmation won) is fine.
		if err != nil && !errors.Is(err, model.ErrInvalidUploadState) {
			return fmt.Errorf("persist uploading status: %w", err)
		}
	}

	return nil
}

// Complete validates the parts against the provider's own listing, claims
// the session for completion, asks the provider to assemble the object and
// verifies its size. The object stays quarantined: it becomes promotable
// only after VerifyIntegrity checks its SHA-256.
func (s *MultipartService) Complete(
	ctx context.Context,
	tenantID string,
	ownerID string,
	uploadID string,
	input CompleteMultipartInput,
) (model.UploadSession, error) {
	session, err := s.getOwnedSession(ctx, tenantID, ownerID, uploadID)
	if err != nil {
		return model.UploadSession{}, err
	}

	switch session.Status {
	case model.UploadStatusCompleted, model.UploadStatusVerified:
		// A retry after a successful completion is idempotent.
		return session, nil
	case model.UploadStatusCompleting:
		return session, ErrOperationPending
	case model.UploadStatusUploading:
	default:
		return model.UploadSession{}, ErrMultipartNotActive
	}
	if !s.now().Before(session.ExpiresAt) {
		return model.UploadSession{}, ErrMultipartNotActive
	}
	if session.StorageUploadID == "" {
		return model.UploadSession{}, ErrMultipartNotStarted
	}

	stored, err := s.storage.ListParts(ctx, s.bucket, session.ObjectKey, session.StorageUploadID)
	if errors.Is(err, interfaces.ErrStorageUploadNotFound) {
		return model.UploadSession{}, ErrMultipartNotActive
	}
	if err != nil {
		return model.UploadSession{}, fmt.Errorf("inspect provider parts: %w", err)
	}

	// Parts uploaded but never recorded (the confirmation's write failed)
	// are recovered here from the provider's listing.
	recorded, err := s.syncProviderParts(ctx, session, stored)
	if err != nil {
		return model.UploadSession{}, err
	}

	completedParts, err := validateCompletion(session, recorded, input)
	if err != nil {
		return model.UploadSession{}, err
	}

	// Claim the session for completion. Abort and expiry use the same
	// compare-and-set, so only one of them can ever finalize it.
	err = s.uploads.TransitionStatus(ctx, tenantID, uploadID,
		[]model.UploadStatus{model.UploadStatusUploading}, model.UploadStatusCompleting, "", s.now())
	if errors.Is(err, model.ErrInvalidUploadState) {
		return model.UploadSession{}, ErrMultipartNotActive
	}
	if err != nil {
		return model.UploadSession{}, fmt.Errorf("claim upload for completion: %w", err)
	}
	session.Status = model.UploadStatusCompleting

	if err := s.storage.CompleteMultipart(
		ctx,
		s.bucket,
		session.ObjectKey,
		session.StorageUploadID,
		completedParts,
	); err != nil {
		// The provider may have assembled the object even though this call
		// failed. The session stays COMPLETING; reconciliation inspects the
		// provider before deciding, and never completes blindly.
		return session, fmt.Errorf("%w: %w", ErrOperationPending, err)
	}

	outcome, err := s.confirmCompletion(ctx, session)
	if err != nil {
		return session, fmt.Errorf("%w: verify completed object: %w", ErrOperationPending, err)
	}
	if outcome == model.UploadStatusFailed {
		return model.UploadSession{}, ErrObjectSizeMismatch
	}

	session.Status = model.UploadStatusCompleted
	session.UpdatedAt = s.now()
	return session, nil
}

// Abort claims the session for abort and removes the provider upload.
// When the provider call fails the session stays ABORTING and
// reconciliation retries it.
func (s *MultipartService) Abort(
	ctx context.Context,
	tenantID string,
	ownerID string,
	uploadID string,
) (model.UploadSession, error) {
	session, err := s.getOwnedSession(ctx, tenantID, ownerID, uploadID)
	if err != nil {
		return model.UploadSession{}, err
	}

	switch session.Status {
	case model.UploadStatusFailed, model.UploadStatusExpired:
		return session, nil
	case model.UploadStatusPending, model.UploadStatusUploading:
		err := s.uploads.TransitionStatus(ctx, tenantID, uploadID,
			[]model.UploadStatus{model.UploadStatusPending, model.UploadStatusUploading},
			model.UploadStatusAborting, model.FailureAbortedByClient, s.now())
		if errors.Is(err, model.ErrInvalidUploadState) {
			return model.UploadSession{}, ErrMultipartNotActive
		}
		if err != nil {
			return model.UploadSession{}, fmt.Errorf("claim upload for abort: %w", err)
		}
	case model.UploadStatusAborting:
		// A repeated abort request retries the provider cleanup.
	default:
		return model.UploadSession{}, ErrMultipartNotActive
	}

	if _, err := s.ResolveAbort(ctx, tenantID, uploadID); err != nil {
		return model.UploadSession{}, fmt.Errorf("%w: %w", ErrOperationPending, err)
	}

	return s.getSession(ctx, tenantID, uploadID)
}

// requireActive refuses expired, finished or not-yet-started sessions.
func (s *MultipartService) requireActive(session model.UploadSession) error {
	if !s.now().Before(session.ExpiresAt) {
		return ErrMultipartNotActive
	}
	if session.Status != model.UploadStatusPending &&
		session.Status != model.UploadStatusUploading {
		return ErrMultipartNotActive
	}
	if session.StorageUploadID == "" {
		return ErrMultipartNotStarted
	}
	return nil
}

func (s *MultipartService) getOwnedSession(
	ctx context.Context,
	tenantID string,
	ownerID string,
	uploadID string,
) (model.UploadSession, error) {
	if strings.TrimSpace(ownerID) == "" {
		return model.UploadSession{}, ErrMultipartUploadNotFound
	}

	session, err := s.getSession(ctx, tenantID, uploadID)
	if err != nil {
		return model.UploadSession{}, err
	}
	if session.OwnerID != ownerID {
		// Do not reveal whether a different owner's upload exists.
		return model.UploadSession{}, ErrMultipartUploadNotFound
	}

	return session, nil
}

// getSession loads a tenant's session without an owner check; it is for
// system work such as reconciliation, never for client requests.
func (s *MultipartService) getSession(
	ctx context.Context,
	tenantID string,
	uploadID string,
) (model.UploadSession, error) {
	if strings.TrimSpace(tenantID) == "" || strings.TrimSpace(uploadID) == "" {
		return model.UploadSession{}, ErrMultipartUploadNotFound
	}

	session, err := s.uploads.GetByID(ctx, tenantID, uploadID)
	if errors.Is(err, model.ErrUploadNotFound) {
		return model.UploadSession{}, ErrMultipartUploadNotFound
	}
	if err != nil {
		return model.UploadSession{}, fmt.Errorf("get multipart session: %w", err)
	}

	return *session, nil
}

// syncProviderParts records the provider's parts in Neon and returns them.
// Upserting is idempotent, so a part whose confirmation was lost is
// recovered and an already-recorded one is left as it is.
func (s *MultipartService) syncProviderParts(
	ctx context.Context,
	session model.UploadSession,
	stored []interfaces.StoredPart,
) ([]model.UploadPart, error) {
	parts := make([]model.UploadPart, 0, len(stored))
	for _, actual := range stored {
		part := providerPart(session, actual, s.now())
		if err := s.parts.UpsertPart(ctx, session.TenantID, part); err != nil {
			return nil, fmt.Errorf("record provider part %d: %w", part.PartNumber, err)
		}
		parts = append(parts, part)
	}
	return parts, nil
}

func providerPart(session model.UploadSession, actual interfaces.StoredPart, now time.Time) model.UploadPart {
	return model.UploadPart{
		TenantID:   session.TenantID,
		UploadID:   session.ID,
		PartNumber: actual.PartNumber,
		ETag:       actual.ETag,
		SizeBytes:  actual.SizeBytes,
		Status:     model.UploadPartUploaded,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
}

// sameETag compares ETags whether or not they carry S3's quotes.
func sameETag(a, b string) bool {
	return strings.Trim(a, `"`) == strings.Trim(b, `"`)
}

func quarantineKey(tenantID, uploadID string) string {
	return fmt.Sprintf("quarantine/%s/%s", tenantID, uploadID)
}

func validateStartInput(input StartMultipartInput, maxSize int64) error {
	if strings.TrimSpace(input.TenantID) == "" ||
		strings.TrimSpace(input.OwnerID) == "" ||
		strings.TrimSpace(input.FileName) == "" ||
		strings.TrimSpace(input.ContentType) == "" ||
		strings.TrimSpace(input.IdempotencyKey) == "" {
		return ErrInvalidRequest
	}
	if input.SizeBytes <= 0 {
		return ErrInvalidRequest
	}
	if input.SizeBytes > maxSize {
		return ErrUploadTooLarge
	}
	if len(input.ChecksumSHA256) != sha256.Size*2 {
		return ErrInvalidRequest
	}
	if _, err := hex.DecodeString(input.ChecksumSHA256); err != nil {
		return ErrInvalidRequest
	}
	return nil
}

func sameMultipartRequest(
	session model.UploadSession,
	input StartMultipartInput,
) bool {
	return session.TenantID == input.TenantID &&
		session.OwnerID == input.OwnerID &&
		session.FileName == input.FileName &&
		session.ContentType == input.ContentType &&
		session.ExpectedSize == input.SizeBytes &&
		strings.EqualFold(session.ChecksumSHA256, input.ChecksumSHA256)
}

func validateCompletion(
	session model.UploadSession,
	recorded []model.UploadPart,
	input CompleteMultipartInput,
) ([]interfaces.CompletedPart, error) {
	if len(recorded) == 0 || len(input.Parts) == 0 ||
		len(recorded) != len(input.Parts) {
		return nil, ErrInvalidCompletion
	}
	if len(recorded) > maxMultipartParts {
		return nil, ErrInvalidCompletion
	}

	// Re-sort defensively even though the repository orders its results.
	sort.Slice(recorded, func(i, j int) bool {
		return recorded[i].PartNumber < recorded[j].PartNumber
	})

	var totalSize int64
	result := make([]interfaces.CompletedPart, 0, len(recorded))

	for i, part := range recorded {
		requested := input.Parts[i]

		if part.Status != model.UploadPartUploaded ||
			part.PartNumber != requested.PartNumber ||
			!sameETag(part.ETag, requested.ETag) ||
			strings.TrimSpace(part.ETag) == "" {
			return nil, ErrInvalidCompletion
		}

		// Every non-final part must meet the S3 multipart minimum.
		if i < len(recorded)-1 && part.SizeBytes < minNonFinalPartSize {
			return nil, fmt.Errorf(
				"%w: part %d is smaller than 5 MiB",
				ErrInvalidCompletion,
				part.PartNumber,
			)
		}

		if part.SizeBytes <= 0 {
			return nil, ErrInvalidCompletion
		}
		totalSize += part.SizeBytes
		result = append(result, interfaces.CompletedPart{
			PartNumber: part.PartNumber,
			ETag:       part.ETag,
		})
	}

	if totalSize != session.ExpectedSize {
		return nil, fmt.Errorf(
			"%w: expected %d bytes, recorded %d",
			ErrPartSizeMismatch,
			session.ExpectedSize,
			totalSize,
		)
	}

	return result, nil
}
