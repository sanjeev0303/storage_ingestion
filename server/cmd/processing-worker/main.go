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
	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/scanner/clamav"
	s3storage "github.com/sanjeev0303/object-storage-platform/internal/infrastructure/storage/s3"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/config"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/logger"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/retry"
	processingservice "github.com/sanjeev0303/object-storage-platform/internal/service/processing"
	"github.com/sanjeev0303/object-storage-platform/internal/service/promotion"
	uploadservice "github.com/sanjeev0303/object-storage-platform/internal/service/upload"
	processingworker "github.com/sanjeev0303/object-storage-platform/internal/worker/processing"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "processing worker startup failed:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}
	workerCfg, err := config.LoadProcessing()
	if err != nil {
		return fmt.Errorf("load processing configuration: %w", err)
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
	storage := s3storage.NewObjectStorage(s3Client)

	bucket := os.Getenv("S3_BUCKET")
	if bucket == "" {
		return fmt.Errorf("S3_BUCKET is required")
	}
	destinationBucket := workerCfg.PromotionBucket
	if destinationBucket == "" {
		destinationBucket = bucket
	}

	uploads := postgres.NewUploadRepository(db)
	assets := postgres.NewAssetRepository(db)

	// Verification reuses the upload service's integrity check.
	multipartService, err := uploadservice.NewMultipartService(
		uploads,
		postgres.NewMultipartUploadRepository(db),
		storage,
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

	promoter, err := promotion.NewService(assets, storage, promotion.Config{
		SourceBucket:      bucket,
		DestinationBucket: destinationBucket,
		Prefix:            workerCfg.PromotionPrefix,
	})
	if err != nil {
		return fmt.Errorf("create promotion service: %w", err)
	}

	deps := processingservice.Dependencies{
		Jobs:      postgres.NewProcessingJobRepository(db),
		Assets:    assets,
		Uploads:   uploads,
		Verifier:  multipartService,
		Storage:   storage,
		Validator: processingservice.ContentValidator{Allowed: workerCfg.AllowedTypes},
		Promoter:  promoter,
	}
	if workerCfg.Scanner == "clamav" {
		deps.Scanner = clamav.New(workerCfg.ClamAVAddress, workerCfg.ClamAVTimeout)
	}

	owner, err := workerID()
	if err != nil {
		return fmt.Errorf("create worker ID: %w", err)
	}

	service, err := processingservice.NewService(deps, processingservice.Config{
		Owner:        owner,
		Bucket:       bucket,
		BatchSize:    workerCfg.BatchSize,
		Concurrency:  workerCfg.Concurrency,
		Lease:        workerCfg.LeaseDuration,
		MaxFileBytes: workerCfg.MaxFileBytes,
		ScanPolicy:   processingservice.ScanPolicy(workerCfg.ScanPolicy),
		Retry: retry.Policy{
			BaseDelay:   workerCfg.BackoffBase,
			MaxDelay:    workerCfg.BackoffMax,
			MaxAttempts: workerCfg.MaxAttempts,
		},
	}, log)
	if err != nil {
		return fmt.Errorf("create processing service: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Info("processing worker starting",
		"worker_id", owner,
		"scanner", workerCfg.Scanner,
		"scan_unavailable_policy", workerCfg.ScanPolicy,
		"concurrency", workerCfg.Concurrency,
		"batch_size", workerCfg.BatchSize,
		"lease", workerCfg.LeaseDuration.String(),
	)
	if workerCfg.Scanner == "none" {
		log.Warn("no malware scanner configured; assets will not be promoted unless the policy allows it",
			"scan_unavailable_policy", workerCfg.ScanPolicy)
	}

	processingworker.Run(ctx, service, workerCfg.PollInterval, log)
	log.Info("processing worker stopped")
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
