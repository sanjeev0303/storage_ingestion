package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
	storageregistry "github.com/sanjeev0303/object-storage-platform/internal/infrastructure/storage/registry"
	s3storage "github.com/sanjeev0303/object-storage-platform/internal/infrastructure/storage/s3"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/config"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/logger"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/metrics"
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
	once := flag.Bool("once", false, "run a single reconciliation cycle, print its report as JSON and exit")
	dryRun := flag.Bool("dry-run", false, "report what would be done without changing storage or the database")
	flag.Parse()

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}
	workerCfg, err := config.LoadReconciliation()
	if err != nil {
		return fmt.Errorf("load reconciliation configuration: %w", err)
	}

	log := logger.New(cfg.AppEnv)
	if *once {
		// stdout carries the JSON report; logs go to stderr.
		log = logger.NewTo(os.Stderr, cfg.AppEnv)
	}

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

	platformStorage := s3storage.NewObjectStorage(s3Client)
	// UPLOAD_* limits are shared with the API, so every process judges sessions alike.
	multipartService, err := uploadservice.NewMultipartService(
		postgres.NewUploadRepository(db),
		postgres.NewMultipartUploadRepository(db),
		platformStorage,
		uploadservice.MultipartServiceConfig{
			Bucket:     bucket,
			MaxSize:    cfg.UploadMaxSize,
			SessionTTL: cfg.UploadSessionTTL,
			PartURLTTL: cfg.UploadURLTTL,
		},
	)
	if err != nil {
		return fmt.Errorf("create multipart service: %w", err)
	}

	owner, err := workerID()
	if err != nil {
		return fmt.Errorf("create worker ID: %w", err)
	}

	registry := metrics.New()
	// Records bound to tenants' storage connections are reached through
	// those connections (Phase 11); without BYOS they do not resolve.
	storageCfg, err := config.LoadStorage()
	if err != nil {
		return fmt.Errorf("load storage connection configuration: %w", err)
	}
	router, err := storageregistry.ForProcess(storageCfg, platformStorage, bucket, bucket, "assets",
		postgres.NewStorageConnectionRepository(db), registry)
	if err != nil {
		return fmt.Errorf("configure storage connections: %w", err)
	}
	multipartService.Route(nil, router)

	if workerCfg.MetricsAddr != "" {
		registry.Publish("reconciliation")
		// expvar serves /debug/vars on the default mux.
		go func() {
			if err := http.ListenAndServe(workerCfg.MetricsAddr, nil); err != nil {
				log.Error("metrics listener stopped", "error", err)
			}
		}()
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
			DryRun:           workerCfg.DryRun || *dryRun,
			ScanPageSize:     workerCfg.ScanPageSize,
			ScanPages:        workerCfg.ScanPages,
			OrphanMinAge:     workerCfg.OrphanMinAge,
			StuckAfter:       workerCfg.StuckAfter,
			SessionScan:      workerCfg.SessionScan,
			SessionLookback:  workerCfg.SessionLookback,
			DeleteUnrecorded: workerCfg.DeleteUnrecorded,
		},
		log,
		registry,
	)
	if err != nil {
		return fmt.Errorf("create reconciliation service: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *once {
		stats, err := service.RunOnce(ctx)
		report, _ := json.MarshalIndent(stats, "", "  ")
		fmt.Println(string(report))
		if err != nil {
			return fmt.Errorf("reconciliation cycle: %w", err)
		}
		return nil
	}

	log.Info("reconciliation worker starting",
		"worker_id", owner,
		"poll_interval", workerCfg.PollInterval.String(),
		"batch_size", workerCfg.BatchSize,
		"lease", workerCfg.LeaseDuration.String(),
		"max_attempts", workerCfg.MaxAttempts,
		"dry_run", workerCfg.DryRun || *dryRun,
		"session_scan", workerCfg.SessionScan,
		"delete_unrecorded", workerCfg.DeleteUnrecorded,
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
