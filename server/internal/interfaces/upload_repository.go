package interfaces

import (
	"context"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// UploadRepository defines persistence operations for upload sessions.
// Implementations must be safe for concurrent use.

type UploadRepository interface {
	Create(
		ctx context.Context,
		upload *model.UploadSession,
	) error

	CreateWithIdempotency(
		ctx context.Context,
		upload *model.UploadSession,
		key string,
	) error

	GetByID(
		ctx context.Context,
		tenantID string,
		uploadID string,
	) (*model.UploadSession, error)

	GetByIdempotencyKey(
		ctx context.Context,
		tenantID string,
		ownerID string,
		key string,
	) (*model.UploadSession, error)

	Update(
		ctx context.Context,
		upload *model.UploadSession,
	) error

	// TransitionStatus moves the session to `to` only while its current
	// status is one of `from`, as one conditional write. It returns
	// model.ErrInvalidUploadState when the session is in any other status,
	// so a stale caller can never overwrite a newer state. Events are
	// written to the outbox in the same transaction: both commit, or
	// neither does.
	TransitionStatus(
		ctx context.Context,
		tenantID string,
		uploadID string,
		from []model.UploadStatus,
		to model.UploadStatus,
		failureReason string,
		at time.Time,
		events ...model.OutboxEvent,
	) error

	// SetStorageUploadID records the provider upload ID once, and only
	// while the session is PENDING. Recording the same ID again succeeds.
	SetStorageUploadID(
		ctx context.Context,
		tenantID string,
		uploadID string,
		storageUploadID string,
		at time.Time,
	) error
}
