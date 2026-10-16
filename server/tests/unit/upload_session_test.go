package unit

import (
	"errors"
	"testing"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

func TestUploadSessionTransitionToCompleted(t *testing.T) {
	now := time.Now().UTC()

	upload := model.UploadSession{
		ID:           "upload-123",
		TenantID:     "tenant-1",
		OwnerID:      "user-1",
		ObjectKey:    "quarantine/tenant-1/upload-123",
		FileName:     "report.pdf",
		ContentType:  "application/pdf",
		ExpectedSize: 1024,
		Status:       model.UploadStatusPending,
		CreatedAt:    now,
		UpdatedAt:    now,
		ExpiresAt:    now.Add(30 * time.Minute),
	}

	err := upload.TransitionTo(model.UploadStatusCompleted)
	if err != nil {
		t.Fatalf("expected transition to succeed, got %v", err)
	}

	if upload.Status != model.UploadStatusCompleted {
		t.Fatalf(
			"expected status %s, got %s",
			model.UploadStatusCompleted,
			upload.Status,
		)
	}

	if upload.UpdatedAt.Before(now) {
		t.Error("expected UpdatedAt to be updated")
	}
}

func TestUploadSessionRejectsInvalidTransition(t *testing.T) {
	upload := model.UploadSession{
		Status: model.UploadStatusCompleted,
	}

	err := upload.TransitionTo(model.UploadStatusUploading)
	if err == nil {
		t.Fatal("expected invalid transition to fail")
	}
}

func TestUploadSessionValidate(t *testing.T) {
	now := time.Now().UTC()

	upload := model.UploadSession{
		ID:           "upload-123",
		TenantID:     "tenant-1",
		OwnerID:      "user-1",
		ObjectKey:    "quarantine/tenant-1/upload-123",
		FileName:     "report.pdf",
		ContentType:  "application/pdf",
		ExpectedSize: 1024,
		Status:       model.UploadStatusPending,
		CreatedAt:    now,
		ExpiresAt:    now.Add(time.Minute),
	}

	if err := upload.Validate(); err != nil {
		t.Fatalf("expected valid upload, got %v", err)
	}

	upload.ExpectedSize = 0

	if err := upload.Validate(); err == nil {
		t.Fatal("expected zero-size upload to fail validation")
	}
}

func TestDomainErrorIdentity(t *testing.T) {
	err := model.ErrUploadNotFound

	if !errors.Is(err, model.ErrUploadNotFound) {
		t.Fatal("expected errors.Is to recognize the sentinel error")
	}
}

func TestUploadLifecycleTransitions(t *testing.T) {
	allowed := []struct{ from, to model.UploadStatus }{
		{model.UploadStatusUploading, model.UploadStatusCompleting},
		{model.UploadStatusCompleting, model.UploadStatusCompleted},
		{model.UploadStatusCompleting, model.UploadStatusFailed},
		{model.UploadStatusCompleted, model.UploadStatusVerified},
		{model.UploadStatusCompleted, model.UploadStatusFailed},
		{model.UploadStatusPending, model.UploadStatusAborting},
		{model.UploadStatusUploading, model.UploadStatusAborting},
		{model.UploadStatusAborting, model.UploadStatusFailed},
		{model.UploadStatusAborting, model.UploadStatusExpired},
	}
	for _, c := range allowed {
		if !model.CanTransition(c.from, c.to) {
			t.Errorf("%s -> %s should be allowed", c.from, c.to)
		}
	}

	refused := []struct{ from, to model.UploadStatus }{
		// Completion and abort can never both finalize a session.
		{model.UploadStatusCompleting, model.UploadStatusAborting},
		{model.UploadStatusAborting, model.UploadStatusCompleting},
		{model.UploadStatusAborting, model.UploadStatusCompleted},
		// Promotion needs verification; terminal states stay terminal.
		{model.UploadStatusCompleting, model.UploadStatusVerified},
		{model.UploadStatusVerified, model.UploadStatusFailed},
		{model.UploadStatusFailed, model.UploadStatusVerified},
		{model.UploadStatusExpired, model.UploadStatusUploading},
	}
	for _, c := range refused {
		if model.CanTransition(c.from, c.to) {
			t.Errorf("%s -> %s should be refused", c.from, c.to)
		}
	}
}

func TestOnlyVerifiedUploadsCanBePromoted(t *testing.T) {
	for _, status := range []model.UploadStatus{
		model.UploadStatusPending, model.UploadStatusUploading, model.UploadStatusCompleting,
		model.UploadStatusCompleted, model.UploadStatusAborting, model.UploadStatusFailed, model.UploadStatusExpired,
	} {
		if (model.UploadSession{Status: status}).CanPromote() {
			t.Errorf("%s must not be promotable", status)
		}
	}
	if !(model.UploadSession{Status: model.UploadStatusVerified}).CanPromote() {
		t.Error("VERIFIED must be promotable")
	}
}

func TestAssetLifecycle(t *testing.T) {
	allowed := []struct{ from, to model.AssetStatus }{
		{model.AssetStatusQuarantined, model.AssetStatusProcessing},
		{model.AssetStatusProcessing, model.AssetStatusAvailable},
		{model.AssetStatusQuarantined, model.AssetStatusRejected},
		{model.AssetStatusProcessing, model.AssetStatusRejected},
		{model.AssetStatusAvailable, model.AssetStatusDeleted},
		{model.AssetStatusRejected, model.AssetStatusDeleted},
	}
	for _, c := range allowed {
		if !model.CanTransitionAsset(c.from, c.to) {
			t.Errorf("%s -> %s should be allowed", c.from, c.to)
		}
	}

	refused := []struct{ from, to model.AssetStatus }{
		// Nothing skips verification and processing.
		{model.AssetStatusQuarantined, model.AssetStatusAvailable},
		// Rejected, deleted and available assets never go back.
		{model.AssetStatusRejected, model.AssetStatusAvailable},
		{model.AssetStatusRejected, model.AssetStatusProcessing},
		{model.AssetStatusDeleted, model.AssetStatusAvailable},
		{model.AssetStatusAvailable, model.AssetStatusQuarantined},
		{model.AssetStatusAvailable, model.AssetStatusRejected},
	}
	for _, c := range refused {
		if model.CanTransitionAsset(c.from, c.to) {
			t.Errorf("%s -> %s should be refused", c.from, c.to)
		}
	}
}
