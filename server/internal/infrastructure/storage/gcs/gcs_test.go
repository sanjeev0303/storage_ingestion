package gcs

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/netguard"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/retry"
)

func TestPartNamesStayUnderTheUploadsPrefix(t *testing.T) {
	if got := partKey("staging/t1/u1", "abc", 7); got != "staging/t1/u1.parts/abc/part-00007" {
		t.Fatalf("part key %q", got)
	}
	if got := markerKey("staging/t1/u1", "abc"); got != "staging/t1/u1.parts/abc/upload" {
		t.Fatalf("marker key %q", got)
	}
	if !isMultipartArtifact(partKey("k", "u", 1)) || isMultipartArtifact("staging/t1/u1") {
		t.Fatal("listing must hide parts and markers only")
	}
	if _, err := generation("0"); err == nil {
		t.Fatal("generation 0 is not a version")
	}
	if g, err := generation("1729412345678901"); err != nil || g != 1729412345678901 {
		t.Fatalf("generation %d %v", g, err)
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		code      int
		want      error
		permanent bool
	}{
		{http.StatusNotFound, interfaces.ErrObjectNotFound, false},
		{http.StatusPreconditionFailed, interfaces.ErrPreconditionFailed, false},
		{http.StatusRequestedRangeNotSatisfiable, interfaces.ErrInvalidRange, false},
		{http.StatusTooManyRequests, interfaces.ErrStorageThrottled, false},
		{http.StatusServiceUnavailable, interfaces.ErrStorageThrottled, false},
		{http.StatusForbidden, interfaces.ErrStorageAccessDenied, true},
		{http.StatusUnauthorized, interfaces.ErrStorageCredentialsRejected, true},
	}
	for _, c := range cases {
		err := classify("op", &googleapi.Error{Code: c.code, Message: "project 12345 secret detail"})
		if !errors.Is(err, c.want) || retry.IsPermanent(err) != c.permanent {
			t.Errorf("%d: %v (permanent %v)", c.code, err, retry.IsPermanent(err))
		}
		if strings.Contains(err.Error(), "secret detail") {
			t.Errorf("%d: the provider's message must not pass through: %v", c.code, err)
		}
	}
}

func TestSignedURLsNeedAServiceAccountKey(t *testing.T) {
	s := &Storage{}
	if _, err := s.sign("b", "k", http.MethodGet, time.Now().Add(time.Minute), "", nil); !errors.Is(err, model.ErrUnsupportedProvider) {
		t.Fatalf("no key, no signed URL: %v", err)
	}
}

// A real (https) endpoint keeps the service account's credentials. Only a
// plain-http emulator is called anonymously, and only it may go without a key.
func TestAnHTTPSEndpointKeepsTheServiceAccount(t *testing.T) {
	ctx := context.Background()
	seen := make(chan string, 1)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"access_token":"test-token","token_type":"Bearer","expires_in":3600}`)
			return
		}
		select {
		case seen <- r.Header.Get("Authorization"):
		default:
		}
		http.Error(w, `{"error":{"code":404,"message":"not found"}}`, http.StatusNotFound)
	}))
	defer srv.Close()

	var key map[string]string
	if err := json.Unmarshal(serviceAccount(t), &key); err != nil {
		t.Fatal(err)
	}
	key["token_uri"] = srv.URL + "/token"
	keyJSON, _ := json.Marshal(key)

	s, err := New(ctx, Config{ServiceAccountJSON: keyJSON, HTTPClient: srv.Client(), Endpoint: srv.URL + "/storage/v1/"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.HeadObject(ctx, "b", "k"); !errors.Is(err, interfaces.ErrObjectNotFound) {
		t.Fatalf("head through the endpoint: %v", err)
	}
	if got := <-seen; got != "Bearer test-token" {
		t.Fatalf("calls to a real endpoint must carry the service account's token, got %q", got)
	}

	if _, err := New(ctx, Config{HTTPClient: srv.Client(), Endpoint: srv.URL + "/storage/v1/"}); !errors.Is(err, model.ErrCredentialMissing) {
		t.Fatalf("only an emulator may go without a key: %v", err)
	}
}

// Every part sorts before its upload's marker, so an upload with more parts
// than a page holds has its marker on a later page, and must still be found.
// fake-gcs-server cuts a listing at maxResults without a next-page token, so
// a stub pages the way GCS does.
func TestAnUploadWithManyPartsIsStillListed(t *testing.T) {
	ctx := context.Background()
	const key, id = "staging/t1/many", "u1"
	var names []string
	for n := int32(1); n <= 1001; n++ {
		names = append(names, partKey(key, id, n))
	}
	names = append(names, markerKey(key, id))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start, _ := strconv.Atoi(r.URL.Query().Get("pageToken"))
		size, _ := strconv.Atoi(r.URL.Query().Get("maxResults"))
		if size <= 0 || size > 1000 {
			size = 1000
		}
		end := min(start+size, len(names))
		var items []map[string]string
		for _, name := range names[start:end] {
			items = append(items, map[string]string{"name": name, "bucket": "b", "timeCreated": "2026-10-21T00:00:00Z"})
		}
		page := map[string]any{"kind": "storage#objects", "items": items}
		if end < len(names) {
			page["nextPageToken"] = strconv.Itoa(end)
		}
		_ = json.NewEncoder(w).Encode(page)
	}))
	defer srv.Close()

	s, err := New(ctx, Config{HTTPClient: srv.Client(), Endpoint: srv.URL + "/storage/v1/"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if ids, err := s.ListMultipartUploads(ctx, "b", key); err != nil || len(ids) != 1 || ids[0] != id {
		t.Fatalf("the marker is past the first page and must still be found: %v %v", ids, err)
	}
}

// ─── Against fake-gcs-server (TEST_GCS_ENDPOINT, opt-in) ───────────────

// serviceAccount makes a throwaway service account key. The emulator
// does not check signatures; the key only lets the adapter sign.
func serviceAccount(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	b, _ := json.Marshal(map[string]string{
		"type": "service_account", "project_id": "test-project", "private_key_id": "k1",
		"private_key":  string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		"client_email": "tester@test-project.iam.gserviceaccount.com", "client_id": "1",
		"token_uri": "https://oauth2.googleapis.com/token",
	})
	return b
}

func emulator(t *testing.T) (*Storage, string) {
	t.Helper()
	endpoint := os.Getenv("TEST_GCS_ENDPOINT")
	if endpoint == "" {
		t.Skip("TEST_GCS_ENDPOINT is not set (docker compose --profile emulators up -d fake-gcs)")
	}
	policy := netguard.DefaultPolicy()
	policy.AllowPrivate = true
	s, err := New(context.Background(), Config{
		ServiceAccountJSON: serviceAccount(t), HTTPClient: policy.Client(), Endpoint: endpoint + "/storage/v1/", PartBytes: 256 << 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	bucket := fmt.Sprintf("byos-gcs-%d", time.Now().UnixNano())
	if err := s.client.Bucket(bucket).Create(context.Background(), "test-project", nil); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	return s, bucket
}

func TestEmulatorObjectsAndGenerations(t *testing.T) {
	s, bucket := emulator(t)
	ctx := context.Background()
	data := bytes.Repeat([]byte("g"), 600<<10) // spans chunks

	meta, err := s.WriteObject(ctx, bucket, "published/t1/a1", bytes.NewReader(data), int64(len(data)), "application/pdf")
	if err != nil {
		t.Fatal(err)
	}
	if meta.SizeBytes != int64(len(data)) || meta.VersionID == "" || meta.ETag != meta.VersionID {
		t.Fatalf("the generation is the version ID and the validator: %+v", meta)
	}
	if _, err := s.WriteObject(ctx, bucket, "short", bytes.NewReader([]byte("abc")), 10, "text/plain"); err == nil {
		t.Fatal("a short source must not be stored as complete")
	}
	if _, err := s.HeadObject(ctx, bucket, "short"); !errors.Is(err, interfaces.ErrObjectNotFound) {
		t.Fatalf("nothing is left of a short write: %v", err)
	}

	head, err := s.HeadObjectVersion(ctx, bucket, "published/t1/a1", meta.VersionID)
	if err != nil || head.ContentType != "application/pdf" {
		t.Fatalf("head the exact generation: %+v %v", head, err)
	}
	r, err := s.OpenRange(ctx, bucket, "published/t1/a1", 10, 19, interfaces.ReadCondition{IfMatch: meta.ETag})
	if err != nil {
		t.Fatal(err)
	}
	part, _ := io.ReadAll(r)
	r.Close()
	if !bytes.Equal(part, data[10:20]) {
		t.Fatalf("range read %q", part)
	}

	// Overwrite: reads pinned to the old generation no longer match.
	if _, err := s.WriteObject(ctx, bucket, "published/t1/a1", bytes.NewReader([]byte("new")), 3, "text/plain"); err != nil {
		t.Fatal(err)
	}
	if r, err := s.OpenRange(ctx, bucket, "published/t1/a1", 0, -1, interfaces.ReadCondition{IfMatch: meta.ETag}); err == nil {
		got, _ := io.ReadAll(r)
		r.Close()
		if bytes.Equal(got, []byte("new")) {
			t.Fatal("a read pinned to the old generation must not return the new bytes")
		}
	}

	if err := s.CopyObject(ctx, bucket, "published/t1/a1", bucket, "copy/t1/a1"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteObject(ctx, bucket, "copy/t1/a1"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteObject(ctx, bucket, "copy/t1/a1"); err != nil {
		t.Fatalf("deleting a missing object is not an error: %v", err)
	}
	caps, err := s.ProviderCapabilities(ctx, bucket)
	if err != nil || caps.MultipartKind != "GCS_COMPOSE" || caps.VersionIDKind != "GCS_GENERATION" || !caps.PresignedUpload {
		t.Fatalf("capabilities: %+v %v", caps, err)
	}
	if _, err := s.ProviderCapabilities(ctx, "no-such-bucket-here"); !errors.Is(err, interfaces.ErrObjectNotFound) {
		t.Fatalf("a missing bucket: %v", err)
	}

	url, err := s.PresignGet(ctx, bucket, "published/t1/a1", time.Minute, interfaces.DownloadOptions{
		ContentDisposition: `attachment; filename="a.pdf"`, VersionID: meta.VersionID,
	})
	if err != nil || !strings.Contains(url, "X-Goog-Signature=") || !strings.Contains(url, "generation="+meta.VersionID) ||
		!strings.Contains(url, "response-content-disposition=") {
		t.Fatalf("signed download URL %s %v", url, err)
	}
}

// With versioning on, a deleted object stays as a noncurrent generation:
// DeleteObject removes every generation of exactly that name, and leaves a
// name it only prefixes alone.
func TestEmulatorDeleteRemovesEveryGeneration(t *testing.T) {
	s, _ := emulator(t)
	ctx := context.Background()
	bucket := fmt.Sprintf("byos-gcs-v-%d", time.Now().UnixNano())
	// The client retries what the emulator refuses; bound it.
	createCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := s.client.Bucket(bucket).Create(createCtx, "test-project", &storage.BucketAttrs{VersioningEnabled: true}); err != nil {
		t.Skipf("unverified: this fake-gcs-server does not create a versioned bucket: %v", err)
	}
	generations := func(name string) int {
		it := s.client.Bucket(bucket).Objects(ctx, &storage.Query{Prefix: name, Versions: true})
		n := 0
		for {
			a, err := it.Next()
			if errors.Is(err, iterator.Done) {
				return n
			}
			if err != nil {
				t.Fatal(err)
			}
			if a.Name == name {
				n++
			}
		}
	}
	for _, body := range []string{"one", "two"} {
		if _, err := s.WriteObject(ctx, bucket, "published/t1/a1", strings.NewReader(body), 3, "text/plain"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.WriteObject(ctx, bucket, "published/t1/a10", strings.NewReader("other"), 5, "text/plain"); err != nil {
		t.Fatal(err)
	}
	if generations("published/t1/a1") != 2 {
		t.Skipf("unverified: this fake-gcs-server keeps %d generations of an overwritten object", generations("published/t1/a1"))
	}
	// fake-gcs-server keeps the generations of a deleted object but cannot
	// delete them by generation once no live object has the name (404);
	// GCS can.
	probe := s.object(bucket, "probe")
	first := probe.NewWriter(ctx)
	_, _ = first.Write([]byte("1"))
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := probe.Delete(ctx); err != nil {
		t.Fatal(err)
	}
	if err := probe.Generation(first.Attrs().Generation).Delete(ctx); err != nil {
		t.Skipf("unverified: this fake-gcs-server cannot delete a noncurrent generation: %v", err)
	}

	if err := s.DeleteObject(ctx, bucket, "published/t1/a1"); err != nil {
		t.Fatal(err)
	}
	if n := generations("published/t1/a1"); n != 0 {
		t.Fatalf("%d generations remain", n)
	}
	if generations("published/t1/a10") != 1 {
		t.Fatal("a name sharing the prefix was touched")
	}
}

func TestEmulatorCompositeMultipartUpload(t *testing.T) {
	s, bucket := emulator(t)
	ctx := context.Background()
	key := "staging/t1/upload-1"

	id, err := s.BeginMultipart(ctx, bucket, key, "application/octet-stream")
	if err != nil {
		t.Fatal(err)
	}
	// 33 parts forces a second level of composition (32 sources each).
	var want bytes.Buffer
	var completed []interfaces.CompletedPart
	for n := int32(1); n <= 33; n++ {
		part := bytes.Repeat([]byte{byte('a' + n%26)}, 1000+int(n))
		want.Write(part)
		w := s.object(bucket, partKey(key, id, n)).NewWriter(ctx)
		if _, err := w.Write(part); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		sum := md5.Sum(part)
		completed = append(completed, interfaces.CompletedPart{PartNumber: n, ETag: `"` + hex.EncodeToString(sum[:]) + `"`})
	}

	parts, err := s.ListParts(ctx, bucket, key, id)
	if err != nil || len(parts) != 33 || parts[0].ETag != completed[0].ETag || parts[32].PartNumber != 33 {
		t.Fatalf("parts are listed in order with their MD5 validators: %d %v", len(parts), err)
	}
	listing, err := s.ListObjectsPage(ctx, bucket, "staging/", "", 1000)
	if err != nil || len(listing.Objects) != 0 {
		t.Fatalf("parts and the marker are not objects: %+v %v", listing.Objects, err)
	}
	uploads, err := s.ListMultipartUploadsPage(ctx, bucket, "staging/", "", 1000)
	if err != nil || len(uploads.Uploads) != 1 || uploads.Uploads[0].StorageUploadID != id || uploads.Uploads[0].ObjectKey != key {
		t.Fatalf("the upload is listed: %+v %v", uploads, err)
	}
	if ids, err := s.ListMultipartUploads(ctx, bucket, key); err != nil || len(ids) != 1 {
		t.Fatalf("uploads for the exact key: %v %v", ids, err)
	}

	if err := s.CompleteMultipart(ctx, bucket, key, id, completed); err != nil {
		t.Fatal(err)
	}
	r, err := s.OpenObject(ctx, bucket, key)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(r)
	r.Close()
	if !bytes.Equal(got, want.Bytes()) {
		t.Fatalf("composed %d bytes, want %d", len(got), want.Len())
	}
	if meta, _ := s.HeadObject(ctx, bucket, key); meta.ContentType != "application/octet-stream" {
		t.Fatalf("the upload's content type is kept: %+v", meta)
	}
	if _, err := s.ListParts(ctx, bucket, key, id); !errors.Is(err, interfaces.ErrStorageUploadNotFound) {
		t.Fatalf("a completed upload is gone: %v", err)
	}
	everything, _ := s.client.Bucket(bucket).Objects(ctx, nil).Next()
	if everything != nil && isMultipartArtifact(everything.Name) {
		t.Fatalf("parts were removed after composing: %s", everything.Name)
	}

	// Abort removes parts and marker.
	other, _ := s.BeginMultipart(ctx, bucket, "staging/t1/upload-2", "text/plain")
	w := s.object(bucket, partKey("staging/t1/upload-2", other, 1)).NewWriter(ctx)
	_, _ = w.Write([]byte("x"))
	_ = w.Close()
	if err := s.AbortMultipart(ctx, bucket, "staging/t1/upload-2", other); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ListParts(ctx, bucket, "staging/t1/upload-2", other); !errors.Is(err, interfaces.ErrStorageUploadNotFound) {
		t.Fatalf("aborted: %v", err)
	}
}

func TestEmulatorSignedPartUpload(t *testing.T) {
	s, bucket := emulator(t)
	ctx := context.Background()
	key := "staging/t1/signed"
	id, err := s.BeginMultipart(ctx, bucket, key, "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	url, err := s.PresignPart(ctx, bucket, key, id, 1, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(url, "http://127.0.0.1:4443/") || !strings.Contains(url, "X-Goog-Signature=") {
		t.Fatalf("the part URL is signed for the emulator: %s", url)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPut, url, strings.NewReader("hello part"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		t.Skipf("this fake-gcs-server does not accept XML API uploads (HTTP %d); the JSON API path is covered above", resp.StatusCode)
	}
	parts, err := s.ListParts(ctx, bucket, key, id)
	if err != nil || len(parts) != 1 || parts[0].SizeBytes != int64(len("hello part")) {
		t.Fatalf("the signed upload stored the part: %+v %v", parts, err)
	}
}
