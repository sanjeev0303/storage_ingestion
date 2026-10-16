package interfaces

import (
	"context"
	"io"
	"time"
)

// CompletedPart discribes one upoloaded part that should be
// include when the storage provider assembles the final object.
type CompletedPart struct {
	PartNumber int32
	ETag       string
}

// StoredPart is a part as the provider reports it. Unlike a client's
// confirmation, it is evidence that the provider holds these bytes.
type StoredPart struct {
	PartNumber int32
	ETag       string
	SizeBytes  int64
}

// MultipartStorage contains the operations required to upload
// one large object multipart independently retryable parts.
//
// The application uses this interface instead of depending
// directly on the AWS SDK
type MultipartStorage interface {
	ObjectStorage

	BeginMultipart(
		ctx context.Context,
		bucket string,
		objectKey string,
		contentType string,
	) (string, error)

	PresignPart(
		ctx context.Context,
		bucket string,
		objectKey string,
		storageUploadID string,
		partNumber int32,
		ttl time.Duration,
	) (string, error)

	CompleteMultipart(
		ctx context.Context,
		bucket string,
		objectKey string,
		storageUploadID string,
		parts []CompletedPart,
	) error

	AbortMultipart(
		ctx context.Context,
		bucket string,
		objectKey string,
		storageUploadID string,
	) error

	// ListParts returns every part the provider holds for an active
	// multipart upload, in part-number order. It returns
	// ErrStorageUploadNotFound when the upload no longer exists.
	ListParts(
		ctx context.Context,
		bucket string,
		objectKey string,
		storageUploadID string,
	) ([]StoredPart, error)

	// ListMultipartUploads returns the IDs of the in-progress multipart
	// uploads for exactly this object key.
	ListMultipartUploads(
		ctx context.Context,
		bucket string,
		objectKey string,
	) ([]string, error)

	// OpenObject streams a stored object. The caller must close it.
	// It returns ErrObjectNotFound when the object does not exist.
	OpenObject(
		ctx context.Context,
		bucket string,
		objectKey string,
	) (io.ReadCloser, error)

	// CopyObject copies an object inside the provider, without the bytes
	// passing through the application. Copying again overwrites the
	// destination with the same bytes, so it is safe to retry.
	CopyObject(
		ctx context.Context,
		sourceBucket string,
		sourceKey string,
		destinationBucket string,
		destinationKey string,
	) error
}
