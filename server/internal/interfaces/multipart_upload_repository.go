package interfaces

import (
	"context"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// MultipartUploadRepository persists uploaded-part metadata.
// The actual part bytes remain in object storage.
type MultipartUploadRepository interface {
	UpsertPart(
		ctx context.Context,
		tenantID string,
		part model.UploadPart,
	) error

	ListParts(
		ctx context.Context,
		tenantID string,
		uploadID string,
	) ([]model.UploadPart, error)

	DeleteParts(
		ctx context.Context,
		tenantID string,
		uploadID string,
	) error
}
