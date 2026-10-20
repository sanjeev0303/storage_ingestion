package integration

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
	storagememory "github.com/sanjeev0303/object-storage-platform/internal/infrastructure/storage/memory"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/service/delivery"
)

// Needs TEST_DATABASE_URL with migrations through 000005 applied.

func TestPostgresDeliveryUsesCurrentStateAndIsAudited(t *testing.T) {
	env := openReconcileEnv(t)
	ctx := context.Background()
	assets := postgres.NewAssetRepository(env.db)
	audit := postgres.NewDeliveryAuditRepository(env.db)

	env.session(t, "deliver", model.UploadStatusVerified)
	now := time.Now().UTC()
	asset := model.Asset{
		ID: env.tenant + "-asset", TenantID: env.tenant, OwnerID: "owner", UploadID: env.tenant + "-deliver",
		ObjectKey: "assets/" + env.tenant + "/" + env.tenant + "-asset", FileName: "f.pdf", ContentType: "application/pdf",
		SizeBytes: 10, Status: model.AssetStatusAvailable, CreatedAt: now, UpdatedAt: now,
	}
	if err := assets.Create(ctx, &asset); err != nil {
		t.Fatalf("create asset: %v", err)
	}

	storage := storagememory.NewObjectStorage()
	svc, err := delivery.NewService(assets, storage, audit, nil, delivery.Config{
		Bucket: "private", Prefix: "assets", DefaultTTL: time.Minute, MaxTTL: 5 * time.Minute,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	principal := member(env.tenant, "owner", model.RoleDeveloper)

	if _, err := svc.CreateDownloadURL(ctx, principal, asset.ID, delivery.URLRequest{}); err != nil {
		t.Fatalf("download URL: %v", err)
	}
	if _, err := svc.CreateDownloadURL(ctx, member("other-tenant", "owner", model.RoleOwner), asset.ID, delivery.URLRequest{}); !errors.Is(err, delivery.ErrNotFound) {
		t.Fatalf("another tenant must get not found, got %v", err)
	}

	// Deleted directly in the database: the very next request is refused.
	if _, err := env.db.Pool.Exec(ctx, `UPDATE assets SET status = 'DELETED' WHERE id = $1`, asset.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateDownloadURL(ctx, principal, asset.ID, delivery.URLRequest{}); !errors.Is(err, delivery.ErrNotFound) {
		t.Fatalf("a deleted asset must be refused, got %v", err)
	}

	rows, err := env.db.Pool.Query(ctx, `
		SELECT outcome, COALESCE(reason, ''), principal_id, operation
		FROM delivery_audit_events WHERE asset_id = $1 ORDER BY created_at`, asset.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var outcomes []string
	for rows.Next() {
		var outcome, reason, principalID, operation string
		if err := rows.Scan(&outcome, &reason, &principalID, &operation); err != nil {
			t.Fatal(err)
		}
		if operation != model.DeliveryOpDownloadURL {
			t.Fatalf("unexpected operation %s", operation)
		}
		outcomes = append(outcomes, outcome+":"+reason)
	}
	want := []string{"ALLOWED:via_storage", "DENIED:not_found", "DENIED:not_found"}
	if len(outcomes) != len(want) {
		t.Fatalf("audit trail %v, want %v", outcomes, want)
	}
	for i := range want {
		if outcomes[i] != want[i] {
			t.Fatalf("audit trail %v, want %v", outcomes, want)
		}
	}
	t.Cleanup(func() {
		_, _ = env.db.Pool.Exec(context.Background(), `DELETE FROM delivery_audit_events WHERE asset_id = $1`, asset.ID)
	})
}

func TestPostgresAuditRejectsUnknownOperations(t *testing.T) {
	env := openReconcileEnv(t)
	err := postgres.NewDeliveryAuditRepository(env.db).Record(context.Background(), model.DeliveryAuditEvent{
		ID: env.tenant + "-bad", TenantID: env.tenant, PrincipalID: "p", AssetID: "a",
		Operation: "PUBLIC_LINK", Outcome: model.DeliveryAllowed, CreatedAt: time.Now(),
	})
	if err == nil {
		t.Fatal("the audit table must only accept known operations")
	}
}
