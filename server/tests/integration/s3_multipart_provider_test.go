package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	checksum "github.com/sanjeev0303/object-storage-platform/internal/infrastructure/checksum/sha256"
	s3storage "github.com/sanjeev0303/object-storage-platform/internal/infrastructure/storage/s3"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
)

// Runs against the real S3-compatible provider. It needs TEST_S3_BUCKET
// naming a dedicated, disposable test bucket, plus the usual S3_* settings
// (S3_REGION, S3_ENDPOINT, credentials). It deletes what it creates.
func openTestProvider(t *testing.T) (*s3storage.ObjectStorage, string) {
	t.Helper()
	bucket := os.Getenv("TEST_S3_BUCKET")
	if bucket == "" {
		t.Skip("TEST_S3_BUCKET is not set; provider integration tests were not run")
	}

	cfg, err := s3storage.LoadConfig()
	if err != nil {
		t.Skipf("S3 configuration incomplete (%v); provider integration tests were not run", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	client, err := s3storage.NewClient(ctx, cfg)
	if err != nil {
		t.Fatalf("S3 client: %v", err)
	}
	return s3storage.NewObjectStorage(client), bucket
}

// putPart uploads bytes through a presigned part URL, as a client does,
// and returns the ETag the provider answered with.
func putPart(t *testing.T, url string, data []byte) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, url, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("upload part: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("upload part: HTTP %d", resp.StatusCode)
	}
	return resp.Header.Get("ETag")
}

func TestProviderMultipartLifecycle(t *testing.T) {
	storage, bucket := openTestProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	key := fmt.Sprintf("quarantine/integration-test/%d", time.Now().UnixNano())
	t.Cleanup(func() { _ = storage.DeleteObject(context.Background(), bucket, key) })

	uploadID, err := storage.BeginMultipart(ctx, bucket, key, "application/octet-stream")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}

	ids, err := storage.ListMultipartUploads(ctx, bucket, key)
	if err != nil || len(ids) != 1 || ids[0] != uploadID {
		t.Fatalf("list uploads: %v, %v", ids, err)
	}

	first := bytes.Repeat([]byte("p"), 5*1024*1024)
	second := []byte("final")
	var etags []string
	for i, data := range [][]byte{first, second} {
		url, err := storage.PresignPart(ctx, bucket, key, uploadID, int32(i+1), 5*time.Minute)
		if err != nil {
			t.Fatalf("presign part %d: %v", i+1, err)
		}
		etags = append(etags, putPart(t, url, data))
	}

	parts, err := storage.ListParts(ctx, bucket, key, uploadID)
	if err != nil || len(parts) != 2 {
		t.Fatalf("list parts: %+v, %v", parts, err)
	}
	for i, p := range parts {
		if strings.Trim(p.ETag, `"`) != strings.Trim(etags[i], `"`) {
			t.Errorf("part %d ETag: provider %s, upload response %s", i+1, p.ETag, etags[i])
		}
	}
	if parts[0].SizeBytes != int64(len(first)) || parts[1].SizeBytes != int64(len(second)) {
		t.Fatalf("part sizes: %+v", parts)
	}

	err = storage.CompleteMultipart(ctx, bucket, key, uploadID, []interfaces.CompletedPart{
		{PartNumber: 1, ETag: parts[0].ETag},
		{PartNumber: 2, ETag: parts[1].ETag},
	})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}

	// Completion consumed the upload: the provider now answers "absent".
	if _, err := storage.ListParts(ctx, bucket, key, uploadID); !errors.Is(err, interfaces.ErrStorageUploadNotFound) {
		t.Fatalf("expected ErrStorageUploadNotFound after completion, got %v", err)
	}

	meta, err := storage.HeadObject(ctx, bucket, key)
	if err != nil || meta.SizeBytes != int64(len(first)+len(second)) {
		t.Fatalf("head: %+v, %v", meta, err)
	}

	body, err := storage.OpenObject(ctx, bucket, key)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer body.Close()
	got, err := checksum.Sum(ctx, body)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	want := sha256.Sum256(append(append([]byte{}, first...), second...))
	if got != hex.EncodeToString(want[:]) {
		t.Fatal("the streamed SHA-256 does not match the uploaded bytes")
	}
}

func TestProviderAbortAndAbsence(t *testing.T) {
	storage, bucket := openTestProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	key := fmt.Sprintf("quarantine/integration-test/abort-%d", time.Now().UnixNano())
	uploadID, err := storage.BeginMultipart(ctx, bucket, key, "application/octet-stream")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}

	if err := storage.AbortMultipart(ctx, bucket, key, uploadID); err != nil {
		t.Fatalf("abort: %v", err)
	}
	if err := storage.AbortMultipart(ctx, bucket, key, uploadID); err != nil && !errors.Is(err, interfaces.ErrStorageUploadNotFound) {
		// Some providers answer a second abort with success; both are fine.
		t.Fatalf("second abort must report absence or succeed, got %v", err)
	}
	if ids, err := storage.ListMultipartUploads(ctx, bucket, key); err != nil || len(ids) != 0 {
		t.Fatalf("expected no uploads after abort: %v, %v", ids, err)
	}
	if _, err := storage.HeadObject(ctx, bucket, key); !errors.Is(err, interfaces.ErrObjectNotFound) {
		t.Fatalf("expected ErrObjectNotFound, got %v", err)
	}
}

func TestProviderCopyObject(t *testing.T) {
	storage, bucket := openTestProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	run := time.Now().UnixNano()
	source := fmt.Sprintf("quarantine/integration-test/copy-%d", run)
	destination := fmt.Sprintf("assets/integration-test/copy-%d", run)
	t.Cleanup(func() {
		_ = storage.DeleteObject(context.Background(), bucket, source)
		_ = storage.DeleteObject(context.Background(), bucket, destination)
	})

	// Create the source through a single-part multipart upload.
	data := []byte("promotion copy test")
	uploadID, err := storage.BeginMultipart(ctx, bucket, source, "application/octet-stream")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	url, err := storage.PresignPart(ctx, bucket, source, uploadID, 1, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	etag := putPart(t, url, data)
	if err := storage.CompleteMultipart(ctx, bucket, source, uploadID, []interfaces.CompletedPart{{PartNumber: 1, ETag: etag}}); err != nil {
		t.Fatalf("complete: %v", err)
	}

	for i := 0; i < 2; i++ { // copying again is safe
		if err := storage.CopyObject(ctx, bucket, source, bucket, destination); err != nil {
			t.Fatalf("copy %d: %v", i+1, err)
		}
	}
	body, err := storage.OpenObject(ctx, bucket, destination)
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	got, _ := checksum.Sum(ctx, body)
	want := sha256.Sum256(data)
	if got != hex.EncodeToString(want[:]) {
		t.Fatal("the copy's bytes differ from the source")
	}

	missing := fmt.Sprintf("quarantine/integration-test/missing-%d", run)
	if err := storage.CopyObject(ctx, bucket, missing, bucket, destination+"-x"); !errors.Is(err, interfaces.ErrObjectNotFound) {
		t.Fatalf("copying a missing source must report ErrObjectNotFound, got %v", err)
	}
}
