package s3

import (
	"context"
	"errors"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
)

var _ interfaces.ObjectLocker = (*ObjectStorage)(nil)

// ObjectLockEnabled reports whether the bucket has S3 Object Lock on. It
// can only be turned on when a bucket is created.
func (s *ObjectStorage) ObjectLockEnabled(ctx context.Context, bucket string) (bool, error) {
	out, err := s.client.GetObjectLockConfiguration(ctx, &s3.GetObjectLockConfigurationInput{Bucket: aws.String(bucket)})
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) && apiErr.ErrorCode() == "ObjectLockConfigurationNotFoundError" {
		return false, nil
	}
	if err != nil {
		return false, classify("get object lock configuration", err)
	}
	return out.ObjectLockConfiguration != nil && out.ObjectLockConfiguration.ObjectLockEnabled == types.ObjectLockEnabledEnabled, nil
}

// LockObjectVersion sets a version's retention in GOVERNANCE mode (only a
// principal allowed s3:BypassGovernanceRetention can shorten it) when
// until is set, and its legal hold.
func (s *ObjectStorage) LockObjectVersion(ctx context.Context, bucket, key, versionID string, until time.Time, legalHold bool) error {
	if bucket == "" || key == "" || versionID == "" {
		return errors.New("bucket, object key and version ID are required")
	}
	if !until.IsZero() {
		_, err := s.client.PutObjectRetention(ctx, &s3.PutObjectRetentionInput{
			Bucket: aws.String(bucket), Key: aws.String(key), VersionId: aws.String(versionID),
			Retention: &types.ObjectLockRetention{Mode: types.ObjectLockRetentionModeGovernance, RetainUntilDate: aws.Time(until.UTC())},
		})
		if err != nil {
			return classify("put object retention", err)
		}
	}
	status := types.ObjectLockLegalHoldStatusOff
	if legalHold {
		status = types.ObjectLockLegalHoldStatusOn
	}
	_, err := s.client.PutObjectLegalHold(ctx, &s3.PutObjectLegalHoldInput{
		Bucket: aws.String(bucket), Key: aws.String(key), VersionId: aws.String(versionID),
		LegalHold: &types.ObjectLockLegalHold{Status: status},
	})
	if err != nil {
		return classify("put object legal hold", err)
	}
	return nil
}
