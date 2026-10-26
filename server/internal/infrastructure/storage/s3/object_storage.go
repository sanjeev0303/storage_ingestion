package s3

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/retry"
)

// ObjectStorage wraps the AWS SDK S3 client to implement both the
// single-upload and multipart-upload application interfaces.
type ObjectStorage struct {
	client    *s3.Client
	presigner *s3.PresignClient
	// partBytes is the buffer of a streaming write (connection.go).
	partBytes int64
	// singleCopyBytes and copyPartBytes override the copy limits in
	// tests (copy.go); zero means S3's own.
	singleCopyBytes int64
	copyPartBytes   int64
}

// Compile-time assertions: ObjectStorage must satisfy both interfaces.
var _ interfaces.ObjectStorage = (*ObjectStorage)(nil)
var _ interfaces.MultipartStorage = (*ObjectStorage)(nil)

func NewObjectStorage(client *s3.Client) *ObjectStorage {
	return &ObjectStorage{
		client:    client,
		presigner: s3.NewPresignClient(client),
	}
}

// CreateUploadPlan returns a temporary presigned URL for uploading one
// complete object. The client sends the file bytes directly to object storage.
func (s *ObjectStorage) CreateUploadPlan(
	ctx context.Context,
	target interfaces.UploadTarget,
) (interfaces.UploadPlan, error) {
	if target.Bucket == "" || target.ObjectKey == "" {
		return interfaces.UploadPlan{}, fmt.Errorf("bucket and object key are required")
	}

	ttl := time.Until(target.ExpiresAt)
	if ttl <= 0 {
		return interfaces.UploadPlan{}, fmt.Errorf("upload URL expiry must be in the future")
	}

	// S3 SigV4 presigned URLs have a maximum validity of seven days.
	// Application upload URLs should normally be much shorter.
	if ttl > 7*24*time.Hour {
		ttl = 7 * 24 * time.Hour
	}

	input := &s3.PutObjectInput{
		Bucket:      aws.String(target.Bucket),
		Key:         aws.String(target.ObjectKey),
		ContentType: aws.String(target.ContentType),
	}

	presigned, err := s.presigner.PresignPutObject(
		ctx,
		input,
		func(options *s3.PresignOptions) {
			options.Expires = ttl
		},
	)
	if err != nil {
		return interfaces.UploadPlan{}, fmt.Errorf("presign object upload: %w", err)
	}

	return interfaces.UploadPlan{
		UploadURL: presigned.URL,
		ExpiresAt: time.Now().UTC().Add(ttl),
	}, nil
}

// HeadObject retrieves storage metadata for verification.
func (s *ObjectStorage) HeadObject(
	ctx context.Context,
	bucket string,
	objectKey string,
) (interfaces.ObjectMetadata, error) {
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(objectKey),
	})
	if err != nil {
		return interfaces.ObjectMetadata{}, classify("head object", err)
	}

	metadata := interfaces.ObjectMetadata{
		SizeBytes: aws.ToInt64(out.ContentLength),
		ETag:      aws.ToString(out.ETag),
		VersionID: aws.ToString(out.VersionId),
	}

	if out.ContentType != nil {
		metadata.ContentType = *out.ContentType
	}

	return metadata, nil
}

// DeleteObject removes an object from the specified bucket. A delete
// marker in the answer means the bucket keeps versions (on or suspended),
// so the key's versions are removed as well.
func (s *ObjectStorage) DeleteObject(
	ctx context.Context,
	bucket string,
	objectKey string,
) error {
	out, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(objectKey),
	})
	if err != nil {
		return classify("delete object", err)
	}
	if aws.ToBool(out.DeleteMarker) || aws.ToString(out.VersionId) != "" {
		return s.deleteVersions(ctx, bucket, objectKey)
	}

	return nil
}

// BeginMultipart asks the storage provider to initiate a multipart upload
// and returns the provider-generated upload ID. The caller must persist this
// ID in upload_sessions.storage_upload_id before accepting any part uploads.
func (s *ObjectStorage) BeginMultipart(
	ctx context.Context,
	bucket string,
	objectKey string,
	contentType string,
) (string, error) {
	if strings.TrimSpace(bucket) == "" {
		return "", errors.New("bucket is required")
	}
	if strings.TrimSpace(objectKey) == "" {
		return "", errors.New("object key is required")
	}
	if strings.TrimSpace(contentType) == "" {
		return "", errors.New("content type is required")
	}

	input := &s3.CreateMultipartUploadInput{
		Bucket:      aws.String(bucket),
		Key:         aws.String(objectKey),
		ContentType: aws.String(contentType),
	}

	output, err := s.client.CreateMultipartUpload(ctx, input)
	if err != nil {
		return "", classify("create multipart upload", err)
	}

	if output.UploadId == nil || strings.TrimSpace(*output.UploadId) == "" {
		return "", errors.New("storage provider returned an empty multipart upload ID")
	}

	return *output.UploadId, nil
}

// PresignPart generates a time-limited URL that allows the client to upload
// one numbered part directly to the storage provider.
//
// Security note: a presigned URL is a temporary bearer credential.
// Validate tenant ownership and upload state before generating one.
func (s *ObjectStorage) PresignPart(
	ctx context.Context,
	bucket string,
	objectKey string,
	storageUploadID string,
	partNumber int32,
	ttl time.Duration,
) (string, error) {
	if strings.TrimSpace(bucket) == "" {
		return "", errors.New("bucket is required")
	}
	if strings.TrimSpace(objectKey) == "" {
		return "", errors.New("object key is required")
	}
	if strings.TrimSpace(storageUploadID) == "" {
		return "", errors.New("storage upload ID is required")
	}
	if partNumber < 1 || partNumber > 10_000 {
		return "", errors.New("part number must be between 1 and 10000")
	}
	if ttl <= 0 || ttl > 7*24*time.Hour {
		return "", errors.New("presigned URL TTL must be greater than zero and at most 7 days")
	}

	input := &s3.UploadPartInput{
		Bucket:     aws.String(bucket),
		Key:        aws.String(objectKey),
		UploadId:   aws.String(storageUploadID),
		PartNumber: aws.Int32(partNumber),
	}

	presigned, err := s.presigner.PresignUploadPart(
		ctx,
		input,
		func(options *s3.PresignOptions) {
			options.Expires = ttl
		},
	)
	if err != nil {
		return "", fmt.Errorf("presign multipart part %d: %w", partNumber, err)
	}

	return presigned.URL, nil
}

// toSDKCompletedParts validates the application-owned part list and converts
// it to the AWS SDK representation required by CompleteMultipartUpload.
// Parts must be in strictly ascending part-number order.
func toSDKCompletedParts(
	parts []interfaces.CompletedPart,
) ([]types.CompletedPart, error) {
	if len(parts) == 0 {
		return nil, errors.New("at least one completed part is required")
	}

	result := make([]types.CompletedPart, 0, len(parts))
	var previousPartNumber int32

	for _, part := range parts {
		if part.PartNumber < 1 || part.PartNumber > 10_000 {
			return nil, fmt.Errorf("invalid part number %d", part.PartNumber)
		}

		if part.PartNumber <= previousPartNumber {
			return nil, errors.New(
				"completed parts must be in strictly ascending part-number order",
			)
		}

		if strings.TrimSpace(part.ETag) == "" {
			return nil, fmt.Errorf("ETag is required for part %d", part.PartNumber)
		}

		result = append(result, types.CompletedPart{
			PartNumber: aws.Int32(part.PartNumber),
			ETag:       aws.String(part.ETag),
		})

		previousPartNumber = part.PartNumber
	}

	return result, nil
}

// CompleteMultipart instructs the storage provider to assemble all uploaded
// parts into one object. The method returning nil means the SDK reported
// success; the service must separately verify the assembled object.
func (s *ObjectStorage) CompleteMultipart(
	ctx context.Context,
	bucket string,
	objectKey string,
	storageUploadID string,
	parts []interfaces.CompletedPart,
) error {
	if strings.TrimSpace(bucket) == "" {
		return errors.New("bucket is required")
	}
	if strings.TrimSpace(objectKey) == "" {
		return errors.New("object key is required")
	}
	if strings.TrimSpace(storageUploadID) == "" {
		return errors.New("storage upload ID is required")
	}

	sdkParts, err := toSDKCompletedParts(parts)
	if err != nil {
		return err
	}

	input := &s3.CompleteMultipartUploadInput{
		Bucket:   aws.String(bucket),
		Key:      aws.String(objectKey),
		UploadId: aws.String(storageUploadID),
		MultipartUpload: &types.CompletedMultipartUpload{
			Parts: sdkParts,
		},
	}

	_, err = s.client.CompleteMultipartUpload(ctx, input)
	if err != nil {
		return classify("complete multipart upload", err)
	}

	return nil
}

// AbortMultipart cancels an incomplete multipart upload and releases the
// storage provider's associated resources.
//
// An abort request can fail if the upload was already completed or aborted.
// The service should handle that error and reconcile state rather than
// deleting the Neon upload-session record unconditionally.
func (s *ObjectStorage) AbortMultipart(
	ctx context.Context,
	bucket string,
	objectKey string,
	storageUploadID string,
) error {
	if strings.TrimSpace(bucket) == "" {
		return errors.New("bucket is required")
	}
	if strings.TrimSpace(objectKey) == "" {
		return errors.New("object key is required")
	}
	if strings.TrimSpace(storageUploadID) == "" {
		return errors.New("storage upload ID is required")
	}

	_, err := s.client.AbortMultipartUpload(
		ctx,
		&s3.AbortMultipartUploadInput{
			Bucket:   aws.String(bucket),
			Key:      aws.String(objectKey),
			UploadId: aws.String(storageUploadID),
		},
	)
	if err != nil {
		return classify("abort multipart upload", err)
	}

	return nil
}

// ListParts pages through the provider's own record of the parts.
func (s *ObjectStorage) ListParts(
	ctx context.Context,
	bucket string,
	objectKey string,
	storageUploadID string,
) ([]interfaces.StoredPart, error) {
	if strings.TrimSpace(storageUploadID) == "" {
		return nil, errors.New("storage upload ID is required")
	}

	var parts []interfaces.StoredPart
	var marker *string

	for {
		out, err := s.client.ListParts(ctx, &s3.ListPartsInput{
			Bucket:           aws.String(bucket),
			Key:              aws.String(objectKey),
			UploadId:         aws.String(storageUploadID),
			PartNumberMarker: marker,
		})
		if err != nil {
			return nil, classify("list multipart parts", err)
		}

		for _, part := range out.Parts {
			parts = append(parts, interfaces.StoredPart{
				PartNumber: aws.ToInt32(part.PartNumber),
				ETag:       aws.ToString(part.ETag),
				SizeBytes:  aws.ToInt64(part.Size),
			})
		}

		if !aws.ToBool(out.IsTruncated) || out.NextPartNumberMarker == nil {
			return parts, nil
		}
		marker = out.NextPartNumberMarker
	}
}

// ListMultipartUploads returns in-progress upload IDs for exactly objectKey.
// Prefix matching can return longer keys, so each key is compared exactly.
func (s *ObjectStorage) ListMultipartUploads(
	ctx context.Context,
	bucket string,
	objectKey string,
) ([]string, error) {
	var ids []string
	var keyMarker, uploadIDMarker *string

	for {
		out, err := s.client.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{
			Bucket:         aws.String(bucket),
			Prefix:         aws.String(objectKey),
			KeyMarker:      keyMarker,
			UploadIdMarker: uploadIDMarker,
		})
		if err != nil {
			return nil, classify("list multipart uploads", err)
		}

		for _, upload := range out.Uploads {
			if aws.ToString(upload.Key) == objectKey {
				ids = append(ids, aws.ToString(upload.UploadId))
			}
		}

		if !aws.ToBool(out.IsTruncated) {
			return ids, nil
		}
		keyMarker = out.NextKeyMarker
		uploadIDMarker = out.NextUploadIdMarker
	}
}

// OpenObject streams the stored object; the body is read in chunks by the
// caller, never buffered whole here.
func (s *ObjectStorage) OpenObject(
	ctx context.Context,
	bucket string,
	objectKey string,
) (io.ReadCloser, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(objectKey),
	})
	if err != nil {
		return nil, classify("get object", err)
	}

	return out.Body, nil
}

// CopyObject is a server-side copy, pinned to the source's ETag read
// first. One CopyObject call copies up to 5 GiB; a larger source is copied
// in parts (copy.go).
func (s *ObjectStorage) CopyObject(
	ctx context.Context,
	sourceBucket string,
	sourceKey string,
	destinationBucket string,
	destinationKey string,
) error {
	if sourceBucket == "" || sourceKey == "" || destinationBucket == "" || destinationKey == "" {
		return errCopyArguments
	}

	source, err := s.HeadObject(ctx, sourceBucket, sourceKey)
	if err != nil {
		return err
	}
	limit := s.singleCopyBytes
	if limit <= 0 {
		limit = maxSingleCopyBytes
	}
	if source.SizeBytes > limit {
		return s.copyInParts(ctx, sourceBucket, sourceKey, source.ETag, source.SizeBytes, destinationBucket, destinationKey)
	}

	_, err = s.client.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket:            aws.String(destinationBucket),
		Key:               aws.String(destinationKey),
		CopySource:        aws.String(copySource(sourceBucket, sourceKey)),
		CopySourceIfMatch: aws.String(source.ETag),
	})
	if err != nil {
		return classify("copy object", err)
	}

	return nil
}

// permanentCodes are provider answers that retrying cannot change:
// configuration, authorization or a malformed request.
var permanentCodes = map[string]bool{
	"AccessDenied":          true,
	"InvalidAccessKeyId":    true,
	"SignatureDoesNotMatch": true,
	"NoSuchBucket":          true,
	"NotImplemented":        true,
	"InvalidArgument":       true,
	"InvalidRequest":        true,
	"InvalidPart":           true,
	"InvalidPartOrder":      true,
	"EntityTooSmall":        true,
	"MalformedXML":          true,
}

// classify turns an SDK error into the application's error kinds:
// known-absent results, permanent failures, or (anything else) an error
// whose outcome may be unknown and is safe only to reconcile, not assume.
func classify(operation string, err error) error {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch code := apiErr.ErrorCode(); {
		case code == "NoSuchKey" || code == "NotFound" || code == "NoSuchVersion":
			return fmt.Errorf("%s: %w", operation, interfaces.ErrObjectNotFound)
		case code == "NoSuchUpload":
			return fmt.Errorf("%s: %w", operation, interfaces.ErrStorageUploadNotFound)
		case code == "PreconditionFailed":
			return fmt.Errorf("%s: %w", operation, interfaces.ErrPreconditionFailed)
		case code == "InvalidRange":
			return fmt.Errorf("%s: %w", operation, interfaces.ErrInvalidRange)
		case throttlingCodes[code]:
			return fmt.Errorf("%s: %s: %w", operation, code, interfaces.ErrStorageThrottled)
		case code == "AccessDenied":
			return retry.Permanent(fmt.Errorf("%s: %s: %w", operation, code, interfaces.ErrStorageAccessDenied))
		case code == "InvalidAccessKeyId" || code == "SignatureDoesNotMatch" || code == "InvalidToken" || code == "ExpiredToken":
			return retry.Permanent(fmt.Errorf("%s: %s: %w", operation, code, interfaces.ErrStorageCredentialsRejected))
		case permanentCodes[code]:
			return retry.Permanent(fmt.Errorf("%s: %s", operation, code))
		default:
			return classifyStatus(operation, err, "provider error "+code)
		}
	}

	return classifyStatus(operation, err, err.Error())
}

// throttlingCodes are the providers' "slow down" answers.
var throttlingCodes = map[string]bool{
	"SlowDown":                   true,
	"Throttling":                 true,
	"ThrottlingException":        true,
	"RequestLimitExceeded":       true,
	"TooManyRequests":            true,
	"ServiceUnavailable":         true,
	"RequestThrottled":           true,
	"XMinioServerNotInitialized": true,
}

// classifyStatus uses the HTTP status when a provider answered without a
// recognisable code (a HEAD response has no body, for one).
func classifyStatus(operation string, err error, text string) error {
	var withStatus interface{ HTTPStatusCode() int }
	if errors.As(err, &withStatus) {
		switch withStatus.HTTPStatusCode() {
		case 404:
			return fmt.Errorf("%s: %w", operation, interfaces.ErrObjectNotFound)
		case 412:
			return fmt.Errorf("%s: %w", operation, interfaces.ErrPreconditionFailed)
		case 416:
			return fmt.Errorf("%s: %w", operation, interfaces.ErrInvalidRange)
		case 429, 503:
			return fmt.Errorf("%s: %w", operation, interfaces.ErrStorageThrottled)
		}
	}
	if text == err.Error() {
		return fmt.Errorf("%s: %w", operation, err)
	}
	return fmt.Errorf("%s: %s", operation, text)
}

// ListMultipartUploadsPage lists one page of in-progress sessions. The S3
// listing continues from a key marker and an upload ID marker; both travel
// in the opaque cursor, joined by a NUL that cannot occur in either.
func (s *ObjectStorage) ListMultipartUploadsPage(
	ctx context.Context,
	bucket string,
	prefix string,
	cursor string,
	limit int,
) (interfaces.MultipartUploadPage, error) {
	if limit <= 0 || limit > 1000 {
		return interfaces.MultipartUploadPage{}, errors.New("page size must be between 1 and 1000")
	}

	input := &s3.ListMultipartUploadsInput{
		Bucket:     aws.String(bucket),
		Prefix:     aws.String(prefix),
		MaxUploads: aws.Int32(int32(limit)),
	}
	if cursor != "" {
		keyMarker, uploadMarker, _ := strings.Cut(cursor, "\x00")
		input.KeyMarker = aws.String(keyMarker)
		if uploadMarker != "" {
			input.UploadIdMarker = aws.String(uploadMarker)
		}
	}

	out, err := s.client.ListMultipartUploads(ctx, input)
	if err != nil {
		return interfaces.MultipartUploadPage{}, classify("list multipart uploads", err)
	}

	page := interfaces.MultipartUploadPage{}
	for _, upload := range out.Uploads {
		page.Uploads = append(page.Uploads, interfaces.StoredMultipartUpload{
			ObjectKey:       aws.ToString(upload.Key),
			StorageUploadID: aws.ToString(upload.UploadId),
			Initiated:       aws.ToTime(upload.Initiated),
		})
	}
	if aws.ToBool(out.IsTruncated) {
		page.Cursor = aws.ToString(out.NextKeyMarker) + "\x00" + aws.ToString(out.NextUploadIdMarker)
	}
	return page, nil
}
