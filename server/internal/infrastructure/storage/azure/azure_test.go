package azure

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	containerpkg "github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/sas"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/netguard"
)

func TestBlockIDsAreFixedLengthAndDecodeToTheirPart(t *testing.T) {
	id := newUploadID()
	if len(blockID(id, 1)) != len(blockID(id, 50000)) {
		t.Fatal("every block ID of a blob must have the same length")
	}
	for _, n := range []int32{1, 42, 50000} {
		if got, ok := partOf(id, blockID(id, n)); !ok || got != n {
			t.Fatalf("part %d decoded as %d %v", n, got, ok)
		}
	}
	if _, ok := partOf(newUploadID(), blockID(id, 1)); ok {
		t.Fatal("another upload's block is not this upload's part")
	}
	if _, ok := partOf(id, "not base64!"); ok {
		t.Fatal("a foreign block ID is ignored")
	}
}

func TestExactReaderRefusesAShortSource(t *testing.T) {
	r := &exactReader{r: strings.NewReader("abc"), left: 5}
	if _, err := io.ReadAll(r); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("a short source must fail the write: %v", err)
	}
	r = &exactReader{r: strings.NewReader("abcdef"), left: 3}
	if got, err := io.ReadAll(r); err != nil || string(got) != "abc" {
		t.Fatalf("reads exactly size bytes: %q %v", got, err)
	}
}

func TestErrorsNeverCarryTheSASToken(t *testing.T) {
	err := classify("get object", &url.Error{Op: "Get", URL: "https://acct.blob.core.windows.net/c/k?sv=2024&sig=SECRETSIG", Err: errors.New("connection refused")})
	if strings.Contains(err.Error(), "SECRETSIG") || strings.Contains(err.Error(), "sig=") {
		t.Fatalf("the request URL leaked: %v", err)
	}
}

func TestNewNeedsExactlyOneCredential(t *testing.T) {
	client := netguard.DefaultPolicy().Client()
	if _, err := New(Config{AccountName: "acct", HTTPClient: client}); !errors.Is(err, model.ErrCredentialMissing) {
		t.Fatalf("no credential: %v", err)
	}
	if _, err := New(Config{AccountName: "acct", AccountKey: "not base64!", HTTPClient: client}); !errors.Is(err, model.ErrInvalidConnection) {
		t.Fatalf("a malformed key: %v", err)
	}
	s, err := New(Config{AccountName: "acct", SASToken: "sv=2024&sig=x", HTTPClient: client})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateUploadPlan(context.Background(), interfaces.UploadTarget{Bucket: "c", ObjectKey: "k", ExpiresAt: time.Now().Add(time.Minute)}); !errors.Is(err, model.ErrUnsupportedProvider) {
		t.Fatalf("a SAS-token connection cannot sign client URLs: %v", err)
	}
}

// ─── Against Azurite (TEST_AZURITE_ENDPOINT and TEST_AZURITE_KEY) ─────

func azurite(t *testing.T) (*Storage, string) {
	t.Helper()
	endpoint, key := os.Getenv("TEST_AZURITE_ENDPOINT"), os.Getenv("TEST_AZURITE_KEY")
	if endpoint == "" || key == "" {
		t.Skip("TEST_AZURITE_ENDPOINT/TEST_AZURITE_KEY are not set (docker compose --profile emulators up -d azurite)")
	}
	policy := netguard.DefaultPolicy()
	policy.AllowPrivate = true
	s, err := New(Config{AccountName: "devstoreaccount1", AccountKey: key, Endpoint: endpoint + "/devstoreaccount1",
		HTTPClient: policy.Client(), PartBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	container := fmt.Sprintf("byos-%d", time.Now().UnixNano())
	if _, err := s.container(container).Create(context.Background(), nil); err != nil {
		t.Fatalf("create container: %v", err)
	}
	return s, container
}

func put(t *testing.T, rawURL string, headers map[string]string, body []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPut, rawURL, bytes.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT answered %d", resp.StatusCode)
	}
}

func TestAzuriteSignedUploadsAndReads(t *testing.T) {
	s, container := azurite(t)
	ctx := context.Background()

	plan, err := s.CreateUploadPlan(ctx, interfaces.UploadTarget{Bucket: container, ObjectKey: "staging/t1/u1",
		ContentType: "text/plain", ExpiresAt: time.Now().Add(5 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Headers["x-ms-blob-type"] != "BlockBlob" {
		t.Fatalf("the client is told to send x-ms-blob-type: %+v", plan.Headers)
	}
	put(t, plan.UploadURL, plan.Headers, []byte("signed upload"))

	meta, err := s.HeadObject(ctx, container, "staging/t1/u1")
	if err != nil || meta.SizeBytes != int64(len("signed upload")) || meta.ETag == "" {
		t.Fatalf("head: %+v %v", meta, err)
	}
	r, err := s.OpenRange(ctx, container, "staging/t1/u1", 7, 12, interfaces.ReadCondition{IfMatch: meta.ETag})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(r)
	r.Close()
	if string(got) != "upload" {
		t.Fatalf("range %q", got)
	}
	if _, err := s.OpenRange(ctx, container, "staging/t1/u1", 0, -1, interfaces.ReadCondition{IfMatch: `"0x8DEADBEEF"`}); !errors.Is(err, interfaces.ErrPreconditionFailed) {
		t.Fatalf("a stale validator is refused: %v", err)
	}

	link, err := s.PresignGet(ctx, container, "staging/t1/u1", time.Minute, interfaces.DownloadOptions{ContentDisposition: `attachment; filename="u.txt"`})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(link)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "signed upload" || !strings.Contains(resp.Header.Get("Content-Disposition"), "u.txt") {
		t.Fatalf("signed download: %d %q %q", resp.StatusCode, body, resp.Header.Get("Content-Disposition"))
	}

	if err := s.CopyObject(ctx, container, "staging/t1/u1", container, "published/t1/a1"); err != nil {
		t.Fatal(err)
	}
	if meta, err := s.HeadObject(ctx, container, "published/t1/a1"); err != nil || meta.SizeBytes != int64(len("signed upload")) {
		t.Fatalf("server-side copy: %+v %v", meta, err)
	}
	if err := s.DeleteObject(ctx, container, "published/t1/a1"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteObject(ctx, container, "published/t1/a1"); err != nil {
		t.Fatalf("deleting a missing blob is not an error: %v", err)
	}
	if _, err := s.HeadObject(ctx, container, "published/t1/a1"); !errors.Is(err, interfaces.ErrObjectNotFound) {
		t.Fatalf("deleted: %v", err)
	}
}

// With account versioning on, a deleted blob stays as a previous version:
// DeleteObject removes every version of exactly that name, and leaves a
// name it only prefixes alone.
func TestAzuriteDeleteRemovesEveryVersion(t *testing.T) {
	s, container := azurite(t)
	ctx := context.Background()
	versions := func(name string) int {
		pager := s.container(container).NewListBlobsFlatPager(&containerpkg.ListBlobsFlatOptions{
			Prefix: &name, Include: containerpkg.ListBlobsInclude{Versions: true},
		})
		n := 0
		for pager.More() {
			page, err := pager.NextPage(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, item := range page.Segment.BlobItems {
				if *item.Name == name && item.VersionID != nil {
					n++
				}
			}
		}
		return n
	}
	for _, body := range []string{"one", "two"} {
		if _, err := s.WriteObject(ctx, container, "published/t1/a1", strings.NewReader(body), 3, "text/plain"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.WriteObject(ctx, container, "published/t1/a10", strings.NewReader("other"), 5, "text/plain"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteObject(ctx, container, "published/t1/a1"); err != nil {
		t.Fatalf("the versions listing must work even without versioning: %v", err)
	}
	if _, err := s.HeadObject(ctx, container, "published/t1/a1"); !errors.Is(err, interfaces.ErrObjectNotFound) {
		t.Fatalf("deleted: %v", err)
	}
	if _, err := s.HeadObject(ctx, container, "published/t1/a10"); err != nil {
		t.Fatalf("a name sharing the prefix was touched: %v", err)
	}
	if versions("published/t1/a10") == 0 {
		t.Skip("unverified: Azurite keeps no blob versions, so removing them is not exercised")
	}
	if n := versions("published/t1/a1"); n != 0 {
		t.Fatalf("%d versions remain", n)
	}
}

func TestAzuriteStreamingWriteUsesBoundedBlocks(t *testing.T) {
	s, container := azurite(t)
	ctx := context.Background()
	data := bytes.Repeat([]byte("z"), 2<<20+12345) // three 1 MiB blocks
	meta, err := s.WriteObject(ctx, container, "published/t1/big", bytes.NewReader(data), int64(len(data)), "application/octet-stream")
	if err != nil || meta.SizeBytes != int64(len(data)) {
		t.Fatalf("write: %+v %v", meta, err)
	}
	r, _ := s.OpenObject(ctx, container, "published/t1/big")
	got, _ := io.ReadAll(r)
	r.Close()
	if !bytes.Equal(got, data) {
		t.Fatal("the streamed blob differs")
	}
	if _, err := s.WriteObject(ctx, container, "published/t1/short", strings.NewReader("abc"), 100, "text/plain"); err == nil {
		t.Fatal("a short source must not commit a blob")
	}
	if _, err := s.HeadObject(ctx, container, "published/t1/short"); !errors.Is(err, interfaces.ErrObjectNotFound) {
		t.Fatalf("nothing committed for a short write: %v", err)
	}
}

func TestAzuriteBlockUploads(t *testing.T) {
	s, container := azurite(t)
	ctx := context.Background()
	key := "staging/t1/multi"

	id, err := s.BeginMultipart(ctx, container, key, "application/zip")
	if err != nil {
		t.Fatal(err)
	}
	var want bytes.Buffer
	for n := int32(1); n <= 3; n++ {
		part := bytes.Repeat([]byte{byte('0' + n)}, 1000*int(n))
		want.Write(part)
		u, err := s.PresignPart(ctx, container, key, id, n, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		put(t, u, nil, part)
	}
	parts, err := s.ListParts(ctx, container, key, id)
	if err != nil || len(parts) != 3 || parts[2].SizeBytes != 3000 || parts[0].PartNumber != 1 {
		t.Fatalf("staged blocks are this upload's parts: %+v %v", parts, err)
	}
	listing, _ := s.ListObjectsPage(ctx, container, "staging/", "", 100)
	if len(listing.Objects) != 0 {
		t.Fatalf("the marker is not an object: %+v", listing.Objects)
	}
	uploads, _ := s.ListMultipartUploadsPage(ctx, container, "staging/", "", 100)
	if len(uploads.Uploads) != 1 || uploads.Uploads[0].StorageUploadID != id || uploads.Uploads[0].ObjectKey != key {
		t.Fatalf("the upload is listed: %+v", uploads)
	}

	completed := []interfaces.CompletedPart{{PartNumber: 1}, {PartNumber: 2}, {PartNumber: 3}}
	if err := s.CompleteMultipart(ctx, container, key, id, completed); err != nil {
		t.Fatal(err)
	}
	r, _ := s.OpenObject(ctx, container, key)
	got, _ := io.ReadAll(r)
	r.Close()
	if !bytes.Equal(got, want.Bytes()) {
		t.Fatal("committed blocks in part order")
	}
	if meta, _ := s.HeadObject(ctx, container, key); meta.ContentType != "application/zip" {
		t.Fatalf("the upload's content type: %+v", meta)
	}
	if _, err := s.ListParts(ctx, container, key, id); !errors.Is(err, interfaces.ErrStorageUploadNotFound) {
		t.Fatalf("a completed upload is gone: %v", err)
	}

	// Abort before anything was committed leaves no blob behind.
	other := "staging/t1/aborted"
	oid, _ := s.BeginMultipart(ctx, container, other, "text/plain")
	u, _ := s.PresignPart(ctx, container, other, oid, 1, time.Minute)
	put(t, u, nil, []byte("never committed"))
	if err := s.AbortMultipart(ctx, container, other, oid); err != nil {
		t.Fatal(err)
	}
	if _, err := s.HeadObject(ctx, container, other); !errors.Is(err, interfaces.ErrObjectNotFound) {
		t.Fatalf("an aborted upload leaves no blob: %v", err)
	}
	if ids, _ := s.ListMultipartUploads(ctx, container, other); len(ids) != 0 {
		t.Fatalf("its marker is gone: %v", ids)
	}
}

func TestAzuriteSASTokenConnectionReadsButCannotSign(t *testing.T) {
	keyed, container := azurite(t)
	ctx := context.Background()
	if _, err := keyed.WriteObject(ctx, container, "published/t1/a", strings.NewReader("hello"), 5, "text/plain"); err != nil {
		t.Fatal(err)
	}
	// A container SAS, as a customer would hand over.
	token, err := sas.BlobSignatureValues{
		Protocol: sas.ProtocolHTTPSandHTTP, ExpiryTime: time.Now().Add(time.Hour).UTC(), ContainerName: container,
		Permissions: (&sas.ContainerPermissions{Read: true, Add: true, Create: true, Write: true, Delete: true, List: true}).String(),
	}.SignWithSharedKey(keyed.sharedKey)
	if err != nil {
		t.Fatal(err)
	}
	policy := netguard.DefaultPolicy()
	policy.AllowPrivate = true
	s, err := New(Config{AccountName: "devstoreaccount1", SASToken: token.Encode(),
		Endpoint: os.Getenv("TEST_AZURITE_ENDPOINT") + "/devstoreaccount1", HTTPClient: policy.Client()})
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.OpenObject(ctx, container, "published/t1/a")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(r)
	r.Close()
	if string(got) != "hello" {
		t.Fatalf("read with the SAS token: %q", got)
	}
	caps, err := s.ProviderCapabilities(ctx, container)
	if err != nil || caps.PresignedUpload || caps.PresignedDownload || caps.Multipart {
		t.Fatalf("a SAS-token connection signs nothing for clients: %+v %v", caps, err)
	}
	if _, err := s.PresignGet(ctx, container, "published/t1/a", time.Minute, interfaces.DownloadOptions{}); !errors.Is(err, model.ErrUnsupportedProvider) {
		t.Fatalf("no download URL from a SAS token: %v", err)
	}
	_, err = s.HeadObject(ctx, "missing-container-x", "k")
	if err == nil || strings.Contains(err.Error(), "sig=") {
		t.Fatalf("errors never carry the token: %v", err)
	}
}
