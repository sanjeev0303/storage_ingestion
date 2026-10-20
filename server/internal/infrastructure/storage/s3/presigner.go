package s3

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
)

var _ interfaces.ObjectDelivery = (*ObjectStorage)(nil)

// PresignGet signs a GET for exactly this bucket and key. The response
// headers are signed into the URL, so a holder cannot change them.
func (s *ObjectStorage) PresignGet(
	ctx context.Context,
	bucket string,
	objectKey string,
	ttl time.Duration,
	options interfaces.DownloadOptions,
) (string, error) {
	if bucket == "" || objectKey == "" {
		return "", errors.New("bucket and object key are required")
	}
	if ttl <= 0 || ttl > 7*24*time.Hour {
		return "", errors.New("download URL TTL must be greater than zero and at most 7 days")
	}

	input := &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(objectKey),
	}
	if options.ContentDisposition != "" {
		input.ResponseContentDisposition = aws.String(options.ContentDisposition)
	}
	if options.ContentType != "" {
		input.ResponseContentType = aws.String(options.ContentType)
	}
	if options.VersionID != "" {
		input.VersionId = aws.String(options.VersionID)
	}

	presigned, err := s.presigner.PresignGetObject(ctx, input, func(o *s3.PresignOptions) {
		o.Expires = ttl
	})
	if err != nil {
		return "", classify("presign download", err)
	}
	return presigned.URL, nil
}

// OpenRange streams part or all of an object straight from the provider.
// The condition is enforced by the provider (If-Match, versionId), so the
// bytes always come from the object the caller checked.
func (s *ObjectStorage) OpenRange(
	ctx context.Context,
	bucket string,
	objectKey string,
	first int64,
	last int64,
	cond interfaces.ReadCondition,
) (io.ReadCloser, error) {
	input := &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(objectKey),
	}
	if cond.IfMatch != "" {
		input.IfMatch = aws.String(cond.IfMatch)
	}
	if cond.VersionID != "" {
		input.VersionId = aws.String(cond.VersionID)
	}
	if last >= 0 {
		if first < 0 || first > last {
			return nil, fmt.Errorf("invalid byte range %d-%d", first, last)
		}
		input.Range = aws.String(fmt.Sprintf("bytes=%d-%d", first, last))
	}

	out, err := s.client.GetObject(ctx, input)
	if err != nil {
		return nil, classify("get object range", err)
	}
	return out.Body, nil
}
