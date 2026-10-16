// Package memory is an in-memory multipart object store for tests. It
// behaves like S3 where reconciliation depends on it (absent uploads and
// objects, part listings, completion consuming the upload) and lets a test
// inject transient, permanent or ambiguous failures.
package memory

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
)

type upload struct {
	key   string
	parts map[int32][]byte
}

// ObjectStorage implements interfaces.MultipartStorage in memory.
type ObjectStorage struct {
	mu      sync.Mutex
	uploads map[string]*upload
	objects map[string][]byte
	nextID  int

	// Failure injection. Each is returned once and then cleared.
	BeginErr     error // fail without creating an upload
	BeginLost    error // create the upload, then fail (lost response)
	CompleteErr  error // fail without completing
	CompleteLost error // complete, then fail (lost response)
	AbortErr     error // fail without aborting
	HeadErr      error // fail HeadObject
	CopyErr      error // fail without copying
	CopyLost     error // copy, then fail (lost response)
	// CopyCorrupts, when true, makes the next copy write damaged bytes, to
	// simulate a destination that fails verification.
	CopyCorrupts bool
	// SizeOverride, when set, is reported by HeadObject instead of the
	// real size, to simulate a provider object of the wrong length.
	SizeOverride int64

	BeginCalls    int
	CompleteCalls int
	AbortCalls    int
	CopyCalls     int

	PresignErr error // fail PresignGet once
	RangeErr   error // fail OpenRange once
	Presigns   []Presigned
}

func (s *ObjectStorage) CopyObject(ctx context.Context, _ string, sourceKey string, _ string, destinationKey string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	s.CopyCalls++
	if err := take(&s.CopyErr); err != nil {
		return err
	}
	data, ok := s.objects[sourceKey]
	if !ok {
		return fmt.Errorf("copy object: %w", interfaces.ErrObjectNotFound)
	}
	copied := append([]byte(nil), data...)
	if s.CopyCorrupts {
		s.CopyCorrupts = false
		copied[0] ^= 0xff
	}
	s.objects[destinationKey] = copied
	return take(&s.CopyLost)
}

// PutObject stores a whole object directly, as a completed upload would.
func (s *ObjectStorage) PutObject(key string, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[key] = data
}

var _ interfaces.MultipartStorage = (*ObjectStorage)(nil)

func NewObjectStorage() *ObjectStorage {
	return &ObjectStorage{
		uploads: make(map[string]*upload),
		objects: make(map[string][]byte),
	}
}

func take(err *error) error {
	e := *err
	*err = nil
	return e
}

func (s *ObjectStorage) CreateUploadPlan(_ context.Context, target interfaces.UploadTarget) (interfaces.UploadPlan, error) {
	return interfaces.UploadPlan{UploadURL: "https://storage.test/" + target.ObjectKey, ExpiresAt: target.ExpiresAt}, nil
}

func (s *ObjectStorage) HeadObject(ctx context.Context, _ string, key string) (interfaces.ObjectMetadata, error) {
	if err := ctx.Err(); err != nil {
		return interfaces.ObjectMetadata{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := take(&s.HeadErr); err != nil {
		return interfaces.ObjectMetadata{}, err
	}
	data, ok := s.objects[key]
	if !ok {
		return interfaces.ObjectMetadata{}, fmt.Errorf("head object: %w", interfaces.ErrObjectNotFound)
	}
	size := int64(len(data))
	if s.SizeOverride != 0 {
		size = s.SizeOverride
	}
	return interfaces.ObjectMetadata{SizeBytes: size, ETag: etag(data)}, nil
}

func (s *ObjectStorage) DeleteObject(ctx context.Context, _ string, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.objects, key)
	return nil
}

func (s *ObjectStorage) BeginMultipart(ctx context.Context, _ string, key string, _ string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	s.BeginCalls++
	if err := take(&s.BeginErr); err != nil {
		return "", err
	}
	id := s.addUpload(key)
	if err := take(&s.BeginLost); err != nil {
		return "", err
	}
	return id, nil
}

// AddUpload creates a provider upload directly, as if some earlier,
// unrecorded initiation had succeeded.
func (s *ObjectStorage) AddUpload(key string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addUpload(key)
}

func (s *ObjectStorage) addUpload(key string) string {
	s.nextID++
	id := fmt.Sprintf("provider-upload-%d", s.nextID)
	s.uploads[id] = &upload{key: key, parts: make(map[int32][]byte)}
	return id
}

func (s *ObjectStorage) PresignPart(_ context.Context, _ string, key string, uploadID string, part int32, _ time.Duration) (string, error) {
	return fmt.Sprintf("https://storage.test/%s?uploadId=%s&partNumber=%d", key, uploadID, part), nil
}

// PutPart stores a part as a client's direct upload would, and returns
// the ETag the provider reports for it.
func (s *ObjectStorage) PutPart(uploadID string, part int32, data []byte) string {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.uploads[uploadID].parts[part] = data
	return etag(data)
}

func (s *ObjectStorage) CompleteMultipart(ctx context.Context, _ string, key string, uploadID string, parts []interfaces.CompletedPart) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	s.CompleteCalls++
	if err := take(&s.CompleteErr); err != nil {
		return err
	}
	u, ok := s.uploads[uploadID]
	if !ok || u.key != key {
		return fmt.Errorf("complete multipart upload: %w", interfaces.ErrStorageUploadNotFound)
	}

	var object bytes.Buffer
	for _, p := range parts {
		data, ok := u.parts[p.PartNumber]
		if !ok || etag(data) != p.ETag {
			return fmt.Errorf("complete multipart upload: InvalidPart %d", p.PartNumber)
		}
		object.Write(data)
	}
	s.objects[key] = object.Bytes()
	delete(s.uploads, uploadID)

	return take(&s.CompleteLost)
}

func (s *ObjectStorage) AbortMultipart(ctx context.Context, _ string, key string, uploadID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	s.AbortCalls++
	if err := take(&s.AbortErr); err != nil {
		return err
	}
	u, ok := s.uploads[uploadID]
	if !ok || u.key != key {
		return fmt.Errorf("abort multipart upload: %w", interfaces.ErrStorageUploadNotFound)
	}
	delete(s.uploads, uploadID)
	return nil
}

func (s *ObjectStorage) ListParts(ctx context.Context, _ string, key string, uploadID string) ([]interfaces.StoredPart, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	u, ok := s.uploads[uploadID]
	if !ok || u.key != key {
		return nil, fmt.Errorf("list multipart parts: %w", interfaces.ErrStorageUploadNotFound)
	}

	parts := make([]interfaces.StoredPart, 0, len(u.parts))
	for n, data := range u.parts {
		parts = append(parts, interfaces.StoredPart{PartNumber: n, ETag: etag(data), SizeBytes: int64(len(data))})
	}
	sort.Slice(parts, func(i, j int) bool { return parts[i].PartNumber < parts[j].PartNumber })
	return parts, nil
}

func (s *ObjectStorage) ListMultipartUploads(ctx context.Context, _ string, key string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	var ids []string
	for id, u := range s.uploads {
		if u.key == key {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids, nil
}

func (s *ObjectStorage) OpenObject(ctx context.Context, _ string, key string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	data, ok := s.objects[key]
	if !ok {
		return nil, fmt.Errorf("get object: %w", interfaces.ErrObjectNotFound)
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

// Object returns a stored object and whether it exists.
func (s *ObjectStorage) Object(key string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.objects[key]
	return data, ok
}

// UploadCount reports how many multipart uploads are still in progress.
func (s *ObjectStorage) UploadCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.uploads)
}

// etag mimics S3's quoted MD5 part ETag. It is not a content checksum the
// application relies on.
func etag(data []byte) string {
	sum := md5.Sum(data)
	return `"` + hex.EncodeToString(sum[:]) + `"`
}

var _ interfaces.ObjectDelivery = (*ObjectStorage)(nil)

// Presigned records one PresignGet call, for assertions.
type Presigned struct {
	Bucket  string
	Key     string
	TTL     time.Duration
	Options interfaces.DownloadOptions
}

// PresignGet returns a fake URL and records the request. PresignErr, when
// set, is returned once instead.
func (s *ObjectStorage) PresignGet(ctx context.Context, bucket, key string, ttl time.Duration, options interfaces.DownloadOptions) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := take(&s.PresignErr); err != nil {
		return "", err
	}
	s.Presigns = append(s.Presigns, Presigned{Bucket: bucket, Key: key, TTL: ttl, Options: options})
	return fmt.Sprintf("https://storage.test/%s/%s?X-Amz-Expires=%d&X-Amz-Signature=fake", bucket, key, int(ttl.Seconds())), nil
}

// OpenRange returns bytes first..last of an object, or all of it when last < 0.
func (s *ObjectStorage) OpenRange(ctx context.Context, _ string, key string, first, last int64) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := take(&s.RangeErr); err != nil {
		return nil, err
	}
	data, ok := s.objects[key]
	if !ok {
		return nil, fmt.Errorf("get object range: %w", interfaces.ErrObjectNotFound)
	}
	if last < 0 {
		return io.NopCloser(bytes.NewReader(data)), nil
	}
	if first < 0 || first > last || last >= int64(len(data)) {
		return nil, fmt.Errorf("invalid byte range %d-%d", first, last)
	}
	return io.NopCloser(bytes.NewReader(data[first : last+1])), nil
}
