package reconciliation

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	storagememory "github.com/sanjeev0303/object-storage-platform/internal/infrastructure/storage/memory"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/retry"
	"github.com/sanjeev0303/object-storage-platform/internal/repository/memory"
	reconciliationservice "github.com/sanjeev0303/object-storage-platform/internal/service/reconciliation"
	uploadservice "github.com/sanjeev0303/object-storage-platform/internal/service/upload"
)

func TestWorkerStopsOnShutdown(t *testing.T) {
	uploads := memory.NewUploadRepository()
	multipart, err := uploadservice.NewMultipartService(uploads, memory.NewMultipartUploadRepository(), storagememory.NewObjectStorage(),
		uploadservice.MultipartServiceConfig{Bucket: "b", MaxSize: 1 << 20, SessionTTL: time.Hour, PartURLTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	service, err := reconciliationservice.NewService(memory.NewReconciliationRepository(uploads), multipart, reconciliationservice.Config{
		Owner: "w", StorageProvider: "s3", BatchSize: 1, Lease: time.Minute, Grace: time.Minute,
		Retry:        retry.Policy{BaseDelay: time.Second, MaxDelay: time.Minute, MaxAttempts: 3},
		ScanPageSize: 10, ScanPages: 1, OrphanMinAge: time.Hour, StuckAfter: time.Hour,
	}, log, nil)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		Run(ctx, service, time.Hour, log)
		close(done)
	}()
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the worker did not stop after shutdown was requested")
	}
}
