package integration

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/service/delivery"
	"github.com/sanjeev0303/object-storage-platform/internal/service/processing"
)

// Workers act on IDs from queue messages and from the database. They
// trust no message for its tenant: every lookup pairs the tenant with the
// ID, and the database refuses rows that join two tenants.
func TestWorkersActOnlyWithinTheirTenant(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	acme := s.team(t, "wrk-a")
	globex := s.team(t, "wrk-g")
	published := s.availableAsset(t, acme.developer, "published", textData("published", 64))
	pending := s.upload(t, acme.developer, "pending", textData("pending", 64))
	if _, err := s.ie.single.CompleteFor(ctx, acme.developer, pending.ID); err != nil {
		t.Fatal(err)
	}

	// A message naming acme's upload under globex's tenant finds nothing.
	if _, err := s.ie.proc.Intake(ctx, globex.org.ID, pending.ID); !errors.Is(err, model.ErrUploadNotFound) {
		t.Fatalf("processing another tenant's upload: %v", err)
	}
	if _, err := s.ie.single.Complete(ctx, globex.org.ID, pending.ID); !errors.Is(err, model.ErrUploadNotFound) {
		t.Fatalf("completing another tenant's upload: %v", err)
	}
	quarantine := processing.NewQuarantine(s.ie.assets, s.ie.processingEnv.jobs, postgres.NewQuarantineRepository(s.db),
		postgres.NewProcessingEventRepository(s.db), s.audit)
	if _, err := quarantine.Inspect(ctx, globex.org.ID, published.ID); !errors.Is(err, model.ErrAssetNotFound) {
		t.Fatalf("inspecting another tenant's asset: %v", err)
	}
	if _, err := quarantine.Requeue(ctx, globex.org.ID, published.ID, "worker"); !errors.Is(err, model.ErrAssetNotFound) {
		t.Fatalf("requeueing another tenant's asset: %v", err)
	}
	// Service principals are scoped to one tenant like any other.
	worker := model.ServicePrincipal(globex.org.ID, "it-worker")
	if _, err := s.delivery.CreateDownloadURL(ctx, worker, published.ID, delivery.URLRequest{}); !errors.Is(err, delivery.ErrNotFound) {
		t.Fatalf("a worker of globex signing acme's asset: %v", err)
	}
	// acme's upload is untouched and still processes for acme.
	if _, err := s.ie.proc.Intake(ctx, acme.org.ID, pending.ID); err != nil {
		t.Fatalf("processing for its own tenant: %v", err)
	}

	// The database refuses an asset of globex built on acme's upload, and
	// a processing job of globex for acme's asset.
	now := time.Now().UTC()
	orphan := s.upload(t, acme.developer, "orphan", textData("orphan", 8))
	forged := model.Asset{
		ID: "forged-" + s.run, TenantID: globex.org.ID, OwnerID: globex.devUser.ID, UploadID: orphan.ID,
		ObjectKey: "quarantine/" + globex.org.ID + "/x", FileName: "x.txt", ContentType: "text/plain", SizeBytes: 1,
		Status: model.AssetStatusQuarantined, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.ie.assets.Create(ctx, &forged); err == nil {
		t.Fatal("an asset joining two tenants must be refused")
	}
	// The same asset for acme is fine; a globex job for it is not.
	forged.ID, forged.TenantID, forged.OwnerID = "acme-"+s.run, acme.org.ID, acme.devUser.ID
	if err := s.ie.assets.Create(ctx, &forged); err != nil {
		t.Fatalf("an asset of one tenant: %v", err)
	}
	theirs := s.upload(t, globex.developer, "theirs", textData("theirs", 8))
	_, err := s.db.Pool.Exec(ctx, `
		INSERT INTO processing_jobs (id, tenant_id, upload_id, asset_id, stage, state, max_attempts, next_attempt_at, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 'VERIFY', 'PENDING', 3, $5, $5, $5)
	`, "forged-job-"+s.run, globex.org.ID, theirs.ID, forged.ID, now)
	if err == nil || !strings.Contains(err.Error(), "foreign key") {
		t.Fatalf("a job joining two tenants must be refused: %v", err)
	}

	// Queue messages carry IDs, never credentials.
	rows, err := s.db.Pool.Query(ctx, `SELECT payload::text FROM outbox_events WHERE tenant_id = $1`, acme.org.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	messages := 0
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			t.Fatal(err)
		}
		messages++
		for _, secret := range []string{"sat_", "srt_", "sct_", "sit_", "Signature", "password", acme.devPW} {
			if strings.Contains(payload, secret) {
				t.Fatalf("a queue message holds %q: %s", secret, payload)
			}
		}
	}
	if messages == 0 {
		t.Fatal("expected acme's uploads to have written events")
	}
}
