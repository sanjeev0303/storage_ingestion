package integration

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
)

// Runs against the dedicated private test bucket (TEST_S3_BUCKET).
func TestProviderPresignedDownload(t *testing.T) {
	storage, bucket := openTestProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	key := fmt.Sprintf("assets/integration-test/download-%d", time.Now().UnixNano())
	t.Cleanup(func() { _ = storage.DeleteObject(context.Background(), bucket, key) })

	data := []byte("0123456789abcdefghij")
	uploadID, err := storage.BeginMultipart(ctx, bucket, key, "application/pdf")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	partURL, err := storage.PresignPart(ctx, bucket, key, uploadID, 1, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	etag := putPart(t, partURL, data)
	if err := storage.CompleteMultipart(ctx, bucket, key, uploadID, []interfaces.CompletedPart{{PartNumber: 1, ETag: etag}}); err != nil {
		t.Fatalf("complete: %v", err)
	}

	signed, err := storage.PresignGet(ctx, bucket, key, time.Minute, interfaces.DownloadOptions{
		ContentDisposition: `attachment; filename="report.pdf"`, ContentType: "application/pdf",
	})
	if err != nil {
		t.Fatal(err)
	}

	get := func(rawURL string, rangeHeader string) (*http.Response, string) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if rangeHeader != "" {
			req.Header.Set("Range", rangeHeader)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp, string(body)
	}

	resp, body := get(signed, "")
	if resp.StatusCode != http.StatusOK || body != string(data) ||
		resp.Header.Get("Content-Disposition") != `attachment; filename="report.pdf"` {
		t.Fatalf("presigned GET: %d %q %v", resp.StatusCode, body, resp.Header.Get("Content-Disposition"))
	}

	resp, body = get(signed, "bytes=5-9")
	if resp.StatusCode != http.StatusPartialContent || body != "56789" || resp.Header.Get("Content-Range") != "bytes 5-9/20" {
		t.Fatalf("range: %d %q %s", resp.StatusCode, body, resp.Header.Get("Content-Range"))
	}

	// The object is private: the same URL without a signature is refused.
	unsigned, _ := url.Parse(signed)
	unsigned.RawQuery = ""
	if resp, _ := get(unsigned.String(), ""); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("unsigned access must be denied, got %d", resp.StatusCode)
	}

	// The signature covers the key: pointing it at another object fails.
	other := strings.Replace(signed, "download-", "other-", 1)
	if resp, _ := get(other, ""); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a signature for one key must not open another, got %d", resp.StatusCode)
	}

	short, err := storage.PresignGet(ctx, bucket, key, time.Second, interfaces.DownloadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Second)
	if resp, _ := get(short, ""); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("an expired URL must be denied, got %d", resp.StatusCode)
	}
}
