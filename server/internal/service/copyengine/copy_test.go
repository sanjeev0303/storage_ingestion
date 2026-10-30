package copyengine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/storage/memory"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

type router map[string]interfaces.StorageLocation

func (r router) Locate(_ context.Context, _, id string) (interfaces.StorageLocation, error) {
	loc, ok := r[id]
	if !ok {
		return loc, model.ErrConnectionNotFound
	}
	return loc, nil
}

func place(id string, s *memory.ObjectStorage) interfaces.StorageLocation {
	return interfaces.StorageLocation{StoragePlace: interfaces.StoragePlace{ConnectionID: id, ReadyBucket: "b", ReadyPrefix: "ready"}, Storage: s}
}

func TestCopiesAreVerifiedEndToEnd(t *testing.T) {
	ctx := context.Background()
	src, dst := memory.NewObjectStorage(), memory.NewObjectStorage()
	s := NewService(Dependencies{Router: router{"a": place("a", src), "b": place("b", dst)}}, Config{},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	data := []byte("bytes worth keeping\n")
	sum := sha256.Sum256(data)
	if _, err := src.WriteObject(ctx, "b", "ready/t/a1", bytes.NewReader(data), int64(len(data)), "text/plain"); err != nil {
		t.Fatal(err)
	}
	a := postgres.StoredAsset{AssetID: "a1", TenantID: "t", ConnectionID: "a", ObjectKey: "ready/t/a1", SizeBytes: int64(len(data)),
		SHA256: hex.EncodeToString(sum[:]), ContentType: "text/plain"}

	if _, err := s.copyTo(ctx, a, place("b", dst), "ready/t/a1"); err != nil {
		t.Fatalf("a good copy: %v", err)
	}
	if _, ok := dst.Object("ready/t/a1"); !ok {
		t.Fatal("the copy is not there")
	}

	// A source that no longer matches its record is never copied.
	bad := a
	bad.SHA256 = hex.EncodeToString(make([]byte, 32))
	if _, err := s.copyTo(ctx, bad, place("b", dst), "ready/t/bad"); !errors.Is(err, model.ErrChecksumMismatch) {
		t.Fatalf("a mismatched source: %v", err)
	}
	if _, ok := dst.Object("ready/t/bad"); ok {
		t.Fatal("a copy of mismatched bytes was left behind")
	}
	// And reading it back finds the damage too.
	if err := s.reread(ctx, bad, false); err == nil || err.Error()[:8] != "MISMATCH" {
		t.Fatalf("reread: %v", err)
	}
	gone := a
	gone.ObjectKey = "ready/t/missing"
	if err := s.reread(ctx, gone, false); err == nil || err.Error()[:7] != "MISSING" {
		t.Fatalf("reread of a missing object: %v", err)
	}
}

func TestMigrationPlanGroupsSharedObjects(t *testing.T) {
	s := NewService(Dependencies{Router: router{}}, Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	assets := []postgres.StoredAsset{
		{AssetID: "x", ConnectionID: "a", ObjectKey: "ready/t/x", SizeBytes: 10},
		{AssetID: "y", ConnectionID: "a", ObjectKey: "ready/t/x", SizeBytes: 10, SharedFrom: "x"},
		{AssetID: "z", ConnectionID: "b", ObjectKey: "ready/t/z", SizeBytes: 5},
	}
	plan, todo := s.plan(context.Background(), "t", assets, "b")
	if plan.Objects != 1 || plan.Bytes != 10 || plan.AlreadyThere != 1 || len(todo) != 1 {
		t.Fatalf("plan: %+v %d", plan, len(todo))
	}
	if promotedKey("ready2", postgres.StoredAsset{TenantID: "t", AssetID: "y", SharedFrom: "x"}) != "ready2/t/x" {
		t.Fatal("a sharer moves with its owner's key")
	}
	if cleanPath("../a//b/./c") != "a/b/c" {
		t.Fatalf("clean path: %s", cleanPath("../a//b/./c"))
	}
}
