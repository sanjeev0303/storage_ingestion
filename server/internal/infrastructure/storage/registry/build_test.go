package registry

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/netguard"
)

var accessKeys = model.StorageSecrets{AccessKeyID: "AKIDTEST", SecretAccessKey: "test-secret"}

func TestR2SignsWithRegionAutoAtTheAccountEndpoint(t *testing.T) {
	b := NewBuilder(netguard.DefaultPolicy(), 16<<20)
	c := model.StorageConnection{ID: "sc_r2", TenantID: "t1", Provider: model.ProviderR2, AuthMethod: model.AuthAccessKey,
		Bucket: "media", AccountID: "0123abcd", Region: "us-east-1"}
	p, _, err := b.Build(context.Background(), c, accessKeys)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := p.PresignGet(context.Background(), "media", "published/t1/a", time.Minute, interfaces.DownloadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(signed)
	if u.Host != "0123abcd.r2.cloudflarestorage.com" || !strings.HasPrefix(u.Path, "/media/") {
		t.Fatalf("R2 is addressed at the account endpoint, path style: %s", signed)
	}
	if credential := u.Query().Get("X-Amz-Credential"); !strings.Contains(credential, "/auto/s3/aws4_request") {
		t.Fatalf("R2 signs with region auto, never an AWS region: %s", credential)
	}
	if strings.Contains(signed, accessKeys.SecretAccessKey) {
		t.Fatal("the secret never appears in a URL")
	}
}

func TestAWSSignsInTheConnectionsRegion(t *testing.T) {
	b := NewBuilder(netguard.DefaultPolicy(), 16<<20)
	c := model.StorageConnection{ID: "sc_aws", TenantID: "t1", Provider: model.ProviderAWSS3, AuthMethod: model.AuthAccessKey,
		Bucket: "media-bucket", Region: "eu-west-2"}
	p, _, err := b.Build(context.Background(), c, accessKeys)
	if err != nil {
		t.Fatal(err)
	}
	signed, _ := p.PresignGet(context.Background(), "media-bucket", "k", time.Minute, interfaces.DownloadOptions{})
	u, _ := url.Parse(signed)
	if !netguard.HostIn(u.Hostname(), "amazonaws.com") || !strings.Contains(u.Query().Get("X-Amz-Credential"), "/eu-west-2/s3/") {
		t.Fatalf("AWS: %s", signed)
	}
}

func TestBuildRefusesWhatTheConnectionDoesNotAllow(t *testing.T) {
	b := NewBuilder(netguard.DefaultPolicy(), 16<<20)
	ctx := context.Background()
	local := model.StorageConnection{ID: "sc", TenantID: "t1", Provider: model.ProviderS3Compatible, AuthMethod: model.AuthAccessKey,
		Bucket: "b", Endpoint: "http://127.0.0.1:9000"}
	if _, _, err := b.Build(ctx, local, accessKeys); !errors.Is(err, model.ErrEndpointRejected) {
		t.Fatalf("the production policy refuses a local endpoint: %v", err)
	}
	wrongSecret := model.StorageSecrets{AccountKey: "x"}
	if _, _, err := b.Build(ctx, local, wrongSecret); !errors.Is(err, model.ErrInvalidConnection) {
		t.Fatalf("a credential of another kind is not reinterpreted: %v", err)
	}
	platform := model.StorageConnection{Provider: model.ProviderPlatform, AuthMethod: model.AuthPlatform}
	if _, _, err := b.Build(ctx, platform, model.StorageSecrets{}); !errors.Is(err, model.ErrUnsupportedProvider) {
		t.Fatalf("the platform is not built from a tenant row: %v", err)
	}
}

// A name that passes validation but resolves to a private address is
// refused when the connection is made, not only when it is saved.
func TestPrivateAddressIsRefusedAtConnectTime(t *testing.T) {
	b := NewBuilder(netguard.DefaultPolicy(), 16<<20)
	c := model.StorageConnection{ID: "sc", TenantID: "t1", Provider: model.ProviderS3Compatible, AuthMethod: model.AuthAccessKey,
		Bucket: "b", Endpoint: "https://localhost:9443", ForcePathStyle: true}
	p, _, err := b.Build(context.Background(), c, accessKeys)
	if err != nil {
		t.Fatalf("a public-looking name passes the static check: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err = p.HeadObject(ctx, "b", "k")
	if !errors.Is(err, netguard.ErrRejected) {
		t.Fatalf("the dial to a loopback address must be refused: %v", err)
	}
}

// The registry closes a cached client when credentials rotate or the cache
// evicts it, while requests that already hold the client may still use it.
// The GCS SDK documents that uses after Close may panic; with its HTTP
// transport (v1.68) they do not. This guards that against an upgrade.
func TestAGCSClientIsUsableAfterTheCacheLetsItGo(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":404,"message":"No such object"}}`))
	}))
	defer stub.Close()
	policy := netguard.DefaultPolicy()
	policy.AllowPrivate = true
	c := model.StorageConnection{ID: "sc_gcs", TenantID: "t1", Provider: model.ProviderGCS, AuthMethod: model.AuthServiceAccountKey,
		Bucket: "b", Endpoint: stub.URL}
	// An http endpoint is an emulator: the key is parsed, never used to sign in.
	key := `{"type":"service_account","client_email":"test@example.test","private_key":"unused"}`
	p, closeClient, err := NewBuilder(policy, 16<<20).Build(context.Background(), c, model.StorageSecrets{ServiceAccountJSON: key})
	if err != nil {
		t.Fatal(err)
	}
	closeClient()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("a request holding the client panicked after the cache closed it: %v", r)
		}
	}()
	if _, err := p.HeadObject(context.Background(), "b", "missing"); !errors.Is(err, interfaces.ErrObjectNotFound) {
		t.Fatalf("head after close: %v", err)
	}
}

func minio(t *testing.T) (interfaces.StorageProvider, string) {
	t.Helper()
	endpoint := os.Getenv("TEST_S3_COMPATIBLE_ENDPOINT")
	if endpoint == "" {
		t.Skip("TEST_S3_COMPATIBLE_ENDPOINT is not set (the compose MinIO: http://127.0.0.1:9000)")
	}
	bucket := os.Getenv("TEST_S3_COMPATIBLE_BUCKET")
	if bucket == "" {
		bucket = "object-storage-it"
	}
	policy := netguard.DefaultPolicy()
	policy.AllowPrivate = true
	b := NewBuilder(policy, 5<<20)
	c := model.StorageConnection{ID: "sc_minio", TenantID: "t1", Provider: model.ProviderS3Compatible, AuthMethod: model.AuthAccessKey,
		Bucket: bucket, Endpoint: endpoint, ForcePathStyle: true}
	p, _, err := b.Build(context.Background(), c, model.StorageSecrets{
		AccessKeyID: os.Getenv("TEST_S3_COMPATIBLE_ACCESS_KEY"), SecretAccessKey: os.Getenv("TEST_S3_COMPATIBLE_SECRET_KEY"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return p, bucket
}

func TestMinIOAsAnS3CompatibleConnection(t *testing.T) {
	p, bucket := minio(t)
	ctx := context.Background()
	prefix := "byos-it/" + time.Now().Format("150405.000000") + "/"

	// Streaming write in 5 MiB parts: 11 MiB is three parts, one in memory.
	data := bytes.Repeat([]byte("m"), 11<<20)
	meta, err := p.WriteObject(ctx, bucket, prefix+"big", bytes.NewReader(data), int64(len(data)), "application/octet-stream")
	if err != nil || meta.SizeBytes != int64(len(data)) {
		t.Fatalf("streaming write: %+v %v", meta, err)
	}
	// A source that ends early fails instead of storing a truncated object.
	if _, err := p.WriteObject(ctx, bucket, prefix+"short", bytes.NewReader(data[:7<<20]), int64(len(data)), "application/octet-stream"); err == nil {
		t.Fatal("a short source must not complete the object")
	}
	if _, err := p.HeadObject(ctx, bucket, prefix+"short"); !errors.Is(err, interfaces.ErrObjectNotFound) {
		t.Fatalf("no truncated object is left behind: %v", err)
	}
	r, err := p.OpenRange(ctx, bucket, prefix+"big", 5<<20, 5<<20+3, interfaces.ReadCondition{IfMatch: meta.ETag})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(r)
	r.Close()
	if string(got) != "mmmm" {
		t.Fatalf("range across a part boundary: %q", got)
	}

	plan, err := p.CreateUploadPlan(ctx, interfaces.UploadTarget{Bucket: bucket, ObjectKey: prefix + "signed", ContentType: "text/plain",
		ExpiresAt: time.Now().Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPut, plan.UploadURL, strings.NewReader("presigned"))
	req.Header.Set("Content-Type", "text/plain")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("presigned PUT: %v %v", resp, err)
	}
	resp.Body.Close()

	id, err := p.BeginMultipart(ctx, bucket, prefix+"multi", "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	partURL, err := p.PresignPart(ctx, bucket, prefix+"multi", id, 1, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	req, _ = http.NewRequest(http.MethodPut, partURL, strings.NewReader("only part"))
	resp, err = http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("part PUT: %v %v", resp, err)
	}
	etag := resp.Header.Get("ETag")
	resp.Body.Close()
	parts, err := p.ListParts(ctx, bucket, prefix+"multi", id)
	if err != nil || len(parts) != 1 || parts[0].ETag != etag {
		t.Fatalf("the provider confirms the part: %+v %v", parts, err)
	}
	if err := p.CompleteMultipart(ctx, bucket, prefix+"multi", id, []interfaces.CompletedPart{{PartNumber: 1, ETag: etag}}); err != nil {
		t.Fatal(err)
	}

	page, err := p.ListObjectsPage(ctx, bucket, prefix, "", 10)
	if err != nil || len(page.Objects) != 3 {
		t.Fatalf("listing: %+v %v", page.Objects, err)
	}
	caps, err := p.ProviderCapabilities(ctx, bucket)
	if err != nil || !caps.Multipart || caps.VersioningStatus == "" && caps.ProviderVersioning {
		t.Fatalf("capabilities: %+v %v", caps, err)
	}
	if _, err := p.ProviderCapabilities(ctx, "no-such-bucket-byos"); err == nil {
		t.Fatal("a missing bucket fails the capability check")
	}
	for _, o := range page.Objects {
		if err := p.DeleteObject(ctx, bucket, o.ObjectKey); err != nil {
			t.Fatal(err)
		}
	}
}
