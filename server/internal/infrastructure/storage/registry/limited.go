package registry

import (
	"context"
	"io"
	"sync"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/metrics"
)

var _ interfaces.StorageProvider = (*limited)(nil)

// limited bounds one connection's provider: at most n operations at once
// (an open read stream holds its slot until closed), each metadata call
// under the operation timeout. A customer's slow or failing provider
// fills its own slots and nobody else's. Metrics are per provider type,
// never per connection or tenant.
type limited struct {
	inner    interfaces.StorageProvider
	provider string
	slots    chan struct{}
	timeout  time.Duration
	metrics  *metrics.Registry
}

func newLimited(inner interfaces.StorageProvider, provider model.StorageProviderType, n int, timeout time.Duration, m *metrics.Registry) *limited {
	if n < 1 {
		n = 32
	}
	return &limited{inner: inner, provider: string(provider), slots: make(chan struct{}, n), timeout: timeout, metrics: m}
}

func (l *limited) acquire(ctx context.Context) error {
	select {
	case l.slots <- struct{}{}:
		return nil
	case <-ctx.Done():
		l.metrics.Add("storage."+l.provider+".saturated", 1)
		return ctx.Err()
	}
}

func (l *limited) release() { <-l.slots }

func (l *limited) record(op string, start time.Time, err error) {
	name := "storage." + l.provider + "." + op
	l.metrics.Add(name+".count", 1)
	l.metrics.Add(name+".ms", time.Since(start).Milliseconds())
	if err != nil {
		l.metrics.Add(name+".errors", 1)
	}
}

// call runs a short operation in a slot, under the operation timeout.
func call[T any](l *limited, ctx context.Context, op string, fn func(context.Context) (T, error)) (T, error) {
	var zero T
	if err := l.acquire(ctx); err != nil {
		return zero, err
	}
	defer l.release()
	if l.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, l.timeout)
		defer cancel()
	}
	start := time.Now()
	v, err := fn(ctx)
	l.record(op, start, err)
	return v, err
}

// transfer runs a long operation (a copy or a streaming write) in a slot,
// bounded only by the caller's context.
func (l *limited) transfer(ctx context.Context, op string, fn func() error) error {
	if err := l.acquire(ctx); err != nil {
		return err
	}
	defer l.release()
	l.metrics.Add("storage.active_transfers", 1)
	defer l.metrics.Add("storage.active_transfers", -1)
	start := time.Now()
	err := fn()
	l.record(op, start, err)
	return err
}

// stream opens a read that keeps its slot until the reader is closed.
func (l *limited) stream(ctx context.Context, op string, open func() (io.ReadCloser, error)) (io.ReadCloser, error) {
	if err := l.acquire(ctx); err != nil {
		return nil, err
	}
	start := time.Now()
	r, err := open()
	l.record(op, start, err)
	if err != nil {
		l.release()
		return nil, err
	}
	l.metrics.Add("storage.active_transfers", 1)
	return &slotReader{ReadCloser: r, done: func() {
		l.metrics.Add("storage.active_transfers", -1)
		l.release()
	}}, nil
}

type slotReader struct {
	io.ReadCloser
	once sync.Once
	done func()
}

func (r *slotReader) Close() error {
	err := r.ReadCloser.Close()
	r.once.Do(r.done)
	return err
}

func (l *limited) CreateUploadPlan(ctx context.Context, target interfaces.UploadTarget) (interfaces.UploadPlan, error) {
	return call(l, ctx, "presign_upload", func(ctx context.Context) (interfaces.UploadPlan, error) {
		return l.inner.CreateUploadPlan(ctx, target)
	})
}

func (l *limited) HeadObject(ctx context.Context, bucket, key string) (interfaces.ObjectMetadata, error) {
	return call(l, ctx, "head", func(ctx context.Context) (interfaces.ObjectMetadata, error) {
		return l.inner.HeadObject(ctx, bucket, key)
	})
}

func (l *limited) HeadObjectVersion(ctx context.Context, bucket, key, versionID string) (interfaces.ObjectMetadata, error) {
	return call(l, ctx, "head", func(ctx context.Context) (interfaces.ObjectMetadata, error) {
		return l.inner.HeadObjectVersion(ctx, bucket, key, versionID)
	})
}

func (l *limited) DeleteObject(ctx context.Context, bucket, key string) error {
	_, err := call(l, ctx, "delete", func(ctx context.Context) (struct{}, error) {
		return struct{}{}, l.inner.DeleteObject(ctx, bucket, key)
	})
	return err
}

func (l *limited) DeleteObjectVersion(ctx context.Context, bucket, key, versionID string) error {
	_, err := call(l, ctx, "delete", func(ctx context.Context) (struct{}, error) {
		return struct{}{}, l.inner.DeleteObjectVersion(ctx, bucket, key, versionID)
	})
	return err
}

func (l *limited) BeginMultipart(ctx context.Context, bucket, key, contentType string) (string, error) {
	return call(l, ctx, "multipart_begin", func(ctx context.Context) (string, error) {
		return l.inner.BeginMultipart(ctx, bucket, key, contentType)
	})
}

func (l *limited) PresignPart(ctx context.Context, bucket, key, uploadID string, partNumber int32, ttl time.Duration) (string, error) {
	return call(l, ctx, "presign_part", func(ctx context.Context) (string, error) {
		return l.inner.PresignPart(ctx, bucket, key, uploadID, partNumber, ttl)
	})
}

func (l *limited) CompleteMultipart(ctx context.Context, bucket, key, uploadID string, parts []interfaces.CompletedPart) error {
	return l.transfer(ctx, "multipart_complete", func() error {
		return l.inner.CompleteMultipart(ctx, bucket, key, uploadID, parts)
	})
}

func (l *limited) AbortMultipart(ctx context.Context, bucket, key, uploadID string) error {
	_, err := call(l, ctx, "multipart_abort", func(ctx context.Context) (struct{}, error) {
		return struct{}{}, l.inner.AbortMultipart(ctx, bucket, key, uploadID)
	})
	return err
}

func (l *limited) ListParts(ctx context.Context, bucket, key, uploadID string) ([]interfaces.StoredPart, error) {
	return call(l, ctx, "list_parts", func(ctx context.Context) ([]interfaces.StoredPart, error) {
		return l.inner.ListParts(ctx, bucket, key, uploadID)
	})
}

func (l *limited) ListMultipartUploads(ctx context.Context, bucket, key string) ([]string, error) {
	return call(l, ctx, "list_uploads", func(ctx context.Context) ([]string, error) {
		return l.inner.ListMultipartUploads(ctx, bucket, key)
	})
}

func (l *limited) ListMultipartUploadsPage(ctx context.Context, bucket, prefix, cursor string, limit int) (interfaces.MultipartUploadPage, error) {
	return call(l, ctx, "list_uploads", func(ctx context.Context) (interfaces.MultipartUploadPage, error) {
		return l.inner.ListMultipartUploadsPage(ctx, bucket, prefix, cursor, limit)
	})
}

func (l *limited) ListObjectsPage(ctx context.Context, bucket, prefix, cursor string, limit int) (interfaces.ObjectPage, error) {
	return call(l, ctx, "list_objects", func(ctx context.Context) (interfaces.ObjectPage, error) {
		return l.inner.ListObjectsPage(ctx, bucket, prefix, cursor, limit)
	})
}

func (l *limited) PresignGet(ctx context.Context, bucket, key string, ttl time.Duration, options interfaces.DownloadOptions) (string, error) {
	return call(l, ctx, "presign_download", func(ctx context.Context) (string, error) {
		return l.inner.PresignGet(ctx, bucket, key, ttl, options)
	})
}

func (l *limited) ProviderCapabilities(ctx context.Context, bucket string) (model.ProviderCapabilities, error) {
	return call(l, ctx, "capabilities", func(ctx context.Context) (model.ProviderCapabilities, error) {
		return l.inner.ProviderCapabilities(ctx, bucket)
	})
}

func (l *limited) OpenObject(ctx context.Context, bucket, key string) (io.ReadCloser, error) {
	return l.stream(ctx, "read", func() (io.ReadCloser, error) { return l.inner.OpenObject(ctx, bucket, key) })
}

func (l *limited) OpenRange(ctx context.Context, bucket, key string, first, last int64, cond interfaces.ReadCondition) (io.ReadCloser, error) {
	return l.stream(ctx, "read", func() (io.ReadCloser, error) {
		return l.inner.OpenRange(ctx, bucket, key, first, last, cond)
	})
}

func (l *limited) CopyObject(ctx context.Context, sourceBucket, sourceKey, destinationBucket, destinationKey string) error {
	return l.transfer(ctx, "copy", func() error {
		return l.inner.CopyObject(ctx, sourceBucket, sourceKey, destinationBucket, destinationKey)
	})
}

func (l *limited) WriteObject(ctx context.Context, bucket, key string, body io.Reader, size int64, contentType string) (interfaces.ObjectMetadata, error) {
	var meta interfaces.ObjectMetadata
	err := l.transfer(ctx, "write", func() error {
		var err error
		meta, err = l.inner.WriteObject(ctx, bucket, key, body, size, contentType)
		return err
	})
	return meta, err
}

// Object Lock goes through to providers that have it; others report it
// off.

func (l *limited) ObjectLockEnabled(ctx context.Context, bucket string) (bool, error) {
	locker, ok := l.inner.(interfaces.ObjectLocker)
	if !ok {
		return false, nil
	}
	return call(l, ctx, "object_lock", func(ctx context.Context) (bool, error) { return locker.ObjectLockEnabled(ctx, bucket) })
}

func (l *limited) LockObjectVersion(ctx context.Context, bucket, key, versionID string, until time.Time, legalHold bool) error {
	locker, ok := l.inner.(interfaces.ObjectLocker)
	if !ok {
		return model.ErrUnsupportedProvider
	}
	_, err := call(l, ctx, "object_lock", func(ctx context.Context) (struct{}, error) {
		return struct{}{}, locker.LockObjectVersion(ctx, bucket, key, versionID, until, legalHold)
	})
	return err
}
