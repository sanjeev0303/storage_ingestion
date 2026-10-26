package interfaces

import (
	"context"
	"errors"
	"time"
)

// Provider outcomes that are known, not ambiguous. Adapters wrap them with
// %w. Any other storage error may be transient or of unknown outcome.
var (
	// ErrObjectNotFound: the provider answered that the object is absent.
	ErrObjectNotFound = errors.New("storage object not found")
	// ErrStorageUploadNotFound: the provider answered that the multipart
	// upload is absent (completed, aborted, or never created).
	ErrStorageUploadNotFound = errors.New("storage multipart upload not found")
)

// UploadTarget describes the object that the storage adapter
// should prepare for upload.
type UploadTarget struct {
	Bucket      string
	ObjectKey   string
	ContentType string
	SizeBytes   int64
	ExpiresAt   time.Time
}

// UploadPlan contains the information the API needs to give
// the client permission to upload an object.
type UploadPlan struct {
	UploadURL string
	ExpiresAt time.Time
	// Headers the client must send with the upload, beyond Content-Type
	// (Azure needs x-ms-blob-type). Empty for S3 and GCS.
	Headers map[string]string
}

// ObjectStorage describes the storage operations needed by
// the application, independently of any cloud provider.
type ObjectStorage interface {
	CreateUploadPlan(
		ctx context.Context,
		target UploadTarget,
	) (UploadPlan, error)

	HeadObject(
		ctx context.Context,
		bucket string,
		objectKey string,
	) (ObjectMetadata, error)

	// DeleteObject removes the object, and an object already gone is not
	// an error. On a bucket that keeps versions, deleting by key alone
	// would only hide the bytes behind a delete marker or a noncurrent
	// version, so every version of exactly this key goes too. Keys are
	// generated per upload or asset, so all of them are that record's.
	DeleteObject(
		ctx context.Context,
		bucket string,
		objectKey string,
	) error
}

// ObjectMetadata contains information verified by the storage adapter.
type ObjectMetadata struct {
	SizeBytes   int64
	ContentType string
	ETag        string
	VersionID   string
}
