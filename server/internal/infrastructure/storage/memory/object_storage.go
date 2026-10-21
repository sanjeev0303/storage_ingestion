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
	"strings"
	"sync"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

type upload struct {
	key       string
	parts     map[int32][]byte
	initiated time.Time
}

// ObjectStorage implements interfaces.MultipartStorage in memory.
type ObjectStorage struct {
	mu       sync.Mutex
	uploads  map[string]*upload
	objects  map[string][]byte
	modified map[string]time.Time
	// Now, when set, is the provider's clock for initiation and
	// modification times; otherwise the real time.
	Now    func() time.Time
	nextID int

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
	ListCalls     int

	PresignErr error // fail PresignGet once
	RangeErr   error // fail OpenRange once
	Presigns   []Presigned
	// WrapRange, when set, wraps every body OpenRange returns, to simulate
	// slow, failing or watched streams. ctx is the read's context, which a
	// real provider body honours.
	WrapRange  func(ctx context.Context, body io.ReadCloser) io.ReadCloser
	RangeCalls int

	DeleteErr  error // fail DeleteObject once, deleting nothing
	DeleteLost error // delete, then fail once (lost response or crash)
	ListErr    error // fail ListObjectsPage once
	// DeleteCalls counts DeleteObject and DeleteObjectVersion calls.
	DeleteCalls int
	// versions holds the current version ID of objects a test made
	// versioned, and versionData every version's key and bytes, so an
	// overwritten version stays readable by its ID as in S3. Other objects
	// behave as in an unversioned bucket.
	versions        map[string]string
	versionData     map[string]storedVersion
	DeletedVersions []string
	// Versioning is what Capabilities reports: a test of a versioned
	// bucket sets it.
	Versioning bool
	// Caps, when set, is what ProviderCapabilities reports, for a test of
	// a provider that cannot do something.
	Caps *model.ProviderCapabilities
	// WriteCalls counts streaming writes (transfers between connections).
	WriteCalls int
}

var _ interfaces.StorageProvider = (*ObjectStorage)(nil)

// WriteObject stores exactly size bytes from body.
func (s *ObjectStorage) WriteObject(ctx context.Context, _ string, key string, body io.Reader, size int64, _ string) (interfaces.ObjectMetadata, error) {
	data, err := io.ReadAll(io.LimitReader(body, size))
	if err != nil {
		return interfaces.ObjectMetadata{}, fmt.Errorf("read source: %w", err)
	}
	if int64(len(data)) != size {
		return interfaces.ObjectMetadata{}, fmt.Errorf("read source: %d of %d bytes", len(data), size)
	}
	s.mu.Lock()
	s.WriteCalls++
	s.objects[key] = data
	s.modified[key] = s.clock()
	s.newVersion(key)
	s.mu.Unlock()
	return s.HeadObject(ctx, "", key)
}

// ProviderCapabilities reports Caps, or what an S3 bucket can do.
func (s *ObjectStorage) ProviderCapabilities(ctx context.Context, _ string) (model.ProviderCapabilities, error) {
	if err := ctx.Err(); err != nil {
		return model.ProviderCapabilities{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Caps != nil {
		return *s.Caps, nil
	}
	caps := model.ProviderCapabilities{
		PresignedUpload: true, MaxSingleUploadBytes: 5 << 30, Multipart: true, MultipartKind: "S3", MaxParts: 10000,
		MinPartBytes: 5 << 20, PartValidator: "CONTENT", ListIncompleteUploads: true, ServerSideCopy: true, RangeReads: true,
		ConditionalReads: true, PresignedDownload: true, ListObjects: true, VersioningStatus: "", ChecksumAlgorithms: []string{"MD5"},
	}
	if s.Versioning {
		caps.ProviderVersioning, caps.VersioningStatus, caps.VersionIDKind = true, "Enabled", "S3_VERSION_ID"
	}
	return caps, nil
}

// Capabilities reports the bucket as versioned when Versioning is set.
func (s *ObjectStorage) Capabilities(ctx context.Context, _ string) (interfaces.StorageCapabilities, error) {
	if err := ctx.Err(); err != nil {
		return interfaces.StorageCapabilities{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Versioning {
		return interfaces.StorageCapabilities{ProviderVersioning: true, VersioningStatus: "Enabled"}, nil
	}
	return interfaces.StorageCapabilities{}, nil
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
	s.modified[destinationKey] = s.clock()
	s.newVersion(destinationKey)
	return take(&s.CopyLost)
}

// newVersion gives a written key a new version ID in a versioned bucket.
// The caller holds the lock.
func (s *ObjectStorage) newVersion(key string) {
	if !s.Versioning {
		return
	}
	if s.versions == nil {
		s.versions = make(map[string]string)
	}
	s.nextID++
	s.versions[key] = fmt.Sprintf("version-%d", s.nextID)
	s.keepVersion(key, s.versions[key])
}

type storedVersion struct {
	key  string
	data []byte
}

// keepVersion remembers key's current bytes as versionID. The caller holds
// the lock.
func (s *ObjectStorage) keepVersion(key, versionID string) {
	if s.versionData == nil {
		s.versionData = make(map[string]storedVersion)
	}
	s.versionData[versionID] = storedVersion{key: key, data: s.objects[key]}
}

// PutObject stores a whole object directly, as a completed upload would.
func (s *ObjectStorage) PutObject(key string, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[key] = data
	s.modified[key] = s.clock()
	s.newVersion(key)
}

var _ interfaces.MultipartStorage = (*ObjectStorage)(nil)

func NewObjectStorage() *ObjectStorage {
	return &ObjectStorage{
		uploads:  make(map[string]*upload),
		objects:  make(map[string][]byte),
		modified: make(map[string]time.Time),
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
	return interfaces.ObjectMetadata{SizeBytes: size, ETag: etag(data), VersionID: s.versions[key]}, nil
}

// DeleteObject removes an object. Like S3, deleting a missing key succeeds.
func (s *ObjectStorage) DeleteObject(ctx context.Context, _ string, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	s.DeleteCalls++
	if err := take(&s.DeleteErr); err != nil {
		return err
	}
	delete(s.objects, key)
	return take(&s.DeleteLost)
}

// SetVersion makes key versioned with the given current version ID.
func (s *ObjectStorage) SetVersion(key, versionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.versions == nil {
		s.versions = make(map[string]string)
	}
	s.versions[key] = versionID
	if _, ok := s.objects[key]; ok {
		s.keepVersion(key, versionID)
	}
}

// HeadObjectVersion finds one version of key, current or not.
func (s *ObjectStorage) HeadObjectVersion(ctx context.Context, _ string, key, versionID string) (interfaces.ObjectMetadata, error) {
	if err := ctx.Err(); err != nil {
		return interfaces.ObjectMetadata{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := take(&s.HeadErr); err != nil {
		return interfaces.ObjectMetadata{}, err
	}
	v, ok := s.versionData[versionID]
	if !ok || v.key != key {
		return interfaces.ObjectMetadata{}, fmt.Errorf("head object version: %w", interfaces.ErrObjectNotFound)
	}
	return interfaces.ObjectMetadata{SizeBytes: int64(len(v.data)), ETag: etag(v.data), VersionID: versionID}, nil
}

// DeleteObjectVersion removes key when versionID is its current version.
func (s *ObjectStorage) DeleteObjectVersion(ctx context.Context, _ string, key, versionID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	s.DeleteCalls++
	if err := take(&s.DeleteErr); err != nil {
		return err
	}
	s.DeletedVersions = append(s.DeletedVersions, versionID)
	if v, ok := s.versionData[versionID]; ok && v.key == key {
		delete(s.versionData, versionID)
	}
	if s.versions[key] == versionID {
		delete(s.objects, key)
		delete(s.versions, key)
	}
	return take(&s.DeleteLost)
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
	s.uploads[id] = &upload{key: key, parts: make(map[int32][]byte), initiated: s.clock()}
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
	s.modified[key] = s.clock()
	s.newVersion(key)
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

// OpenRange returns bytes first..last of an object, or all of it when
// last < 0, if the object still has the ETag in cond.IfMatch. A set
// VersionID reads that version, which only a versioned bucket has.
func (s *ObjectStorage) OpenRange(ctx context.Context, _ string, key string, first, last int64, cond interfaces.ReadCondition) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	s.RangeCalls++
	if err := take(&s.RangeErr); err != nil {
		return nil, err
	}
	data, ok := s.objects[key]
	if cond.VersionID != "" {
		v, found := s.versionData[cond.VersionID]
		data, ok = v.data, found && v.key == key
	}
	if !ok {
		return nil, fmt.Errorf("get object range: %w", interfaces.ErrObjectNotFound)
	}
	if cond.IfMatch != "" && cond.IfMatch != etag(data) {
		return nil, fmt.Errorf("get object range: %w", interfaces.ErrPreconditionFailed)
	}

	var body io.ReadCloser
	switch {
	case last < 0:
		body = io.NopCloser(bytes.NewReader(data))
	case first < 0 || first > last || last >= int64(len(data)):
		return nil, fmt.Errorf("get object range: %w", interfaces.ErrInvalidRange)
	default:
		body = io.NopCloser(bytes.NewReader(data[first : last+1]))
	}
	if s.WrapRange != nil {
		body = s.WrapRange(ctx, body)
	}
	return body, nil
}

// ETag returns the provider ETag of a stored object, as HeadObject would.
func (s *ObjectStorage) ETag(key string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return etag(s.objects[key])
}

func (s *ObjectStorage) clock() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now().UTC()
}

// ListMultipartUploadsPage pages through sessions in key, then ID, order.
func (s *ObjectStorage) ListMultipartUploadsPage(ctx context.Context, _ string, prefix string, cursor string, limit int) (interfaces.MultipartUploadPage, error) {
	if err := ctx.Err(); err != nil {
		return interfaces.MultipartUploadPage{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ListCalls++

	var all []interfaces.StoredMultipartUpload
	for id, u := range s.uploads {
		if strings.HasPrefix(u.key, prefix) {
			all = append(all, interfaces.StoredMultipartUpload{ObjectKey: u.key, StorageUploadID: id, Initiated: u.initiated})
		}
	}
	sort.Slice(all, func(i, j int) bool {
		return all[i].ObjectKey+"\x00"+all[i].StorageUploadID < all[j].ObjectKey+"\x00"+all[j].StorageUploadID
	})

	page := interfaces.MultipartUploadPage{}
	for _, u := range all {
		if cursor != "" && u.ObjectKey+"\x00"+u.StorageUploadID <= cursor {
			continue
		}
		if len(page.Uploads) == limit {
			last := page.Uploads[len(page.Uploads)-1]
			page.Cursor = last.ObjectKey + "\x00" + last.StorageUploadID
			break
		}
		page.Uploads = append(page.Uploads, u)
	}
	return page, nil
}

// ListObjectsPage pages through objects in key order.
func (s *ObjectStorage) ListObjectsPage(ctx context.Context, _ string, prefix string, cursor string, limit int) (interfaces.ObjectPage, error) {
	if err := ctx.Err(); err != nil {
		return interfaces.ObjectPage{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ListCalls++
	if err := take(&s.ListErr); err != nil {
		return interfaces.ObjectPage{}, err
	}

	var keys []string
	for key := range s.objects {
		if strings.HasPrefix(key, prefix) && key > cursor {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)

	page := interfaces.ObjectPage{}
	for _, key := range keys {
		if len(page.Objects) == limit {
			page.Cursor = page.Objects[len(page.Objects)-1].ObjectKey
			break
		}
		page.Objects = append(page.Objects, interfaces.StoredObject{
			ObjectKey: key, SizeBytes: int64(len(s.objects[key])), LastModified: s.modified[key], ETag: etag(s.objects[key]),
		})
	}
	return page, nil
}
