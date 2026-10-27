package integration

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// A multipart upload may declare its SHA-256 at completion. PostgreSQL
// records it once, only while UPLOADING, and refuses a second, different one.
func TestPostgresDeclaredChecksumIsRecordedOnce(t *testing.T) {
	env := openReconcileEnv(t)
	ctx := context.Background()
	env.session(t, "late", model.UploadStatusUploading)
	env.session(t, "done", model.UploadStatusCompleted)
	id := env.tenant + "-late"
	sum, other := strings.Repeat("ab", 32), strings.Repeat("cd", 32)

	if err := env.uploads.SetDeclaredChecksum(ctx, env.tenant, id, sum, time.Now()); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := env.uploads.SetDeclaredChecksum(ctx, env.tenant, id, sum, time.Now()); err != nil {
		t.Fatalf("same again: %v", err)
	}
	if err := env.uploads.SetDeclaredChecksum(ctx, env.tenant, id, other, time.Now()); !errors.Is(err, model.ErrInvalidUploadState) {
		t.Fatalf("a different one: %v, want ErrInvalidUploadState", err)
	}
	if err := env.uploads.SetDeclaredChecksum(ctx, env.tenant, env.tenant+"-done", sum, time.Now()); !errors.Is(err, model.ErrInvalidUploadState) {
		t.Fatalf("on a completed upload: %v, want ErrInvalidUploadState", err)
	}
	if err := env.uploads.SetDeclaredChecksum(ctx, env.tenant, env.tenant+"-missing", sum, time.Now()); !errors.Is(err, model.ErrUploadNotFound) {
		t.Fatalf("on no upload: %v, want ErrUploadNotFound", err)
	}

	got, err := env.uploads.GetByID(ctx, env.tenant, id)
	if err != nil || got.ChecksumSHA256 != sum {
		t.Fatalf("stored %q, %v; want %q", got.ChecksumSHA256, err, sum)
	}
}
