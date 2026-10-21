package memory

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// UploadRepository stores upload sessions in memeory.
// It is intended for development and uit testing, not production.
type UploadRepository struct {
	mu      sync.RWMutex
	uploads map[string]model.UploadSession
	// tenant, ownner and idempotency key identify a retry.
	idempotency map[string]string
	// outbox keeps the events written with transitions, for tests.
	outbox []model.OutboxEvent
}

var _ interfaces.UploadRepository = (*UploadRepository)(nil)

// NewUploadRepository initializes an empty in-memory repository.
func NewUploadRepository() *UploadRepository {
	return &UploadRepository{
		uploads:     make(map[string]model.UploadSession),
		idempotency: make(map[string]string),
	}
}

// Create stores a copy of upload session.
// The caller cannot mutate the sotred value tnrough its original pointer.
func (r *UploadRepository) Create(
	ctx context.Context,
	upload *model.UploadSession,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if upload == nil {
		return errors.New("upload session must not be nil")
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.uploads[upload.ID]; exists {
		return model.ErrUploadAlreadyExists
	}

	r.uploads[upload.ID] = boundSession(*upload)
	return nil
}

// boundSession gives a session stored without a binding the platform's,
// as the columns' default does.
func boundSession(s model.UploadSession) model.UploadSession {
	s.StorageConnectionID = connectionOrPlatform(s.StorageConnectionID)
	s.StagingConnectionID = connectionOrPlatform(s.StagingConnectionID)
	return s
}

// GetByID retrieves an upload belonging to the requeste tenant.
func (r *UploadRepository) GetByID(
	ctx context.Context,
	tenantID string,
	uploadID string,
) (*model.UploadSession, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	upload, exists := r.uploads[uploadID]
	if !exists || upload.TenantID != tenantID {
		return nil, model.ErrUploadNotFound
	}

	// Return a copy so callers cannot mutate repository state directly.
	result := upload
	return &result, nil
}

// GetByIdempotencyKey retrieves a prior request result.
// A missning key returns ErrUploadNotFound.
func (r *UploadRepository) GetByIdempotencyKey(
	ctx context.Context,
	tenantID string,
	ownerID string,
	key string,
) (*model.UploadSession, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	indexKey := makeIdempotencyKey(tenantID, ownerID, key)

	r.mu.RLock()
	defer r.mu.RUnlock()

	uploadID, exists := r.idempotency[indexKey]
	if !exists {
		return nil, model.ErrUploadNotFound
	}

	upload, exists := r.uploads[uploadID]
	if !exists {
		return nil, model.ErrUploadNotFound
	}

	result := upload
	return &result, nil
}

// CreateWithIdempotency atomically stores the session and its key.
// This additional method is used by the uopload service.
func (r *UploadRepository) CreateWithIdempotency(
	ctx context.Context,
	upload *model.UploadSession,
	key string,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	if upload == nil {
		return errors.New("upload session must not be nil")
	}

	indexKey := makeIdempotencyKey(
		upload.TenantID,
		upload.OwnerID,
		key,
	)

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.uploads[upload.ID]; exists {
		return model.ErrUploadAlreadyExists
	}

	if _, exists := r.idempotency[indexKey]; exists {
		return model.ErrIdempotencyConflict
	}

	r.uploads[upload.ID] = boundSession(*upload)
	r.idempotency[indexKey] = upload.ID

	return nil
}

// Update replaces the stored session after checking tenant ownership
func (r *UploadRepository) Update(
	ctx context.Context,
	upload *model.UploadSession,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	if upload == nil {
		return errors.New("upload session must not be nil")
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	current, exists := r.uploads[upload.ID]
	if !exists || current.TenantID != upload.TenantID {
		return model.ErrUploadNotFound
	}

	r.uploads[upload.ID] = *upload
	return nil
}

// TransitionStatus mirrors the PostgreSQL compare-and-set under the lock.
func (r *UploadRepository) TransitionStatus(
	ctx context.Context,
	tenantID string,
	uploadID string,
	from []model.UploadStatus,
	to model.UploadStatus,
	failureReason string,
	at time.Time,
	events ...model.OutboxEvent,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	current, exists := r.uploads[uploadID]
	if !exists || current.TenantID != tenantID {
		return model.ErrUploadNotFound
	}
	if !slices.Contains(from, current.Status) || !model.CanTransition(current.Status, to) {
		return model.ErrInvalidUploadState
	}

	current.Status = to
	if failureReason != "" {
		current.FailureReason = failureReason
	}
	stamp := at
	switch to {
	case model.UploadStatusCompleted:
		current.CompletedAt = &stamp
	case model.UploadStatusAborted, model.UploadStatusExpired:
		current.AbortedAt = &stamp
	}
	current.UpdatedAt = at
	r.uploads[uploadID] = current
	r.outbox = append(r.outbox, events...)
	return nil
}

// Outbox returns the events written with transitions so far.
func (r *UploadRepository) Outbox() []model.OutboxEvent {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return slices.Clone(r.outbox)
}

// SetStorageUploadID mirrors the PostgreSQL set-once update.
func (r *UploadRepository) SetStorageUploadID(
	ctx context.Context,
	tenantID string,
	uploadID string,
	storageUploadID string,
	at time.Time,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if storageUploadID == "" {
		return errors.New("storage upload ID must not be empty")
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	current, exists := r.uploads[uploadID]
	if !exists || current.TenantID != tenantID {
		return model.ErrUploadNotFound
	}
	if current.Status != model.UploadStatusPending ||
		(current.StorageUploadID != "" && current.StorageUploadID != storageUploadID) {
		return model.ErrInvalidUploadState
	}

	current.StorageUploadID = storageUploadID
	current.UpdatedAt = at
	r.uploads[uploadID] = current
	return nil
}

// makeIdempotencyKey keeps idempotency keys scoped to a tenant and owner
func makeIdempotencyKey(tenantID, ownerID, key string) string {
	return tenantID + "\x00" + ownerID + "\x00" + key
}
