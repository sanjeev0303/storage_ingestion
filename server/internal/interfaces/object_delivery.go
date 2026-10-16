package interfaces

import (
	"context"
	"io"
	"time"
)

// DownloadOptions shape the response the storage provider sends for a
// presigned download. Both are signed into the URL.
type DownloadOptions struct {
	ContentDisposition string
	ContentType        string
}

// ObjectDelivery is what delivery needs from storage. Objects stay
// private: access is either a short-lived presigned URL or a stream the
// API itself authorizes.
type ObjectDelivery interface {
	// PresignGet returns a URL valid for ttl that downloads exactly
	// bucket/objectKey. The URL is a bearer credential: never log or store it.
	PresignGet(ctx context.Context, bucket, objectKey string, ttl time.Duration, options DownloadOptions) (string, error)

	// OpenRange streams bytes first..last (inclusive) of an object, or the
	// whole object when last < 0. The caller closes it. It returns
	// ErrObjectNotFound when the object does not exist.
	OpenRange(ctx context.Context, bucket, objectKey string, first, last int64) (io.ReadCloser, error)
}
