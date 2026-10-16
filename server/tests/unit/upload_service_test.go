package unit

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/dto/request"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/repository/memory"
	uploadservice "github.com/sanjeev0303/object-storage-platform/internal/service/upload"
)

func TestCreateUpload(t *testing.T) {
	repo := memory.NewUploadRepository()

	svc := uploadservice.NewService(
		repo,
		&fakeObjectStorage{},
		"test-bucket",
		1024*1024,
		time.Minute,
	)

	input := uploadservice.CreateInput{
		TenantID: "tenant-1",
		OwnerID:  "user-1",
		Request: request.CreateUploadRequest{
			FileName:       "report.pdf",
			ContentType:    "application/pdf",
			SizeBytes:      1024,
			IdempotencyKey: "request-1",
		},
	}

	result, err := svc.Create(context.Background(), input)
	if err != nil {
		t.Fatalf("create upload: %v", err)
	}

	if result.Reused {
		t.Fatal("first request should not be marked as reused")
	}

	if result.Upload.Status != model.UploadStatusPending {
		t.Fatalf("unexpected status: %s", result.Upload.Status)
	}

	if result.Upload.ObjectKey == "" {
		t.Fatal("expected an object key")
	}

	if result.Upload.ID == "" {
		t.Fatal("expected an upload ID")
	}
}

func TestCreateUploadIdempotentRetry(t *testing.T) {
	repo := memory.NewUploadRepository()

	svc := uploadservice.NewService(
		repo,
		&fakeObjectStorage{},
		"test-bucket",
		1024*1024,
		time.Minute,
	)

	input := uploadservice.CreateInput{
		TenantID: "tenant-1",
		OwnerID:  "user-1",
		Request: request.CreateUploadRequest{
			FileName:       "report.pdf",
			ContentType:    "application/pdf",
			SizeBytes:      1024,
			IdempotencyKey: "request-1",
		},
	}

	first, err := svc.Create(context.Background(), input)
	if err != nil {
		t.Fatalf("first request: %v", err)
	}

	second, err := svc.Create(context.Background(), input)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}

	if !second.Reused {
		t.Fatal("expected retry to reuse the original upload")
	}

	if first.Upload.ID != second.Upload.ID {
		t.Fatal("retry created a different upload ID")
	}
}

func TestCreateUploadRejectsIdempotencyConflict(t *testing.T) {
	repo := memory.NewUploadRepository()

	svc := uploadservice.NewService(
		repo,
		&fakeObjectStorage{},
		"test-bucket",
		1024*1024,
		time.Minute,
	)

	input := uploadservice.CreateInput{
		TenantID: "tenant-1",
		OwnerID:  "user-1",
		Request: request.CreateUploadRequest{
			FileName:       "report.pdf",
			ContentType:    "application/pdf",
			SizeBytes:      1024,
			IdempotencyKey: "request-1",
		},
	}

	if _, err := svc.Create(context.Background(), input); err != nil {
		t.Fatalf("first request: %v", err)
	}

	input.Request.SizeBytes = 2048

	_, err := svc.Create(context.Background(), input)
	if !errors.Is(err, uploadservice.ErrIdempotencyConflict) {
		t.Fatalf("expected idempotency conflict, got %v", err)
	}
}

func TestCreateUploadRejectsOversizedFile(t *testing.T) {
	repo := memory.NewUploadRepository()

	svc := uploadservice.NewService(
		repo,
		&fakeObjectStorage{},
		"test-bucket",
		1024,
		time.Minute,
	)

	input := uploadservice.CreateInput{
		TenantID: "tenant-1",
		OwnerID:  "user-1",
		Request: request.CreateUploadRequest{
			FileName:       "large.bin",
			ContentType:    "application/octet-stream",
			SizeBytes:      2048,
			IdempotencyKey: "request-2",
		},
	}

	_, err := svc.Create(context.Background(), input)
	if !errors.Is(err, uploadservice.ErrUploadTooLarge) {
		t.Fatalf("expected size limit error, got %v", err)
	}
}

func TestCreateUploadRejectsCancelledContext(t *testing.T) {
	repo := memory.NewUploadRepository()

	svc := uploadservice.NewService(
		repo,
		&fakeObjectStorage{},
		"test-bucket",
		1024,
		time.Minute,
	)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	input := uploadservice.CreateInput{
		TenantID: "tenant-1",
		OwnerID:  "user-1",
		Request: request.CreateUploadRequest{
			FileName:       "report.pdf",
			ContentType:    "application/pdf",
			SizeBytes:      100,
			IdempotencyKey: "request-3",
		},
	}

	_, err := svc.Create(ctx, input)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context cancellation, got %v", err)
	}
}
