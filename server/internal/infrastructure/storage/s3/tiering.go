package s3

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

var _ interfaces.TierStorage = (*ObjectStorage)(nil)

// S3's storage classes for each tier: Standard-IA, Glacier Flexible
// Retrieval and Glacier Deep Archive.
var s3Classes = map[model.StorageTier]types.StorageClass{
	model.TierStandard:    types.StorageClassStandard,
	model.TierInfrequent:  types.StorageClassStandardIa,
	model.TierArchive:     types.StorageClassGlacier,
	model.TierDeepArchive: types.StorageClassDeepArchive,
}

func tierOfClass(class types.StorageClass) model.StorageTier {
	for tier, c := range s3Classes {
		if c == class {
			return tier
		}
	}
	switch class {
	case "", types.StorageClassReducedRedundancy, types.StorageClassIntelligentTiering, types.StorageClassExpressOnezone:
		return model.TierStandard
	case types.StorageClassOnezoneIa, types.StorageClassGlacierIr:
		return model.TierInfrequent
	}
	return model.TierStandard
}

// SetTier copies the object onto itself in the tier's storage class. The
// copy names the expected ETag, so a changed object is never moved.
func (s *ObjectStorage) SetTier(ctx context.Context, bucket, key, etag string, size int64, tier model.StorageTier) (interfaces.ObjectMetadata, error) {
	class, ok := s3Classes[tier]
	if !ok {
		return interfaces.ObjectMetadata{}, fmt.Errorf("%w: no S3 storage class for %s", model.ErrTierUnsupported, tier)
	}
	limit := s.singleCopyBytes
	if limit <= 0 {
		limit = parallelCopyBytes
	}
	if size > limit {
		if err := s.copyInParts(ctx, bucket, key, etag, size, bucket, key, class); err != nil {
			return interfaces.ObjectMetadata{}, err
		}
		return s.HeadObject(ctx, bucket, key)
	}
	input := &s3.CopyObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key), CopySource: aws.String(copySource(bucket, key)),
		CopySourceIfMatch: aws.String(etag), StorageClass: class, MetadataDirective: types.MetadataDirectiveCopy,
	}
	if s.kmsKey != "" {
		input.ServerSideEncryption, input.SSEKMSKeyId = types.ServerSideEncryptionAwsKms, aws.String(s.kmsKey)
	}
	out, err := s.client.CopyObject(ctx, input)
	if err != nil {
		return interfaces.ObjectMetadata{}, classify("change storage class", err)
	}
	meta := interfaces.ObjectMetadata{SizeBytes: size, VersionID: aws.ToString(out.VersionId)}
	if out.CopyObjectResult != nil {
		meta.ETag = aws.ToString(out.CopyObjectResult.ETag)
	}
	return meta, nil
}

func (s *ObjectStorage) TierState(ctx context.Context, bucket, key, versionID string) (interfaces.TierState, error) {
	input := &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)}
	if versionID != "" {
		input.VersionId = aws.String(versionID)
	}
	out, err := s.client.HeadObject(ctx, input)
	if err != nil {
		return interfaces.TierState{}, classify("read storage class", err)
	}
	state := interfaces.TierState{Tier: tierOfClass(out.StorageClass)}
	state.Restoring, state.ReadableUntil = parseRestore(aws.ToString(out.Restore))
	return state, nil
}

// parseRestore reads S3's x-amz-restore header:
// ongoing-request="false", expiry-date="Fri, 21 Dec 2012 00:00:00 GMT".
func parseRestore(header string) (bool, *time.Time) {
	if header == "" {
		return false, nil
	}
	if strings.Contains(header, `ongoing-request="true"`) {
		return true, nil
	}
	_, rest, ok := strings.Cut(header, `expiry-date="`)
	if !ok {
		return false, nil
	}
	date, _, _ := strings.Cut(rest, `"`)
	at, err := time.Parse(time.RFC1123, date)
	if err != nil {
		return false, nil
	}
	return false, &at
}

var restoreTiers = map[model.RestoreSpeed]types.Tier{
	model.RestoreFast: types.TierExpedited, model.RestoreStandard: types.TierStandard, model.RestoreBulk: types.TierBulk,
}

// Restore asks S3 for a temporary readable copy of a Glacier object. A
// restore already under way is not an error.
func (s *ObjectStorage) Restore(ctx context.Context, bucket, key, versionID string, days int, speed model.RestoreSpeed) error {
	input := &s3.RestoreObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key),
		RestoreRequest: &types.RestoreRequest{Days: aws.Int32(int32(days)),
			GlacierJobParameters: &types.GlacierJobParameters{Tier: restoreTiers[speed]}},
	}
	if versionID != "" {
		input.VersionId = aws.String(versionID)
	}
	_, err := s.client.RestoreObject(ctx, input)
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) && apiErr.ErrorCode() == "RestoreAlreadyInProgress" {
		return nil
	}
	if err != nil {
		return classify("restore archived object", err)
	}
	return nil
}
