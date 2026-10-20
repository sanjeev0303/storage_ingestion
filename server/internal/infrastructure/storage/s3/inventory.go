package s3

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
)

// ListObjectsPage lists one page of objects with ListObjectsV2.
func (s *ObjectStorage) ListObjectsPage(
	ctx context.Context,
	bucket string,
	prefix string,
	cursor string,
	limit int,
) (interfaces.ObjectPage, error) {
	if limit <= 0 || limit > 1000 {
		return interfaces.ObjectPage{}, errors.New("page size must be between 1 and 1000")
	}

	input := &s3.ListObjectsV2Input{
		Bucket:  aws.String(bucket),
		Prefix:  aws.String(prefix),
		MaxKeys: aws.Int32(int32(limit)),
	}
	if cursor != "" {
		input.ContinuationToken = aws.String(cursor)
	}

	out, err := s.client.ListObjectsV2(ctx, input)
	if err != nil {
		return interfaces.ObjectPage{}, classify("list objects", err)
	}

	page := interfaces.ObjectPage{}
	for _, object := range out.Contents {
		page.Objects = append(page.Objects, interfaces.StoredObject{
			ObjectKey:    aws.ToString(object.Key),
			SizeBytes:    aws.ToInt64(object.Size),
			LastModified: aws.ToTime(object.LastModified),
			ETag:         aws.ToString(object.ETag),
		})
	}
	if aws.ToBool(out.IsTruncated) {
		page.Cursor = aws.ToString(out.NextContinuationToken)
	}
	return page, nil
}
