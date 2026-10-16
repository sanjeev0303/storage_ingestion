package integration

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

func TestPostgresUploadIdempotency(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set TEST_DATABASE_URL to run PostgreSQL integration tests")
	}

	ctx, cancel := context.WithTimeout(
		context.Background(),
		10*time.Second,
	)
	defer cancel()

	db, err := postgres.Open(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	defer db.Close()

	repo := postgres.NewUploadRepository(db)

	now := time.Now().UTC()
	upload := &model.UploadSession{
		ID:           "integration-upload-001",
		TenantID:     "integration-tenant",
		OwnerID:      "integration-user",
		ObjectKey:    "quarantine/integration-tenant/integration-upload-001",
		FileName:     "report.pdf",
		ContentType:  "application/pdf",
		ExpectedSize: 1024,
		Status:       model.UploadStatusPending,
		CreatedAt:    now,
		UpdatedAt:    now,
		ExpiresAt:    now.Add(time.Minute),
	}

	// Ensure a previous interrupted test does not leave this fixture behind.
	_, err = db.Pool.Exec(ctx, `
	DELETE FROM upload_sessions
	WHERE id = $1
	`, upload.ID)
	if err != nil {
		t.Fatalf("clean test fixture: %v", err)
	}

	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(
			context.Background(),
			5*time.Second,
		)
		defer cleanupCancel()

		_, _ = db.Pool.Exec(
			cleanupCtx,
			`DELETE FROM upload_sessions WHERE id = $1`,
			upload.ID,
		)
	})

	const key = "integration-idempotency-001"

	if err := repo.CreateWithIdempotency(ctx, upload, key); err != nil {
		t.Fatalf("create upload: %v", err)
	}

	got, err := repo.GetByIdempotencyKey(
		ctx,
		upload.TenantID,
		upload.OwnerID,
		key,
	)
	if err != nil {
		t.Fatalf("get upload by idempotency key: %v", err)
	}

	if got.ID != upload.ID {
		t.Fatalf("expected upload %q, got %q", upload.ID, got.ID)
	}

	// A second insert with the same key must fail.
	duplicate := *upload
	duplicate.ID = "integration-upload-002"

	err = repo.CreateWithIdempotency(ctx, &duplicate, key)
	if !errors.Is(err, model.ErrIdempotencyConflict) {
		t.Fatalf("expected idempotency conflict, got %v", err)
	}
}
