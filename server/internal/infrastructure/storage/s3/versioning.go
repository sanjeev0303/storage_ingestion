package s3

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
)

var _ interfaces.LifecycleStorage = (*ObjectStorage)(nil)

// VersioningStatus reports the bucket's versioning: "Enabled", "Suspended",
// or "" for a bucket that never had it.
func (s *ObjectStorage) VersioningStatus(ctx context.Context, bucket string) (string, error) {
	out, err := s.client.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{Bucket: aws.String(bucket)})
	if err != nil {
		return "", classify("get bucket versioning", err)
	}
	return string(out.Status), nil
}

// HeadObjectVersion checks one version of an object.
func (s *ObjectStorage) HeadObjectVersion(ctx context.Context, bucket, objectKey, versionID string) (interfaces.ObjectMetadata, error) {
	if versionID == "" {
		return interfaces.ObjectMetadata{}, errors.New("version ID is required")
	}
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket:    aws.String(bucket),
		Key:       aws.String(objectKey),
		VersionId: aws.String(versionID),
	})
	if err != nil {
		return interfaces.ObjectMetadata{}, classify("head object version", err)
	}
	return interfaces.ObjectMetadata{
		SizeBytes: aws.ToInt64(out.ContentLength),
		ETag:      aws.ToString(out.ETag),
		VersionID: aws.ToString(out.VersionId),
	}, nil
}

// DeleteObjectVersion removes exactly one version of an object. On a
// versioned bucket, deleting by key alone only adds a delete marker and
// keeps the data.
func (s *ObjectStorage) DeleteObjectVersion(ctx context.Context, bucket, objectKey, versionID string) error {
	if bucket == "" || objectKey == "" || versionID == "" {
		return errors.New("bucket, object key and version ID are required")
	}
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket:    aws.String(bucket),
		Key:       aws.String(objectKey),
		VersionId: aws.String(versionID),
	})
	if err != nil {
		return classify("delete object version", err)
	}
	return nil
}

var _ interfaces.CapabilityReporter = (*ObjectStorage)(nil)

// Capabilities asks the provider what the bucket does. S3-compatible
// providers differ: some have no versioning at all and answer with an
// error, which is reported rather than taken for "unversioned".
func (s *ObjectStorage) Capabilities(ctx context.Context, bucket string) (interfaces.StorageCapabilities, error) {
	status, err := s.VersioningStatus(ctx, bucket)
	if err != nil {
		return interfaces.StorageCapabilities{}, err
	}
	return interfaces.StorageCapabilities{ProviderVersioning: status == "Enabled", VersioningStatus: status}, nil
}
