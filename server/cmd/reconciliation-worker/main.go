package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
	s3storage "github.com/sanjeev0303/object-storage-platform/internal/infrastructure/storage/s3"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/config"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/logger"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/retry"
	reconciliationservice "github.com/sanjeev0303/object-storage-platform/internal/service/reconciliation"
	uploadservice "github.com/sanjeev0303/object-storage-platform/internal/service/upload"
	reconciliationworker "github.com/sanjeev0303/object-storage-platform/internal/worker/reconciliation"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "reconciliation worker startup failed:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}
	workerCfg, err := config.LoadReconciliation()
	if err != nil {
		return fmt.Errorf("load reconciliation configuration: %w", err)
	}

	log := logger.New(cfg.AppEnv)

	startupCtx, cancelStartup := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelStartup()

	db, err := postgres.Open(startupCtx, cfg.DatabaseUrl)
	if err != nil {
		return fmt.Errorf("connect to PostgreSQL: %w", err)
	}
	defer db.Close()

	storageConfig, err := s3storage.LoadConfig()
	if err != nil {
		return fmt.Errorf("load object storage configuration: %w", err)
	}
	s3Client, err := s3storage.NewClient(startupCtx, storageConfig)
	if err != nil {
		return fmt.Errorf("initialize S3 client: %w", err)
	}

	bucket := os.Getenv("S3_BUCKET")
	if bucket == "" {
		return fmt.Errorf("S3_BUCKET is required")
	}

	// The same limits as the API, so both judge sessions alike.
	multipartService, err := uploadservice.NewMultipartService(
		postgres.NewUploadRepository(db),
		postgres.NewMultipartUploadRepository(db),
		s3storage.NewObjectStorage(s3Client),
		uploadservice.MultipartServiceConfig{
			Bucket:     bucket,
			MaxSize:    5 * 1024 * 1024 * 1024,
			SessionTTL: 30 * time.Minute,
			PartURLTTL: 15 * time.Minute,
		},
	)
	if err != nil {
		return fmt.Errorf("create multipart service: %w", err)
	}

	owner, err := workerID()
	if err != nil {
		return fmt.Errorf("create worker ID: %w", err)
	}

	service, err := reconciliationservice.NewService(
		postgres.NewReconciliationRepository(db),
		multipartService,
		reconciliationservice.Config{
			Owner:           owner,
			StorageProvider: workerCfg.StorageProvider,
			BatchSize:       workerCfg.BatchSize,
			Lease:           workerCfg.LeaseDuration,
			Grace:           workerCfg.GracePeriod,
			Retry: retry.Policy{
				BaseDelay:   workerCfg.BackoffBase,
				MaxDelay:    workerCfg.BackoffMax,
				MaxAttempts: workerCfg.MaxAttempts,
			},
		},
		log,
	)
	if err != nil {
		return fmt.Errorf("create reconciliation service: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Info("reconciliation worker starting",
		"worker_id", owner,
		"poll_interval", workerCfg.PollInterval.String(),
		"batch_size", workerCfg.BatchSize,
		"lease", workerCfg.LeaseDuration.String(),
		"max_attempts", workerCfg.MaxAttempts,
	)
	reconciliationworker.Run(ctx, service, workerCfg.PollInterval, log)
	log.Info("reconciliation worker stopped")

	return nil
}

// workerID names this process on the leases it takes.
func workerID() (string, error) {
	host, err := os.Hostname()
	if err != nil {
		host = "unknown-host"
	}

	var suffix [4]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", err
	}

	return fmt.Sprintf("%s-%d-%s", host, os.Getpid(), hex.EncodeToString(suffix[:])), nil
}
