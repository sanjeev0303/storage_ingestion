package s3

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/retry"
)

func TestCopyPartSizeKeepsWithinS3Limits(t *testing.T) {
	fiveTiBParts := int64(5<<40+maxParts-1) / maxParts // 10,240 parts of 512 MiB would be too many
	cases := []struct {
		size, configured, want int64
	}{
		{6 << 30, copyPartBytes, copyPartBytes},
		{5 << 40, copyPartBytes, fiveTiBParts},
		{5 << 40, 1 << 20, fiveTiBParts},
		{12 << 20, 1 << 20, minPartBytes},
	}
	for _, c := range cases {
		got, err := copyPartSize(c.size, c.configured)
		if err != nil || got != c.want {
			t.Fatalf("copyPartSize(%d, %d) = %d, %v; want %d", c.size, c.configured, got, err, c.want)
		}
		if parts := (c.size + got - 1) / got; parts > maxParts || got > maxPartBytes {
			t.Fatalf("%d bytes in %d parts of %d", c.size, parts, got)
		}
	}
	if _, err := copyPartSize(5<<40+1, copyPartBytes); !retry.IsPermanent(err) {
		t.Fatalf("an object above 5 TiB is refused for good: %v", err)
	}
}

// minio connects to the compose MinIO (TEST_S3_BUCKET and S3_*), with the
// copy limits lowered so 13 MiB objects copy in three parts.
func minio(t *testing.T) (*ObjectStorage, *s3.Client, string) {
	t.Helper()
	bucket := os.Getenv("TEST_S3_BUCKET")
	if bucket == "" {
		t.Skip("TEST_S3_BUCKET is not set")
	}
	cfg, err := LoadConfig()
	if err != nil {
		t.Skipf("S3 configuration incomplete: %v", err)
	}
	client, err := NewClient(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	s := NewObjectStorage(client)
	s.SetCopyLimits(6<<20, 5<<20)
	return s, client, bucket
}

func putRandom(t *testing.T, client *s3.Client, bucket, key string, size int) []byte {
	t.Helper()
	data := make([]byte, size)
	_, _ = rand.Read(data)
	if _, err := client.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key), Body: bytes.NewReader(data),
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = (&ObjectStorage{client: client}).DeleteObject(context.Background(), bucket, key) })
	return data
}

func TestMinIOCopyAboveTheSingleCopyLimitGoesInParts(t *testing.T) {
	s, client, bucket := minio(t)
	ctx := context.Background()
	run := time.Now().UnixNano()
	source, destination := fmt.Sprintf("copy-test/%d/source", run), fmt.Sprintf("copy-test/%d/destination", run)
	data := putRandom(t, client, bucket, source, 13<<20)
	t.Cleanup(func() { _ = s.DeleteObject(context.Background(), bucket, destination) })

	// A crashed earlier attempt left an upload on the destination key.
	if _, err := s.BeginMultipart(ctx, bucket, destination, "application/octet-stream"); err != nil {
		t.Fatal(err)
	}
	if err := s.CopyObject(ctx, bucket, source, bucket, destination); err != nil {
		t.Fatal(err)
	}
	body, err := s.OpenObject(ctx, bucket, destination)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(body)
	body.Close()
	if !bytes.Equal(got, data) {
		t.Fatalf("the copy differs: %d of %d bytes", len(got), len(data))
	}
	meta, _ := s.HeadObject(ctx, bucket, destination)
	if !bytes.HasSuffix([]byte(meta.ETag), []byte(`-3"`)) {
		t.Fatalf("copied in three parts, ETag %s", meta.ETag)
	}
	if left, err := s.ListMultipartUploads(ctx, bucket, destination); err != nil || len(left) != 0 {
		t.Fatalf("no upload is left on the destination: %v %v", left, err)
	}

	// Below the limit it is one CopyObject call.
	small := fmt.Sprintf("copy-test/%d/small", run)
	putRandom(t, client, bucket, small, 1<<20)
	t.Cleanup(func() { _ = s.DeleteObject(context.Background(), bucket, small+"-copy") })
	if err := s.CopyObject(ctx, bucket, small, bucket, small+"-copy"); err != nil {
		t.Fatal(err)
	}
	if meta, err := s.HeadObject(ctx, bucket, small+"-copy"); err != nil || meta.SizeBytes != 1<<20 {
		t.Fatalf("single copy: %+v %v", meta, err)
	}
}

// A source replaced after the copy read its ETag fails every part: the
// copy is aborted and leaves nothing on the destination.
func TestMinIOCopyOfAReplacedSourceFailsAndLeavesNothing(t *testing.T) {
	s, client, bucket := minio(t)
	ctx := context.Background()
	run := time.Now().UnixNano()
	source, destination := fmt.Sprintf("copy-test/%d/source", run), fmt.Sprintf("copy-test/%d/destination", run)
	putRandom(t, client, bucket, source, 13<<20)
	before, err := s.HeadObject(ctx, bucket, source)
	if err != nil {
		t.Fatal(err)
	}
	putRandom(t, client, bucket, source, 13<<20)

	err = s.copyInParts(ctx, bucket, source, before.ETag, 13<<20, bucket, destination)
	if !errors.Is(err, interfaces.ErrPreconditionFailed) {
		t.Fatalf("a replaced source must fail the copy: %v", err)
	}
	if _, err := s.HeadObject(ctx, bucket, destination); !errors.Is(err, interfaces.ErrObjectNotFound) {
		t.Fatalf("nothing is written: %v", err)
	}
	if left, err := s.ListMultipartUploads(ctx, bucket, destination); err != nil || len(left) != 0 {
		t.Fatalf("the failed copy's upload is aborted: %v %v", left, err)
	}
}

// cancelOnPartCopy cancels the copy's context as its first part copy is
// sent, after the multipart upload has begun.
type cancelOnPartCopy struct {
	inner  s3.HTTPClient
	cancel context.CancelFunc
}

func (c cancelOnPartCopy) Do(req *http.Request) (*http.Response, error) {
	if req.Header.Get("X-Amz-Copy-Source-Range") != "" {
		c.cancel()
	}
	return c.inner.Do(req)
}

// A copy cancelled mid-way (a lost lease, a shutdown) stops and still
// aborts its upload.
func TestMinIOCancelledCopyAbortsItsUpload(t *testing.T) {
	s, client, bucket := minio(t)
	run := time.Now().UnixNano()
	source, destination := fmt.Sprintf("copy-test/%d/source", run), fmt.Sprintf("copy-test/%d/destination", run)
	putRandom(t, client, bucket, source, 13<<20)
	meta, err := s.HeadObject(context.Background(), bucket, source)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	options := client.Options()
	cancelling := NewObjectStorage(s3.New(options, func(o *s3.Options) { o.HTTPClient = cancelOnPartCopy{options.HTTPClient, cancel} }))
	cancelling.SetCopyLimits(6<<20, 5<<20)
	if err := cancelling.copyInParts(ctx, bucket, source, meta.ETag, meta.SizeBytes, bucket, destination); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled copy must fail with the cancellation: %v", err)
	}
	if left, err := s.ListMultipartUploads(context.Background(), bucket, destination); err != nil || len(left) != 0 {
		t.Fatalf("no upload is left: %v %v", left, err)
	}
	if _, err := s.HeadObject(context.Background(), bucket, destination); !errors.Is(err, interfaces.ErrObjectNotFound) {
		t.Fatalf("nothing is written: %v", err)
	}
}

// repeating reads one random block over and over: large test objects
// without holding them in memory.
type repeating struct {
	block []byte
	at    int
}

func (r *repeating) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) {
		c := copy(p[n:], r.block[r.at:])
		n += c
		r.at = (r.at + c) % len(r.block)
	}
	return n, nil
}

// Opt-in (TEST_S3_LARGE_COPY_BYTES, more than 5 GiB): a real copy above
// S3's single-copy limit, with S3's own limits, timed. It needs twice the
// size free in the bucket's storage and deletes both objects after.
func TestMinIOLargeCopyAboveFiveGiB(t *testing.T) {
	size, _ := strconv.ParseInt(os.Getenv("TEST_S3_LARGE_COPY_BYTES"), 10, 64)
	if size <= maxSingleCopyBytes {
		t.Skip("TEST_S3_LARGE_COPY_BYTES is not set above 5 GiB")
	}
	s, client, bucket := minio(t)
	s.SetCopyLimits(0, 0)
	s.SetTransferPartBytes(64 << 20)
	ctx := context.Background()
	run := time.Now().UnixNano()
	source, destination := fmt.Sprintf("copy-test/%d/large-source", run), fmt.Sprintf("copy-test/%d/large-destination", run)
	t.Cleanup(func() {
		_ = s.DeleteObject(context.Background(), bucket, source)
		_ = s.DeleteObject(context.Background(), bucket, destination)
	})

	block := make([]byte, 1<<20)
	_, _ = rand.Read(block)
	sum := sha256.New()
	started := time.Now()
	if _, err := s.WriteObject(ctx, bucket, source, io.TeeReader(io.LimitReader(&repeating{block: block}, size), sum), size, "application/octet-stream"); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %d bytes in %s", size, time.Since(started).Round(time.Millisecond))

	// What promotion did before: one CopyObject call, refused above 5 GiB.
	_, err := client.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(destination + "-single"), CopySource: aws.String(copySource(bucket, source)),
	})
	t.Logf("a single CopyObject call: %v", err)
	if err == nil {
		_ = s.DeleteObject(ctx, bucket, destination+"-single")
		t.Log("this provider copied above 5 GiB in one call")
	}

	started = time.Now()
	if err := s.CopyObject(ctx, bucket, source, bucket, destination); err != nil {
		t.Fatal(err)
	}
	copied := time.Since(started)
	meta, err := s.HeadObject(ctx, bucket, destination)
	if err != nil || meta.SizeBytes != size {
		t.Fatalf("copy: %+v %v", meta, err)
	}
	t.Logf("copied in parts in %s (%.0f MiB/s), ETag %s", copied.Round(time.Millisecond), float64(size)/(1<<20)/copied.Seconds(), meta.ETag)

	// Promotion then hashes the copy before publishing it.
	started = time.Now()
	body, err := s.OpenObject(ctx, bucket, destination)
	if err != nil {
		t.Fatal(err)
	}
	check := sha256.New()
	if _, err := io.Copy(check, body); err != nil {
		t.Fatal(err)
	}
	body.Close()
	hashed := time.Since(started)
	if !bytes.Equal(check.Sum(nil), sum.Sum(nil)) {
		t.Fatal("the copy's SHA-256 differs from the source's")
	}
	t.Logf("verified SHA-256 in %s (%.0f MiB/s)", hashed.Round(time.Millisecond), float64(size)/(1<<20)/hashed.Seconds())
}
