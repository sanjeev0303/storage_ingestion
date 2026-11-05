package integration

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/service/adoption"
	"github.com/sanjeev0303/object-storage-platform/internal/service/processing"
)

type fixedQuota struct{ q model.TenantQuota }

func (f *fixedQuota) Effective(context.Context, string) (model.TenantQuota, error) { return f.q, nil }

// An adoption reserves its bytes before it adopts anything, as an upload
// does: a job that cannot fit fails at once, and what it reserved is given
// back when it ends.
func TestAdoptionReservesQuota(t *testing.T) {
	stack := openContentStack(t)
	ctx := context.Background()
	db := stack.env.db
	tenant := stack.env.tenant
	conn := "conn-quota-" + fmt.Sprint(time.Now().UnixNano())
	if _, err := db.Pool.Exec(ctx, `INSERT INTO storage_connections (id, tenant_id, name, provider, status, auth_method, bucket,
		created_by, created_at, updated_at) VALUES ($1, $2, 'byo', 'S3_COMPATIBLE', 'ACTIVE', 'ACCESS_KEY', $3, 'it', now(), now())`,
		conn, tenant, stack.bucket); err != nil {
		t.Fatal(err)
	}
	prefix := fmt.Sprintf("quota-%d/", time.Now().UnixNano())
	var lines []string
	for i, body := range [][]byte{bytes.Repeat([]byte("a,b\n"), 100), bytes.Repeat([]byte("c,d\n"), 200), bytes.Repeat([]byte("e,f\n"), 3000)} {
		key := fmt.Sprintf("%sf%d.csv", prefix, i)
		if _, err := stack.provider.WriteObject(ctx, stack.bucket, key, bytes.NewReader(body), int64(len(body)), "text/csv"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = stack.provider.DeleteObject(context.Background(), stack.bucket, key) })
		sum := md5.Sum(body)
		lines = append(lines, fmt.Sprintf(`{"key":%q,"size":%d,"md5":%q}`, key, len(body), hex.EncodeToString(sum[:])))
	}
	manifest := strings.Join(lines, "\n") // 400 + 800 + 12000 bytes

	quota := &fixedQuota{}
	store := postgres.NewAdoptionRepository(db)
	svc := adoption.NewService(adoption.Dependencies{
		Store: store, Router: oneConnection{storage: stack.provider, bucket: stack.bucket}, Platform: stack.provider, PlatformBucket: stack.bucket,
		Validators: []interfaces.FileValidator{processing.BasicValidator{}, processing.TypeValidator{}},
		Scanner:    &signatureScanner{version: "sigs-1", bad: []byte("BADSIGNATURE")},
		Policies:   postgres.NewContentPolicyRepository(db), Checks: postgres.NewContentCheckRepository(db), Quota: quota,
	}, adoption.Config{Owner: "it-quota", Policy: model.ContentPolicy{MaxSizeBytes: 1 << 30, UnknownTypeAction: "allow"}},
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	usage := func() (reserved, uploads, stored int64) {
		t.Helper()
		if err := db.Pool.QueryRow(ctx, `SELECT COALESCE(max(reserved_bytes), 0), COALESCE(max(reserved_uploads), 0), COALESCE(max(stored_bytes), 0)
			FROM tenant_usage WHERE tenant_id = $1`, tenant).Scan(&reserved, &uploads, &stored); err != nil {
			t.Fatal(err)
		}
		return
	}
	newJob := func() string {
		id := fmt.Sprintf("adopt-q-%d", time.Now().UnixNano())
		if err := store.CreateJob(ctx, model.AdoptionJob{ID: id, TenantID: tenant, ConnectionID: conn, Source: model.AdoptManifest,
			InlineSource: manifest, Mode: model.AdoptionDetach, RequireDigest: true, CreatedBy: "owner", CreatedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	step := func(id string) model.AdoptionJob {
		t.Helper()
		if _, err := svc.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		job, err := store.GetJob(ctx, tenant, id)
		if err != nil {
			t.Fatal(err)
		}
		return job
	}

	reserved0, uploads0, stored0 := usage()

	// Too little room: the job fails before adopting anything.
	quota.q = model.TenantQuota{MaxStorageBytes: stored0 + reserved0 + 1000}
	job := step(newJob())
	if job.State != model.AdoptionFailed || !strings.Contains(job.Error, "storage_bytes") || job.Adopted != 0 {
		t.Fatalf("an adoption past the quota: %+v", job)
	}
	if r, u, _ := usage(); r != reserved0 || u != uploads0 {
		t.Fatalf("a failed reservation left usage at %d bytes, %d uploads (was %d, %d)", r, u, reserved0, uploads0)
	}

	// Room for all but the largest object, which is over the per-object limit.
	quota.q = model.TenantQuota{MaxObjectBytes: 5000}
	id := newJob()
	job = step(id) // reads the manifest and reserves
	if job.State != model.AdoptionRunning {
		t.Fatalf("after reading: %+v", job)
	}
	if r, u, _ := usage(); r != reserved0+1200 || u != uploads0+2 {
		t.Fatalf("reserved %d bytes for %d objects, want %d and %d", r-reserved0, u-uploads0, 1200, 2)
	}
	for i := 0; i < 5 && job.State == model.AdoptionRunning; i++ {
		job = step(id)
	}
	if job.State != model.AdoptionCompleted || job.Adopted != 2 || job.Rejected != 1 {
		t.Fatalf("job: %+v", job)
	}
	entries, err := store.Entries(ctx, tenant, id, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if entries[2].Code != "quota_exceeded" {
		t.Fatalf("the oversized entry: %+v", entries[2])
	}
	if r, u, s := usage(); r != reserved0 || u != uploads0 || s != stored0+1200 {
		t.Fatalf("after the job: reserved %d (was %d), uploads %d (was %d), stored %d (want %d)", r, reserved0, u, uploads0, s, stored0+1200)
	}
}
