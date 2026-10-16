package integration

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

func TestMultipartUploadRepositoryUpsertListDelete(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	db, err := postgres.Open(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open PostgreSQL connection: %v", err)
	}
	defer db.Close()

	// Use a separate pool for fixture cleanup. This keeps the
	// production DB wrapper's internal pool private.
	cleanupPool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open cleanup pool: %v", err)
	}
	// t.Cleanup is LIFO: Close (registered first) runs LAST,
	// so the pool is still open when the DELETE cleanup fires.
	t.Cleanup(cleanupPool.Close)

	uploadRepo := postgres.NewUploadRepository(db)
	partRepo := postgres.NewMultipartUploadRepository(db)

	uploadID := fmt.Sprintf("test-upload-%d", time.Now().UnixNano())
	tenantID := "integration-test-tenant"

	session := model.UploadSession{
		ID:             uploadID,
		TenantID:       tenantID,
		OwnerID:        "integration-test-owner",
		ObjectKey:      "quarantine/" + tenantID + "/" + uploadID,
		FileName:       "integration-test.bin",
		ContentType:    "application/octet-stream",
		ExpectedSize:   10 * 1024 * 1024,
		ChecksumSHA256: strings.Repeat("a", 64),
		Status:         model.UploadStatusPending,
		ExpiresAt:      time.Now().UTC().Add(time.Hour),
		CreatedAt:      time.Now().UTC(),
		UpdatedAt:      time.Now().UTC(),
	}

	if err := uploadRepo.Create(ctx, &session); err != nil {
		t.Fatalf("create upload session: %v", err)
	}

	t.Cleanup(func() {
		_, cleanupErr := cleanupPool.Exec(
			context.Background(),
			`DELETE FROM upload_sessions WHERE id = $1`,
			uploadID,
		)
		if cleanupErr != nil {
			t.Errorf("clean up upload session: %v", cleanupErr)
		}
	})

	// Insert part 2 first to verify that listing is sorted.
	part2 := model.UploadPart{
		TenantID:   tenantID,
		UploadID:   uploadID,
		PartNumber: 2,
		ETag:       `"etag-part-2"`,
		SizeBytes:  5 * 1024 * 1024,
		Status:     model.UploadPartUploaded,
	}

	part1 := model.UploadPart{
		TenantID:   tenantID,
		UploadID:   uploadID,
		PartNumber: 1,
		ETag:       `"etag-part-1"`,
		SizeBytes:  5 * 1024 * 1024,
		Status:     model.UploadPartUploaded,
	}

	if err := partRepo.UpsertPart(ctx, tenantID, part2); err != nil {
		t.Fatalf("insert part 2: %v", err)
	}

	if err := partRepo.UpsertPart(ctx, tenantID, part1); err != nil {
		t.Fatalf("insert part 1: %v", err)
	}

	// Retrying the same part number must update its metadata,
	// rather than creating a duplicate row.
	part1.ETag = `"etag-part-1-retried"`

	if err := partRepo.UpsertPart(ctx, tenantID, part1); err != nil {
		t.Fatalf("retry part 1: %v", err)
	}

	parts, err := partRepo.ListParts(ctx, tenantID, uploadID)
	if err != nil {
		t.Fatalf("list parts: %v", err)
	}

	if len(parts) != 2 {
		t.Fatalf("expected 2 parts, got %d", len(parts))
	}

	if parts[0].PartNumber != 1 || parts[1].PartNumber != 2 {
		t.Fatalf("parts are not ordered: got %d, %d",
			parts[0].PartNumber, parts[1].PartNumber)
	}

	if parts[0].ETag != `"etag-part-1-retried"` {
		t.Fatalf("retry was not persisted: got ETag %q", parts[0].ETag)
	}

	if err := partRepo.DeleteParts(ctx, tenantID, uploadID); err != nil {
		t.Fatalf("delete parts: %v", err)
	}

	parts, err = partRepo.ListParts(ctx, tenantID, uploadID)
	if err != nil {
		t.Fatalf("list parts after deletion: %v", err)
	}

	if len(parts) != 0 {
		t.Fatalf("expected no parts after deletion, got %d", len(parts))
	}
}
