// Package azure is the Azure Blob Storage adapter, on the official Go
// SDK. It implements the platform's storage operations the Azure way:
//
//   - Service SAS URLs, signed with the account key, for client uploads and
//     downloads. A connection authenticated by a SAS token cannot sign URLs
//     (handing out the customer's token would hand out their credential),
//     so it stages uploads on the platform and serves downloads through
//     the API.
//   - Large uploads as block blobs: the client stages each part as a block
//     whose ID encodes the upload and the part number, and completion
//     commits the block list. A marker blob records the upload and carries
//     its content type. Blocks have no content ETag, so parts are checked
//     by ID and size (PartValidator BLOCK_ID); the platform's SHA-256 check
//     after upload is what proves the bytes.
//   - Blob version IDs when the account has versioning on.
package azure

import (
	"context"
	"crypto/rand"
	"encoding/base64"
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

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blockblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/sas"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/service"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/retry"
)

var _ interfaces.StorageProvider = (*Storage)(nil)

// Config connects to one storage account.
type Config struct {
	AccountName string
	// AccountKey (ACCOUNT_KEY) or SASToken (SAS_TOKEN), never both.
	AccountKey string
	SASToken   string
	// Endpoint is the blob service URL; empty means the account's public
	// endpoint. An Azurite URL for local testing.
	Endpoint string
	// HTTPClient is the network-guarded client every request goes through.
	HTTPClient *http.Client
	// PartBytes is the block size of a streaming write.
	PartBytes int64
}

// Storage is an Azure storage account client.
type Storage struct {
	service   *service.Client
	sharedKey *service.SharedKeyCredential
	insecure  bool
	partBytes int64
}

// New builds an Azure client. The SAS token, when that is the credential,
// travels only in request URLs to the account itself; errors never carry
// those URLs.
func New(cfg Config) (*Storage, error) {
	if cfg.HTTPClient == nil {
		return nil, errors.New("an Azure client needs the guarded HTTP client")
	}
	serviceURL := cfg.Endpoint
	if serviceURL == "" {
		serviceURL = "https://" + cfg.AccountName + ".blob.core.windows.net/"
	}
	s := &Storage{partBytes: cfg.PartBytes, insecure: strings.HasPrefix(serviceURL, "http://")}
	if s.partBytes <= 0 {
		s.partBytes = 16 << 20
	}
	opts := &service.ClientOptions{ClientOptions: azcore.ClientOptions{
		Transport: cfg.HTTPClient,
		Retry:     policy.RetryOptions{MaxRetries: 2},
	}}
	var err error
	switch {
	case cfg.AccountKey != "":
		s.sharedKey, err = service.NewSharedKeyCredential(cfg.AccountName, cfg.AccountKey)
		if err != nil {
			return nil, fmt.Errorf("%w: the account key is not valid", model.ErrInvalidConnection)
		}
		s.service, err = service.NewClientWithSharedKeyCredential(serviceURL, s.sharedKey, opts)
	case cfg.SASToken != "":
		s.service, err = service.NewClientWithNoCredential(serviceURL+"?"+strings.TrimPrefix(cfg.SASToken, "?"), opts)
	default:
		return nil, fmt.Errorf("%w: Azure needs an account key or a SAS token", model.ErrCredentialMissing)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: the Azure endpoint is not valid", model.ErrInvalidConnection)
	}
	return s, nil
}

func (s *Storage) container(bucket string) *container.Client {
	return s.service.NewContainerClient(bucket)
}

func (s *Storage) blob(bucket, key string) *blockblob.Client {
	return s.container(bucket).NewBlockBlobClient(key)
}

// signedURL makes a service SAS URL for one blob.
func (s *Storage) signedURL(bucket, key string, values sas.BlobSignatureValues) (string, error) {
	if s.sharedKey == nil {
		return "", fmt.Errorf("%w: signed URLs need the account key", model.ErrUnsupportedProvider)
	}
	values.Protocol = sas.ProtocolHTTPS
	if s.insecure {
		values.Protocol = sas.ProtocolHTTPSandHTTP
	}
	values.ContainerName, values.BlobName = bucket, key
	query, err := values.SignWithSharedKey(s.sharedKey)
	if err != nil {
		return "", fmt.Errorf("sign URL: %w", err)
	}
	return s.blob(bucket, key).URL() + "?" + query.Encode(), nil
}

func writePermission() string {
	return (&sas.BlobPermissions{Create: true, Write: true}).String()
}

func (s *Storage) CreateUploadPlan(_ context.Context, target interfaces.UploadTarget) (interfaces.UploadPlan, error) {
	if target.Bucket == "" || target.ObjectKey == "" {
		return interfaces.UploadPlan{}, errors.New("bucket and object key are required")
	}
	if time.Until(target.ExpiresAt) <= 0 {
		return interfaces.UploadPlan{}, errors.New("upload URL expiry must be in the future")
	}
	u, err := s.signedURL(target.Bucket, target.ObjectKey, sas.BlobSignatureValues{
		ExpiryTime: target.ExpiresAt.UTC(), Permissions: writePermission(),
	})
	if err != nil {
		return interfaces.UploadPlan{}, err
	}
	return interfaces.UploadPlan{
		UploadURL: u, ExpiresAt: target.ExpiresAt.UTC(),
		Headers: map[string]string{"x-ms-blob-type": "BlockBlob"},
	}, nil
}

func metadataOf(size *int64, contentType *string, etag *azcore.ETag, versionID *string) interfaces.ObjectMetadata {
	m := interfaces.ObjectMetadata{}
	if size != nil {
		m.SizeBytes = *size
	}
	if contentType != nil {
		m.ContentType = *contentType
	}
	if etag != nil {
		m.ETag = string(*etag)
	}
	if versionID != nil {
		m.VersionID = *versionID
	}
	return m
}

func (s *Storage) HeadObject(ctx context.Context, bucket, key string) (interfaces.ObjectMetadata, error) {
	p, err := s.blob(bucket, key).GetProperties(ctx, nil)
	if err != nil {
		return interfaces.ObjectMetadata{}, classify("head object", err)
	}
	return metadataOf(p.ContentLength, p.ContentType, p.ETag, p.VersionID), nil
}

func (s *Storage) version(bucket, key, versionID string) (*blockblob.Client, error) {
	if versionID == "" {
		return nil, errors.New("version ID is required")
	}
	return s.blob(bucket, key).WithVersionID(versionID)
}

func (s *Storage) HeadObjectVersion(ctx context.Context, bucket, key, versionID string) (interfaces.ObjectMetadata, error) {
	b, err := s.version(bucket, key, versionID)
	if err != nil {
		return interfaces.ObjectMetadata{}, err
	}
	p, err := b.GetProperties(ctx, nil)
	if err != nil {
		return interfaces.ObjectMetadata{}, classify("head object version", err)
	}
	return metadataOf(p.ContentLength, p.ContentType, p.ETag, p.VersionID), nil
}

// DeleteObject removes the blob; deleting one that is gone is not an
// error, as with S3.
// DeleteObject removes the blob. With account versioning on, the deleted
// blob stays as a previous version, so every version of exactly this key
// is removed too. Blobs are listed in name order: the key's own versions
// come before any longer name it prefixes.
func (s *Storage) DeleteObject(ctx context.Context, bucket, key string) error {
	_, err := s.blob(bucket, key).Delete(ctx, nil)
	if err != nil && !bloberror.HasCode(err, bloberror.BlobNotFound) {
		return classify("delete object", err)
	}
	pager := s.container(bucket).NewListBlobsFlatPager(&container.ListBlobsFlatOptions{
		Prefix: &key, Include: container.ListBlobsInclude{Versions: true},
	})
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return classify("list blob versions", err)
		}
		for _, item := range page.Segment.BlobItems {
			if item.Name == nil || *item.Name != key {
				return nil
			}
			if item.VersionID == nil {
				continue
			}
			if err := s.DeleteObjectVersion(ctx, bucket, key, *item.VersionID); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Storage) DeleteObjectVersion(ctx context.Context, bucket, key, versionID string) error {
	b, err := s.version(bucket, key, versionID)
	if err != nil {
		return err
	}
	_, err = b.Delete(ctx, nil)
	if err != nil && !bloberror.HasCode(err, bloberror.BlobNotFound) {
		return classify("delete object version", err)
	}
	return nil
}

func (s *Storage) OpenObject(ctx context.Context, bucket, key string) (io.ReadCloser, error) {
	r, err := s.blob(bucket, key).DownloadStream(ctx, nil)
	if err != nil {
		return nil, classify("get object", err)
	}
	return r.Body, nil
}

func (s *Storage) OpenRange(ctx context.Context, bucket, key string, first, last int64, cond interfaces.ReadCondition) (io.ReadCloser, error) {
	b := s.blob(bucket, key)
	if cond.VersionID != "" {
		var err error
		if b, err = b.WithVersionID(cond.VersionID); err != nil {
			return nil, err
		}
	}
	opts := &blob.DownloadStreamOptions{}
	if cond.IfMatch != "" {
		etag := azcore.ETag(cond.IfMatch)
		opts.AccessConditions = &blob.AccessConditions{ModifiedAccessConditions: &blob.ModifiedAccessConditions{IfMatch: &etag}}
	}
	if last >= 0 {
		if first < 0 || first > last {
			return nil, fmt.Errorf("invalid byte range %d-%d", first, last)
		}
		opts.Range = blob.HTTPRange{Offset: first, Count: last - first + 1}
	}
	r, err := b.DownloadStream(ctx, opts)
	if err != nil {
		return nil, classify("get object range", err)
	}
	return r.Body, nil
}

func (s *Storage) PresignGet(_ context.Context, bucket, key string, ttl time.Duration, options interfaces.DownloadOptions) (string, error) {
	if bucket == "" || key == "" || ttl <= 0 || ttl > 7*24*time.Hour {
		return "", errors.New("bucket, key and a TTL of at most 7 days are required")
	}
	values := sas.BlobSignatureValues{
		ExpiryTime: time.Now().Add(ttl).UTC(), Permissions: (&sas.BlobPermissions{Read: true}).String(),
		ContentDisposition: options.ContentDisposition, ContentType: options.ContentType,
		BlobVersion: options.VersionID,
	}
	u, err := s.signedURL(bucket, key, values)
	if err != nil {
		return "", err
	}
	if options.VersionID != "" {
		u += "&versionid=" + url.QueryEscape(options.VersionID)
	}
	return u, nil
}

// CopyObject copies inside the account and waits for the copy to finish.
// The source is read through a short-lived read SAS, or through the
// connection's own SAS token.
func (s *Storage) CopyObject(ctx context.Context, sourceBucket, sourceKey, destinationBucket, destinationKey string) error {
	source := s.blob(sourceBucket, sourceKey).URL()
	if s.sharedKey != nil {
		var err error
		source, err = s.signedURL(sourceBucket, sourceKey, sas.BlobSignatureValues{
			ExpiryTime: time.Now().Add(15 * time.Minute).UTC(), Permissions: (&sas.BlobPermissions{Read: true}).String(),
		})
		if err != nil {
			return err
		}
	}
	destination := s.blob(destinationBucket, destinationKey)
	started, err := destination.StartCopyFromURL(ctx, source, nil)
	if err != nil {
		return classify("copy object", err)
	}
	status := blob.CopyStatusTypePending
	if started.CopyStatus != nil {
		status = *started.CopyStatus
	}
	for status == blob.CopyStatusTypePending {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
		p, err := destination.GetProperties(ctx, nil)
		if err != nil {
			return classify("copy status", err)
		}
		if p.CopyStatus == nil {
			break
		}
		status = *p.CopyStatus
	}
	if status != blob.CopyStatusTypePending && status != blob.CopyStatusTypeSuccess {
		return fmt.Errorf("copy object: copy ended %s", status)
	}
	return nil
}

// exactReader yields exactly size bytes or fails, so a short source never
// gets committed as a whole blob.
type exactReader struct {
	r    io.Reader
	left int64
}

func (e *exactReader) Read(p []byte) (int, error) {
	if e.left <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > e.left {
		p = p[:e.left]
	}
	n, err := e.r.Read(p)
	e.left -= int64(n)
	if errors.Is(err, io.EOF) && e.left > 0 {
		return n, io.ErrUnexpectedEOF
	}
	return n, err
}

// WriteObject stages blocks of PartBytes one at a time and commits them.
func (s *Storage) WriteObject(ctx context.Context, bucket, key string, body io.Reader, size int64, contentType string) (interfaces.ObjectMetadata, error) {
	if bucket == "" || key == "" || size < 0 {
		return interfaces.ObjectMetadata{}, errors.New("bucket, object key and size are required")
	}
	// A blob holds at most 50,000 blocks: larger objects get larger ones.
	blockSize := max(s.partBytes, (size+49999)/50000)
	_, err := s.blob(bucket, key).UploadStream(ctx, &exactReader{r: body, left: size}, &blockblob.UploadStreamOptions{
		BlockSize: blockSize, Concurrency: 1,
		HTTPHeaders: &blob.HTTPHeaders{BlobContentType: &contentType},
	})
	if err != nil {
		return interfaces.ObjectMetadata{}, classify("write object", err)
	}
	return s.HeadObject(ctx, bucket, key)
}

func (s *Storage) ListObjectsPage(ctx context.Context, bucket, prefix, cursor string, limit int) (interfaces.ObjectPage, error) {
	items, next, err := s.listPage(ctx, bucket, prefix, cursor, limit)
	if err != nil {
		return interfaces.ObjectPage{}, classify("list objects", err)
	}
	page := interfaces.ObjectPage{Cursor: next}
	for _, item := range items {
		if strings.Contains(item.name, uploadsSuffix) {
			continue // markers of uploads in progress
		}
		page.Objects = append(page.Objects, interfaces.StoredObject{
			ObjectKey: item.name, SizeBytes: item.size, LastModified: item.modified, ETag: item.etag,
		})
	}
	return page, nil
}

type listedBlob struct {
	name     string
	size     int64
	modified time.Time
	created  time.Time
	etag     string
}

func (s *Storage) listPage(ctx context.Context, bucket, prefix, cursor string, limit int) ([]listedBlob, string, error) {
	if limit <= 0 || limit > 1000 {
		return nil, "", errors.New("page size must be between 1 and 1000")
	}
	opts := &container.ListBlobsFlatOptions{MaxResults: ptr(int32(limit))}
	if prefix != "" {
		opts.Prefix = &prefix
	}
	if cursor != "" {
		opts.Marker = &cursor
	}
	resp, err := s.container(bucket).NewListBlobsFlatPager(opts).NextPage(ctx)
	if err != nil {
		return nil, "", err
	}
	var items []listedBlob
	if resp.Segment != nil {
		for _, b := range resp.Segment.BlobItems {
			if b.Name == nil || b.Properties == nil {
				continue
			}
			item := listedBlob{name: *b.Name}
			p := b.Properties
			if p.ContentLength != nil {
				item.size = *p.ContentLength
			}
			if p.LastModified != nil {
				item.modified = *p.LastModified
			}
			if p.CreationTime != nil {
				item.created = *p.CreationTime
			}
			if p.ETag != nil {
				item.etag = string(*p.ETag)
			}
			items = append(items, item)
		}
	}
	next := ""
	if resp.NextMarker != nil {
		next = *resp.NextMarker
	}
	return items, next, nil
}

// ProviderCapabilities reports what an Azure container can do. Versioning
// is an account setting the data plane does not report; it shows as
// UNKNOWN here, and a connection test's write probe sees version IDs when
// it is on.
func (s *Storage) ProviderCapabilities(ctx context.Context, bucket string) (model.ProviderCapabilities, error) {
	if _, _, err := s.listPage(ctx, bucket, "", "", 1); err != nil {
		return model.ProviderCapabilities{}, classify("list objects", err)
	}
	signs := s.sharedKey != nil
	return model.ProviderCapabilities{
		PresignedUpload: signs, MaxSingleUploadBytes: 5000 << 20,
		Multipart: signs, MultipartKind: "AZURE_BLOCKS", MaxParts: 50000, PartValidator: "BLOCK_ID",
		ListIncompleteUploads: true, ServerSideCopy: true, RangeReads: true, ConditionalReads: true,
		PresignedDownload: signs, ListObjects: true, VersioningStatus: "UNKNOWN", VersionIDKind: "AZURE_VERSION_ID",
		ChecksumAlgorithms: []string{"MD5", "CRC64"},
	}, nil
}

// ─── Block uploads ────────────────────────────────────────────────────

const uploadsSuffix = ".uploads/"

func markerKey(key, uploadID string) string { return key + uploadsSuffix + uploadID }

// blockID encodes the upload and part number. Every ID of a blob must have
// the same length, which the fixed-width upload ID and part number give.
func blockID(uploadID string, part int32) string {
	return base64.StdEncoding.EncodeToString(fmt.Appendf(nil, "%s-%05d", uploadID, part))
}

// partOf decodes a block ID back to its part number, if it is this
// upload's.
func partOf(uploadID, id string) (int32, bool) {
	raw, err := base64.StdEncoding.DecodeString(id)
	if err != nil {
		return 0, false
	}
	number, ok := strings.CutPrefix(string(raw), uploadID+"-")
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(number)
	if err != nil {
		return 0, false
	}
	return int32(n), true
}

func newUploadID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// BeginMultipart writes the upload's marker blob, whose content type is
// the upload's.
func (s *Storage) BeginMultipart(ctx context.Context, bucket, key, contentType string) (string, error) {
	if bucket == "" || key == "" || contentType == "" {
		return "", errors.New("bucket, object key and content type are required")
	}
	id := newUploadID()
	_, err := s.blob(bucket, markerKey(key, id)).UploadBuffer(ctx, nil, &blockblob.UploadBufferOptions{
		HTTPHeaders: &blob.HTTPHeaders{BlobContentType: &contentType},
	})
	if err != nil {
		return "", classify("create upload marker", err)
	}
	return id, nil
}

// marker returns the upload's content type, or ErrStorageUploadNotFound
// when the upload no longer exists.
func (s *Storage) marker(ctx context.Context, bucket, key, uploadID string) (string, error) {
	p, err := s.blob(bucket, markerKey(key, uploadID)).GetProperties(ctx, nil)
	if bloberror.HasCode(err, bloberror.BlobNotFound) {
		return "", fmt.Errorf("upload marker: %w", interfaces.ErrStorageUploadNotFound)
	}
	if err != nil {
		return "", classify("upload marker", err)
	}
	if p.ContentType == nil {
		return "", nil
	}
	return *p.ContentType, nil
}

// PresignPart signs a Put Block URL for one part.
func (s *Storage) PresignPart(ctx context.Context, bucket, key, uploadID string, partNumber int32, ttl time.Duration) (string, error) {
	if partNumber < 1 || partNumber > 50000 || ttl <= 0 || ttl > 7*24*time.Hour {
		return "", errors.New("part number must be 1-50000 and the TTL at most 7 days")
	}
	if _, err := s.marker(ctx, bucket, key, uploadID); err != nil {
		return "", err
	}
	u, err := s.signedURL(bucket, key, sas.BlobSignatureValues{
		ExpiryTime: time.Now().Add(ttl).UTC(), Permissions: writePermission(),
	})
	if err != nil {
		return "", err
	}
	return u + "&comp=block&blockid=" + url.QueryEscape(blockID(uploadID, partNumber)), nil
}

// ListParts lists this upload's blocks, committed ones included so that a
// completion retried after its commit still finds its parts.
func (s *Storage) ListParts(ctx context.Context, bucket, key, uploadID string) ([]interfaces.StoredPart, error) {
	if _, err := s.marker(ctx, bucket, key, uploadID); err != nil {
		return nil, err
	}
	list, err := s.blob(bucket, key).GetBlockList(ctx, blockblob.BlockListTypeAll, nil)
	if bloberror.HasCode(err, bloberror.BlobNotFound) {
		return nil, nil // no block staged yet
	}
	if err != nil {
		return nil, classify("list parts", err)
	}
	seen := map[int32]bool{}
	var parts []interfaces.StoredPart
	for _, b := range append(list.UncommittedBlocks, list.CommittedBlocks...) {
		if b.Name == nil || b.Size == nil {
			continue
		}
		n, ok := partOf(uploadID, *b.Name)
		if !ok || seen[n] {
			continue
		}
		seen[n] = true
		parts = append(parts, interfaces.StoredPart{PartNumber: n, ETag: *b.Name, SizeBytes: *b.Size})
	}
	sort.Slice(parts, func(i, j int) bool { return parts[i].PartNumber < parts[j].PartNumber })
	return parts, nil
}

// CompleteMultipart commits the parts' blocks, in order, then removes the
// marker. Committing again is harmless, so a retry can finish the job.
func (s *Storage) CompleteMultipart(ctx context.Context, bucket, key, uploadID string, parts []interfaces.CompletedPart) error {
	contentType, err := s.marker(ctx, bucket, key, uploadID)
	if err != nil {
		return err
	}
	if len(parts) == 0 {
		return errors.New("at least one completed part is required")
	}
	ids := make([]string, len(parts))
	for i, p := range parts {
		if i > 0 && p.PartNumber <= parts[i-1].PartNumber {
			return retry.Permanent(errors.New("completed parts must be in strictly ascending part-number order"))
		}
		ids[i] = blockID(uploadID, p.PartNumber)
	}
	_, err = s.blob(bucket, key).CommitBlockList(ctx, ids, &blockblob.CommitBlockListOptions{
		HTTPHeaders: &blob.HTTPHeaders{BlobContentType: &contentType},
	})
	if err != nil {
		return classify("commit block list", err)
	}
	return s.DeleteObject(ctx, bucket, markerKey(key, uploadID))
}

// AbortMultipart discards the staged blocks and the marker. Azure has no
// call that drops uncommitted blocks, but committing an empty list does,
// so when no blob was committed yet an empty one is committed and deleted.
// Left alone, uncommitted blocks expire after a week.
func (s *Storage) AbortMultipart(ctx context.Context, bucket, key, uploadID string) error {
	if _, err := s.marker(ctx, bucket, key, uploadID); err != nil {
		return err
	}
	b := s.blob(bucket, key)
	noBlob := azcore.ETagAny
	_, err := b.CommitBlockList(ctx, nil, &blockblob.CommitBlockListOptions{
		AccessConditions: &blob.AccessConditions{ModifiedAccessConditions: &blob.ModifiedAccessConditions{IfNoneMatch: &noBlob}},
	})
	switch {
	case err == nil:
		if err := s.DeleteObject(ctx, bucket, key); err != nil {
			return err
		}
	case bloberror.HasCode(err, bloberror.ConditionNotMet, bloberror.BlobAlreadyExists):
		// A blob was committed; it is not this abort's to delete.
	default:
		return classify("discard blocks", err)
	}
	return s.DeleteObject(ctx, bucket, markerKey(key, uploadID))
}

func (s *Storage) ListMultipartUploads(ctx context.Context, bucket, key string) ([]string, error) {
	var ids []string
	cursor := ""
	for {
		page, err := s.ListMultipartUploadsPage(ctx, bucket, key+uploadsSuffix, cursor, 1000)
		if err != nil {
			return nil, err
		}
		for _, u := range page.Uploads {
			if u.ObjectKey == key {
				ids = append(ids, u.StorageUploadID)
			}
		}
		if page.Cursor == "" {
			return ids, nil
		}
		cursor = page.Cursor
	}
}

func (s *Storage) ListMultipartUploadsPage(ctx context.Context, bucket, prefix, cursor string, limit int) (interfaces.MultipartUploadPage, error) {
	items, next, err := s.listPage(ctx, bucket, prefix, cursor, limit)
	if err != nil {
		return interfaces.MultipartUploadPage{}, classify("list uploads", err)
	}
	page := interfaces.MultipartUploadPage{Cursor: next}
	for _, item := range items {
		key, id, ok := strings.Cut(item.name, uploadsSuffix)
		if ok && id != "" && !strings.Contains(id, "/") {
			page.Uploads = append(page.Uploads, interfaces.StoredMultipartUpload{ObjectKey: key, StorageUploadID: id, Initiated: item.created})
		}
	}
	return page, nil
}

func ptr[T any](v T) *T { return &v }

// classify maps Azure errors to the platform's kinds. Messages never carry
// a request URL: with SAS-token auth the URL holds the credential.
func classify(operation string, err error) error {
	switch {
	case bloberror.HasCode(err, bloberror.BlobNotFound, bloberror.ContainerNotFound):
		return fmt.Errorf("%s: %w", operation, interfaces.ErrObjectNotFound)
	case bloberror.HasCode(err, bloberror.ConditionNotMet):
		return fmt.Errorf("%s: %w", operation, interfaces.ErrPreconditionFailed)
	case bloberror.HasCode(err, bloberror.InvalidRange):
		return fmt.Errorf("%s: %w", operation, interfaces.ErrInvalidRange)
	case bloberror.HasCode(err, bloberror.AuthenticationFailed):
		return retry.Permanent(fmt.Errorf("%s: %w", operation, interfaces.ErrStorageCredentialsRejected))
	}
	var respErr *azcore.ResponseError
	if errors.As(err, &respErr) {
		switch respErr.StatusCode {
		case http.StatusNotFound:
			return fmt.Errorf("%s: %w", operation, interfaces.ErrObjectNotFound)
		case http.StatusPreconditionFailed:
			return fmt.Errorf("%s: %w", operation, interfaces.ErrPreconditionFailed)
		case http.StatusRequestedRangeNotSatisfiable:
			return fmt.Errorf("%s: %w", operation, interfaces.ErrInvalidRange)
		case http.StatusTooManyRequests, http.StatusServiceUnavailable:
			return fmt.Errorf("%s: %w", operation, interfaces.ErrStorageThrottled)
		case http.StatusUnauthorized, http.StatusForbidden:
			return retry.Permanent(fmt.Errorf("%s: %d: %w", operation, respErr.StatusCode, interfaces.ErrStorageAccessDenied))
		case http.StatusBadRequest:
			return retry.Permanent(fmt.Errorf("%s: bad request (%s)", operation, respErr.ErrorCode))
		}
		return fmt.Errorf("%s: provider error %d", operation, respErr.StatusCode)
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return fmt.Errorf("%s: %w", operation, urlErr.Err)
	}
	return fmt.Errorf("%s: %w", operation, err)
}
