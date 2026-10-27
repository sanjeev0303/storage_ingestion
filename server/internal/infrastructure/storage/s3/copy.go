package s3

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/sanjeev0303/object-storage-platform/internal/platform/retry"
)

// S3's copy limits: one CopyObject call copies at most 5 GiB; a larger
// object is copied in parts, of 5 MiB to 5 GiB each and at most 10,000 of
// them, up to S3's largest object, 5 TiB.
const (
	maxSingleCopyBytes = 5 << 30
	maxObjectBytes     = 5 << 40
	maxParts           = 10000
	minPartBytes       = 5 << 20
	maxPartBytes       = 5 << 30
	// copyPartBytes keeps each part copy a short request: a provider on
	// slow disks cut 512 MiB part copies short (IncompleteBody) in the
	// Phase 12 run. Objects above 1.22 TiB get larger parts to fit 10,000.
	copyPartBytes = 128 << 20
	// copyConcurrency bounds the part copies of one object in flight.
	copyConcurrency = 4
	// parallelCopyBytes is the size above which a copy goes in parts even
	// where one call could do it: one CopyObject copies serially inside
	// the provider, four part copies at once finish sooner.
	parallelCopyBytes = 256 << 20
)

// SetCopyLimits lowers the size above which a copy goes in parts, and the
// part size, so tests can copy in parts without 5 GiB objects.
func (s *ObjectStorage) SetCopyLimits(singleCopyBytes, partBytes int64) {
	s.singleCopyBytes, s.copyPartBytes = singleCopyBytes, partBytes
}

// copySource is CopySource's "bucket/key", URL-encoded segment by segment.
func copySource(bucket, key string) string {
	segments := strings.Split(key, "/")
	for i, segment := range segments {
		segments[i] = url.PathEscape(segment)
	}
	return bucket + "/" + strings.Join(segments, "/")
}

// copyPartSize is the part size for copying size bytes: the configured
// size, raised so the parts fit in 10,000.
func copyPartSize(size, configured int64) (int64, error) {
	if size > maxObjectBytes {
		return 0, retry.Permanent(fmt.Errorf("copy object: %d bytes is more than S3's largest object (5 TiB)", size))
	}
	part := max(configured, minPartBytes, (size+maxParts-1)/maxParts)
	if part > maxPartBytes {
		return 0, retry.Permanent(fmt.Errorf("copy object: a %d-byte part is larger than S3 allows", part))
	}
	return part, nil
}

// copyInParts copies an object too large for one CopyObject call, as a
// multipart upload of part copies. Every part names the source ETag read
// before the copy, so a source replaced meanwhile fails the copy instead
// of mixing bytes. A failure aborts the upload. The destination key holds
// the only record of a copy in progress: an upload a crashed attempt left
// on it is aborted first, so a retry starts clean.
func (s *ObjectStorage) copyInParts(ctx context.Context, sourceBucket, sourceKey, sourceETag string, size int64, destinationBucket, destinationKey string) error {
	configured := s.copyPartBytes
	if configured <= 0 {
		configured = copyPartBytes
	}
	partSize, err := copyPartSize(size, configured)
	if err != nil {
		return err
	}

	left, err := s.ListMultipartUploads(ctx, destinationBucket, destinationKey)
	if err != nil {
		return err
	}
	for _, id := range left {
		if err := s.AbortMultipart(ctx, destinationBucket, destinationKey, id); err != nil {
			return fmt.Errorf("abort an earlier copy: %w", err)
		}
	}

	created, err := s.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String(destinationBucket), Key: aws.String(destinationKey),
	})
	if err != nil {
		return classify("begin multipart copy", err)
	}
	uploadID := aws.ToString(created.UploadId)
	abort := func(cause error) error {
		_ = s.AbortMultipart(context.WithoutCancel(ctx), destinationBucket, destinationKey, uploadID)
		return cause
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		mu       sync.Mutex
		parts    []types.CompletedPart
		firstErr error
		wg       sync.WaitGroup
	)
	slots := make(chan struct{}, copyConcurrency)
	source := copySource(sourceBucket, sourceKey)
	for number, first := int32(1), int64(0); first < size; number, first = number+1, first+partSize {
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
		last := min(first+partSize, size) - 1
		wg.Add(1)
		go func(number int32, first, last int64) {
			defer wg.Done()
			defer func() { <-slots }()
			out, err := s.client.UploadPartCopy(ctx, &s3.UploadPartCopyInput{
				Bucket: aws.String(destinationBucket), Key: aws.String(destinationKey), UploadId: aws.String(uploadID),
				PartNumber: aws.Int32(number), CopySource: aws.String(source),
				CopySourceRange:   aws.String(fmt.Sprintf("bytes=%d-%d", first, last)),
				CopySourceIfMatch: aws.String(sourceETag),
			})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = classify("copy part", err)
					cancel()
				}
				return
			}
			parts = append(parts, types.CompletedPart{PartNumber: aws.Int32(number), ETag: out.CopyPartResult.ETag})
		}(number, first, last)
	}
	wg.Wait()
	if firstErr == nil && ctx.Err() != nil {
		firstErr = ctx.Err()
	}
	if firstErr != nil {
		return abort(firstErr)
	}

	sort.Slice(parts, func(i, j int) bool { return aws.ToInt32(parts[i].PartNumber) < aws.ToInt32(parts[j].PartNumber) })
	_, err = s.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: aws.String(destinationBucket), Key: aws.String(destinationKey), UploadId: aws.String(uploadID),
		MultipartUpload: &types.CompletedMultipartUpload{Parts: parts},
	})
	if err != nil {
		// The answer may have been lost after the copy completed; the
		// caller verifies the destination before trusting it either way.
		return abort(classify("complete multipart copy", err))
	}
	return nil
}

var errCopyArguments = errors.New("source and destination bucket and key are required")
