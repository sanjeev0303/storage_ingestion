package s3

import (
	"context"
	"errors"

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

// LockObjectVersion sets a version's retention when lock.Until is set: in
// GOVERNANCE mode only a principal allowed s3:BypassGovernanceRetention
// can shorten it, in COMPLIANCE mode nobody can. It also sets the legal
// hold. The platform never sends the bypass header.
func (s *ObjectStorage) LockObjectVersion(ctx context.Context, bucket, key, versionID string, lock interfaces.VersionLock) error {
	if bucket == "" || key == "" || versionID == "" {
		return errors.New("bucket, object key and version ID are required")
	}
	if !lock.Until.IsZero() {
		mode := types.ObjectLockRetentionModeGovernance
		if lock.Mode == "COMPLIANCE" {
			mode = types.ObjectLockRetentionModeCompliance
		}
		_, err := s.client.PutObjectRetention(ctx, &s3.PutObjectRetentionInput{
			Bucket: aws.String(bucket), Key: aws.String(key), VersionId: aws.String(versionID),
			Retention: &types.ObjectLockRetention{Mode: mode, RetainUntilDate: aws.Time(lock.Until.UTC())},
		})
		if err != nil {
			return classify("put object retention", err)
		}
	}
	status := types.ObjectLockLegalHoldStatusOff
	if lock.LegalHold {
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

// ObjectLockStatus reads a version's retention and legal hold as S3 holds
// them. A version with neither answers the zero lock.
func (s *ObjectStorage) ObjectLockStatus(ctx context.Context, bucket, key, versionID string) (interfaces.VersionLock, error) {
	var out interfaces.VersionLock
	notSet := func(err error) bool {
		var apiErr smithy.APIError
		return errors.As(err, &apiErr) && apiErr.ErrorCode() == "NoSuchObjectLockConfiguration"
	}
	retention, err := s.client.GetObjectRetention(ctx, &s3.GetObjectRetentionInput{
		Bucket: aws.String(bucket), Key: aws.String(key), VersionId: aws.String(versionID)})
	switch {
	case notSet(err):
	case err != nil:
		return out, classify("get object retention", err)
	case retention.Retention != nil:
		out.Mode = string(retention.Retention.Mode)
		if retention.Retention.RetainUntilDate != nil {
			out.Until = retention.Retention.RetainUntilDate.UTC()
		}
	}
	hold, err := s.client.GetObjectLegalHold(ctx, &s3.GetObjectLegalHoldInput{
		Bucket: aws.String(bucket), Key: aws.String(key), VersionId: aws.String(versionID)})
	switch {
	case notSet(err):
	case err != nil:
		return out, classify("get object legal hold", err)
	case hold.LegalHold != nil:
		out.LegalHold = hold.LegalHold.Status == types.ObjectLockLegalHoldStatusOn
	}
	return out, nil
}
