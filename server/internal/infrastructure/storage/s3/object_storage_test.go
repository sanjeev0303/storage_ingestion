package s3

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aws/smithy-go"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/retry"
)

func TestToSDKCompletedParts(t *testing.T) {
	tests := []struct {
		name    string
		parts   []interfaces.CompletedPart
		wantErr bool
	}{
		{
			name: "valid ordered parts",
			parts: []interfaces.CompletedPart{
				{PartNumber: 1, ETag: `"etag-1"`},
				{PartNumber: 2, ETag: `"etag-2"`},
			},
		},
		{
			name:    "empty parts",
			parts:   nil,
			wantErr: true,
		},
		{
			name: "duplicate part number",
			parts: []interfaces.CompletedPart{
				{PartNumber: 1, ETag: `"etag-1"`},
				{PartNumber: 1, ETag: `"etag-1-retry"`},
			},
			wantErr: true,
		},
		{
			name: "out of order parts",
			parts: []interfaces.CompletedPart{
				{PartNumber: 2, ETag: `"etag-2"`},
				{PartNumber: 1, ETag: `"etag-1"`},
			},
			wantErr: true,
		},
		{
			name: "missing ETag",
			parts: []interfaces.CompletedPart{
				{PartNumber: 1, ETag: " "},
			},
			wantErr: true,
		},
		{
			name: "invalid part number",
			parts: []interfaces.CompletedPart{
				{PartNumber: 10001, ETag: `"etag"`},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := toSDKCompletedParts(tt.parts)

			if tt.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if len(got) != len(tt.parts) {
				t.Fatalf("got %d parts, want %d", len(got), len(tt.parts))
			}

			for i, part := range got {
				if part.PartNumber == nil || *part.PartNumber != tt.parts[i].PartNumber {
					t.Errorf("part %d has incorrect part number", i)
				}
				if part.ETag == nil || *part.ETag != tt.parts[i].ETag {
					t.Errorf("part %d has incorrect ETag", i)
				}
			}
		})
	}
}

func TestClassifyProviderErrors(t *testing.T) {
	apiErr := func(code string) error { return &smithy.GenericAPIError{Code: code, Message: "m"} }

	if err := classify("head object", apiErr("NotFound")); !errors.Is(err, interfaces.ErrObjectNotFound) {
		t.Errorf("NotFound: %v", err)
	}
	if err := classify("get object", apiErr("NoSuchKey")); !errors.Is(err, interfaces.ErrObjectNotFound) {
		t.Errorf("NoSuchKey: %v", err)
	}
	if err := classify("abort", apiErr("NoSuchUpload")); !errors.Is(err, interfaces.ErrStorageUploadNotFound) {
		t.Errorf("NoSuchUpload: %v", err)
	}
	if err := classify("begin", apiErr("AccessDenied")); !retry.IsPermanent(err) {
		t.Errorf("AccessDenied must be permanent: %v", err)
	}

	// Throttling, server errors and timeouts have an unknown outcome:
	// neither "absent" nor permanent, so they are reconciled and retried.
	for _, err := range []error{apiErr("SlowDown"), apiErr("InternalError"), context.DeadlineExceeded} {
		got := classify("complete", err)
		if retry.IsPermanent(got) || errors.Is(got, interfaces.ErrObjectNotFound) || errors.Is(got, interfaces.ErrStorageUploadNotFound) {
			t.Errorf("%v must stay ambiguous, got %v", err, got)
		}
	}
}

// Presigning is local computation, so this runs without a provider.
func TestPresignGetSignsKeyExpiryAndResponseHeaders(t *testing.T) {
	client, err := NewClient(context.Background(), Config{
		Region: "ap-southeast-1", Endpoint: "http://127.0.0.1:9000", ForcePathStyle: true,
		AccessKeyID: "test-access-key", SecretAccessKey: "test-secret-key",
	})
	if err != nil {
		t.Fatal(err)
	}
	storage := NewObjectStorage(client)

	raw, err := storage.PresignGet(context.Background(), "private-bucket", "assets/tenant-1/a1", 5*time.Minute,
		interfaces.DownloadOptions{ContentDisposition: `attachment; filename="report.pdf"`, ContentType: "application/pdf"})
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if u.Path != "/private-bucket/assets/tenant-1/a1" || q.Get("X-Amz-Expires") != "300" || q.Get("X-Amz-Signature") == "" {
		t.Fatalf("unexpected presigned URL %s", u.Redacted())
	}
	if q.Get("response-content-disposition") != `attachment; filename="report.pdf"` || q.Get("response-content-type") != "application/pdf" {
		t.Fatal("response headers must be signed into the URL")
	}
	if strings.Contains(raw, "test-secret-key") {
		t.Fatal("the secret must never appear in a URL")
	}

	for _, ttl := range []time.Duration{0, 8 * 24 * time.Hour} {
		if _, err := storage.PresignGet(context.Background(), "b", "k", ttl, interfaces.DownloadOptions{}); err == nil {
			t.Fatalf("TTL %s must be refused", ttl)
		}
	}
}
