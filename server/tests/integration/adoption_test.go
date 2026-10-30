package integration

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/netguard"
	"github.com/sanjeev0303/object-storage-platform/internal/service/adoption"
	"github.com/sanjeev0303/object-storage-platform/internal/service/processing"
	uploadservice "github.com/sanjeev0303/object-storage-platform/internal/service/upload"
)

// oneConnection routes every connection ID to the test bucket, as a
// customer's own connection would be.
type oneConnection struct {
	storage interfaces.StorageProvider
	bucket  string
}

func (o oneConnection) Locate(_ context.Context, _, id string) (interfaces.StorageLocation, error) {
	return interfaces.StorageLocation{Storage: o.storage, StoragePlace: interfaces.StoragePlace{ConnectionID: id,
		StagingBucket: o.bucket, StagingPrefix: "quarantine", ReadyBucket: o.bucket, ReadyPrefix: promotionPrefix}}, nil
}

// Objects already in a bucket are checked where they are and published in
// place; URL imports go through the ordinary pipeline.
func TestLocalStackAdoption(t *testing.T) {
	stack := openContentStack(t)
	ctx := context.Background()
	db := stack.env.db
	tenant := stack.env.tenant
	conn := "conn-adopt-" + fmt.Sprint(time.Now().UnixNano())
	if _, err := db.Pool.Exec(ctx, `INSERT INTO storage_connections (id, tenant_id, name, provider, status, auth_method, bucket,
		created_by, created_at, updated_at) VALUES ($1, $2, 'byo', 'S3_COMPATIBLE', 'ACTIVE', 'ACCESS_KEY', $3, 'it', now(), now())`,
		conn, tenant, stack.bucket); err != nil {
		t.Fatal(err)
	}
	prefix := fmt.Sprintf("lab-%d/run1/", time.Now().UnixNano())
	put := func(name string, body []byte) string {
		key := prefix + name
		if _, err := stack.provider.WriteObject(ctx, stack.bucket, key, bytes.NewReader(body), int64(len(body)), "application/octet-stream"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = stack.provider.DeleteObject(context.Background(), stack.bucket, key) })
		return key
	}
	md5hex := func(b []byte) string { s := md5.Sum(b); return hex.EncodeToString(s[:]) }
	good := []byte("sample,value\nA,1\nB,2\n")
	badFastq := []byte("@r1\nACGT\n+\nII\n")
	evil := []byte("header BADSIGNATURE trailer")
	goodKey, fastqKey, evilKey, lieKey := put("good.csv", good), put("reads.fastq", badFastq), put("evil.bin", evil), put("lie.bin", []byte("x"))

	manifest := strings.Join([]string{
		fmt.Sprintf(`{"key":%q,"path":"run1/good.csv","md5":%q,"metadata":{"lab":"bio"}}`, goodKey, md5hex(good)),
		fmt.Sprintf(`{"key":%q,"md5":%q}`, fastqKey, md5hex(badFastq)),
		fmt.Sprintf(`{"key":%q,"md5":%q}`, evilKey, md5hex(evil)),
		fmt.Sprintf(`{"key":%q,"md5":"00000000000000000000000000000000"}`, lieKey),
		fmt.Sprintf(`{"key":%q}`, goodKey),
		fmt.Sprintf(`{"key":"%sassets-it/x","md5":"00000000000000000000000000000000"}`, ""),
	}, "\n")

	scanner := &signatureScanner{version: "sigs-1", bad: []byte("BADSIGNATURE")}
	store := postgres.NewAdoptionRepository(db)
	router := oneConnection{storage: stack.provider, bucket: stack.bucket}
	policy := netguard.DefaultPolicy()
	policy.AllowPrivate = true
	uploads := uploadservice.NewService(stack.env.uploads, stack.provider, stack.bucket, 1<<30, time.Hour)
	svc := adoption.NewService(adoption.Dependencies{
		Store: store, Router: router, Platform: stack.provider, PlatformBucket: stack.bucket,
		Validators: []interfaces.FileValidator{processing.BasicValidator{}, processing.TypeValidator{}, processing.ContentInspector{
			Limits:   processing.ArchiveLimits{MaxArchiveBytes: 1 << 30, MaxEntries: 1000, MaxUncompressedBytes: 1 << 30, MaxRatio: 100},
			MaxBytes: 1 << 30, SecretScanBytes: 1 << 20}},
		Scanner: scanner, Policies: postgres.NewContentPolicyRepository(db), Checks: postgres.NewContentCheckRepository(db),
		Uploads: uploads, Net: policy,
	}, adoption.Config{Owner: "it-adopt", Policy: model.ContentPolicy{MaxSizeBytes: 1 << 30, UnknownTypeAction: "allow"}},
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	run := func(id string) model.AdoptionJob {
		t.Helper()
		for i := 0; i < 10; i++ {
			if _, err := svc.RunOnce(ctx); err != nil {
				t.Fatal(err)
			}
			job, err := store.GetJob(ctx, tenant, id)
			if err != nil {
				t.Fatal(err)
			}
			if job.State == model.AdoptionCompleted || job.State == model.AdoptionFailed {
				return job
			}
		}
		t.Fatalf("job %s did not finish", id)
		return model.AdoptionJob{}
	}
	newJob := func(source, inline string) string {
		id := fmt.Sprintf("adopt-%d", time.Now().UnixNano())
		if err := store.CreateJob(ctx, model.AdoptionJob{ID: id, TenantID: tenant, ConnectionID: conn, Source: source, InlineSource: inline,
			Mode: model.AdoptionDetach, RequireDigest: true, CreatedBy: "owner", CreatedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
		return id
	}

	job := run(newJob(model.AdoptManifest, manifest))
	if job.State != model.AdoptionCompleted || job.Entries != 6 || job.Adopted != 1 || job.Bytes != int64(len(good)) {
		t.Fatalf("job: %+v", job)
	}
	entries, err := store.Entries(ctx, tenant, job.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct{ state, code string }{
		{model.AdoptAdopted, ""},
		{model.AdoptRejected, model.RejectContentCheck},
		{model.AdoptRejected, model.RejectMalwareDetected},
		{model.AdoptRejected, "digest_mismatch"},
		{model.AdoptRejected, "digest_required"},
		{model.AdoptRejected, "platform_key"},
	}
	for i, w := range want {
		if entries[i].State != w.state || entries[i].Code != w.code {
			t.Errorf("line %d: %s %s (%s), want %s %s", i+1, entries[i].State, entries[i].Code, entries[i].Detail, w.state, w.code)
		}
	}

	// Published where it was: no copy, the version pinned, the catalog
	// object and its metadata, the processing evidence.
	asset, err := stack.env.assets.GetByID(ctx, tenant, entries[0].AssetID)
	if err != nil {
		t.Fatal(err)
	}
	if asset.Status != model.AssetStatusAvailable || asset.ObjectKey != goodKey || asset.Adoption != model.AdoptionDetach ||
		asset.StorageConnectionID != conn || asset.Version != 1 || asset.Metadata["lab"] != "bio" || asset.ObjectID == "" {
		t.Fatalf("adopted asset: %+v", asset)
	}
	pj, err := stack.env.jobs.GetByAsset(ctx, tenant, asset.ID)
	if err != nil || pj.ScanResult != model.ScanClean || pj.VerificationStatus != model.StagePassed || pj.ScannerVersion != "sigs-1" {
		t.Fatalf("processing record: %+v %v", pj, err)
	}

	// The same object again is skipped, not adopted twice.
	again := run(newJob(model.AdoptManifest, fmt.Sprintf(`{"key":%q,"md5":%q}`, goodKey, md5hex(good))))
	second, _ := store.Entries(ctx, tenant, again.ID, 0, 10)
	if second[0].State != model.AdoptSkipped || second[0].Code != "already_adopted" || second[0].AssetID != asset.ID {
		t.Fatalf("second adoption: %+v", second[0])
	}

	// A URL import is fetched, checked and uploaded; processing publishes it.
	page := []byte("imported over http\n")
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/data/page.txt" {
			w.Header().Set("Content-Type", "text/plain")
			w.Write(page)
			return
		}
		http.NotFound(w, r)
	}))
	defer site.Close()
	imports := newJob(model.AdoptURLList, fmt.Sprintf(`{"url":"%s/data/page.txt","path":"web/page.txt","md5":%q}
{"url":"%s/missing.txt"}`, site.URL, md5hex(page), site.URL))
	if _, err := db.Pool.Exec(ctx, `UPDATE adoption_jobs SET connection_id = 'platform', require_digest = false WHERE id = $1`, imports); err != nil {
		t.Fatal(err)
	}
	importJob := run(imports)
	imported, _ := store.Entries(ctx, tenant, importJob.ID, 0, 10)
	if imported[0].State != model.AdoptImported || imported[0].UploadID == "" || imported[1].State != model.AdoptRejected || imported[1].Code != "not_found" {
		t.Fatalf("imports: %+v", imported)
	}
	runCycle(t, stack.service(t, serviceOptions{scanner: scanner}))
	imported, _ = store.Entries(ctx, tenant, importJob.ID, 0, 10)
	if imported[0].State != model.AdoptAdopted || imported[0].AssetID == "" {
		t.Fatalf("after processing: %+v", imported[0])
	}
	t.Cleanup(func() {
		_, _ = db.Pool.Exec(context.Background(), `DELETE FROM adoption_jobs WHERE tenant_id = $1`, tenant)
	})
}
