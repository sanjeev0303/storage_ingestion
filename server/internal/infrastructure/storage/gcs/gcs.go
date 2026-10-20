// Package gcs is the Google Cloud Storage adapter, on the official Go
// SDK. It implements the platform's storage operations the GCS way:
//
//   - Signed URLs (V4) for client uploads and downloads.
//   - Large uploads as a parallel composite upload: each part is its own
//     object under "<key>.parts/<upload ID>/", uploaded with a signed URL,
//     then composed (at most 32 sources per compose, in levels) into the
//     final object, after which the parts are deleted. A marker object
//     records the upload and its content type.
//   - Object generations as version IDs. A generation is not an S3 version
//     ID: it is kept as the provider's own decimal identifier, and reads
//     and deletes pin it with GCS's generation conditions.
//   - Composite objects carry a CRC32C but no MD5; the platform verifies
//     SHA-256 by reading the bytes, so no provider checksum is relied on.
package gcs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/storage"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/retry"
)

var _ interfaces.StorageProvider = (*Storage)(nil)

// Config connects to one bucket's project.
type Config struct {
	// ServiceAccountJSON authenticates the platform and signs URLs.
	ServiceAccountJSON []byte
	// HTTPClient is the network-guarded client every request goes through.
	HTTPClient *http.Client
	// Endpoint, when set, is an emulator's JSON API URL; requests to it are
	// unauthenticated and signed URLs point at it. Local testing only.
	Endpoint string
	// PartBytes is the chunk of a streaming write.
	PartBytes int
}

// Storage is a GCS bucket client.
type Storage struct {
	client     *storage.Client
	accessID   string
	privateKey []byte
	// signHost and insecure point signed URLs at an emulator.
	signHost  string
	insecure  bool
	partBytes int
}

// New builds a GCS client from a service account key.
func New(ctx context.Context, cfg Config) (*Storage, error) {
	if cfg.HTTPClient == nil {
		return nil, errors.New("a GCS client needs the guarded HTTP client")
	}
	s := &Storage{partBytes: cfg.PartBytes}
	if s.partBytes <= 0 {
		s.partBytes = 16 << 20
	}
	var opts []option.ClientOption
	if len(cfg.ServiceAccountJSON) > 0 {
		jwt, err := google.JWTConfigFromJSON(cfg.ServiceAccountJSON, storage.ScopeFullControl)
		if err != nil {
			return nil, fmt.Errorf("%w: the service account key is not valid", model.ErrInvalidConnection)
		}
		s.accessID, s.privateKey = jwt.Email, jwt.PrivateKey
		if cfg.Endpoint == "" {
			// Token requests go through the guarded client too.
			tokenCtx := context.WithValue(context.Background(), oauth2.HTTPClient, cfg.HTTPClient)
			opts = append(opts, option.WithHTTPClient(oauth2.NewClient(tokenCtx, jwt.TokenSource(tokenCtx))))
		}
	} else if cfg.Endpoint == "" {
		return nil, fmt.Errorf("%w: GCS needs a service account key", model.ErrCredentialMissing)
	}
	if cfg.Endpoint != "" {
		u, err := url.Parse(cfg.Endpoint)
		if err != nil {
			return nil, err
		}
		s.signHost, s.insecure = u.Host, u.Scheme == "http"
		opts = append(opts, option.WithEndpoint(cfg.Endpoint), option.WithoutAuthentication(), option.WithHTTPClient(cfg.HTTPClient))
	}
	client, err := storage.NewClient(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("create GCS client: %w", err)
	}
	s.client = client
	return s, nil
}

// Close releases the client's connections.
func (s *Storage) Close() error { return s.client.Close() }

func (s *Storage) object(bucket, key string) *storage.ObjectHandle {
	return s.client.Bucket(bucket).Object(key)
}

// sign makes a V4 signed URL for one object.
func (s *Storage) sign(bucket, key, method string, expires time.Time, contentType string, query url.Values) (string, error) {
	if s.privateKey == nil {
		return "", fmt.Errorf("%w: signed URLs need a service account key", model.ErrUnsupportedProvider)
	}
	opts := &storage.SignedURLOptions{
		Scheme: storage.SigningSchemeV4, Method: method, Expires: expires, ContentType: contentType,
		GoogleAccessID: s.accessID, PrivateKey: s.privateKey, QueryParameters: query, Style: storage.PathStyle(),
	}
	if s.signHost != "" {
		opts.Hostname, opts.Insecure = s.signHost, s.insecure
	}
	return s.client.Bucket(bucket).SignedURL(key, opts)
}

func (s *Storage) CreateUploadPlan(_ context.Context, target interfaces.UploadTarget) (interfaces.UploadPlan, error) {
	if target.Bucket == "" || target.ObjectKey == "" {
		return interfaces.UploadPlan{}, errors.New("bucket and object key are required")
	}
	expires := target.ExpiresAt
	if time.Until(expires) <= 0 {
		return interfaces.UploadPlan{}, errors.New("upload URL expiry must be in the future")
	}
	if time.Until(expires) > 7*24*time.Hour {
		expires = time.Now().Add(7 * 24 * time.Hour)
	}
	u, err := s.sign(target.Bucket, target.ObjectKey, http.MethodPut, expires, target.ContentType, nil)
	if err != nil {
		return interfaces.UploadPlan{}, fmt.Errorf("sign upload: %w", err)
	}
	return interfaces.UploadPlan{UploadURL: u, ExpiresAt: expires.UTC()}, nil
}

func metadataOf(a *storage.ObjectAttrs) interfaces.ObjectMetadata {
	gen := strconv.FormatInt(a.Generation, 10)
	// The generation identifies the stored bytes; it serves as both the
	// validator and the version.
	return interfaces.ObjectMetadata{SizeBytes: a.Size, ContentType: a.ContentType, ETag: gen, VersionID: gen}
}

func (s *Storage) HeadObject(ctx context.Context, bucket, key string) (interfaces.ObjectMetadata, error) {
	a, err := s.object(bucket, key).Attrs(ctx)
	if err != nil {
		return interfaces.ObjectMetadata{}, classify("head object", err)
	}
	return metadataOf(a), nil
}

func generation(versionID string) (int64, error) {
	gen, err := strconv.ParseInt(versionID, 10, 64)
	if err != nil || gen <= 0 {
		return 0, fmt.Errorf("%q is not a GCS generation", versionID)
	}
	return gen, nil
}

func (s *Storage) HeadObjectVersion(ctx context.Context, bucket, key, versionID string) (interfaces.ObjectMetadata, error) {
	gen, err := generation(versionID)
	if err != nil {
		return interfaces.ObjectMetadata{}, err
	}
	a, err := s.object(bucket, key).Generation(gen).Attrs(ctx)
	if err != nil {
		return interfaces.ObjectMetadata{}, classify("head object version", err)
	}
	return metadataOf(a), nil
}

// DeleteObject removes the live object; deleting one that is gone is not
// an error, as with S3.
func (s *Storage) DeleteObject(ctx context.Context, bucket, key string) error {
	err := s.object(bucket, key).Delete(ctx)
	if err != nil && !errors.Is(err, storage.ErrObjectNotExist) {
		return classify("delete object", err)
	}
	return nil
}

func (s *Storage) DeleteObjectVersion(ctx context.Context, bucket, key, versionID string) error {
	gen, err := generation(versionID)
	if err != nil {
		return err
	}
	err = s.object(bucket, key).Generation(gen).Delete(ctx)
	if err != nil && !errors.Is(err, storage.ErrObjectNotExist) {
		return classify("delete object version", err)
	}
	return nil
}

func (s *Storage) OpenObject(ctx context.Context, bucket, key string) (io.ReadCloser, error) {
	r, err := s.object(bucket, key).NewReader(ctx)
	if err != nil {
		return nil, classify("get object", err)
	}
	return r, nil
}

// OpenRange reads a range, pinned to a generation when the condition
// names one (IfMatch and VersionID both hold generations here).
func (s *Storage) OpenRange(ctx context.Context, bucket, key string, first, last int64, cond interfaces.ReadCondition) (io.ReadCloser, error) {
	o := s.object(bucket, key)
	for _, pin := range []string{cond.VersionID, cond.IfMatch} {
		if pin == "" {
			continue
		}
		gen, err := generation(strings.Trim(pin, `"`))
		if err != nil {
			return nil, fmt.Errorf("get object range: %w", interfaces.ErrPreconditionFailed)
		}
		o = o.Generation(gen)
	}
	length := int64(-1)
	if last >= 0 {
		if first < 0 || first > last {
			return nil, fmt.Errorf("invalid byte range %d-%d", first, last)
		}
		length = last - first + 1
	} else {
		first = 0
	}
	r, err := o.NewRangeReader(ctx, first, length)
	if err != nil {
		return nil, classify("get object range", err)
	}
	return r, nil
}

func (s *Storage) PresignGet(_ context.Context, bucket, key string, ttl time.Duration, options interfaces.DownloadOptions) (string, error) {
	if bucket == "" || key == "" || ttl <= 0 || ttl > 7*24*time.Hour {
		return "", errors.New("bucket, key and a TTL of at most 7 days are required")
	}
	query := url.Values{}
	if options.ContentDisposition != "" {
		query.Set("response-content-disposition", options.ContentDisposition)
	}
	if options.ContentType != "" {
		query.Set("response-content-type", options.ContentType)
	}
	if options.VersionID != "" {
		query.Set("generation", options.VersionID)
	}
	u, err := s.sign(bucket, key, http.MethodGet, time.Now().Add(ttl), "", query)
	if err != nil {
		return "", fmt.Errorf("sign download: %w", err)
	}
	return u, nil
}

// CopyObject copies inside GCS (a rewrite, for any size).
func (s *Storage) CopyObject(ctx context.Context, sourceBucket, sourceKey, destinationBucket, destinationKey string) error {
	_, err := s.object(destinationBucket, destinationKey).CopierFrom(s.object(sourceBucket, sourceKey)).Run(ctx)
	if err != nil {
		return classify("copy object", err)
	}
	return nil
}

// WriteObject streams size bytes in chunks of PartBytes. A failed or
// short source cancels the writer's context, which discards the upload:
// no partial object is ever created.
func (s *Storage) WriteObject(ctx context.Context, bucket, key string, body io.Reader, size int64, contentType string) (interfaces.ObjectMetadata, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	w := s.object(bucket, key).NewWriter(ctx)
	w.ContentType = contentType
	w.ChunkSize = s.partBytes
	n, err := io.Copy(w, io.LimitReader(body, size))
	if err == nil && n != size {
		err = fmt.Errorf("%d of %d bytes: %w", n, size, io.ErrUnexpectedEOF)
	}
	if err != nil {
		cancel()
		_ = w.Close()
		return interfaces.ObjectMetadata{}, fmt.Errorf("write object: %w", err)
	}
	if err := w.Close(); err != nil {
		return interfaces.ObjectMetadata{}, classify("write object", err)
	}
	return metadataOf(w.Attrs()), nil
}

func (s *Storage) ListObjectsPage(ctx context.Context, bucket, prefix, cursor string, limit int) (interfaces.ObjectPage, error) {
	if limit <= 0 || limit > 1000 {
		return interfaces.ObjectPage{}, errors.New("page size must be between 1 and 1000")
	}
	it := s.client.Bucket(bucket).Objects(ctx, &storage.Query{Prefix: prefix})
	var objects []*storage.ObjectAttrs
	next, err := iterator.NewPager(it, limit, cursor).NextPage(&objects)
	if err != nil {
		return interfaces.ObjectPage{}, classify("list objects", err)
	}
	page := interfaces.ObjectPage{Cursor: next}
	for _, a := range objects {
		if isMultipartArtifact(a.Name) {
			continue // parts and markers of uploads in progress
		}
		page.Objects = append(page.Objects, interfaces.StoredObject{
			ObjectKey: a.Name, SizeBytes: a.Size, LastModified: a.Updated, ETag: strconv.FormatInt(a.Generation, 10),
		})
	}
	return page, nil
}

// ProviderCapabilities reports a GCS bucket's capabilities.
func (s *Storage) ProviderCapabilities(ctx context.Context, bucket string) (model.ProviderCapabilities, error) {
	a, err := s.client.Bucket(bucket).Attrs(ctx)
	if err != nil {
		return model.ProviderCapabilities{}, classify("bucket attributes", err)
	}
	signs := s.privateKey != nil
	caps := model.ProviderCapabilities{
		PresignedUpload: signs, MaxSingleUploadBytes: 5 << 40,
		Multipart: signs, MultipartKind: "GCS_COMPOSE", MaxParts: 10000, PartValidator: "CONTENT",
		ListIncompleteUploads: true, ServerSideCopy: true, RangeReads: true, ConditionalReads: true,
		PresignedDownload: signs, ListObjects: true, VersionIDKind: "GCS_GENERATION",
		ChecksumAlgorithms: []string{"MD5", "CRC32C"},
		VersioningStatus:   "Suspended",
	}
	if a.VersioningEnabled {
		caps.ProviderVersioning, caps.VersioningStatus = true, "Enabled"
	}
	return caps, nil
}

// ─── Parallel composite uploads ───────────────────────────────────────

const partsSuffix = ".parts/"

func partsPrefix(key, uploadID string) string { return key + partsSuffix + uploadID + "/" }
func markerKey(key, uploadID string) string   { return partsPrefix(key, uploadID) + "upload" }
func partKey(key, uploadID string, n int32) string {
	return fmt.Sprintf("%spart-%05d", partsPrefix(key, uploadID), n)
}

func isMultipartArtifact(name string) bool { return strings.Contains(name, partsSuffix) }

func newUploadID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// BeginMultipart records an upload with its marker object and returns its
// ID.
func (s *Storage) BeginMultipart(ctx context.Context, bucket, key, contentType string) (string, error) {
	if bucket == "" || key == "" || contentType == "" {
		return "", errors.New("bucket, object key and content type are required")
	}
	id := newUploadID()
	w := s.object(bucket, markerKey(key, id)).NewWriter(ctx)
	w.Metadata = map[string]string{"content-type": contentType}
	if err := w.Close(); err != nil {
		return "", classify("create upload marker", err)
	}
	return id, nil
}

// marker returns the upload's recorded content type, or
// ErrStorageUploadNotFound when the upload no longer exists.
func (s *Storage) marker(ctx context.Context, bucket, key, uploadID string) (string, error) {
	a, err := s.object(bucket, markerKey(key, uploadID)).Attrs(ctx)
	if errors.Is(err, storage.ErrObjectNotExist) {
		return "", fmt.Errorf("upload marker: %w", interfaces.ErrStorageUploadNotFound)
	}
	if err != nil {
		return "", classify("upload marker", err)
	}
	return a.Metadata["content-type"], nil
}

func (s *Storage) PresignPart(ctx context.Context, bucket, key, uploadID string, partNumber int32, ttl time.Duration) (string, error) {
	if partNumber < 1 || partNumber > 10000 || ttl <= 0 || ttl > 7*24*time.Hour {
		return "", errors.New("part number must be 1-10000 and the TTL at most 7 days")
	}
	if _, err := s.marker(ctx, bucket, key, uploadID); err != nil {
		return "", err
	}
	return s.sign(bucket, partKey(key, uploadID, partNumber), http.MethodPut, time.Now().Add(ttl), "", nil)
}

// ListParts lists the part objects. A part's validator is its MD5, quoted,
// as the XML API's ETag header for the part's upload is.
func (s *Storage) ListParts(ctx context.Context, bucket, key, uploadID string) ([]interfaces.StoredPart, error) {
	if _, err := s.marker(ctx, bucket, key, uploadID); err != nil {
		return nil, err
	}
	it := s.client.Bucket(bucket).Objects(ctx, &storage.Query{Prefix: partsPrefix(key, uploadID) + "part-"})
	var parts []interfaces.StoredPart
	for {
		a, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, classify("list parts", err)
		}
		n, err := strconv.Atoi(strings.TrimPrefix(a.Name, partsPrefix(key, uploadID)+"part-"))
		if err != nil {
			continue
		}
		parts = append(parts, interfaces.StoredPart{PartNumber: int32(n), ETag: `"` + hex.EncodeToString(a.MD5) + `"`, SizeBytes: a.Size})
	}
	sort.Slice(parts, func(i, j int) bool { return parts[i].PartNumber < parts[j].PartNumber })
	return parts, nil
}

// CompleteMultipart composes the parts, in their order, into the object,
// in levels of at most 32 sources, then removes the parts and marker. The
// marker goes last: until then a retry can complete again.
func (s *Storage) CompleteMultipart(ctx context.Context, bucket, key, uploadID string, parts []interfaces.CompletedPart) error {
	contentType, err := s.marker(ctx, bucket, key, uploadID)
	if err != nil {
		return err
	}
	if len(parts) == 0 {
		return errors.New("at least one completed part is required")
	}
	sources := make([]string, len(parts))
	for i, p := range parts {
		if i > 0 && p.PartNumber <= parts[i-1].PartNumber {
			return retry.Permanent(errors.New("completed parts must be in strictly ascending part-number order"))
		}
		sources[i] = partKey(key, uploadID, p.PartNumber)
	}
	var intermediates []string
	for level := 0; len(sources) > 32; level++ {
		var next []string
		for start := 0; start < len(sources); start += 32 {
			end := min(start+32, len(sources))
			name := fmt.Sprintf("%scompose-%d-%05d", partsPrefix(key, uploadID), level, start/32)
			if err := s.compose(ctx, bucket, name, sources[start:end], ""); err != nil {
				return err
			}
			next = append(next, name)
			intermediates = append(intermediates, name)
		}
		sources = next
	}
	if err := s.compose(ctx, bucket, key, sources, contentType); err != nil {
		return err
	}
	for _, p := range parts {
		_ = s.DeleteObject(ctx, bucket, partKey(key, uploadID, p.PartNumber))
	}
	for _, name := range intermediates {
		_ = s.DeleteObject(ctx, bucket, name)
	}
	return s.DeleteObject(ctx, bucket, markerKey(key, uploadID))
}

func (s *Storage) compose(ctx context.Context, bucket, destination string, sources []string, contentType string) error {
	handles := make([]*storage.ObjectHandle, len(sources))
	for i, name := range sources {
		handles[i] = s.object(bucket, name)
	}
	c := s.object(bucket, destination).ComposerFrom(handles...)
	if contentType != "" {
		c.ContentType = contentType
	}
	if _, err := c.Run(ctx); err != nil {
		return classify("compose object", err)
	}
	return nil
}

// AbortMultipart deletes the upload's parts and its marker.
func (s *Storage) AbortMultipart(ctx context.Context, bucket, key, uploadID string) error {
	if _, err := s.marker(ctx, bucket, key, uploadID); err != nil {
		return err
	}
	it := s.client.Bucket(bucket).Objects(ctx, &storage.Query{Prefix: partsPrefix(key, uploadID)})
	for {
		a, err := it.Next()
		if errors.Is(err, iterator.Done) {
			return nil
		}
		if err != nil {
			return classify("list parts", err)
		}
		if err := s.DeleteObject(ctx, bucket, a.Name); err != nil {
			return err
		}
	}
}

func (s *Storage) ListMultipartUploads(ctx context.Context, bucket, key string) ([]string, error) {
	page, err := s.uploadsUnder(ctx, bucket, key+partsSuffix, "", 1000)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, u := range page.Uploads {
		if u.ObjectKey == key {
			ids = append(ids, u.StorageUploadID)
		}
	}
	return ids, nil
}

func (s *Storage) ListMultipartUploadsPage(ctx context.Context, bucket, prefix, cursor string, limit int) (interfaces.MultipartUploadPage, error) {
	return s.uploadsUnder(ctx, bucket, prefix, cursor, limit)
}

// uploadsUnder lists marker objects under prefix, a page of listing at a
// time.
func (s *Storage) uploadsUnder(ctx context.Context, bucket, prefix, cursor string, limit int) (interfaces.MultipartUploadPage, error) {
	if limit <= 0 || limit > 1000 {
		return interfaces.MultipartUploadPage{}, errors.New("page size must be between 1 and 1000")
	}
	it := s.client.Bucket(bucket).Objects(ctx, &storage.Query{Prefix: prefix})
	var objects []*storage.ObjectAttrs
	next, err := iterator.NewPager(it, limit, cursor).NextPage(&objects)
	if err != nil {
		return interfaces.MultipartUploadPage{}, classify("list uploads", err)
	}
	page := interfaces.MultipartUploadPage{Cursor: next}
	for _, a := range objects {
		key, rest, ok := strings.Cut(a.Name, partsSuffix)
		id, name, _ := strings.Cut(rest, "/")
		if ok && name == "upload" {
			page.Uploads = append(page.Uploads, interfaces.StoredMultipartUpload{ObjectKey: key, StorageUploadID: id, Initiated: a.Created})
		}
	}
	return page, nil
}

// classify maps GCS errors to the platform's kinds.
func classify(operation string, err error) error {
	if errors.Is(err, storage.ErrObjectNotExist) || errors.Is(err, storage.ErrBucketNotExist) {
		return fmt.Errorf("%s: %w", operation, interfaces.ErrObjectNotFound)
	}
	var apiErr *googleapi.Error
	if errors.As(err, &apiErr) {
		switch apiErr.Code {
		case http.StatusNotFound:
			return fmt.Errorf("%s: %w", operation, interfaces.ErrObjectNotFound)
		case http.StatusPreconditionFailed:
			return fmt.Errorf("%s: %w", operation, interfaces.ErrPreconditionFailed)
		case http.StatusRequestedRangeNotSatisfiable:
			return fmt.Errorf("%s: %w", operation, interfaces.ErrInvalidRange)
		case http.StatusTooManyRequests, http.StatusServiceUnavailable:
			return fmt.Errorf("%s: %w", operation, interfaces.ErrStorageThrottled)
		case http.StatusUnauthorized:
			return retry.Permanent(fmt.Errorf("%s: %w", operation, interfaces.ErrStorageCredentialsRejected))
		case http.StatusForbidden:
			return retry.Permanent(fmt.Errorf("%s: %w", operation, interfaces.ErrStorageAccessDenied))
		case http.StatusBadRequest:
			return retry.Permanent(fmt.Errorf("%s: bad request", operation))
		}
		return fmt.Errorf("%s: provider error %d", operation, apiErr.Code)
	}
	return fmt.Errorf("%s: %w", operation, err)
}
