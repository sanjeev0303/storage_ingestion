package s3

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/storage/rangeread"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
)

const packBody = "aaaaaaaaaabbbbbbbbbbcccccccccc"

// A server standing in for an S3-compatible provider. answer decides what it
// sends back for a GET with a Range header.
func rangeServer(t *testing.T, answer func(w http.ResponseWriter)) *ObjectStorage {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "bytes=10-19" {
			t.Errorf("Range sent: %q", r.Header.Get("Range"))
		}
		answer(w)
	}))
	t.Cleanup(srv.Close)
	client := s3.New(s3.Options{
		BaseEndpoint: aws.String(srv.URL),
		Region:       "us-east-1",
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider("key", "secret", ""),
	})
	return NewObjectStorage(client)
}

func readRange(s *ObjectStorage) ([]byte, error) {
	body, err := s.OpenRange(context.Background(), "b", "pack", 10, 19, interfaces.ReadCondition{})
	if err != nil {
		return nil, err
	}
	defer body.Close()
	return io.ReadAll(body)
}

func TestOpenRangeReturnsTheRange(t *testing.T) {
	s := rangeServer(t, func(w http.ResponseWriter) {
		w.Header().Set("Content-Range", "bytes 10-19/30")
		w.WriteHeader(http.StatusPartialContent)
		io.WriteString(w, packBody[10:20])
	})
	got, err := readRange(s)
	if err != nil || string(got) != "bbbbbbbbbb" {
		t.Fatalf("%q, %v", got, err)
	}
}

func TestOpenRangeRefusesAProviderThatIgnoresRange(t *testing.T) {
	s := rangeServer(t, func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, packBody)
	})
	got, err := readRange(s)
	if !errors.Is(err, rangeread.ErrRangeNotHonoured) {
		t.Fatalf("got %q, %v; want ErrRangeNotHonoured, never the neighbours' bytes", got, err)
	}
}

func TestOpenRangeRefusesABodyLongerThanItsContentRange(t *testing.T) {
	s := rangeServer(t, func(w http.ResponseWriter) {
		w.Header().Set("Content-Range", "bytes 10-19/30")
		w.WriteHeader(http.StatusPartialContent)
		io.WriteString(w, packBody[10:])
	})
	got, err := readRange(s)
	if !errors.Is(err, rangeread.ErrRangeNotHonoured) || len(got) > 10 {
		t.Fatalf("got %q, %v; want ErrRangeNotHonoured", got, err)
	}
}
