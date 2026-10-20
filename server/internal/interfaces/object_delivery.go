package interfaces

import (
	"context"
	"errors"
	"io"
	"time"
)

// Delivery-relevant provider outcomes. Adapters wrap them with %w so the
// service can answer without seeing provider details.
var (
	// ErrPreconditionFailed: the stored object is not the version the read
	// was conditioned on (If-Match or version ID).
	ErrPreconditionFailed = errors.New("storage object changed")
	// ErrInvalidRange: the provider refused the byte range.
	ErrInvalidRange = errors.New("storage range not satisfiable")
	// ErrStorageThrottled: the provider asked us to slow down.
	ErrStorageThrottled = errors.New("storage throttled the request")
	// ErrStorageAccessDenied: our own credentials may not read the object.
	ErrStorageAccessDenied = errors.New("storage access denied")
)

// DownloadOptions shape the response the storage provider sends for a
// presigned download. All are signed into the URL.
type DownloadOptions struct {
	ContentDisposition string
	ContentType        string
	// VersionID pins the URL to one object version where the provider
	// versions objects.
	VersionID string
}

// ReadCondition pins a read to one stored object. Empty fields are not
// checked.
type ReadCondition struct {
	IfMatch   string // the object's ETag, quoted as the provider returned it
	VersionID string
}

// ObjectDelivery is what delivery needs from storage. Objects stay
// private: access is either a short-lived presigned URL or a stream the
// API itself authorizes.
type ObjectDelivery interface {
	// PresignGet returns a URL valid for ttl that downloads exactly
	// bucket/objectKey. The URL is a bearer credential: never log or store it.
	PresignGet(ctx context.Context, bucket, objectKey string, ttl time.Duration, options DownloadOptions) (string, error)

	// HeadObject returns the stored object's size and validators.
	HeadObject(ctx context.Context, bucket, objectKey string) (ObjectMetadata, error)

	// OpenRange streams bytes first..last (inclusive) of an object, or the
	// whole object when last < 0, only if the object still matches cond.
	// The caller closes it. Errors wrap ErrObjectNotFound,
	// ErrPreconditionFailed, ErrInvalidRange, ErrStorageThrottled or
	// ErrStorageAccessDenied where the provider said so.
	OpenRange(ctx context.Context, bucket, objectKey string, first, last int64, cond ReadCondition) (io.ReadCloser, error)
}
