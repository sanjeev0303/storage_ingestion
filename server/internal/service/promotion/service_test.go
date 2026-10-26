package promotion

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	storagememory "github.com/sanjeev0303/object-storage-platform/internal/infrastructure/storage/memory"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/repository/memory"
)

// A failed confirmation (processing's lease renewal) publishes nothing,
// even after the copy was made and verified; the same promotion goes
// through once confirmation passes.
func TestNothingIsPublishedWithoutConfirmation(t *testing.T) {
	ctx := context.Background()
	assets := memory.NewAssetRepository()
	storage := storagememory.NewObjectStorage()
	svc, err := NewService(assets, storage, Config{SourceBucket: "b", DestinationBucket: "b", Prefix: "assets"})
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("verified and scanned bytes")
	sum := sha256.Sum256(data)
	storage.PutObject("quarantine/t1/up-1", data)
	at := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	asset := model.Asset{ID: "a1", TenantID: "t1", OwnerID: "u1", UploadID: "up-1", ObjectKey: "quarantine/t1/up-1",
		FileName: "f.txt", ContentType: "text/plain", SizeBytes: int64(len(data)), ChecksumSHA256: hex.EncodeToString(sum[:]),
		Status: model.AssetStatusProcessing, CreatedAt: at, UpdatedAt: at}
	if err := assets.Create(ctx, &asset); err != nil {
		t.Fatal(err)
	}
	stored, _ := assets.GetByID(ctx, "t1", "a1")

	lost := func(context.Context) error { return model.ErrLeaseLost }
	if _, err := svc.Promote(ctx, *stored, "assets/t1/a1", lost); !errors.Is(err, model.ErrLeaseLost) {
		t.Fatalf("promote without confirmation: %v", err)
	}
	if after, _ := assets.GetByID(ctx, "t1", "a1"); after.Status != model.AssetStatusProcessing || after.StorageETag != "" {
		t.Fatalf("published without confirmation: %+v", after)
	}

	confirmed := func(context.Context) error { return nil }
	if status, err := svc.Promote(ctx, *stored, "assets/t1/a1", confirmed); err != nil || status != model.AssetStatusAvailable {
		t.Fatalf("confirmed promotion: %s %v", status, err)
	}
}
